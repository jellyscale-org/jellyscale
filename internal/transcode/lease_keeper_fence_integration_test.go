//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-105, the fence before a put. A put's check asks Postgres at put time,
// through the lease keeper: a lease that was taken away (a cancel, a reap's cut
// and the deletion of the worker's row) is seen by it, which a worker that only
// trusted its own clock against the last renewal would not see for up to a
// heartbeat. These cases take the lease away the way the product does while the
// worker's ffmpeg goes on making segments, and ask that no put begins more than
// a round trip afterwards (putAllowance, which JS-066 and JS-104 already allow).
// A put that was already begun when the lease went is not counted. A build whose
// fence only compares the clock with the last renewal begins puts for up to
// 9 seconds more, which these cases see on any machine.

// A viewer's stop or seek cancels a job at any moment, so a renewal answered just
// before it is the worst case: the keeper's last word is "yours" and it is
// 3 seconds old by the next heartbeat. ffmpeg is fast (8x) so that it has
// segments for the worker to store all the while.
func TestNoPutBeginsAfterAJobWasCancelledJustAfterARenewal(t *testing.T) {
	slowAt(t, "8")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	waitFor(t, "the job to have stored two segments", keeperHang, func() bool {
		return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 2
	})
	var cancelledAt time.Time
	cancelled := w.keep.OnStatement(isTheRenewal, nil, func() {
		// The renewal has run and its answer is on its way: the job stops being the worker's now.
		r.exec(t, "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1", job)
		cancelledAt = time.Now()
	})
	select {
	case <-cancelled:
	case <-time.After(keeperHang):
		t.Fatal("the worker sent no renewal")
	}
	waitFor(t, "the worker's ffmpeg to be gone", keeperHang, func() bool { _, ok := r.ffmpegOf(w); return !ok })
	time.Sleep(time.Second)
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == w.id && p.at.After(cancelledAt.Add(putAllowance)) {
			t.Errorf("worker %s began a put of %s at %v, %v after job %d was cancelled just after a renewal; a put's check allows %v",
				p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(cancelledAt).Round(time.Millisecond), job, putAllowance)
		}
	}
	r.noFailed(t)
}

// A reap deletes the worker's row and cuts its jobs' leases to half a second
// (liveness.go, reap), as another worker's look does when it finds the lock gone.
// The pool is held, so the worker cannot look at its own row, and the keeper's
// connection answers: its last renewal says "yours" for another 9.5 seconds by its
// own clock. No put begins more than a round trip after the cut.
func TestNoPutBeginsAfterAReapCutTheLeaseAndDeletedTheRowWhileThePoolIsHeld(t *testing.T) {
	slowAt(t, "8")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	waitFor(t, "the job to have stored two segments", keeperHang, func() bool {
		return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 2
	})
	w.db.Hold() // Only the pool: the worker's own look and its segment commits wait. The keeper answers.
	t.Cleanup(w.db.Release)
	if _, err := r.pool.Exec(ctx, `WITH cut AS (UPDATE transcode_jobs SET lease_expires_at = LEAST(lease_expires_at, now() + make_interval(secs => 0.5))
			WHERE state = 'running' AND worker = $1 RETURNING id)
		DELETE FROM transcode_nodes WHERE id = $1`, w.id); err != nil {
		t.Fatal(err)
	}
	cutAt := time.Now()
	waitFor(t, "the worker's ffmpeg to be gone", keeperHang, func() bool { _, ok := r.ffmpegOf(w); return !ok })
	time.Sleep(time.Second)
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == w.id && p.at.After(cutAt.Add(putAllowance)) {
			t.Errorf("worker %s began a put of %s at %v, %v after a reap deleted its row and cut job %d's lease while its pool was held; a put's check allows %v",
				p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(cutAt).Round(time.Millisecond), job, putAllowance)
		}
	}
	w.db.Release()
	waitFor(t, "the job to end", keeperHang, func() bool { return r.jobState(t, job) != "running" })
	r.noFailed(t)
}

