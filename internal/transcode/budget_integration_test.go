//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
)

// JS-078: the transcode store's size budget. Each check runs on the POSIX
// directory and on each S3 target. Sizes are bytes of stored objects, and a
// spec is the init segment and the segments under segments/<hash>/. A spec is
// unwatched when it has no queued or running job and no unstopped viewer who
// asked for it in the last 10 minutes; every spec below is unwatched unless
// the check says otherwise, and the age sweep (24h) never meets the ones that
// were asked for within a day.

type budgetSpec struct {
	hash  string
	sizes []int64
	seen  time.Duration // How long ago a viewer last asked; negative: no viewer row at all.
}

func TestTheBudgetRemovesWholeSpecsLeastRecentlyRequestedFirstAndOnlyAsManyAsItTakes(t *testing.T) {
	a, b, c := hex32("a"), hex32("b"), hex32("c")
	three := []int64{1000, 1000, 1000}
	cases := []struct {
		name   string
		budget int64
		specs  []budgetSpec // In the order they are stored, which is not the order they were requested.
		gone   []string
	}{
		// 9000 bytes in three specs, a budget that holds two: exactly the oldest goes,
		// and the store is then 6000 bytes, which is the budget: to or under.
		{"three unwatched specs and a budget that holds two", 6000, []budgetSpec{{a, three, 2 * time.Hour}, {c, three, time.Hour}, {b, three, 3 * time.Hour}}, []string{b}},
		// One byte over the budget after the oldest goes: the next goes too, and never the newest.
		{"two have to go", 5000, []budgetSpec{{a, []int64{1000}, 3 * time.Hour}, {b, three, 2 * time.Hour}, {c, three, time.Hour}}, []string{a, b}},
		// At the budget is not over it.
		{"a store at its budget", 9000, []budgetSpec{{a, three, 2 * time.Hour}, {b, three, 3 * time.Hour}, {c, three, time.Hour}}, nil},
		{"a store under its budget", 1 << 40, []budgetSpec{{a, three, 2 * time.Hour}, {b, three, 3 * time.Hour}}, nil},
	}
	for _, tg := range targets(t) {
		for _, tc := range cases {
			t.Run(tg.name+"/"+tc.name, func(t *testing.T) {
				tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
				st := tg.open(t)
				r := newRig(t, st)
				for _, s := range tc.specs {
					r.seedSpec(t, st, s.hash, s.sizes, s.seen)
				}
				before := bytesUnder(t, st, "segments")
				log, _ := newLogs()
				w := r.sweeper(st, tc.budget, log)
				if err := w.Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
				for _, s := range tc.specs {
					left := keysUnder(t, st, "segments/"+s.hash)
					if contains(tc.gone, s.hash) {
						if len(left) != 0 {
							t.Errorf("spec %.1s was to go whole and keeps %v", s.hash, left)
						}
					} else if len(left) != len(s.sizes) {
						t.Errorf("spec %.1s was to stay whole and has %d of %d objects", s.hash, len(left), len(s.sizes))
					}
				}
				if after := bytesUnder(t, st, "segments"); before > tc.budget && after > tc.budget {
					t.Errorf("the store holds %d bytes after the sweep, over its budget of %d", after, tc.budget)
				}
			})
		}
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// With no budget there is none: the store is as large as the viewers make it,
// and only the age sweep (transcodeRetention) removes. With a budget the age
// sweep goes on beside it, also for a store far under its budget.
func TestWithoutABudgetOnlyTheAgeSweepRemovesAndWithOneItRunsToo(t *testing.T) {
	for _, tg := range targets(t) {
		for _, budget := range []int64{0, 1 << 40} {
			t.Run(fmt.Sprintf("%s/budget %d", tg.name, budget), func(t *testing.T) {
				tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
				st := tg.open(t)
				r := newRig(t, st)
				old, fresh, big := hex32("a"), hex32("b"), hex32("c")
				r.seedSpec(t, st, old, []int64{1000, 1000}, 25*time.Hour)
				r.seedSpec(t, st, fresh, []int64{1000, 1000}, time.Hour)
				r.seedSpec(t, st, big, []int64{50_000, 50_000, 50_000}, 2*time.Hour)
				log, _ := newLogs()
				if err := r.sweeper(st, budget, log).Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
				if left := keysUnder(t, st, "segments/"+old); len(left) != 0 {
					t.Errorf("the spec nobody asked for in 25 hours keeps %v", left)
				}
				if n := len(keysUnder(t, st, "segments/"+fresh)) + len(keysUnder(t, st, "segments/"+big)); n != 5 {
					t.Errorf("specs asked for within the day keep %d of 5 objects", n)
				}
			})
		}
	}
}

// Watching is a queued or running job (a helper's too), or an unstopped viewer
// who asked within 10 minutes (invariant 37's window for a paused film). The
// watched spec is the least recently requested one here, so the order of use
// alone would remove it first. The budget is a byte, so everything is over.
func TestTheBudgetNeverRemovesAWatchedSpecButRemovesAStoppedViewersAndLogsIt(t *testing.T) {
	watched, filler := hex32("d"), hex32("e")
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig)
		kept  bool
	}{
		{"a running job and a viewer gone for hours", func(t *testing.T, r *rig) {
			r.viewerAt(t, watched, 4*time.Hour, false)
			r.jobRow(t, watched, "running", false)
		}, true},
		{"a queued job and a viewer gone for hours", func(t *testing.T, r *rig) {
			r.viewerAt(t, watched, 4*time.Hour, false)
			r.jobRow(t, watched, "queued", false)
		}, true},
		{"a running helper's job and no viewer row", func(t *testing.T, r *rig) { r.jobRow(t, watched, "running", true) }, true},
		{"an unstopped viewer 9 minutes ago", func(t *testing.T, r *rig) { r.viewerAt(t, watched, 9*time.Minute, false) }, true},
		{"an unstopped viewer 11 minutes ago", func(t *testing.T, r *rig) { r.viewerAt(t, watched, 11*time.Minute, false) }, false},
		{"a stopped viewer a minute ago", func(t *testing.T, r *rig) { r.viewerAt(t, watched, time.Minute, true) }, false},
		{"a stopped viewer and a job that is done", func(t *testing.T, r *rig) {
			r.viewerAt(t, watched, 4*time.Hour, true)
			r.jobRow(t, watched, "done", false)
		}, false},
	}
	for _, tg := range targets(t) {
		for _, tc := range cases {
			t.Run(tg.name+"/"+tc.name, func(t *testing.T) {
				tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
				st := tg.open(t)
				r := newRig(t, st)
				r.seedSpec(t, st, filler, []int64{1000, 1000}, 3*time.Hour)
				for i, size := range []int64{1000, 1000} {
					putSized(t, st, SegmentKey(watched, i-1), size)
				}
				tc.setup(t, r)
				log, logged := newLogs()
				if err := r.sweeper(st, 1, log).Sweep(context.Background()); err != nil {
					t.Fatal(err)
				}
				if left := keysUnder(t, st, "segments/"+filler); len(left) != 0 {
					t.Errorf("the unwatched filler keeps %v: the budget did not run", left)
				}
				left := keysUnder(t, st, "segments/"+watched)
				switch {
				case tc.kept && len(left) != 2:
					t.Errorf("a spec a viewer is watching was removed: %v left of 2 objects", left)
				case !tc.kept && len(left) != 0:
					t.Errorf("a spec nobody is watching keeps %v", left)
				}
				// The watched specs alone exceed the budget: the store stays over it, and the log says so.
				if tc.kept && !strings.Contains(strings.ToLower(logged.String()), "budget") {
					t.Errorf("the log does not say that the watched specs exceed the budget:\n%s", logged)
				}
			})
		}
	}
}

