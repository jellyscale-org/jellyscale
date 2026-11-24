package transcode

import (
	"testing"

	"jellyscale/internal/store"
)

// What a worker declares: its cores, else its slots as cores (a worker built by
// a test, as the release before declared them), else the CPUs the process may use
// (cpu.max when there is one, else GOMAXPROCS(0), injected here as a Kernel so that the
// host's cgroup does not decide), each at the reference machine's speed, and no floor.
func TestAWorkersDeclaredCores(t *testing.T) {
	// transcodeCores is a ceiling on the processors the worker may use (D-064, JS-177).
	if got := (&Worker{Cores: 2.5, Slots: 5, Kernel: &fakeKernel{cpus: 16}}).cores(); got != 2.5 {
		t.Errorf("cores and slots declared on 16 CPUs: %v, want the cores", got)
	}
	if got := (&Worker{Cores: 2.5, Slots: 5, Kernel: &fakeKernel{cpus: 2}}).cores(); got != 2 {
		t.Errorf("cores 2.5 on 2 CPUs: %v, want the 2 CPUs: a ceiling above them is not counted as written", got)
	}
	if got := (&Worker{Slots: 3, Kernel: &fakeKernel{cpus: 16}}).cores(); got != 3 {
		t.Errorf("slots declared: %v, want 3", got)
	}
	// JS-161 (D-044, D-062): a worker that declares nothing counts each processor at the reference machine's speed, with
	// no floor and no measurement.
	if got := (&Worker{Kernel: &fakeKernel{cpus: 3.5}}).cores(); got != 3.5 {
		t.Errorf("nothing declared on a 3.5 CPU limit: %v, want 3.5", got)
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
