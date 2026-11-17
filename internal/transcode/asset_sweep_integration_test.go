//go:build integration

package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/media"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-078: the asset store's sweep. A worker's Sweep removes from each library's
// asset store what no version of any file needs: the subtitle text (inside the
// film or a file beside it), the .sup copies, the primary image, the fonts
// manifest and the chapter pictures of a file that was removed (its item is
// gone, after missingGrace) and of the previous version of a changed file (a
// new ETag, or a new fingerprint of a subtitle file beside the film), and
// the fonts no remaining manifest names. Every key below is made by the
// functions the code uses (subtitles.Key, SupKey, FontsKey, FontKey,
// library.ChapterDir and ChapterKey, and the scan's images/<item>/<tag>), so a
// change of a key's shape changes the fixture with it.

type assetEnv struct {
	t      *testing.T
	r      *rig
	tg     target
	st     store.Store
	cache  *store.Cache
	lib    string
	log    *slog.Logger
	logged *logs
}

func newAssetEnv(t *testing.T, tg target) *assetEnv {
	t.Helper()
	st := tg.open(t)
	r := newRig(t, store.NewPosix(t.TempDir())) // The transcode store: empty.
	if _, err := r.pool.Exec(context.Background(), "UPDATE libraries SET asset_store_uri = $1", tg.uri); err != nil {
		t.Fatal(err)
	}
	e := &assetEnv{t: t, r: r, tg: tg, st: st, cache: &store.Cache{}}
	e.log, e.logged = newLogs()
	if err := r.pool.QueryRow(context.Background(), "SELECT id::text FROM libraries").Scan(&e.lib); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *assetEnv) worker() *Worker {
	return &Worker{Pool: e.r.pool, Segments: e.r.segments, Log: e.log, Media: e.cache.Open, Retention: 24 * time.Hour}
}

func (e *assetEnv) sweep() error { return e.worker().Sweep(context.Background()) }

