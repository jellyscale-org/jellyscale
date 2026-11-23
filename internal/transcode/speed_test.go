package transcode

import (
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// JS-161 (D-044, D-062): prices stay one table in cores of a named reference machine (docs/transcoding.md), and each
// worker advertises its capacity in those units: what it declares (transcodeCores, or transcodeSlots as that many
// cores), else its processors each at the reference speed, with no floor and no measurement. A worker's capacity is
// what cores() returns and the row of transcode_nodes carries.

// priceRows are the 13 rows of docs/transcoding.md's table of what a job costs, in cores.
func priceRows(t *testing.T) []float64 {
	t.Helper()
	var rows []float64
	row := regexp.MustCompile(`^\| (?:Re-encode|HEVC fallback|AV1 fallback|A copy)[^|]*\| ([0-9.]+) \|$`)
	for _, line := range strings.Split(doc(t, "docs/transcoding.md"), "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			cost, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, cost)
		}
	}
	if len(rows) != 13 {
		t.Fatalf("docs/transcoding.md has %d price rows, the task says 13", len(rows))
	}
	return rows
}

// A worker that declares nothing counts each of its processors at the reference machine's speed, with no floor, whatever
// the number of processors: the processors the process may use, 1 or 3 or 16, and never the dearest job's 3.2.
func TestAnUndeclaredWorkerCountsItsProcessorsAtTheReferenceSpeedWithNoFloor(t *testing.T) {
	for _, procs := range []int{1, 2, 3, 16} {
		prev := runtime.GOMAXPROCS(procs)
		got := (&Worker{}).cores()
		runtime.GOMAXPROCS(prev)
		if got != float64(procs) {
			t.Errorf("%d processors, nothing declared: %v cores, want %d", procs, got, procs)
		}
	}
}

// transcodeCores, when declared, is the capacity whatever the processors, above or below them; transcodeSlots when
// transcodeCores is not declared.
func TestDeclaredCoresAndSlotsAreTheCapacityWhateverTheProcessors(t *testing.T) {
	for _, procs := range []int{1, 4, 16} {
		prev := runtime.GOMAXPROCS(procs)
		for what, got := range map[string]float64{
			"cores 6":           (&Worker{Cores: 6}).cores(),
			"cores 0.5":         (&Worker{Cores: 0.5}).cores(),
			"slots 3":           (&Worker{Slots: 3}).cores(),
			"cores 2.5 slots 3": (&Worker{Cores: 2.5, Slots: 3}).cores(),
		} {
			want := map[string]float64{"cores 6": 6, "cores 0.5": 0.5, "slots 3": 3, "cores 2.5 slots 3": 2.5}[what]
			if got != want {
				t.Errorf("%d processors, %s: %v cores, want %v", procs, what, got, want)
			}
		}
		runtime.GOMAXPROCS(prev)
	}
}

// Every row of the price table is admitted by the one rule: a worker whose capacity is at least the row's cost admits
// it, and one just under it refuses it, whether the capacity is declared small or counted from many processors.
func TestEveryRowOfThePriceTableIsAdmittedByTheOneRule(t *testing.T) {
	for _, cost := range priceRows(t) {
		for _, side := range []struct {
			name   string
			cores  float64
			admits bool
		}{{"at its cost", cost, true}, {"over its cost", cost + 0.3, true}, {"just under its cost", cost - 0.01, false}} {
			w := &Worker{Cores: side.cores}
			l := newLedger([]capNode{{ID: "w", Cores: w.cores()}}, nil, func(capJob) bool { return true })
			if _, ok := l.room(capJob{Cost: cost}); ok != side.admits {
				t.Errorf("a %.2f core row, a worker with %.3f cores (%s): admitted %v, want %v", cost, w.cores(), side.name, ok, side.admits)
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
