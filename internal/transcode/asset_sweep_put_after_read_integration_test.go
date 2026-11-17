//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-078: an asset that is put after the asset sweep read its state is never
// deleted by that sweep (QA, 2026-11-17). The sweep lists a store, reads what
// Postgres says is current or being written, and then deletes what it judged an
// orphan; a scan or a job that puts the same key after the read was judged by
// state that no longer holds. The keys are content-addressed or per version, so
// an object of a version that went can sit at the very key a later put uses
// (a file changed back to an earlier version, an image the scan makes again,
// a font a later film carries).
//
// Forced: the sweep is held after its state read (before its first delete: at
// its own database connection, once the last statement of the read has been
// answered) or after its first delete (the store's Delete has returned), the
// writer puts and commits what the real scan or job does, and then the sweep is
// released. Nothing here waits on a timing: the writer acts while the sweep is
// held, and the sweep goes on after.
//
// What the hold at the state read could hide: it is the statement that
// readAssetState ends with (SELECT kind, source_id FROM subtitle_jobs); a build
// that reads the state in another order is held in the middle of its read,
// where the writer's put is still seen by the rest of it, and the check would
// pass a build that never asks again. Such a build is to be held where its read
// ends, with the check corrected.
//
// Mutants it must fail every run: a sweep that judges once, from its first read
// (the build before this task's last criteria); one that asks again only before
// its first delete.

// raceWriter is what a scan or a job does after the sweep read its state.
type raceWriter struct {
	name string
	// act puts (and commits) the object; it returns the keys that must be in the
	// store after the sweep, and after the next one.
	act func(a *assetRace) []string
	// check is what the thing that names the object must still be able to read.
	check func(t *testing.T, a *assetRace)
}

type assetRace struct {
	t      *testing.T
	e      *assetEnv
	f      assetFilm
	v1, v2 version // The film's previous version (its assets are orphans) and current one.
	gone   version // A removed film's, which nobody puts again.
	font   string  // A font sum of v1 that nothing current names.
	lock   func()  // Releases a scan's library lock the writer holds, if it holds one.
}

// newAssetRace is one film at its current version, with its previous
// version's assets still in the store (every shape, each an orphan to the
// sweep), and a removed film's as well. The film's id sorts after the removed
// film's, so that the sweep removes the removed film's objects first and the
// previous version's later: the object the writer puts is never the first
// deleted, which would be gone before the hold.
func newAssetRace(t *testing.T, tg target) *assetRace {
	t.Helper()
	e := newAssetEnv(t, tg.fresh(t))
	a, b := e.addFilm(e.lib, "a", "n2", streams("fp")), e.addFilm(e.lib, "b", "n2", streams("fp"))
	f, gone := a, b
	if gone.id > f.id {
		f, gone = gone, f
	}
	r := &assetRace{t: t, e: e, f: f, font: fontSum("race font")}
	r.v2 = e.putVersion(f, "n2", streams("fp"))
	e.current(f, r.v2)
	r.v1 = e.putVersion(f, "o1", streams("fp"), r.font)
	r.gone = e.putVersion(gone, "g1", streams("fp"), fontSum("removed film's font"))
	e.remove(gone)
	return r
}

// revert is the scan finding the file at its earlier version again.
func (a *assetRace) revert() {
	a.e.exec("UPDATE media_sources SET object_etag = 'o1' WHERE id = $1", a.f.id)
}

// job runs a job of the film for the writer: queued before it puts, over after.
func (a *assetRace) job(kind, key string, put func()) {
	a.e.exec(`INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, lease_expires_at, attempts)
		VALUES ($1, $2, $3, $3, 1, 'running', 'w', now() + interval '1 hour', 1)`, key, kind, a.f.id)
	put()
	a.e.exec("UPDATE subtitle_jobs SET state = 'done', ended_at = now()")
}

func (a *assetRace) put(key string) {
	a.t.Helper()
	putSized(a.t, a.e.st, key, 10)
}

