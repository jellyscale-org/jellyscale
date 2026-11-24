package transcode

import (
	"testing"
)

// JS-161 (D-044, D-062): prices stay one table in cores of a named reference machine (docs/transcoding.md), and each
// worker advertises its capacity in those units: what it declares (transcodeCores, or transcodeSlots as that many
// cores), else its processors each at the reference speed, with no floor and no measurement. A worker's capacity is
// what cores() returns and the row of transcode_nodes carries. (D-064, JS-177: transcodeCores only lowers the start, and
// the figure then moves with what the worker learns; capacity_learn_test.go has the rest.)

// jobCosts are job prices in cores, from the cheapest copy to the dearest AV1 fallback (internal/playback/cost.go).
var jobCosts = []float64{0.05, 0.8, 1.0, 2.0, 3.2, 4.2}

// A worker that declares nothing counts each of its processors at the reference machine's speed, with no floor, whatever
// the number of processors: the processors the process may use, 1 or 3 or 16, and never the dearest job's 3.2.
func TestAnUndeclaredWorkerCountsItsProcessorsAtTheReferenceSpeedWithNoFloor(t *testing.T) {
	for _, procs := range []int{1, 2, 3, 16} {
		got := (&Worker{Kernel: &fakeKernel{cpus: float64(procs)}}).cores()
		if got != float64(procs) {
			t.Errorf("%d processors, nothing declared: %v cores, want %d", procs, got, procs)
		}
	}
}

// transcodeCores is an optional ceiling (D-064, JS-177): the capacity before any move is the lesser of the processors the
// worker may use and the ceiling, so a ceiling above the processors is not counted as written; transcodeSlots, which
// JS-177 does not touch, is as it was.
func TestTranscodeCoresIsACeilingNotAFigureWhateverTheProcessors(t *testing.T) {
	for _, procs := range []int{1, 4, 16} {
		kernel := &fakeKernel{cpus: float64(procs)} // The CPUs the worker may use, injected: not the host's cpu.max.
		for what, got := range map[string]float64{
			"cores 6":           (&Worker{Cores: 6, Kernel: kernel}).cores(),
			"cores 0.5":         (&Worker{Cores: 0.5, Kernel: kernel}).cores(),
			"slots 3":           (&Worker{Slots: 3, Kernel: kernel}).cores(),
			"cores 2.5 slots 3": (&Worker{Cores: 2.5, Slots: 3, Kernel: kernel}).cores(),
		} {
			want := map[string]float64{"cores 6": min(6, float64(procs)), "cores 0.5": 0.5, "slots 3": 3, "cores 2.5 slots 3": min(2.5, float64(procs))}[what]
			if got != want {
				t.Errorf("%d processors, %s: %v cores, want %v", procs, what, got, want)
			}
		}
	}
}

// Every job price is admitted by the one rule: a worker whose capacity is at least the row's cost admits
// it, and one just under it refuses it, whether the capacity is declared small or counted from many processors.
func TestEveryJobPriceIsAdmittedByTheOneRule(t *testing.T) {
	for _, cost := range jobCosts {
		for _, side := range []struct {
			name   string
			cores  float64
			admits bool
		}{{"at its cost", cost, true}, {"over its cost", cost + 0.3, true}, {"just under its cost", cost - 0.01, false}} {
			// The row's cores are the figure the ledger counts, whatever the host's processors (a ceiling above them is not
			// counted as written, D-064, so a Worker would hold the processors here).
			l := newLedger([]capNode{{ID: "w", Cores: side.cores}}, nil, func(capJob) bool { return true })
			if _, ok := l.room(capJob{Cost: cost}); ok != side.admits {
				t.Errorf("a %.2f core row, a worker with %.3f cores (%s): admitted %v, want %v", cost, side.cores, side.name, ok, side.admits)
			}
		}
	}
}

// 4 processors at the reference speed hold five 1080p SDR re-encodes, or a 4K tone mapped one beside a 1080p SDR one,
// and never two 4K tone mapped ones: 4.0 cores, by the table's 0.8 and 3.2.
func TestAFourCoreWorkerAtTheReferenceSpeedHoldsWhatTheFloorUsedToGiveIt(t *testing.T) {
	capacity := 4 * 1.0 // 4 processors, the reference speed per core.
	l := newLedger([]capNode{{ID: "w", Cores: capacity}}, nil, func(capJob) bool { return true })
	for i := range 5 {
		if _, ok := l.place(capJob{Cost: 0.8}); !ok {
			t.Fatalf("1080p SDR re-encode %d of 5 does not fit in %v cores", i+1, capacity)
		}
	}
	if _, ok := l.place(capJob{Cost: 0.8}); ok {
		t.Error("a sixth 1080p SDR re-encode fits")
	}
	l = newLedger([]capNode{{ID: "w", Cores: capacity}}, nil, func(capJob) bool { return true })
	if _, ok := l.place(capJob{Cost: 3.2}); !ok {
		t.Fatal("a 4K tone mapped re-encode does not fit")
	}
	if _, ok := l.place(capJob{Cost: 0.8}); !ok {
		t.Error("a 1080p SDR re-encode beside it does not fit")
	}
	l = newLedger([]capNode{{ID: "w", Cores: capacity}}, nil, func(capJob) bool { return true })
	l.place(capJob{Cost: 3.2})
	if _, ok := l.place(capJob{Cost: 3.2}); ok {
		t.Error("two 4K tone mapped re-encodes fit on 4 cores")
	}
}