func (e *assetEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.r.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

type assetFilm struct{ id, library string }

// streams of a film: a text track inside it (0), a PGS track (1), and a
// subtitle file beside it (2) at the given fingerprint.
func streams(fingerprint string) []media.MediaStream {
	return []media.MediaStream{
		{Index: 0, Type: "Subtitle", Codec: "subrip", IsTextSubtitleStream: true},
		{Index: 1, Type: "Subtitle", Codec: "hdmv_pgs_subtitle"},
		{Index: 2, Type: "Subtitle", Codec: "subrip", IsExternal: true, IsTextSubtitleStream: true, Path: "film.en.srt", Fingerprint: fingerprint},
	}
}

// addFilm makes the item and its source, at the etag and streams.
func (e *assetEnv) addFilm(library, name, etag string, st []media.MediaStream) assetFilm {
	e.t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(st)
	if err != nil {
		e.t.Fatal(err)
	}
	e.exec(`INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', $3, $3, 'video:' || $3, 900000000, '{"MediaSources": [{}], "MediaStreams": []}')`, id, library, name)
	e.exec(`INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams) VALUES ($1, $1, $2, 1, $3, 'mkv', $4)`, id, name+".mkv", etag, raw)
	return assetFilm{id, library}
}

// version is every asset of one version of a film, by what it is.
type version struct {
	text, sup, chapters, fonts []string
	manifest, image, tag       string
}

func (v version) all() []string {
	all := append(append(append(append([]string{}, v.text...), v.sup...), v.chapters...), v.fonts...)
	all = append(all, v.manifest, v.image)
	return all
}

func fontSum(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// putVersion stores the assets of the film's version (its etag and streams),
// the fonts it names (by sum; stored if not already), and an image.
func (e *assetEnv) putVersion(f assetFilm, etag string, st []media.MediaStream, fonts ...string) version {
	e.t.Helper()
	src := items.Source{ID: f.id, ETag: etag, Streams: st}
	v := version{
		text:     []string{subtitles.Key(src, 0), subtitles.Key(src, 2)},
		sup:      []string{subtitles.SupKey(src, 1)},
		manifest: subtitles.FontsKey(f.id, etag),
	}
	dir := library.ChapterDir(f.id, etag)
	v.chapters = []string{library.ChapterKey(dir, 0), library.ChapterKey(dir, 1)}
	digest := sha256.Sum256([]byte("image " + f.id + etag))
	v.tag = hex.EncodeToString(digest[:16])
	v.image = fmt.Sprintf("images/%s/%s", f.id, v.tag)
	named := map[int]string{}
	for i, sum := range fonts {
		named[3+i] = sum
		v.fonts = append(v.fonts, subtitles.FontKey(sum))
	}
	body, err := json.Marshal(named)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, key := range append(append(append(append([]string{}, v.text...), v.sup...), v.chapters...), v.fonts...) {
		putSized(e.t, e.st, key, 10)
	}
	putSized(e.t, e.st, v.image, 10)
	if err := e.st.Put(context.Background(), v.manifest, strings.NewReader(string(body))); err != nil {
		e.t.Fatal(err)
	}
	return v
}

// current makes v the version the item shows, as the scan does.
func (e *assetEnv) current(f assetFilm, v version) {
	e.exec("UPDATE items SET primary_image_key = $2, primary_image_tag = $3 WHERE id = $1", f.id, v.image, v.tag)
}

// remove deletes the item, as the first scan after missingGrace does, with its source.
func (e *assetEnv) remove(f assetFilm) { e.exec("DELETE FROM items WHERE id = $1", f.id) }

func (e *assetEnv) expect(keys []string, present bool, why string) {
	e.t.Helper()
	for _, key := range keys {
		_, err := e.st.Stat(context.Background(), key)
		switch {
		case present && err != nil:
			e.t.Errorf("%s: %s is gone (%v)", why, key, err)
		case !present && err == nil:
			e.t.Errorf("%s: %s is still there", why, key)
		case !present && !errors.Is(err, store.ErrNotFound):
			e.t.Errorf("%s: asking for %s: %v", why, key, err)
		}
	}
}

func TestTheAssetSweepRemovesWhatNoVersionNeedsAndKeepsTheRest(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			shared, onlyK, onlyR, old, current, hidden, orphan := fontSum("shared"), fontSum("k"), fontSum("r"), fontSum("old"), fontSum("new"), fontSum("h"), fontSum("none")

			// A film that stays: its current version, and a font a removed film names too.
			kept := e.addFilm(e.lib, "kept", "k1", streams("fp"))
			keptV := e.putVersion(kept, "k1", streams("fp"), shared, onlyK)
			e.current(kept, keptV)

			// A film whose item was removed after missingGrace.
			removed := e.addFilm(e.lib, "removed", "r1", streams("fp"))
			removedV := e.putVersion(removed, "r1", streams("fp"), shared, onlyR)
			e.remove(removed)

			// A film replaced by a new file (new ETag): the old version goes, the new stays.
			changed := e.addFilm(e.lib, "changed", "c2", streams("fp"))
			oldV := e.putVersion(changed, "c1", streams("fp"), old)
			newV := e.putVersion(changed, "c2", streams("fp"), current)
			e.current(changed, newV)

			// A subtitle file beside the film edited (same file, new fingerprint): the old text goes.
			edited := e.addFilm(e.lib, "edited", "e1", streams("fp2"))
			editedOld := e.putVersion(edited, "e1", streams("fp1"))
			editedNew := e.putVersion(edited, "e1", streams("fp2"))
			e.current(edited, editedNew)
			oldText := subtitles.Key(items.Source{ID: edited.id, ETag: "e1", Streams: streams("fp1")}, 2)
			newText := subtitles.Key(items.Source{ID: edited.id, ETag: "e1", Streams: streams("fp2")}, 2)
			if oldText == newText {
				t.Fatal("the fingerprint is not in the key of a subtitle file beside the film")
			}

			// A file hidden as missing, inside missingGrace: everything stays for its return.
			away := e.addFilm(e.lib, "away", "h1", streams("fp"))
			awayV := e.putVersion(away, "h1", streams("fp"), hidden)
			e.current(away, awayV)
			e.exec("UPDATE items SET missing_since = now() WHERE id = $1", away.id)

			// A font no manifest names.
			putSized(t, e.st, subtitles.FontKey(orphan), 10)

			spy := &spyStore{Store: e.st}
			e.cache.Put(tg.uri, spy)
			if err := e.sweep(); err != nil {
				t.Fatal(err)
			}

			e.expect(keptV.all(), true, "the current version of a film that stays")
			e.expect(newV.all(), true, "the current version of a changed film")
			e.expect(append(editedNew.all(), newText), true, "the current version of a film whose subtitle file was edited")
			e.expect(awayV.all(), true, "a file hidden within missingGrace")

			e.expect(removedV.text, false, "subtitle text of a removed file")
			e.expect(removedV.sup, false, ".sup of a removed file")
			e.expect([]string{removedV.image}, false, "the image of a removed file")
			e.expect(removedV.chapters, false, "chapter pictures of a removed file")
			e.expect([]string{removedV.manifest, subtitles.FontKey(onlyR)}, false, "the fonts manifest and a font of a removed file")
			e.expect([]string{subtitles.FontKey(shared)}, true, "a font a removed film and a kept film share")

			e.expect(oldV.text, false, "subtitle text of the previous version")
			e.expect(oldV.sup, false, ".sup of the previous version")
			e.expect([]string{oldV.image}, false, "the image of the previous version")
			e.expect(oldV.chapters, false, "chapter pictures of the previous version")
			e.expect([]string{oldV.manifest, subtitles.FontKey(old)}, false, "the manifest and a font of the previous version")

			e.expect([]string{oldText}, false, "the old text of a subtitle file that was edited")
			e.expect(editedOld.chapters, true, "the chapter pictures of the same version of the edited film")

			e.expect([]string{subtitles.FontKey(orphan)}, false, "a font that no manifest names")

			// The log says how many objects of each kind went, and they add up to what was deleted.
			total := 0
			for _, kind := range []string{"subtitle", "sup", "image", "font", "chapter", "manifest"} {
				match := regexp.MustCompile(`\b`+kind+`\w*=(\d+)`).FindAllStringSubmatch(e.logged.String(), -1)
				for _, m := range match {
					n, _ := strconv.Atoi(m[1])
					total += n
				}
				if kind != "manifest" && len(match) == 0 {
					t.Errorf("the log does not say how many %s objects were removed:\n%s", kind, e.logged)
				}
			}
			if deleted := spy.deleteCount(); total != deleted {
				t.Errorf("the log counts %d objects removed, and %d were deleted:\n%s", total, deleted, e.logged)
			}

			// A second sweep has nothing to do, and nothing that stays goes.
			deleted := spy.deleteCount()
			if err := e.sweep(); err != nil {
				t.Fatal(err)
			}
			if n := spy.deleteCount() - deleted; n != 0 {
				t.Errorf("a second sweep deleted %d more objects", n)
			}
		})
	}
}