// The size is every object under segments/: init segments, a spec with only
// some of its segments, and one with no viewer row, which is the least
// recently used. The partial spec's viewer is older than the full spec's.
func TestTheSizeCountsEveryObjectAndASpecWithNoViewerRowGoesFirst(t *testing.T) {
	noRow, partial, normal := hex32("1"), hex32("2"), hex32("3")
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			r := newRig(t, st)
			r.seedSpec(t, st, normal, []int64{1000, 1000, 1000}, time.Hour)
			// Only the init segment and segment 3 of the partial spec are stored.
			putSized(t, st, SegmentKey(partial, -1), 700)
			putSized(t, st, SegmentKey(partial, 3), 800)
			r.viewerAt(t, partial, 2*time.Hour, false)
			r.seedSpec(t, st, noRow, []int64{1000, 1000}, -1)
			total := bytesUnder(t, st, "segments") // 3000 + 1500 + 2000
			if total != 6500 {
				t.Fatalf("the fixture holds %d bytes", total)
			}
			log, _ := newLogs()
			w := r.sweeper(st, 4500, log)
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if left := keysUnder(t, st, "segments/"+noRow); len(left) != 0 {
				t.Errorf("the spec with no viewer row keeps %v: it counts as the least recently used", left)
			}
			if n := len(keysUnder(t, st, "segments/"+partial)); n != 2 {
				t.Errorf("the partial spec has %d of 2 objects left; removing the no-row spec is enough", n)
			}
			if n := len(keysUnder(t, st, "segments/"+normal)); n != 3 {
				t.Errorf("the newest spec has %d of 3 objects left", n)
			}
			if bytes, ok := storeBytes(t, w); !ok || bytes != 4500 {
				t.Errorf("the metric after the sweep is %v (reported: %v), the store holds 4500 bytes", bytes, ok)
			}
		})
	}
}

