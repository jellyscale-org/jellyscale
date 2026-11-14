//go:build integration

package transcode

import (
	"fmt"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// A worker stopped past both its lease and the stall limit, while no viewer waits
// on its job, goes on and ends the job cancelled, never failed: the stall check
// reads the stop as a hung ffmpeg, and a failed job refuses its spec to every
// viewer for 30 seconds (failureMemory) though nothing was wrong with the film.
//
// The lease (3 s) and the stall limit (5 s) are shorter than production's, so that
// the stop is short; a stop past both is what matters, and the ratio of the two
// (the stall limit is the longer) is production's. What the shorter timing could
// hide: a stall limit of 30 s is crossed by a stop of 31 s, in which more is
// queued behind the worker, and nothing here shows what that does.
//
// Two shapes: the worker alone (its ffmpeg goes on and has segments waiting when
// the worker does, so the worker's first look is the upload, which sees the lease
// gone), and the worker and its ffmpeg together, as a frozen host or a paused
// container stops them, where nothing is waiting and the heartbeat is first to
// see that the job has made no segment for longer than the stall limit. Three
// rounds each, since the worker's poll and its heartbeat race to be first.
func TestAWorkerStoppedPastItsLeaseAndTheStallLimitEndsItsJobCancelled(t *testing.T) {
	for _, withFfmpeg := range []bool{false, true} {
		for round := range 3 {
			t.Run(fmt.Sprintf("ffmpeg stopped too %v round %d", withFfmpeg, round), func(t *testing.T) {
				segments := t.TempDir()
				r := newRig(t, store.NewPosix(segments))
				slowAt(t, "2")
				a := r.workerProcess(t, "worker-a", segments, "JELLYSCALE_TEST_LEASE=3s", "JELLYSCALE_TEST_HEARTBEAT=500ms", "JELLYSCALE_TEST_STALL=5s")
				j := r.jobs(t)
				if _, err := r.fetch(t, j, psid(0), 0); err != nil {
					t.Fatal(err)
				}
				job := int64(r.count(t, "SELECT id FROM transcode_jobs"))
				// Segment 0 is readable before the row that says so is written: stop only once it is,
				// and a moment on, so that the worker is not stopped in the middle of an upload.
				waitFor(t, "the first segment to be recorded", 30*time.Second, func() bool {
					return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 1
				})
				time.Sleep(400 * time.Millisecond)
				signal := func(sig syscall.Signal) {
					a.Process.Signal(sig) //nolint:errcheck
					if withFfmpeg {
						if p, ok := r.ffmpegOfPid(a.Process.Pid); ok {
							syscall.Kill(p.Pid, sig) //nolint:errcheck
						}
					}
				}
				signal(syscall.SIGSTOP)
				// Nobody asks for anything meanwhile: no viewer waits on the job, and none takes it over.
				time.Sleep(9 * time.Second)
				signal(syscall.SIGCONT)
				waitFor(t, "the job to end after the worker went on", 60*time.Second, func() bool { return r.jobState(t, job) != "running" })
				if got := r.jobState(t, job); got != "cancelled" {
					t.Errorf("a worker stopped past its lease and the stall limit ended its job %s, want cancelled", got)
				}
				waitFor(t, "the worker's ffmpeg to be gone", 20*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
			})
		}
	}
}

// ffmpegOfPid is the ffmpeg run by the process with the given id.
func (r *rig) ffmpegOfPid(parent int) (ffmpegProcess, bool) {
	for _, p := range r.ffmpegs() {
		if p.Parent == parent {
			return p, true
		}
	}
	return ffmpegProcess{}, false
}
