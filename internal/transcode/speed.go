package transcode

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"jellyscale/internal/mediatool"
	"jellyscale/internal/playback"
)

// JS-161 (D-044): the price table is in cores of one named reference machine (docs/transcoding.md), and each worker
// measures its own speed against it at start, so that its capacity is in the same unit as the jobs' prices.
const (
	// referenceSeconds is how long the reference machine (the AMD Ryzen 7 7800X3D, ffmpeg 9.0.2) takes to encode the
	// benchmark below on one thread: 1.37 to 1.43 seconds in six runs. A worker's speed is that over its own time.
	referenceSeconds = 1.4
	// measureTimeout ends a measurement that has not finished. docs/running.md says the worker measures within 20
	// seconds of its start, so this leaves the rest for starting the process and writing the row.
	measureTimeout = 15 * time.Second
	// measureRuns is how many times the benchmark runs; the fastest counts, since a run that was interrupted by another
	// process only ever comes out slower than the worker is.
	measureRuns = 2
)

// benchmark is one thread of x264 veryfast on a synthetic 720p picture, which is what a job's encode is made of. One
// copy runs on each processor at once, so that the speed is what a processor gives under full load: on a host whose
// processors are hardware threads, or that has busy neighbours, one copy alone runs far faster than each of sixteen
// (6x the throughput on sixteen threads, not 16x, measured), and capacity is the processors times that speed.
var benchmark = []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24", "-frames:v", "240",
	"-c:v", "libx264", "-preset", "veryfast", "-threads", "1", "-f", "null", "-"}

// dearestRow is the price of the dearest job in the table, the AV1 fallback of a 4K HDR source.
var dearestRow = playback.Spec{Recode: "av1", ToneMap: "hable", SourceHeight: 2160}.Cost()

// MeasureSpeed measures the worker's speed against the reference machine and says so, once. A worker that cannot say
// (no ffmpeg, a tool killed, the bound passed) logs why and counts the processors at the reference speed, which is not
// quiet: it is on /metrics too. Declared cores or slots are the capacity whatever it finds. It runs before the worker
// registers, so a row never carries a capacity that is not the measured one.
func (w *Worker) MeasureSpeed(ctx context.Context) {
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, measureTimeout)
	defer cancel()
	best := time.Duration(0)
	var err error
	for range measureRuns {
		var took time.Duration
		if took, err = benchmarkOnce(runCtx); err != nil {
			break
		}
		if best == 0 || took < best {
			best = took
		}
		// A slow host (many processors sharing few cores) has no time for a second round inside the bound.
		if best > measureTimeout/4 {
			break
		}
	}
	if ctx.Err() != nil {
		return
	}
	if best == 0 {
		reason := failureReason(runCtx, err)
		w.speedMu.Lock()
		w.speedFailures = map[string]int{reason: 1}
		w.speedMu.Unlock()
		w.Log.Warn("transcode speed measurement failed", "reason", reason, "error", err,
			"counts_as", "the reference speed for each of its processors", "bound", measureTimeout)
	} else {
		speed := math.Round(referenceSeconds/best.Seconds()*1000) / 1000
		w.speedMu.Lock()
		w.Speed = speed
		w.speedMu.Unlock()
		w.Log.Info("transcode speed measured", "speed", speed, "cores", w.cores(), "processors", processors(), "elapsed", time.Since(started).Round(time.Millisecond),
			"declared", w.Cores > 0 || w.Slots > 0, "load", load())
	}
	// What the worker counts as is said once more where it is below what the dearest job needs: it refuses that play, and
	// the operator should read why from the start and not from a viewer's complaint.
	if cores := w.cores(); cores < dearestRow {
		w.speedMu.Lock()
		w.belowDearest = true
		w.speedMu.Unlock()
		w.Log.Warn("transcode capacity below the dearest row", "cost", dearestRow, "cores", cores,
			"note", "plays that cost more than its cores are refused or shared with a second worker")
	}
}

// benchmarkOnce runs the benchmark on every processor at once and returns how long the last took to finish.
func benchmarkOnce(ctx context.Context) (time.Duration, error) {
	began := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for range processors() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := mediatool.Run(ctx, "ffmpeg", benchmark, mediatool.Options{}); err != nil {
				mu.Lock()
				defer mu.Unlock()
				if first == nil {
					first = err
				}
			}
		}()
	}
	wg.Wait()
	return time.Since(began), first
}

// failureReason is a word for why the benchmark did not finish: the metric's only label.
func failureReason(ctx context.Context, err error) string {
	var exited *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, exec.ErrNotFound):
		return "no_ffmpeg"
	case errors.As(err, &exited) && exited.ExitCode() < 0 && exited.Sys().(syscall.WaitStatus).Signaled():
		return "killed"
	}
	return "failed"
}

func processors() int { return runtime.GOMAXPROCS(0) }

// load is the host's load average over the last minute, which says whether the measurement was taken beside busy
// neighbours: a worker that shares its host and measured at a busy moment counts less than it has (docs/running.md).
func load() string {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(raw)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// speedOf is the worker's speed per processor, 0 until measured.
func (w *Worker) speedOf() float64 {
	w.speedMu.Lock()
	defer w.speedMu.Unlock()
	return w.Speed
}

// speedState is what /metrics shows of the measurement.
func (w *Worker) speedState() (failures map[string]int, below bool) {
	w.speedMu.Lock()
	defer w.speedMu.Unlock()
	failures = map[string]int{}
	for reason, n := range w.speedFailures {
		failures[reason] = n
	}
	return failures, w.belowDearest
}
