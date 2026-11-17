//go:build integration

package transcode

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-078 (architecture review of the first checks): asking again before every
// delete must cost a keyed query or a Stat per object, never a read of the
// whole library. A sweep of a store that has never been cleaned starts with
// thousands of films' orphans; a build that re-ran readAssetState before each
// delete would read every media_sources row, every item with an image and take
// every library's scan lock once per object (about 37 ms of Postgres CPU and
// 4.5 MB on the wire per read at 5,000 films).
//
// The mechanism is counted, not timed: the statements the sweep sends through a
// proxy, by what they read. A whole-library read is a statement that reads
// media_sources or items with no WHERE on one row, that reads the streams'
// subtitle array, or that tries every library's scan lock. Mutant it must fail
// every run: readAssetState called before each Delete in sweepAssetStore (here
// 70 orphans make 70 such reads against a bound of 6 statements, two reads of three).
//
// What the simple protocol could hide: the proxy sees each statement's text only
// when the client sends it, so the pool of this check speaks the simple protocol,
// which prepared statements of a build would not change (the text is the same).
func TestTheAssetSweepRereadsNoLibraryBeforeEachDelete(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			e := newAssetEnv(t, tg.fresh(t))
			// Ten removed films, each with every shape of asset: well over fifty orphans.
			var orphans []string
			for i := 0; i < 10; i++ {
				f := e.addFilm(e.lib, "film"+string(rune('a'+i)), "n1", streams("fp"))
				v := e.putVersion(f, "n1", streams("fp"), fontSum("font "+f.id))
				e.remove(f)
				orphans = append(orphans, v.all()...)
			}
			// And one film that stays, so a read of the library has something to read.
			keep := e.addFilm(e.lib, "kept", "n1", streams("fp"))
			e.current(keep, e.putVersion(keep, "n1", streams("fp")))

			proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
			pool, err := db.Open(context.Background(), withParam(t, proxy.DSN(), "default_query_exec_mode", "simple_protocol"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			var wholeLibrary, total atomic.Int32
			proxy.OnStatement(func(sent []byte) bool {
				if len(sent) == 0 || sent[0] != 'Q' {
					return false
				}
				sql := string(sent)
				total.Add(1)
				readsRows := strings.Contains(sql, "FROM media_sources") || strings.Contains(sql, "FROM items")
				whole := strings.Contains(sql, "jsonb_path_query_array") ||
					strings.Contains(sql, "pg_try_advisory_xact_lock") ||
					(readsRows && !strings.Contains(sql, "WHERE")) ||
					strings.Contains(sql, "primary_image_key FROM items")
				if whole {
					wholeLibrary.Add(1)
				}
				return false
			}, nil, nil)

			spy := &spyStore{Store: e.st}
			e.cache.Put(e.tg.uri, spy)
			w := e.worker()
			w.Pool = pool
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := spy.deleteCount(); n < 50 {
				t.Fatalf("the sweep deleted %d objects, want at least 50: the fixture does not make a backlog", n)
			}
			e.expect(orphans, false, "the removed films' assets")
			if n := wholeLibrary.Load(); n > 6 {
				t.Errorf("the asset sweep made %d whole-library reads (of %d statements) for %d deletes; want a bound that does not grow with the deletes (a read of the state is three of them, so at most 6)", n, total.Load(), spy.deleteCount())
			}
			t.Logf("%d statements, %d whole-library reads, %d deletes", total.Load(), wholeLibrary.Load(), spy.deleteCount())
		})
	}
}