// Two libraries that share one asset store: neither's current assets go, and
// the removed film's do. A third library has a store of its own with its own
// removed film. Each store is listed once.
func TestTwoLibrariesSharingAnAssetStoreKeepEachOthersAssetsAndEachStoreIsListedOnce(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			shared := e.st
			privateDir := t.TempDir()
			private, privateURI := store.NewPosix(privateDir), "file://"+privateDir
			other, third := uuid.NewString(), uuid.NewString()
			e.exec(`INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'second', 'movies', 'file:///x', $2), ($3, 'third', 'movies', 'file:///y', $4)`,
				other, tg.uri, third, privateURI)

			a := e.addFilm(e.lib, "a", "a1", streams("fp"))
			av := e.putVersion(a, "a1", streams("fp"))
			e.current(a, av)
			b := e.addFilm(other, "b", "b1", streams("fp"))
			bv := e.putVersion(b, "b1", streams("fp"))
			e.current(b, bv)
			gone := e.addFilm(e.lib, "gone", "g1", streams("fp"))
			goneV := e.putVersion(gone, "g1", streams("fp"))
			e.remove(gone)

			e.st = private
			c := e.addFilm(third, "c", "c1", streams("fp"))
			cv := e.putVersion(c, "c1", streams("fp"))
			e.current(c, cv)
			lost := e.addFilm(third, "lost", "l1", streams("fp"))
			lostV := e.putVersion(lost, "l1", streams("fp"))
			e.remove(lost)
			e.st = shared

			spyShared, spyPrivate := &spyStore{Store: shared}, &spyStore{Store: private}
			e.cache.Put(tg.uri, spyShared)
			e.cache.Put(privateURI, spyPrivate)
			if err := e.sweep(); err != nil {
				t.Fatal(err)
			}
			e.expect(av.all(), true, "the first library's current assets")
			e.expect(bv.all(), true, "the second library's current assets, in the same store")
			e.expect(goneV.all(), false, "the assets of a film removed from the first library")
			e.st = private
			e.expect(cv.all(), true, "the third library's current assets")
			e.expect(lostV.all(), false, "the assets of a film removed from the third library")
			if spyShared.listCount() != 1 || spyPrivate.listCount() != 1 {
				t.Errorf("a sweep listed the shared asset store %d times and the private one %d times, once each", spyShared.listCount(), spyPrivate.listCount())
			}
		})
	}
}

