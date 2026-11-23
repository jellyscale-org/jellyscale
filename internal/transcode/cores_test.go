package transcode

import (
	"runtime"
	"testing"

	"jellyscale/internal/store"
)

// What a worker declares: its cores, else its slots as cores (a worker built by
// a test, as the release before declared them), else the processors the process
// may use, each at the reference machine's speed, and no floor.
func TestAWorkersDeclaredCores(t *testing.T) {
	if got := (&Worker{Cores: 2.5, Slots: 5}).cores(); got != 2.5 {
		t.Errorf("cores and slots declared: %v, want the cores", got)
	}
	if got := (&Worker{Slots: 3}).cores(); got != 3 {
		t.Errorf("slots declared: %v, want 3", got)
	}
	// JS-161 (D-044, D-062): a worker that declares nothing counts each processor at the reference machine's speed, with
	// no floor and no measurement.
	if got, want := (&Worker{}).cores(), float64(runtime.GOMAXPROCS(0)); got != want {
		t.Errorf("nothing declared: %v, want the %v processors", got, want)
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
