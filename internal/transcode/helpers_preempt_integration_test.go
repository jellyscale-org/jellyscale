//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// A new viewer never waits behind a helper (JS-072). Helpers fill every free
// slot but one, so a second viewer arriving at the same moment as a first has
// no slot; it takes a helper's, and the helper's chunk falls back to the job
// that asked for help.
//
// These tests use two names the task implies and the build must add, so they
// do not compile until it has: Worker.NoHelpers (true: the worker's jobs ask
// for no helpers), which helperSettings sets in a worker process from
// JELLYSCALE_TEST_NO_HELPERS=1, and the config key transcodeHelpers.

// otherStream is a second stream of the same film: another bitrate, so another
// spec with a job of its own.
func (r *rig) otherStream(bitrate int64) (playback.Spec, string) {
	spec := r.spec
	spec.VideoBitrate = bitrate
	return spec, spec.Hash()
}

// firstSegment is how long a new viewer of the spec waits for its segment 0.
func (r *rig) firstSegment(j *Jobs, spec playback.Spec, hash, psid string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	began := time.Now()
	v := r.viewer(psid)
	if err := j.Watch(ctx, v, hash, 0); err != nil {
		return 0, err
	}
	_, err := j.Segment(ctx, spec, hash, v, 0, 0)
	return time.Since(began), err
}

// helperRows counts the spec's helper rows in a state.
func (r *rig) helperRows(t *testing.T, state string) (n int) {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND helper AND state = $2", r.hash, state)
}

// helpedViewer sets up the cluster of these tests: two worker processes of two slots, so
// four slots; one viewer playing the film from the start takes one and, because
// a helper never takes the last free slot, two helpers take two more. It
// returns once they run, and the channel the viewer's slowest segment arrives
// on when it has played the film.
func (r *rig) helpedViewer(t *testing.T) (*Jobs, chan time.Duration) {
	t.Helper()
	r.workers(t, 2, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	played := r.playInBackground(t, j, psid(0))
	waitFor(t, "two helpers to run beside the main job", 60*time.Second, func() bool {
		return len(running(r.jobsOf(t, r.hash))) == 3 && r.helperRows(t, "running") == 2
	})
	return j, played
}

// Two viewers start at the same moment, with one slot free: one takes it and
// the other must take a helper's. Both get a first segment (the time is logged next to an
// idle cluster's, not judged), and no job of a viewer is the one stopped.
func TestTwoNewViewersStartAtOnceWhileHelpersHoldTheSlots(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 2, 2, 6, r.ownStore(t))
	j := r.jobs(t) // After the workers: ownStore sets the store the replica reads.

	// What a first segment costs with nothing running.
	probe, probeHash := r.otherStream(700_000)
	alone, err := r.firstSegment(j, probe, probeHash, psid(9))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Stop(context.Background(), r.user, psid(9), "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the probe's job and ffmpeg to be gone", 15*time.Second, func() bool {
		return r.activeJobs(t) == 0 && len(r.ffmpegProcesses()) == 0
	})
	t.Logf("first segment on an idle cluster: %v", alone.Round(10*time.Millisecond))

	r.playInBackground(t, j, psid(0))
	waitFor(t, "two helpers to run beside the main job", 60*time.Second, func() bool {
		return len(running(r.jobsOf(t, r.hash))) == 3 && r.helperRows(t, "running") == 2
	})

	type result struct {
		took time.Duration
		err  error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, bitrate := range []int64{800_000, 900_000} {
		spec, hash := r.otherStream(bitrate)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i].took, results[i].err = r.firstSegment(j, spec, hash, psid(10+i))
		}()
	}
	close(start)
	wg.Wait()
	for i, res := range results {
		if res.err != nil {
			t.Errorf("new viewer %d got no first segment: %v (helpers: %+v)", i, res.err, r.jobsOf(t, r.hash))
		} else {
			t.Logf("new viewer %d: first segment after %v (%v on an idle cluster)", i, res.took.Round(10*time.Millisecond), alone.Round(10*time.Millisecond))
		}
	}
	if n := r.helperRows(t, "cancelled"); n < 1 {
		t.Errorf("no helper was stopped for the second viewer: %+v", r.jobsOf(t, r.hash))
	}
	for _, job := range r.jobsOf(t, r.hash) {
		if job.Start == 0 && job.State != "running" {
			t.Errorf("the viewer's own job was stopped: %+v", r.jobsOf(t, r.hash))
		}
	}
}