// A job that is queued or running is writing: what it writes is not removed
// though it belongs to a version no item shows any more, and goes after the
// job has ended. The fonts job puts the fonts first and the manifest last, so a
// font no manifest names yet stays while a fonts job runs, anywhere.
func TestTheAssetSweepLeavesWhatARunningJobIsWritingAndRemovesItWhenTheJobHasEnded(t *testing.T) {
	type jobCase struct {
		name, kind string
		key        func(f assetFilm, old version) string
		writing    func(old version, fontless string) []string
	}
	cases := []jobCase{
		{"a pass over the film's text tracks", "track", func(f assetFilm, _ version) string {
			return subtitles.PassKey(items.Source{ID: f.id, ETag: "o1"})
		}, func(old version, _ string) []string { return old.text[:1] }},
		{"a subtitle file beside the film", "track", func(_ assetFilm, old version) string { return old.text[1] },
			func(old version, _ string) []string { return old.text[1:] }},
		{"a .sup copy", "sup", func(_ assetFilm, old version) string { return old.sup[0] },
			func(old version, _ string) []string { return old.sup }},
		{"the fonts of a film", "fonts", func(f assetFilm, old version) string { return old.manifest },
			func(old version, fontless string) []string {
				return []string{old.manifest, subtitles.FontKey(fontless)}
			}},
		{"chapter pictures", "chapters", func(f assetFilm, _ version) string { return "chapters/" + f.id },
			func(old version, _ string) []string { return old.chapters }},
	}
	for _, tg := range targets(t) {
		for _, tc := range cases {
			t.Run(tg.name+"/"+tc.name, func(t *testing.T) {
				tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
				e := newAssetEnv(t, tg)
				f := e.addFilm(e.lib, "f", "n2", streams("fp"))
				old := e.putVersion(f, "o1", streams("fp"))
				now := e.putVersion(f, "n2", streams("fp"))
				e.current(f, now)
				fontless := fontSum("written before its manifest")
				putSized(t, e.st, subtitles.FontKey(fontless), 10)
				if tc.kind == "chapters" {
					e.exec("INSERT INTO chapter_images (source_id, library_id, etag, state, attempts) VALUES ($1, $2, 'o1', 'making', 1)", f.id, e.lib)
				}
				e.exec(`INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, lease_expires_at, attempts)
					VALUES ($1, $2, $3, $3, 0, 'running', 'w', now() + interval '1 hour', 1)`, tc.key(f, old), tc.kind, f.id)

				if err := e.sweep(); err != nil {
					t.Fatal(err)
				}
				e.expect(tc.writing(old, fontless), true, "what a running "+tc.kind+" job is writing")
				e.expect(now.all(), true, "the current version")

				e.exec("UPDATE subtitle_jobs SET state = 'done', ended_at = now()")
				if tc.kind == "chapters" {
					e.exec("DELETE FROM chapter_images")
				}
				if err := e.sweep(); err != nil {
					t.Fatal(err)
				}
				e.expect(tc.writing(old, fontless), false, "what the "+tc.kind+" job wrote, once it has ended")
				e.expect(now.all(), true, "the current version, after the job ended")
			})
		}
	}
}

