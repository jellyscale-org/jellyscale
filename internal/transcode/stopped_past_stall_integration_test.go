//go:build integration

package transcode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
// Four cases, each forced, since the worker's poll and its lease keeper race to be
// the first to see the stop. The worker alone (its ffmpeg goes on) and the worker
// with its ffmpeg (as a frozen host or a paused container stops them), each with
// the poll first and with the keeper's cut first. Poll first: the worker is stopped
// while its poll's upload is inside a Put that waits for its context, so the upload
// is what ends when the worker goes on. Keeper first: nothing finished is waiting
// to be stored when it goes on, so the poll has nothing to upload and the keeper's
// cut is the only thing to see. The worker's log says which saw the stop first, and
// a case that did not get its order is a harness error, not a verdict on the worker.
//
// The worker's own stall arm (stop("ffmpeg has stalled")) is not reached by a stop
// past the lease: the keeper cuts the job before any beat arrives.
func TestAWorkerStoppedPastItsLeaseAndTheStallLimitEndsItsJobCancelled(t *testing.T) {
	for _, c := range []struct {
		name       string
		withFfmpeg bool
		pollFirst  bool
	}{
		{"the worker alone, poll first", false, true},
		{"the worker alone, heartbeat first", false, false},
		{"the worker and its ffmpeg, poll first", true, true},
		{"the worker and its ffmpeg, heartbeat first", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			segments, slow := t.TempDir(), t.TempDir()
			r := newRig(t, store.NewPosix(segments))
			slowAt(t, "2")
			worker, log := stallWorker(t, r, segments, slow)
			if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
				t.Fatal(err)
			}
			job := int64(r.count(t, "SELECT id FROM transcode_jobs"))
			// Segment 0 is readable before the row that says so is written.
			waitFor(t, "the first segment to be recorded", 30*time.Second, func() bool {
				return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 1
			})
			ffmpeg, found := r.ffmpegOfPid(worker.Process.Pid)
			if !found {
				t.Fatal("no ffmpeg of the worker")
			}
			if c.pollFirst {
				// Every Put now reads nothing and ends with its context: the next segment's upload is inside
				// one when the worker is stopped.
				if err := os.WriteFile(filepath.Join(slow, "worker-a"), []byte("stall all 0s"), 0o644); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "the poll's upload to be inside a held Put", 30*time.Second, func() bool {
					raw, _ := os.ReadFile(filepath.Join(slow, "worker-a.log"))
					return strings.Contains(string(raw), " begin ")
				})
			}
			worker.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
			if c.withFfmpeg {
				syscall.Kill(ffmpeg.Pid, syscall.SIGSTOP) //nolint:errcheck
			}
			// Nobody asks for anything meanwhile: no viewer waits on the job, and none takes it over.
			time.Sleep(9 * time.Second)
			if !c.pollFirst {
				r.dropWaitingSegments(t, job, !c.withFfmpeg)
			}
			worker.Process.Signal(syscall.SIGCONT) //nolint:errcheck
			if c.withFfmpeg {
				syscall.Kill(ffmpeg.Pid, syscall.SIGCONT) //nolint:errcheck
			}
			waitFor(t, "the job to end after the worker went on", 60*time.Second, func() bool { return r.jobState(t, job) != "running" })
			waitFor(t, "the worker's ffmpeg to be gone", 20*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
			reason := firstStopReason(t, log)
			sawPoll := strings.Contains(reason, "upload failed")
			sawKeeper := strings.Contains(reason, errUnrenewed.Error())
			if c.pollFirst && !sawPoll || !c.pollFirst && !sawKeeper {
				t.Fatalf("harness error: the other one saw the stop first: the worker stopped ffmpeg for %q\n%s", reason, log.String())
			}
			if got := r.jobState(t, job); got != "cancelled" {
				t.Errorf("a worker stopped past its lease and the stall limit ended its job %s, want cancelled", got)
			}
		})
	}
}

// stallWorker is worker-a as a process of its own, a lease of 3 s and a stall limit of 5 s, whose store
// is slow as the directory says and whose log the test reads.
func stallWorker(t *testing.T, r *rig, segments, slow string) (*exec.Cmd, *logBuffer) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER=worker-a", "JELLYSCALE_TEST_SEGMENTS="+segments, "JELLYSCALE_TEST_SCRATCH="+r.scratch,
		"JELLYSCALE_TEST_LEASE=3s", "JELLYSCALE_TEST_HEARTBEAT=500ms", "JELLYSCALE_TEST_STALL=5s", slowPutEnv(slow))
	log := &logBuffer{}
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { cmd.Wait(); close(reaped) }() //nolint:errcheck
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
		cmd.Process.Kill()                  //nolint:errcheck
		<-reaped
	})
	waitFor(t, "the worker to register", 30*time.Second, func() bool { return r.locked("worker-a") })
	return cmd, log
}

// dropWaitingSegments removes what ffmpeg has finished and the stopped worker has not stored: the files in the
// job's scratch from the row's next_segment on, so that the worker has nothing to upload when it goes on. ffmpeg
// writes through a temporary name and never makes a number it is past again. A worker alone has an ffmpeg that
// ran meanwhile, so the next segment is there to be seen first.
func (r *rig) dropWaitingSegments(t *testing.T, job int64, ffmpegRan bool) {
	t.Helper()
	next := r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job)
	if ffmpegRan {
		waitFor(t, "ffmpeg to finish the next segment", 30*time.Second, func() bool {
			return len(globOr(filepath.Join(r.scratch, "job-*", strconv.Itoa(next)+".m4s"))) > 0
		})
	}
	for _, file := range globOr(filepath.Join(r.scratch, "job-*", "*.m4s")) {
		if number, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(file), ".m4s")); err == nil && number >= next {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// firstStopReason is the reason of the worker's first "stopping ffmpeg" line.
func firstStopReason(t *testing.T, log *logBuffer) string {
	t.Helper()
	var reason string
	waitFor(t, "the worker to say why it stopped ffmpeg", 10*time.Second, func() bool {
		for _, line := range strings.Split(log.String(), "\n") {
			if at := strings.Index(line, "reason="); at >= 0 && strings.Contains(line, "stopping ffmpeg") {
				reason = line[at:]
				return true
			}
		}
		return false
	})
	return reason
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
