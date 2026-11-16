package transcode

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// JS-106. The plan of fault schedules is a function of the seed, and a seed names a run to replay
// (JELLYSCALE_TEST_FAULT_SEED): the slow-store step is added to the steps planned before it, and
// they keep their kind, target, moment and length. These are the steps planFaults gave at the
// commit before JS-106 (each: kind, target, moment in ns, length in ns, waited, clean, round trip
// in ns), for seed 17371545602420011879, which replays the schedule of BL-139, and four more.
var planBeforeSlowStore = []struct {
	seed   uint64
	index  int
	window time.Duration
	steps  []string
}{
	{17371545602420011879, 0, 91700000000, []string{
		"kill 439 3400000000 0 true false 0",
		"viewer-stop 563 9000000000 0 false false 0",
		"postgres-restart-short 150 17800000000 9500000000 false true 0",
		"stop-short 493 39100000000 9800000000 false true 0",
		"kill 691 63800000000 0 true true 0",
		"delay-small 452 76000000000 9700000000 false true 150000000",
	}},
	{17371545602420011879, 1, 123200000000, []string{
		"stop-past-stall 148 9000000000 35600000000 false false 0",
		"postgres-restart-short 337 14600000000 10600000000 false false 0",
		"postgres-restart-long 919 18200000000 22300000000 false false 0",
		"stop-past-lease 567 70200000000 22000000000 true true 0",
		"viewer-stop 166 94800000000 0 false false 0",
		"delay-past-half-lease 847 102000000000 15200000000 false false 13500000000",
	}},
	{1, 0, 90000000000, []string{
		"kill 151 3600000000 0 true false 0",
		"postgres-restart-short 656 15100000000 10100000000 false true 0",
		"stop-short 512 38400000000 6900000000 false true 0",
		"kill 167 55200000000 0 true true 0",
		"viewer-stop 682 64700000000 0 false false 0",
		"delay-small 594 68000000000 9200000000 false true 50000000",
		"kill 881 86500000000 0 false false 0",
	}},
	{1, 1, 110000000000, []string{
		"stop-past-stall 907 6000000000 34300000000 false false 0",
		"postgres-restart-long 377 20500000000 25900000000 false false 0",
		"delay-past-half-lease 551 46200000000 12200000000 false false 12000000000",
		"viewer-stop 408 67700000000 0 false false 0",
		"stop-past-lease 725 76200000000 23900000000 true true 0",
	}},
	{2, 0, 90000000000, []string{
		"delay-small 363 3000000000 4000000000 false true 150000000",
		"viewer-stop 998 11000000000 0 false false 0",
		"postgres-restart-short 76 15900000000 4000000000 false true 0",
		"stop-short 211 30500000000 8700000000 false true 0",
		"kill 793 55200000000 0 true true 0",
		"kill 281 71300000000 0 true false 0",
	}},
	{2, 1, 116200000000, []string{
		"delay-past-half-lease 528 16700000000 16300000000 false false 12200000000",
		"stop-past-lease 610 44100000000 23900000000 true true 0",
		"stop-past-stall 4 77000000000 33200000000 false false 0",
		"postgres-restart-long 88 80200000000 24200000000 false false 0",
		"viewer-stop 148 87400000000 0 false false 0",
		"postgres-restart-short 143 99800000000 10200000000 false false 0",
	}},
	{3, 0, 90000000000, []string{
		"postgres-restart-short 211 13000000000 10100000000 false true 0",
		"delay-small 709 31500000000 3100000000 false true 150000000",
		"kill 550 45000000000 0 true true 0",
		"viewer-stop 405 63600000000 0 false false 0",
		"stop-short 768 63800000000 3300000000 false true 0",
		"kill 762 82600000000 0 true false 0",
	}},
	{3, 1, 110000000000, []string{
		"delay-past-half-lease 597 12000000000 14900000000 false false 12700000000",
		"stop-past-lease 455 37100000000 22500000000 true true 0",
		"postgres-restart-long 534 66400000000 22500000000 false false 0",
		"viewer-stop 834 69700000000 0 false false 0",
		"stop-past-stall 725 70800000000 33000000000 false false 0",
		"postgres-restart-short 346 95900000000 4900000000 false false 0",
	}},
	{42, 0, 100000000000, []string{
		"postgres-restart-short 261 11600000000 5300000000 false true 0",
		"kill 719 25400000000 0 true false 0",
		"kill 838 41300000000 0 true true 0",
		"stop-short 308 64300000000 9800000000 false true 0",
		"viewer-stop 592 65600000000 0 false false 0",
		"delay-small 650 84000000000 10000000000 false true 150000000",
	}},
	{42, 1, 118600000000, []string{
		"viewer-stop 748 15500000000 0 false false 0",
		"stop-past-lease 682 19000000000 23900000000 true true 0",
		"stop-past-stall 593 59200000000 33700000000 false false 0",
		"postgres-restart-long 383 73900000000 25700000000 false false 0",
		"delay-past-half-lease 663 98000000000 14600000000 false false 13700000000",
	}},
}

