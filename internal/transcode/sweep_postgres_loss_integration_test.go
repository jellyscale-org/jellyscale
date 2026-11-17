//go:build integration

package transcode

import (
	"context"
	"regexp"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-078, design review: a sweep of the transcode store that loses Postgres
// after it has listed the store removes nothing (the spec's lock is a Postgres
// lock, and with the connection gone nothing says the spec is still unwatched).
// As for the asset stores, the sweep returns, or waits for Postgres, which it
// is given back after the count of what it deleted.
func TestASegmentSweepThatLosesPostgresAfterListingRemovesNothing(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t)
			st := tg.open(t)
			r := newRig(t, st)
			r.seedSpec(t, st, hex32("a"), []int64{1000, 1000, 1000}, 3*time.Hour)
			r.seedSpec(t, st, hex32("b"), []int64{1000, 1000, 1000}, 25*time.Hour) // Past its age too.
			proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
			pool, err := db.Open(context.Background(), proxy.DSN())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			spy := &spyStore{Store: st}
			log, logged := newLogs()
			w := r.sweeper(spy, 1, log)
			w.Pool = pool
			spy.afterList = func() { proxy.Away() }
			result := make(chan error, 1)
			go func() { result <- w.Sweep(context.Background()) }()
			returned := false
			select {
			case <-result:
				returned = true
			case <-time.After(5 * time.Second): // A sweep that waits for Postgres is let off here.
			}
			if n := spy.deleteCount(); n != 0 {
				t.Errorf("a sweep without Postgres deleted %d objects", n)
			}
			proxy.Back()
			if !returned {
				<-result
			}
			if returned && !regexp.MustCompile(`level=(WARN|ERROR)`).MatchString(logged.String()) {
				t.Errorf("a sweep that could not reach Postgres neither failed nor logged why:\n%s", logged)
			}
		})
	}
}
