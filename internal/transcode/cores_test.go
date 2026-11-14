package transcode

import (
	"runtime"
	"testing"

	"jellyscale/internal/store"
)

// What a worker declares: its cores, else its slots as cores (a worker built by
// a test, as the release before declared them), else the processors the process
// may use, and never fewer than the dearest job costs, so a worker that
// declares nothing still takes a 4K tone mapped re-encode.
func TestAWorkersDeclaredCores(t *testing.T) {
	if got := (&Worker{Cores: 2.5, Slots: 5}).cores(); got != 2.5 {
		t.Errorf("cores and slots declared: %v, want the cores", got)
	}
	if got := (&Worker{Slots: 3}).cores(); got != 3 {
		t.Errorf("slots declared: %v, want 3", got)
	}
	if got, want := (&Worker{}).cores(), max(float64(runtime.GOMAXPROCS(0)), largestCost); got != want {
		t.Errorf("nothing declared: %v, want %v", got, want)
	}
	if got := (&Worker{}).cores(); got < 3.2 {
		t.Errorf("a worker that declares nothing has %v cores, too few for a 4K tone mapped re-encode", got)
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
