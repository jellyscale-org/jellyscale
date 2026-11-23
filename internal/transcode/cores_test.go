package transcode

import (
	"math"
	"runtime"
	"testing"

	"jellyscale/internal/store"
)

// What a worker declares: its cores, else its slots as cores (a worker built by
// a test, as the release before declared them), else the processors the process
// may use at the speed it measured against the reference machine, with no floor.
func TestAWorkersDeclaredCores(t *testing.T) {
	if got := (&Worker{Cores: 2.5, Slots: 5}).cores(); got != 2.5 {
		t.Errorf("cores and slots declared: %v, want the cores", got)
	}
	if got := (&Worker{Slots: 3}).cores(); got != 3 {
		t.Errorf("slots declared: %v, want 3", got)
	}
	// JS-161 (D-044): a worker that declares nothing has what it measured (speed.go), and is no longer floored to
	// the dearest job's 3.2 cores: its processors times its speed against the reference machine.
	procs := float64(runtime.GOMAXPROCS(0))
	if got := (&Worker{Speed: 1 / procs}).cores(); math.Abs(got-1) > 1e-9 {
		t.Errorf("nothing declared, measured %v of the reference per core on %v processors: %v cores, want 1", 1/procs, procs, got)
	}
}

// The row of a worker carries slots that are the cores rounded up, so that a
// replica of the release before, which counts a worker by slots > 0, counts it;
// and a worker with no store, which runs no transcode, declares none.
func TestAWorkersRowCarriesPositiveSlotsBesideItsCores(t *testing.T) {
	w := &Worker{Cores: 0.5, Segments: store.NewPosix(t.TempDir())}
	if cores, slots := w.registered(); cores != 0.5 || slots != 1 {
		t.Errorf("registered %v cores and %d slots, want 0.5 and 1", cores, slots)
	}
	if cores, slots := (&Worker{Cores: 4}).registered(); cores != 0 || slots != 0 {
		t.Errorf("a worker with no store registered %v cores and %d slots", cores, slots)
	}
}
