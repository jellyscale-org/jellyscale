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
//
// What is checked is that a worker claims the viewer's job before the helper
// beats again, which it can only do once the helper's ffmpeg has left its slot.
// It is not the viewer's first segment: that also counts the encode, which on
// CI's two cores, with two other viewers' encodes running, took 3.75 s longer
// than on an idle cluster with the hint working. The time from the ask to the
// claim is logged.
func TestAPausedHelperLetsGoOfItsSlotAtOnce(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// Three slots, chunks of 4 segments and a lead of 5: the viewer's job makes
	// 0 to 3 and ends at the helper's chunk, and the helper makes 4 and pauses.
	r.workers(t, 3, 1, 4, r.ownStore(t), "JELLYSCALE_TEST_LEAD_SECONDS=20",
		"JELLYSCALE_TEST_HEARTBEAT=10s", "JELLYSCALE_TEST_LEASE=30s")
	j := r.jobs(t)

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
	beat := lease()
	last, lastHash := r.otherStream(1_000_000)
	// When the viewer's job is running: how long that took after the ask, and the
	// helper's lease then, which is the beat above if the helper has not beaten
	// since.
	type claim struct {
		after time.Duration
		lease time.Time
	}
	claimed := make(chan claim, 1)
	began := time.Now()
	go func() {
		for time.Since(began) < 30*time.Second {
			var state string
			r.pool.QueryRow(context.Background(), "SELECT state FROM transcode_jobs WHERE spec_hash = $1 AND NOT helper ORDER BY id DESC LIMIT 1", lastHash).Scan(&state) //nolint:errcheck
			if state == "running" {
				claimed <- claim{time.Since(began), lease()}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		claimed <- claim{time.Since(began), lease()}
	}()
	took, err := r.firstSegment(j, last, lastHash, psid(12))
	if err != nil {
		t.Fatalf("the third viewer got no first segment: %v (film's jobs: %+v)", err, r.jobsOf(t, r.hash))
	}
	if r.helperRows(t, "cancelled") != 1 {
		t.Fatalf("the paused helper was not the one stopped: %+v", r.jobsOf(t, r.hash))
	}
	got := <-claimed
	t.Logf("a worker claimed the viewer's job %v after it asked; first segment after %v", got.after.Round(10*time.Millisecond), took.Round(10*time.Millisecond))
	if !got.lease.Equal(beat) {
		t.Errorf("the viewer's job was claimed only after the helper beat again (its lease went from %v to %v), with a 10 s heartbeat: the helper held its slot until its heartbeat", beat, got.lease)
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
