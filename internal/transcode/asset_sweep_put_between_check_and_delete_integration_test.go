//go:build integration

package transcode

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/subtitles"
)

// JS-078: an asset a job puts after the asset sweep asked whether it is wanted,
// and before the sweep deleted it, is not lost (Verify, 2026-11-17). The sweep
// is held inside the store's Delete of an orphan, its question long answered
// ("no job, not the current version"); then the film's file goes back to the
// version that key belongs to, a job of it is running, and the job puts the key
// through the asset store a worker gives it. The job's put does not wait on the
// sweep's Delete here, it is made in the goroutine that the check waits on
// instead: the check releases the sweep as soon as the put has returned or is
// waiting on a lock of the sweep's in Postgres, and then requires the object
// to exist and be readable. Nothing asserts how long anything took.
func TestAJobsPutBetweenTheAssetSweepsQuestionAndItsDeleteIsNotLost(t *testing.T) {
	cases := []struct {
		name string
		kind string
		key  func(a *assetRace) string
	}{
		{"subtitle text", "track", func(a *assetRace) string { return a.v1.text[0] }},
		{"a .sup copy", "sup", func(a *assetRace) string { return a.v1.sup[0] }},
		{"a fonts manifest", "fonts", func(a *assetRace) string { return a.v1.manifest }},
		{"a font", "fonts", func(a *assetRace) string { return subtitles.FontKey(a.font) }},
	}
	for _, tg := range targets(t) {
		for _, c := range cases {
			t.Run(tg.name+"/"+c.name, func(t *testing.T) {
				a := newAssetRace(t, tg)
				key := c.key(a)
				spy := &spyStore{Store: a.e.st}
				a.e.cache.Put(a.e.tg.uri, spy)
				w := a.e.worker()
				reached, release := make(chan struct{}), make(chan struct{})
				var once, releaseOnce sync.Once
				letGo := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(letGo)
				spy.beforeDelete = func(k string) {
					if k == key {
						once.Do(func() { close(reached); <-release })
					}
				}
				swept := make(chan error, 1)
				go func() { swept <- w.Sweep(context.Background()) }()
				select {
				case <-reached:
				case err := <-swept:
					t.Fatalf("the sweep ended (%v) without deleting %s: nothing was checked", err, key)
				case <-time.After(time.Minute):
					t.Fatal("timed out waiting to hold the sweep in its delete")
				}

				// The scan finds the file at the version the key belongs to; its job runs and puts.
				a.revert()
				a.e.exec(`INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, lease_expires_at, attempts)
					VALUES ($1, $2, $3, $3, 1, 'running', 'w', now() + interval '1 hour', 1)`, key, c.kind, a.f.id)
				assets := lockedAssets{a.e.st, a.e.r.pool}
				put := make(chan error, 1)
				go func() { put <- assets.Put(context.Background(), key, strings.NewReader("job's bytes")) }()
				waiting := func() bool {
					var n int
					err := a.e.r.pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted").Scan(&n)
					return err == nil && n > 0
				}
				var putErr error
				var returned bool
				waitFor(t, "the job's put to return or to wait on the sweep's lock", time.Minute, func() bool {
					select {
					case putErr = <-put:
						returned = true
						return true
					default:
						return waiting()
					}
				})
				letGo()
				if !returned {
					putErr = <-put
				}
				if putErr != nil {
					t.Fatalf("the job's put: %v", putErr)
				}
				select {
				case err := <-swept:
					if err != nil {
						t.Fatalf("the sweep: %v", err)
					}
				case <-time.After(time.Minute):
					t.Fatal("the released sweep did not return in a minute")
				}
				body, err := a.e.st.Open(context.Background(), key, 0)
				if err != nil {
					t.Fatalf("%s, which the job put while the sweep was between its question and its delete, is gone: %v", key, err)
				}
				body.Close()
			})
		}
	}
}