// A viewer's request can start a replacement for the job at once, on the same
// spec hash and so with the same segment keys: while the old worker's lease still
// stood by its own clock, the replacement's ffmpeg and the old worker's both
// stored. The old worker's job is cancelled in Postgres by the replacement's
// ensure (seek far from where it was), and the old worker, with its pool held and
// its keeper answering, begins no put for the spec more than a round trip after.
func TestNoPutBeginsForASpecAReplacementTookOverWhileThePoolIsHeld(t *testing.T) {
	slowAt(t, "8")
	s := newStack(t, 1)
	r, old, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	oldJob := r.jobOf(t, r.hash)
	waitFor(t, "the job to have stored two segments", keeperHang, func() bool {
		return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", oldJob) >= 2
	})
	other := s.addWorker()
	old.db.Hold()
	t.Cleanup(old.db.Release)
	// The same viewer seeks a long way off: its job is cancelled and a new one made, which the idle worker claims.
	far := r.firstMissing(r.hash) + 8
	attempt, stop := context.WithTimeout(ctx, keeperHang)
	defer stop()
	go s.fetch(attempt, s.jobs, r.spec, psid(0), far) //nolint:errcheck
	waitFor(t, "the old job to be cancelled by the viewer's seek", keeperHang, func() bool { return r.jobState(t, oldJob) == "cancelled" })
	cancelledAt := time.Now()
	waitFor(t, "the other worker to run a job of the spec", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND id > $2 AND worker = $3", r.hash, oldJob, other.id) == 1
	})
	time.Sleep(2 * time.Second)
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == old.id && p.at.After(cancelledAt.Add(putAllowance)) {
			t.Errorf("worker %s began a put of %s at %v, %v after a replacement took over its spec; a put's check allows %v",
				p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(cancelledAt).Round(time.Millisecond), putAllowance)
		}
	}
}

// A fence renews the lease of the job that asked and no other: the worker's three
// jobs are one that stores a segment a second or so (each put asks) and two that
// sit at their lead and store nothing. Postgres is read often, all three leases in
// one statement, and each change is looked at: the leases of the two that did not
// ask move at the heartbeat, together with the other's, and never alone. A build
// whose fence renews every job of the worker moves all three at each put, so no
// sample has the one moving alone.
func TestAFenceRenewsOnlyTheJobThatAsked(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_CORES=2.5", "JELLYSCALE_TEST_SLOTS=3", "JELLYSCALE_TEST_LEAD_SECONDS=8")
	r, w, ctx := s.r, s.workers[0], context.Background()
	specs := []playback.Spec{r.spec, r.spec, r.spec}
	specs[1].VideoBitrate, specs[2].VideoBitrate = 1_200_000, 1_000_000
	count := playback.EncodedLayout(900000000).Count()
	for _, spec := range specs[1:] { // Asked for one segment: each stops at its lead.
		if _, err := s.fetch(ctx, s.jobs, spec, psid(1), 0); err != nil {
			t.Fatal(err)
		}
	}
	s.play(s.jobs, specs[0], psid(0), 0, count) // And this one is played, so it keeps storing.
	waitFor(t, "two jobs paused at their lead and the played one storing", keeperHang, func() bool {
		paused := 0
		for _, p := range r.ffmpegs() {
			if p.Parent == w.cmd.Process.Pid && p.State == "T" {
				paused++
			}
		}
		return paused == 2 && r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' AND next_segment >= 2", specs[0].Hash()) == 1
	})
	ids := make([]int64, len(specs))
	for i, spec := range specs {
		ids[i] = r.jobOf(t, spec.Hash())
	}
	leases := func() (at [3]time.Time) { // One statement: a renewal is seen whole or not at all.
		if err := r.pool.QueryRow(ctx, `SELECT (SELECT lease_expires_at FROM transcode_jobs WHERE id = $1),
			(SELECT lease_expires_at FROM transcode_jobs WHERE id = $2), (SELECT lease_expires_at FROM transcode_jobs WHERE id = $3)`,
			ids[0], ids[1], ids[2]).Scan(&at[0], &at[1], &at[2]); err != nil {
			t.Fatal(err)
		}
		return at
	}
	var fenceOnly, together, restWithout int
	last, movesOfTheAsker := leases(), 0
	for deadline := time.Now().Add(keeperHang); movesOfTheAsker < 8 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		now := leases()
		askerMoved := now[0].After(last[0])
		restMoved := now[1].After(last[1]) || now[2].After(last[2])
		switch {
		case askerMoved && restMoved:
			together++
		case askerMoved:
			fenceOnly++
		case restMoved:
			restWithout++
		}
		if askerMoved {
			movesOfTheAsker++
		}
		last = now
	}
	if movesOfTheAsker < 8 {
		t.Fatalf("the played job's lease moved %d times in %v", movesOfTheAsker, keeperHang)
	}
	if fenceOnly == 0 {
		t.Errorf("every one of the played job's %d renewals also renewed the two jobs that stored nothing: a fence renews the whole worker", movesOfTheAsker)
	}
	if restWithout > 0 {
		t.Errorf("the jobs that stored nothing were renewed %d times without the played job", restWithout)
	}
	if together == 0 {
		t.Error("no heartbeat renewed the three jobs together")
	}
	r.noFailed(t)
}