func (a *assetRace) writers() []raceWriter {
	return []raceWriter{
		{"an image a scan puts", func(a *assetRace) []string {
			a.put(a.v1.image)
			a.e.exec("UPDATE items SET primary_image_key = $2, primary_image_tag = $3 WHERE id = $1", a.f.id, a.v1.image, a.v1.tag)
			return []string{a.v1.image}
		}, func(t *testing.T, a *assetRace) {
			var key string
			if err := a.e.r.pool.QueryRow(context.Background(), "SELECT primary_image_key FROM items WHERE id = $1", a.f.id).Scan(&key); err != nil || key != a.v1.image {
				t.Errorf("the item names %q (%v), not the image the scan put", key, err)
			}
		}},
		{"an image a scan has put and not yet committed", func(a *assetRace) []string {
			conn, err := a.e.r.pool.Acquire(context.Background())
			if err != nil {
				a.t.Fatal(err)
			}
			if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_lock(hashtextextended('scan:' || $1, 0))", a.e.lib); err != nil {
				a.t.Fatal(err)
			}
			a.lock = func() {
				conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended('scan:' || $1, 0))", a.e.lib) //nolint:errcheck
				conn.Release()
			}
			a.put(a.v1.image)
			return []string{a.v1.image}
		}, func(t *testing.T, a *assetRace) {
			// The scan commits after the sweep went on; the image is still there for the item to name.
			a.e.exec("UPDATE items SET primary_image_key = $2, primary_image_tag = $3 WHERE id = $1", a.f.id, a.v1.image, a.v1.tag)
			a.lock()
		}},
		{"a font and the manifest a fonts job puts", func(a *assetRace) []string {
			manifest := subtitles.FontsKey(a.f.id, "n2")
			body, err := json.Marshal(map[int]string{3: a.font})
			if err != nil {
				a.t.Fatal(err)
			}
			a.job("fonts", manifest, func() {
				a.put(subtitles.FontKey(a.font))
				if err := a.e.st.Put(context.Background(), manifest, strings.NewReader(string(body))); err != nil {
					a.t.Fatal(err)
				}
			})
			return []string{subtitles.FontKey(a.font), manifest}
		}, func(t *testing.T, a *assetRace) {
			sums, err := readManifest(context.Background(), a.e.st, subtitles.FontsKey(a.f.id, "n2"))
			if err != nil || len(sums) != 1 || sums[0] != a.font {
				t.Errorf("the manifest names %v (%v), want the font the job put", sums, err)
			}
		}},
		{"a fonts manifest a fonts job puts at an old version's key", func(a *assetRace) []string {
			a.revert()
			body, err := json.Marshal(map[int]string{3: a.font})
			if err != nil {
				a.t.Fatal(err)
			}
			a.job("fonts", a.v1.manifest, func() {
				a.put(subtitles.FontKey(a.font))
				if err := a.e.st.Put(context.Background(), a.v1.manifest, strings.NewReader(string(body))); err != nil {
					a.t.Fatal(err)
				}
			})
			return []string{a.v1.manifest, subtitles.FontKey(a.font)}
		}, nil},
		{"subtitle text a subtitle job puts", func(a *assetRace) []string {
			a.revert()
			a.job("track", a.v1.text[0], func() { a.put(a.v1.text[0]) })
			return []string{a.v1.text[0]}
		}, nil},
		{"a .sup copy a subtitle job puts", func(a *assetRace) []string {
			a.revert()
			a.job("sup", a.v1.sup[0], func() { a.put(a.v1.sup[0]) })
			return []string{a.v1.sup[0]}
		}, nil},
	}
}

// heldSweep runs the sweep of w until it is held at where, calls act, then
// releases it and returns its result.
func (a *assetRace) heldSweep(where string, act func()) error {
	t := a.t
	t.Helper()
	spy := &spyStore{Store: a.e.st}
	a.e.cache.Put(a.e.tg.uri, spy)
	w := a.e.worker()
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	hold := func() { close(reached); <-release }
	if where == "after its state read" {
		proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
		pool, err := db.Open(context.Background(), withParam(t, proxy.DSN(), "default_query_exec_mode", "simple_protocol"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		w.Pool = pool
		proxy.OnStatement(func(sent []byte) bool {
			return len(sent) > 0 && sent[0] == 'Q' && strings.Contains(string(sent), "kind, source_id FROM subtitle_jobs")
		}, nil, hold)
	} else {
		var once sync.Once
		spy.afterDelete = func(string) { once.Do(hold) }
	}
	swept := make(chan error, 1)
	go func() { swept <- w.Sweep(context.Background()) }()
	select {
	case <-reached:
	case err := <-swept:
		t.Fatalf("the sweep ended (%v) without being held %s: nothing was checked", err, where)
	case <-time.After(time.Minute):
		t.Fatalf("timed out waiting to hold the sweep %s", where)
	}
	if where == "after its state read" {
		if n := spy.deleteCount(); n != 0 {
			t.Fatalf("held after its state read, the sweep had deleted %d objects", n)
		}
	}
	act()
	letGo()
	select {
	case err := <-swept:
		return err
	case <-time.After(time.Minute):
		t.Fatal("the released sweep did not return in a minute")
		return nil
	}
}

func TestAnAssetPutAfterTheAssetSweepReadItsStateIsNeverDeletedByIt(t *testing.T) {
	for _, tg := range targets(t) {
		for _, where := range []string{"after its state read", "between two of its deletes"} {
			for _, w := range (&assetRace{}).writers() {
				t.Run(tg.name+"/"+where+"/"+w.name, func(t *testing.T) {
					a := newAssetRace(t, tg)
					var keep []string
					if err := a.heldSweep(where, func() { keep = w.act(a) }); err != nil {
						t.Fatalf("the sweep: %v", err)
					}
					a.e.expect(keep, true, "what was put after the sweep read its state, after that sweep")
					if w.check != nil {
						w.check(t, a)
					}
					// Everything of the film's current version stays, whichever it is by now.
					if err := a.e.sweep(); err != nil {
						t.Fatal(err)
					}
					a.e.expect(keep, true, "what was put after the sweep read its state, after the next sweep")
					// The orphans nobody put again go, by the sweep that was held or by the one after it.
					a.e.expect(a.gone.all(), false, "the removed film's assets, which nobody put again")
				})
			}
		}
	}
}
