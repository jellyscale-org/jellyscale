//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"
)

// A preempted helper gives its slot back at once, not at its next heartbeat.
// The other preemption tests run workers with a 200 ms heartbeat, which hides
// that wait. Here the heartbeat is 10 s, the helper is paused at its
// read-ahead limit, so it uploads nothing that would show it the cancel, and
// the new viewer comes just after the helper's heartbeat: only the preempt
// hint can stop it before the next one, about 10 s later.
func TestAPausedHelperLetsGoOfItsSlotAtOnce(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// Three slots, chunks of 4 segments and a lead of 5: the viewer's job makes
	// 0 to 3 and ends at the helper's chunk, and the helper makes 4 and pauses.
	r.workers(t, 3, 1, 4, r.ownStore(t), "JELLYSCALE_TEST_LEAD_SECONDS=20",
		"JELLYSCALE_TEST_HEARTBEAT=10s", "JELLYSCALE_TEST_LEASE=30s")
	j := r.jobs(t)

	probe, probeHash := r.otherStream(700_000)
	alone, err := r.firstSegment(j, probe, probeHash, psid(9))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Stop(context.Background(), r.user, psid(9), "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the probe's job and ffmpeg to be gone", 30*time.Second, func() bool {
		return r.activeJobs(t) == 0 && len(r.ffmpegProcesses()) == 0
	})

	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	var helper job
	waitFor(t, "the helper to be the film's only job, paused at the lead", 90*time.Second, func() bool {
		jobs := running(r.jobsOf(t, r.hash))
		if len(jobs) != 1 || r.helperRows(t, "running") != 1 || jobs[0].Next < 5 {
			return false
		}
		helper = jobs[0]
		return true
	})
	time.Sleep(time.Second)
	if now := running(r.jobsOf(t, r.hash)); len(now) != 1 || now[0].Next != helper.Next {
		t.Fatalf("the helper is not paused: %+v then %+v", helper, now)
	}

	// Two other viewers take the two free slots, so the next one needs the
	// helper's.
	for i, bitrate := range []int64{800_000, 900_000} {
		spec, hash := r.otherStream(bitrate)
		if _, err := r.firstSegment(j, spec, hash, psid(10+i)); err != nil {
			t.Fatal(err)
		}
		r.keepWatchingStream(t, j, hash, psid(10+i))
	}
	lease := func() (at time.Time) {
		r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", helper.ID).Scan(&at) //nolint:errcheck
		return at
	}
	before := lease()
	waitFor(t, "the helper to beat", 20*time.Second, func() bool { return lease().After(before) })
	last, lastHash := r.otherStream(1_000_000)
	took, err := r.firstSegment(j, last, lastHash, psid(12))
	if err != nil {
		t.Fatalf("the third viewer got no first segment: %v (film's jobs: %+v)", err, r.jobsOf(t, r.hash))
	}
	if r.helperRows(t, "cancelled") != 1 {
		t.Fatalf("the paused helper was not the one stopped: %+v", r.jobsOf(t, r.hash))
	}
	t.Logf("first segment: %v on an idle cluster, %v taking the paused helper's slot", alone.Round(10*time.Millisecond), took.Round(10*time.Millisecond))
	if took > alone+1500*time.Millisecond {
		t.Errorf("the viewer waited %v for a first segment, %v on an idle cluster: the helper held its slot until its heartbeat", took, alone)
	}
}

// keepWatchingStream is keepWatching for another stream than the rig's.
func (r *rig) keepWatchingStream(t *testing.T, j *Jobs, hash, psid string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			j.Watch(ctx, r.viewer(psid), hash, -1) //nolint:errcheck
			time.Sleep(300 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}
