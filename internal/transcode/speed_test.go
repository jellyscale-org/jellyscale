package transcode

import (
	"math"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// JS-161 (D-044): prices stay one table in cores of a named reference machine, and each worker measures its own speed
// against it. These checks use one name the task implies and the build must add: Worker.Speed, the worker's speed per
// processor as a multiple of the reference machine's (1 is the reference; 0 is not measured), set by the measurement at
// the worker's start. A worker's capacity, which cores() returns and the row of transcode_nodes carries, is its
// processors times that speed, unless it declares cores (or slots), which win whatever was measured. They do not
// compile until the build adds the field.

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

// A worker that declares nothing counts what it measured: its processors times its speed, with no floor. Today it is
// floored to 3.2 cores, whatever its speed.
func TestAnUndeclaredWorkerCountsWhatItMeasuredWithNoFloor(t *testing.T) {
	procs := float64(runtime.GOMAXPROCS(0))
	for _, speed := range []float64{0.25 / procs, 1 / procs, 0.625, 1, 1.6} {
		want := procs * speed
		if got := (&Worker{Speed: speed}).cores(); math.Abs(got-want) > 1e-9 {
			t.Errorf("measured %v of the reference per core on %v processors: %v cores, want %v", speed, procs, got, want)
		}
	}
}

// A worker measured at the reference machine's speed per core counts its processors, the cores today's default gives
// it above the 3.2 floor.
func TestAWorkerAtTheReferenceSpeedCountsItsProcessors(t *testing.T) {
	if got, want := (&Worker{Speed: 1}).cores(), float64(runtime.GOMAXPROCS(0)); math.Abs(got-want) > 1e-9 {
		t.Errorf("a worker at the reference speed counts %v cores on %v processors", got, want)
	}
}

// transcodeCores, when declared, is the capacity whatever the measurement says, above or below it; transcodeSlots when
// transcodeCores is not declared.
func TestDeclaredCoresAndSlotsAreTheCapacityWhateverWasMeasured(t *testing.T) {
	for _, speed := range []float64{0.1, 1, 8} {
		if got := (&Worker{Cores: 6, Speed: speed}).cores(); got != 6 {
			t.Errorf("transcodeCores 6, measured speed %v: %v cores", speed, got)
		}
		if got := (&Worker{Cores: 0.5, Speed: speed}).cores(); got != 0.5 {
			t.Errorf("transcodeCores 0.5, measured speed %v: %v cores", speed, got)
		}
		if got := (&Worker{Slots: 3, Speed: speed}).cores(); got != 3 {
			t.Errorf("transcodeSlots 3, measured speed %v: %v cores", speed, got)
		}
		if got := (&Worker{Cores: 2.5, Slots: 3, Speed: speed}).cores(); got != 2.5 {
			t.Errorf("transcodeCores 2.5 and slots 3, measured speed %v: %v cores", speed, got)
		}
	}
}

// Every row of the price table is admitted by the one rule: a worker whose capacity is at least the row's cost admits
// it, and one just under it refuses it. The capacity is made by the measured speed, which is below the reference's
// for the rows that cost more than the host's processors and above it for the rest, so both a throttled and an
// unthrottled worker are met on one host.
func TestEveryRowOfThePriceTableIsAdmittedByTheOneRule(t *testing.T) {
	procs := float64(runtime.GOMAXPROCS(0))
	for _, cost := range priceRows(t) {
		for _, side := range []struct {
			name   string
			cores  float64
			admits bool
		}{{"at its cost", cost, true}, {"over its cost", cost + 0.3, true}, {"just under its cost", cost - 0.01, false}} {
			w := &Worker{Speed: side.cores / procs}
			l := newLedger([]capNode{{ID: "w", Cores: w.cores()}}, nil, func(capJob) bool { return true })
			if _, ok := l.room(capJob{Cost: cost}); ok != side.admits {
				t.Errorf("a %.2f core row, a worker measured at %.3f of the reference with %.3f cores (%s): admitted %v, want %v", cost, w.Speed, w.cores(), side.name, ok, side.admits)
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