// A listing that fails removes nothing, is logged, and is never 0 bytes: the
// metric keeps what the last good listing found, or says nothing. The sweep
// has everything it would otherwise remove: a spec past its age, and a store
// over its budget.
func TestAFailedListingRemovesNothingIsLoggedAndIsNeverZeroBytes(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			spy := &spyStore{Store: st}
			r := newRig(t, spy)
			r.seedSpec(t, st, hex32("a"), []int64{1000, 1000, 1000}, time.Hour)
			log, logged := newLogs()
			w := r.sweeper(spy, 1<<40, log)
			other := r.sweeper(st, 1<<40, log)
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			good, ok := storeBytes(t, w)
			if !ok || good != 3000 {
				t.Fatalf("the metric after a good sweep is %v (reported: %v)", good, ok)
			}
			r.seedSpec(t, st, hex32("b"), []int64{1000, 1000}, 25*time.Hour) // Expired by age.
			r.seedSpec(t, st, hex32("c"), []int64{1000, 1000}, 2*time.Hour)  // Over a budget of 1000, with the others.
			w.Budget = 1000
			spy.failLists(errors.New("the store is on fire"))
			deletesBefore := spy.deleteCount()
			_ = w.Sweep(context.Background()) // An error or not, it removes nothing.
			if n := spy.deleteCount() - deletesBefore; n != 0 {
				t.Errorf("a sweep whose listing failed deleted %d objects", n)
			}
			if n := len(keysUnder(t, st, "segments")); n != 7 { // a's three, b's two and c's two.
				t.Errorf("%d of 7 objects are left after a failed listing", n)
			}
			if !strings.Contains(logged.String(), "the store is on fire") {
				t.Errorf("the log does not say why nothing was removed:\n%s", logged)
			}
			for name, worker := range map[string]*Worker{"the sweeping worker": w, "another worker": other} {
				if v, ok := storeBytes(t, worker); ok && v != good {
					t.Errorf("%s reports %v after a failed listing; the last good listing found %v, and 0 or any other number is a guess", name, v, good)
				}
			}
		})
	}
}

// The store is back within its budget by the next sweep after it goes over.
// The loop is the workers' own, and the period shortened; the first sweep of a
// loop is at its start, so a store that is over at a worker's start is cut at once.
// A mutant: a budget that is only read when the process starts.
func TestAnOverBudgetStoreIsBroughtBackByTheLoop(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			r := newRig(t, st)
			log, _ := newLogs()
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for range 3 {
				w := r.sweeper(st, 6000, log)
				w.SweepEvery = 300 * time.Millisecond
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.sweepLoop(ctx)
				}()
			}
			t.Cleanup(func() { cancel(); wg.Wait() })
			// The store goes over after the workers started.
			for i, hash := range []string{hex32("a"), hex32("b"), hex32("c")} {
				r.seedSpec(t, st, hash, []int64{1000, 1000, 1000}, time.Duration(i+1)*time.Hour)
			}
			began := time.Now()
			waitFor(t, "the store to be back within its budget", time.Minute, func() bool { return bytesUnder(t, st, "segments") <= 6000 })
			t.Logf("back within the budget %v after it went over, with a sweep period of 300ms", time.Since(began))
			if n := len(keysUnder(t, st, "segments/"+hex32("c"))); n != 0 {
				t.Errorf("the oldest spec (c, 3h) keeps %d objects while the newer ones stay", n)
			}
		})
	}
}