func stepLine(s faultStep) string {
	return fmt.Sprintf("%s %d %d %d %v %v %d", s.Kind, s.Target, s.At, s.Length, s.Waited, s.Clean, s.RoundTrip)
}

// Every step planned before the slow-store step is still planned, as it was and in the order it
// was, and the schedule's window is no shorter. A plan that drew the new step from the same
// stream as the others would move every step after it, and the seed of BL-139 would replay
// another schedule.
func TestTheStepsPlannedBeforeTheSlowStoreStepKeepEverythingForTheirSeed(t *testing.T) {
	plans := map[uint64][]faultSchedule{}
	for _, want := range planBeforeSlowStore {
		if plans[want.seed] == nil {
			plans[want.seed] = planFaults(want.seed, faultClock)
		}
		got := plans[want.seed][want.index]
		var kept []string
		for _, s := range got.Steps {
			if s.Kind != faultSlowStore {
				kept = append(kept, stepLine(s))
			}
		}
		if !slices.Equal(kept, want.steps) {
			t.Errorf("seed %d schedule %d: the steps planned before the slow-store step are now\n%q\nwant\n%q", want.seed, want.index, kept, want.steps)
		}
		if got.Window < want.window {
			t.Errorf("seed %d schedule %d: the window is %v, shorter than the %v it was", want.seed, want.index, got.Window, want.window)
		}
	}
}

// Every planned run, whatever its seed, holds a slow-store step, and one of them is clean. Each is
// at least a lease long (a put begins in it), keeps clear of every step that takes a worker away
// (the worker is slowed, not away), and, when clean, of a restart of Postgres, which is what
// the plan keeps quiet around a clean step (quarrels).
func TestEveryPlannedRunHoldsASlowStoreStepAndOneOfThemIsClean(t *testing.T) {
	tm := faultClock
	for seed := uint64(0); seed < 300; seed++ {
		var slow, clean int
		for _, sched := range planFaults(seed, tm) {
			for i, s := range sched.Steps {
				if s.Kind != faultSlowStore {
					continue
				}
				slow++
				if s.Clean {
					clean++
				}
				if s.Length < tm.Lease {
					t.Errorf("seed %d: %v is shorter than a lease, so no put need begin in it", seed, s)
				}
				if earlier(sched.Window+time.Second, s.At+s.Length) {
					t.Errorf("seed %d: %v runs past the window of %v", seed, s, sched.Window)
				}
				for j, o := range sched.Steps {
					if i == j {
						continue
					}
					if out, away := o.workerOut(); away && o.Kind != faultSlowStore && earlier(s.At, o.At+out) && earlier(o.At, s.At+s.Length+5*time.Second) {
						t.Errorf("seed %d: %v overlaps %v, which takes a worker away", seed, s, o)
					}
					if s.Clean && (quarrels(s, o, tm) || (o.Kind == faultSlowStore && earlier(s.At, o.At+o.Length+5*time.Second) && earlier(o.At, s.At+s.Length+5*time.Second))) {
						t.Errorf("seed %d: %v is clean and %v falls with it", seed, s, o)
					}
				}
			}
		}
		if slow == 0 {
			t.Errorf("seed %d: no slow-store step in the run", seed)
		}
		if clean == 0 {
			t.Errorf("seed %d: no clean slow-store step in the run", seed)
		}
	}
}

// The slow-store step is a step of the plan like any other: a function of the seed alone.
func TestTheSlowStoreStepIsAFunctionOfTheSeedAlone(t *testing.T) {
	for _, seed := range []uint64{1, 17371545602420011879} {
		if a, b := planFaults(seed, faultClock), planFaults(seed, faultClock); fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("seed %d planned twice gave two lists:\n%v\n%v", seed, a, b)
		}
	}
	if got := faultSlowStore; got != "slow-store" {
		t.Errorf("the slow-store step's kind is named %q, want slow-store", got)
	}
}