// The helper that was stopped had a chunk; the job that asked for help goes on
// into it (it is free again), so the viewer reaches it with no more than a
// seek's wait, and plays the film to the end.
func TestAPreemptedHelpersChunkIsStillMade(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	j, played := r.helpedViewer(t)

	for i, bitrate := range []int64{800_000, 900_000} {
		spec, hash := r.otherStream(bitrate)
		if _, err := r.firstSegment(j, spec, hash, psid(10+i)); err != nil {
			t.Fatalf("new viewer %d: %v", i, err)
		}
	}
	if r.helperRows(t, "cancelled") < 1 {
		t.Fatalf("no helper was stopped, so there is nothing to show: %+v", r.jobsOf(t, r.hash))
	}
	select {
	case slowest := <-played:
		// Logged, not judged: TestAPreemptedHelpersChunkIsMadeByTheJobThatAskedForIt
		// (mechanism_integration_test.go) checks which job made the chunk.
		t.Logf("slowest wait for one segment of the helped viewer: %v", slowest.Round(10*time.Millisecond))
	case <-time.After(150 * time.Second):
		t.Fatalf("the helped viewer never got the whole film, or a segment failed: %+v", r.jobsOf(t, r.hash))
	}
	for n := 0; n < filmSegments; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d was never made", n)
		}
	}
}

// Helpers hold the cluster full (four slots, three helpers and no main job,
// the rows a dead main job and a race can leave): a helper that wants a slot
// waits for one, and never stops another helper for it.
func TestAHelperNeverStopsAnotherHelper(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 2, 2, 4, r.ownStore(t))
	j := r.jobs(t)
	r.keepWatching(t, j, psid(0))
	insert := func(start int) {
		t.Helper()
		raw, err := json.Marshal(r.spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper)
			VALUES ($1, $2, 'helper', $3, $3, true)`, r.hash, raw, start); err != nil {
			t.Fatal(err)
		}
	}
	for _, start := range []int{4, 8, 12} {
		insert(start)
	}
	waitFor(t, "three helpers to run", 30*time.Second, func() bool { return r.helperRows(t, "running") == 3 })
	insert(16) // A fourth: the cluster has a slot left, which a helper may not take.
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if n := r.helperRows(t, "cancelled"); n != 0 {
			t.Fatalf("%d helpers were stopped for another helper: %+v", n, r.jobsOf(t, r.hash))
		}
	}
}

// With every slot held by viewers' own jobs, a new viewer waits its turn and
// is refused at the queue timeout, and none of the others is stopped for it.
func TestNoViewersJobIsStoppedForAnother(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 1, 2, 4, r.ownStore(t), "JELLYSCALE_TEST_NO_HELPERS=1")
	j := r.jobs(t)
	for i, bitrate := range []int64{700_000, 800_000} {
		spec, hash := r.otherStream(bitrate)
		if _, err := r.firstSegment(j, spec, hash, psid(i)); err != nil {
			t.Fatal(err)
		}
	}
	spec, hash := r.otherStream(900_000)
	if _, err := r.firstSegment(j, spec, hash, psid(2)); err == nil {
		t.Fatal("a third viewer got a segment with both slots held by viewers")
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'"); n != 2 {
		t.Errorf("%d jobs run, want the two viewers' own", n)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'cancelled' AND NOT helper AND ended_at IS NOT NULL AND error IS NOT NULL"); n != 0 {
		t.Errorf("%d viewer jobs were stopped for a newcomer", n)
	}
}

// Operators can turn helpers off. A slow job is then one ffmpeg and no helper
// row exists, however many slots are free.
func TestWithHelpersOffASlowJobIsOneFFmpegAndNoHelperRows(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 4, r.ownStore(t), "JELLYSCALE_TEST_NO_HELPERS=1")
	j := r.jobs(t)
	r.playInBackground(t, j, psid(0))
	waitFor(t, "the viewer's job to run", 30*time.Second, func() bool { return len(running(r.jobsOf(t, r.hash))) == 1 })
	for end := time.Now().Add(25 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper"); n != 0 {
			t.Fatalf("%d helper rows with helpers off: %+v", n, r.jobsOf(t, r.hash))
		}
		if jobs := r.jobsOf(t, r.hash); len(jobs) != 1 {
			t.Fatalf("%d jobs for one slow viewer with helpers off: %+v", len(jobs), jobs)
		}
		if procs := r.ffmpegProcesses(); len(procs) > 1 {
			t.Fatalf("%d ffmpeg processes with helpers off: %v", len(procs), procs)
		}
	}
}