// A scan puts an image before it commits the item's key, and the sweep that
// runs at that moment finds an object no item names: it stays. The scan is the
// real one, held after the put of the image. The sweep goes on once it has
// returned, or waits on a lock the scan holds (its session, by name).
func TestTheAssetSweepLeavesAnImageThatARunningScanHasPutAndNotYetCommitted(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			ctx := context.Background()
			libs, err := library.Sync(ctx, e.r.pool, []library.Config{{Name: "scanned", Kind: "movies", Media: "file://" + film(t), Assets: tg.uri}})
			if err != nil {
				t.Fatal(err)
			}
			lib := libs[0]
			real := lib.Assets
			spy := &spyStore{Store: real}
			put, release := make(chan string, 1), make(chan struct{})
			spy.afterPut = func(key string) {
				if strings.HasPrefix(key, "images/") {
					put <- key
					<-release
				}
			}
			lib.Assets = spy
			scanner := &library.Scanner{Pool: e.r.pool, Log: slog.New(slog.DiscardHandler), Grace: time.Hour, MaxLostShare: 1}
			scanned := make(chan error, 1)
			go func() { scanned <- scanner.Scan(ctx, lib) }()
			var key string
			select {
			case key = <-put:
			case err := <-scanned:
				t.Fatalf("the scan ended without putting an image: %v", err)
			case <-time.After(time.Minute):
				t.Fatal("timed out waiting for the scan to put an image")
			}

			e.cache.Put(tg.uri, real)
			sweeper := e.worker()
			sweeper.Pool = openNamed(t, e.r.dsn, "asset-sweeper")
			swept := make(chan error, 1)
			go func() { swept <- sweeper.Sweep(ctx) }()
			sweepDone := make(chan struct{})
			go func() { err := <-swept; swept <- err; close(sweepDone) }()
			e.r.untilEither(t, "the sweep to return or wait on the scan's lock", sweepDone, "asset-sweeper")
			if _, err := real.Stat(ctx, key); err != nil {
				t.Errorf("the sweep removed the image a scan had put and not yet committed: %v", err)
			}
			close(release)
			if err := <-scanned; err != nil {
				t.Fatal(err)
			}
			<-sweepDone
			var named string
			if err := e.r.pool.QueryRow(ctx, "SELECT primary_image_key FROM items WHERE library_id = $1 AND natural_key LIKE 'video:film.%'", lib.ID).Scan(&named); err != nil || named != key {
				t.Fatalf("the item's image is %q (%v), the scan put %q", named, err, key)
			}
			if _, err := real.Stat(ctx, key); err != nil {
				t.Errorf("the committed image is gone: %v", err)
			}
			if err := e.sweep(); err != nil {
				t.Fatal(err)
			}
			if _, err := real.Stat(ctx, key); err != nil {
				t.Errorf("a sweep after the commit removed the item's current image: %v", err)
			}
		})
	}
}

// A listing of the asset store that fails removes nothing from it and says why.
func TestAFailedAssetListingRemovesNothingAndSaysWhy(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			f := e.addFilm(e.lib, "f", "r1", streams("fp"))
			v := e.putVersion(f, "r1", streams("fp"), fontSum("r"))
			e.remove(f)
			spy := &spyStore{Store: e.st}
			spy.failLists(errors.New("the asset store has gone to lunch"))
			e.cache.Put(tg.uri, spy)
			_ = e.sweep()
			if n := spy.deleteCount(); n != 0 {
				t.Errorf("a sweep whose listing failed deleted %d objects", n)
			}
			e.expect(v.all(), true, "an asset of a removed film, with the listing failing")
			if !strings.Contains(e.logged.String(), "the asset store has gone to lunch") {
				t.Errorf("the log does not say why nothing was removed:\n%s", e.logged)
			}
		})
	}
}

