package transcode

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// The speed is measured with every processor busy, not one alone: capacity is the processors times it, and one copy
// alone overstates a host whose processors are hardware threads (6x the throughput on 16 threads, not 16x). The shim
// ffmpeg ends only once as many copies as there are processors have started, so a measurement that runs them one after
// another never ends and fails by its bound.
func TestTheSpeedIsMeasuredWithEveryProcessorBusy(t *testing.T) {
	for _, procs := range []int{3, 5} {
		prev := runtime.GOMAXPROCS(procs)
		t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		started := filepath.Join(dir, "started")
		for _, d := range []string{bin, started} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		script := "#!/bin/sh\ntouch " + started + "/$$\nwhile [ $(ls " + started + " | wc -l) -lt " + strconv.Itoa(procs) + " ]; do sleep 0.02; done\n"
		if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
		w := &Worker{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		w.MeasureSpeed(t.Context())
		if w.speedOf() == 0 {
			t.Errorf("%d processors: no speed was measured: the copies did not run at once (failures %v)", procs, w.speedFailures)
		}
	}
}
