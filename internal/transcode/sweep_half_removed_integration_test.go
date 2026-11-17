//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"
)

// A spec a killed sweeper began to remove is finished by the next sweep even
// when what is left is within the budget: the removal of a spec, once begun,
// is not a matter of the budget any more.
func TestAHalfRemovedSpecIsFinishedEvenWhenTheStoreIsBackUnderItsBudget(t *testing.T) {
	for _, tg := range targets(t) {
		for _, budget := range []int64{8500, 100000} { // 8000 bytes are left after the kill.
			t.Run(tg.name, func(t *testing.T) {
				tg := tg.fresh(t)
				st := tg.open(t)
				r := newRig(t, st)
				a, b, c := hex32("a"), hex32("b"), hex32("c")
				three := []int64{1000, 1000, 1000}
				r.seedSpec(t, st, a, three, 3*time.Hour)
				r.seedSpec(t, st, b, three, 2*time.Hour)
				r.seedSpec(t, st, c, three, time.Hour)
				// Over its budget at the start, so that the sweeper begins on spec a.
				killed := startSweeper(t, t.TempDir(), "K", r.sweeperEnv(tg, 6000, "segments:delete:2")...)
				killed.await(t, "deleting-K")
				if err := killed.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				<-killed.done
				if left := len(keysUnder(t, st, "segments/"+a)); left == 0 || left == 3 {
					t.Fatalf("the killed sweeper left %d of 3 objects of the oldest spec", left)
				}
				log, _ := newLogs()
				w := r.sweeper(st, budget, log)
				if err := w.Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
				if left := keysUnder(t, st, "segments/"+a); len(left) != 0 {
					t.Errorf("budget %d: the next sweep left %v of the half-removed spec", budget, left)
				}
				for _, hash := range []string{b, c} {
					if n := len(keysUnder(t, st, "segments/"+hash)); n != 3 {
						t.Errorf("spec %.1s has %d of 3 objects", hash, n)
					}
				}
			})
		}
	}
}