// A sweep that loses Postgres after it has listed the store removes nothing
// from it: it cannot tell what is current. (The listing comes before the read
// of what is current: an object put and committed between the two would
// otherwise be listed and not found current, and removed.) The sweep is
// held, and answered, in Postgres's absence: it returns, or waits for
// Postgres, which it is given back after the count of what it deleted.
func TestAnAssetSweepThatLosesPostgresAfterListingRemovesNothing(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			f := e.addFilm(e.lib, "f", "r1", streams("fp"))
			e.putVersion(f, "r1", streams("fp"))
			e.remove(f)
			proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
			pool, err := db.Open(context.Background(), proxy.DSN())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			spy := &spyStore{Store: e.st}
			spy.afterList = func() { proxy.Away() }
			e.cache.Put(tg.uri, spy)
			w := e.worker()
			w.Pool = pool
			result := make(chan error, 1)
			go func() { result <- w.Sweep(context.Background()) }()
			var sweepErr error
			returned := false
			select {
			case sweepErr = <-result:
				returned = true
			case <-time.After(5 * time.Second): // A sweep that waits for Postgres is let off here; a returned one needs no wait.
			}
			if n := spy.deleteCount(); n != 0 {
				t.Errorf("a sweep without Postgres deleted %d objects", n)
			}
			proxy.Back()
			if !returned {
				<-result
			}
			if returned && sweepErr == nil && !regexp.MustCompile(`level=(WARN|ERROR)`).MatchString(e.logged.String()) {
				t.Errorf("a sweep that could not reach Postgres neither failed nor logged why:\n%s", e.logged)
			}
		})
	}
}

// The number of Postgres queries of an asset sweep that read or lock the
// library does not grow with the number of objects in the store: ten films and
// sixty, with as many removed and changed, cost the same. What does grow is the
// one keyed question about each object before it is deleted (a scan's lock, an
// item's image, a source's row or jobs, a fonts job), which is not counted here
// (TestTheAssetSweepRereadsNoLibraryBeforeEachDelete bounds the reads that
// would grow too much). Counted at a proxy that sees every statement (the
// simple protocol sends each one's text).
func TestAnAssetSweepsPostgresQueriesDoNotGrowWithTheObjects(t *testing.T) {
	queries := func(films int) int64 {
		e := newAssetEnv(t, targets(t)[0])
		for i := range films {
			name := fmt.Sprintf("f%d", i)
			kept := e.addFilm(e.lib, name, "k1", streams("fp"))
			e.current(kept, e.putVersion(kept, "k1", streams("fp"), fontSum(name)))
			changed := e.addFilm(e.lib, name+"c", "c2", streams("fp"))
			e.putVersion(changed, "c1", streams("fp"), fontSum(name+"old"))
			e.current(changed, e.putVersion(changed, "c2", streams("fp")))
			gone := e.addFilm(e.lib, name+"g", "g1", streams("fp"))
			e.putVersion(gone, "g1", streams("fp"), fontSum(name+"gone"))
			e.remove(gone)
		}
		proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
		var count atomic.Int64
		proxy.OnStatement(func(sent []byte) bool {
			if len(sent) > 0 && sent[0] == 'Q' && !perObject(string(sent)) {
				count.Add(1)
			}
			return false
		}, nil, nil)
		pool, err := db.Open(context.Background(), withParam(t, proxy.DSN(), "default_query_exec_mode", "simple_protocol"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		w := e.worker()
		w.Pool = pool
		count.Store(0)
		before := len(keysUnder(t, e.st, ""))
		if err := w.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		queried := count.Load()
		// What the sweep must remove: per film, a removed one's 7 objects (+ its 2 fonts
		// less none shared) and a changed one's previous version's.
		if removed := before - len(keysUnder(t, e.st, "")); removed < 2*films*7 {
			t.Fatalf("the sweep removed %d of the %d objects that no version needs", removed, 2*films*7)
		}
		return queried
	}
	small, large := queries(5), queries(40)
	t.Logf("queries: %d for 5 films of each kind, %d for 40", small, large)
	if small == 0 {
		t.Fatal("the sweep asked Postgres nothing: the counter sees no statements")
	}
	if large > small+2 {
		t.Errorf("an asset sweep asked Postgres %d times for 40 films of each kind and %d for 5", large, small)
	}
}

// perObject is a statement of the asset sweep's question about one object
// before it deletes it.
func perObject(sql string) bool {
	for _, part := range []string{"pg_try_advisory_lock", "pg_advisory_unlock", "FROM items WHERE id = ", "FROM media_sources WHERE id = ",
		"FROM subtitle_jobs WHERE source_id = ", "FROM subtitle_jobs WHERE kind = 'fonts'"} {
		if strings.Contains(sql, part) {
			return true
		}
	}
	return false
}