// A viewer who asks for a spec that is being removed gets its segments from a
// job the request starts: no error, and no 90-second wait on segments that are
// gone. The sweep is held right after it has deleted its first object, which
// the viewer asks for, and goes on once the viewer has been answered or is
// waiting on a lock the sweep holds (the viewer's own session, by name): a
// build that holds a lock for the removal and one that holds none both pass,
// and one that answers 500 or waits out the 90 seconds does not. The viewer's
// answer is bounded at 60 seconds, which is a hang's bound, not a wait to measure.
func TestAViewerAskingForASpecThatIsBeingRemovedIsServedByAJobTheRequestStarts(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			r := newRig(t, st)
			r.worker(t, nil)
			ctx := context.Background()
			first := r.jobs(t)
			if _, err := r.fetch(t, first, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the film's job to end", time.Minute, func() bool {
				return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
			})
			if err := first.Stop(ctx, r.user, psid(0), "device", ""); err != nil {
				t.Fatal(err)
			}
			if keys := keysUnder(t, st, "segments/"+r.hash); len(keys) < 3 {
				t.Fatalf("the job stored %v", keys)
			}

			spy := &spyStore{Store: st}
			deleted := make(chan string, 1)
			release := make(chan struct{})
			var once sync.Once
			spy.afterDelete = func(key string) {
				once.Do(func() { deleted <- key; <-release })
			}
			log, _ := newLogs()
			swept := make(chan error, 1)
			go func() { swept <- r.sweeper(spy, 1, log).Sweep(ctx) }()
			var key string
			select {
			case key = <-deleted:
			case err := <-swept:
				t.Fatalf("the sweep ended without removing the unwatched spec: %v", err)
			case <-time.After(time.Minute):
				t.Fatal("timed out waiting for the sweep to remove its first object")
			}
			n := -1
			if !strings.HasSuffix(key, "init.mp4") {
				if _, err := fmt.Sscanf(path.Base(key), "%d.mp4", &n); err != nil {
					t.Fatalf("the first object removed is %s", key)
				}
			}

			viewerPool := openNamed(t, r.dsn, "viewer-of-a-swept-spec")
			vctx, stop := context.WithCancel(ctx)
			defer stop()
			hub := &Hub{Pool: viewerPool, Log: r.log}
			go hub.Run(vctx)
			viewer := &Jobs{Pool: viewerPool, Segments: st, Hub: hub, QueueTimeout: 5 * time.Second, WaitTimeout: 90 * time.Second}
			type answer struct {
				data []byte
				err  error
			}
			answered := make(chan answer, 1)
			go func() {
				data, err := r.fetchAt(t, viewer, psid(1), n, 0)
				answered <- answer{data, err}
			}()
			viewerDone := make(chan struct{})
			var got answer
			go func() { got = <-answered; close(viewerDone) }()
			r.untilEither(t, "the viewer to be answered or to wait on the sweep's lock", viewerDone, "viewer-of-a-swept-spec")
			close(release)
			select {
			case <-viewerDone:
			case <-time.After(time.Minute):
				t.Fatal("the viewer asking for a spec that was being removed was not answered in a minute")
			}
			if got.err != nil {
				t.Fatalf("the viewer asking for %s, just removed, got an error: %v", key, got.err)
			}
			if n >= 0 && !isMP4Segment(got.data) || n < 0 && !strings.Contains(string(got.data[:min(len(got.data), 64)]), "ftyp") {
				t.Errorf("the viewer got %d bytes that are not the segment", len(got.data))
			}
			if err := <-swept; err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			// Asked again after the sweep, and for another segment of the spec: served.
			for _, ask := range []int{n, 6} {
				data, err := r.fetchAt(t, viewer, psid(1), ask, 0)
				if err != nil || len(data) == 0 {
					t.Errorf("segment %d after the sweep: %d bytes, %v", ask, len(data), err)
				}
			}
			// The viewer is watching now: the budget leaves its spec alone however small it is.
			if err := r.sweeper(st, 1, log).Sweep(ctx); err != nil {
				t.Fatal(err)
			}
			if keys := keysUnder(t, st, "segments/"+r.hash); len(keys) == 0 {
				t.Error("the sweep removed the spec a viewer had just asked for")
			}
		})
	}
}
