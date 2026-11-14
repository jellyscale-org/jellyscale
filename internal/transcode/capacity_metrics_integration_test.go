//go:build integration

package transcode

import (
	"context"
	"math"
	"testing"
	"time"
)

// JS-087's metrics. The series are named in capacity_integration_test.go.

func shows(t *testing.T, text, series string, want float64) {
	t.Helper()
	if got := value(text, series); math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", series, got, want)
	}
}

// The signal KEDA scales on is the cores the queued jobs cost, for the whole
// cluster; the number of queued jobs is read beside it, so that copies held
// back by their own bound are not read as no demand.
func TestTheQueueSignalIsTheCostOfTheQueuedJobsInCores(t *testing.T) {
	r := newRig(t, nil)
	w := &Worker{Pool: r.pool, ID: "reader", Cores: 4} // Reads the queue; runs nothing.
	read := func(cores, depth float64, when string) {
		t.Helper()
		text := scrape(t, w)
		for series, want := range map[string]float64{"jellyscale_transcode_queue_cores": cores, "jellyscale_transcode_queue_depth": depth} {
			if got := value(text, series); math.Abs(got-want) > 1e-9 {
				t.Errorf("%s: %s = %v, want %v", when, series, got, want)
			}
		}
	}
	read(0, 0, "an empty queue")
	r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, 0)
	read(0.8, 1, "one queued 1080p SDR job")
	r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 0)
	read(4.0, 2, "a 1080p SDR job and a 4K tone mapped one")
	// Not waited for any more, and not queued: neither counts.
	r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, time.Minute)
	r.running(t, "hdr4k", "reader")
	read(4.0, 2, "an expired job and a running one beside them")
	if _, err := r.pool.Exec(context.Background(), "DELETE FROM transcode_jobs"); err != nil {
		t.Fatal(err)
	}
	r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 0)
	read(3.2, 1, "one queued 4K tone mapped job")
}

// A queued copy is demand that shows in the count of jobs and almost not at all
// in cores.
func TestAQueuedCopyShowsInTheCountAndHardlyInCores(t *testing.T) {
	r := newRig(t, nil)
	r.addJob(t, r.costSpec(t, "copy"), "queued", "", false, 0)
	text := scrape(t, &Worker{Pool: r.pool, ID: "reader", Cores: 4})
	shows(t, text, "jellyscale_transcode_queue_depth", 1)
	copied := r.costSpec(t, "copy").Cost()
	shows(t, text, "jellyscale_transcode_queue_cores", copied)
	if copied <= 0 || copied > 0.1 {
		t.Errorf("a copy costs %v", copied)
	}
}

// A worker reports the cores it declared and the cost of the jobs it holds, in
// the place of its slots.
func TestAWorkerReportsItsCoresAndTheCostOfTheJobsItHolds(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.spec = r.costSpec(t, "sdr1080")
	r.hash = r.spec.Hash()
	var w *Worker
	r.worker(t, func(worker *Worker) {
		w, worker.Cores, worker.Lead, worker.Idle = worker, 4, 12*time.Second, 5*time.Second
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	stop := r.keepWatching(t, j, psid(0))
	text := scrape(t, w)
	shows(t, text, "jellyscale_transcode_cores", 4)
	shows(t, text, "jellyscale_transcode_cores_busy", r.spec.Cost())
	// At its lead the job is paused, and holds the same cores.
	waitFor(t, "the job to pause", 40*time.Second, func() bool { return r.paused() })
	shows(t, scrape(t, w), "jellyscale_transcode_cores_busy", r.spec.Cost())
	stop()
	waitFor(t, "the job to end", 30*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	waitFor(t, "its cores to be free", 5*time.Second, func() bool { return value(scrape(t, w), "jellyscale_transcode_cores_busy") == 0 })
	shows(t, scrape(t, w), "jellyscale_transcode_cores", 4)
}
