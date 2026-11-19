//go:build integration

package transcode

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// A sweep needs no connection beside the one that holds its lock: with a pool of
// one, the smallest there is, it still removes what is over the budget. A sweep
// that asks the pool for a second connection while it holds the first waits for
// itself, and with a pool of 2 beside the hint listener's connection it starves
// every other call of the process too.
func TestASweepCompletesOnAPoolOfOneConnection(t *testing.T) {
	st := store.NewPosix(t.TempDir())
	r := newRig(t, st)
	hash := hex32("a")
	r.seedSpec(t, st, hash, []int64{1000, 1000, 1000}, 3*time.Hour)
	pool, err := db.Open(context.Background(), r.dsn, db.WithMaxConns(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	log, _ := newLogs()
	w := r.sweeper(st, 1, log)
	w.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if left := keysUnder(t, st, "segments/"+hash); len(left) != 0 {
		t.Errorf("the sweep left %v of a spec over the budget", left)
	}
}

// The asset sweep holds a lock for each object it deletes, an asset lock for a
// font and the scan locks of the store's libraries for an image, and asks
// Postgres whether the object is wanted meanwhile: on a pool of one that must
// not wait for a second connection.
func TestTheAssetSweepCompletesOnAPoolOfOneConnection(t *testing.T) {
	tg := targets(t)[0].fresh(t)
	e := newAssetEnv(t, tg)
	font, image := subtitles.FontKey(fontSum("none")), "images/"+uuid.NewString()+"/tag.jpg"
	putSized(t, e.st, font, 10)
	putSized(t, e.st, image, 10)
	e.cache.Put(tg.uri, e.st)
	pool, err := db.Open(context.Background(), e.r.dsn, db.WithMaxConns(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := e.worker()
	w.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	e.expect([]string{font}, false, "a font no manifest names")
	e.expect([]string{image}, false, "an image no item names")
}

// An asset sweep takes its delete locks on one connection however many objects
// it deletes: a connection for each would make a pass over thousands of orphans
// connect and authenticate thousands of times. So the connections a sweep opens
// are the same for 2 orphans of each kind and for 15.
func TestTheAssetSweepOpensOneLockConnectionWhateverTheOrphans(t *testing.T) {
	opened := func(orphans int) int {
		tg := targets(t)[0].fresh(t)
		e := newAssetEnv(t, tg)
		var keys []string
		for i := range orphans {
			keys = append(keys, subtitles.FontKey(fontSum(fmt.Sprint("orphan", i))), "images/"+uuid.NewString()+"/tag.jpg")
		}
		for _, key := range keys {
			putSized(t, e.st, key, 10)
		}
		e.cache.Put(tg.uri, e.st)
		proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
		pool, err := db.Open(context.Background(), proxy.DSN(), db.WithMaxConns(1))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		w := e.worker()
		w.Pool = pool
		before := proxy.Accepted()
		if err := w.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.expect(keys, false, "an orphan")
		return proxy.Accepted() - before
	}
	small, large := opened(2), opened(15)
	t.Logf("connections opened: %d for 2 orphans of each kind, %d for 15", small, large)
	if small == 0 {
		t.Fatal("the sweep opened no connection for its locks: the counter sees none")
	}
	if large > small {
		t.Errorf("an asset sweep opened %d connections for 15 orphans of each kind and %d for 2", large, small)
	}
}
