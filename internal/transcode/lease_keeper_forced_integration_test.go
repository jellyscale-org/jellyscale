//go:build integration

package transcode

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-105, after the merge queue: checks that force the interleavings the queue
// found, rather than wait for them. Each names a one-line change to a right
// build that it fails on.

// isTheCancel matches the write that records a job nobody watches as cancelled.
func isTheCancel(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "UPDATE transcode_jobs") && strings.Contains(s, "SET state = 'cancelled'")
}

// alive says whether the ffmpeg process still exists.
func (r *rig) alive(p ffmpegProcess) bool {
	for _, q := range r.ffmpegs() {
		if q.Pid == p.Pid && q.Start == p.Start {
			return true
		}
	}
	return false
}

// A statement of the keeper never waits for a row while it holds another. The
// worker's commit of the job with the higher id waits for a row another session
// holds; the keeper's renewal of both jobs arrives after it and queues behind it;
// then the other session asks for the lower job's row. If the keeper had locked
// that row while it waited for the higher one, the three would wait for each
// other in a circle, and Postgres aborts the one whose wait began first: the
// worker's commit, which fails the job.
//
// Fails with "the keeper locks rows in ascending order and waits for each" (the
// d0bba71 build), and with a keeper whose batch waits for a locked row (no SKIP
// LOCKED).
func TestTheKeepersRenewalNeverWaitsForARowWhileItHoldsAnother(t *testing.T) {
	slowAt(t, "2")
	// A heartbeat of 600 ms gives a renewal 300 ms to wait, longer than the 250 ms
	// deadlock_timeout, so the circle is there when Postgres looks for it.
	s := newStack(t, 0)
	r, ctx := s.r, context.Background()
	r.exec(t, `DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET deadlock_timeout = ''250ms''', current_database()); END $$`)
	t.Cleanup(func() {
		if _, err := r.pool.Exec(context.Background(), `DO $$ BEGIN EXECUTE format('ALTER DATABASE %I RESET deadlock_timeout', current_database()); END $$`); err != nil {
			t.Errorf("resetting deadlock_timeout: %v", err)
		}
	})
	w := s.addWorker("JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_LEASE=4s", "JELLYSCALE_TEST_HEARTBEAT=600ms")
	other := r.spec
	other.VideoBitrate = 1_200_000
	count := playback.EncodedLayout(900000000).Count()
	s.play(s.jobs, r.spec, psid(0), 0, count)
	s.play(s.jobs, other, psid(1), 0, count)
	waitFor(t, "both jobs to run", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", w.id) == 2
	})
	var low, high int64
	if err := r.pool.QueryRow(ctx, "SELECT min(id), max(id) FROM transcode_jobs WHERE worker = $1", w.id).Scan(&low, &high); err != nil {
		t.Fatal(err)
	}
	var nextBefore int
	if err := r.pool.QueryRow(ctx, "SELECT next_segment FROM transcode_jobs WHERE id = $1", high).Scan(&nextBefore); err != nil {
		t.Fatal(err)
	}

	// The commit of the higher job: its id is the first parameter, sent as text with its length.
	param := append([]byte{0, 0, 0, byte(len(strconv.FormatInt(high, 10)))}, strconv.FormatInt(high, 10)...)
	holder, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback(context.Background()) }) //nolint:errcheck
	t.Cleanup(w.keep.Release)
	var lockErr error
	done := w.db.OnStatement(func(sent []byte) bool { return isTheSegmentCommit(sent) && bytes.Contains(sent, param) }, func() {
		// The keeper's own statements wait until the commit has queued for the row, so
		// that the commit is the first to wait.
		w.keep.Hold()
		_, lockErr = holder.Exec(ctx, "UPDATE transcode_jobs SET lease_expires_at = lease_expires_at WHERE id = $1", high)
	}, nil)
	waiting := func(like string) func() bool {
		return func() bool {
			return r.count(t, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE $1`, like) > 0
		}
	}
	waitFor(t, "the worker's commit of the higher job to wait for its row", keeperHang, waiting("UPDATE transcode_jobs SET next_segment%"))
	if lockErr != nil {
		t.Fatalf("the other session could not lock the row: %v", lockErr)
	}
	w.keep.Release()
	waitFor(t, "the keeper's renewal to wait for the row", keeperHang, waiting("UPDATE transcode_jobs j SET lease_expires_at%"))
	// The other session asks for the lower job's row. It is queued behind the keeper if
	// the keeper locked it, and gets it at once if the keeper holds nothing.
	_, askErr := holder.Exec(ctx, "UPDATE transcode_jobs SET lease_expires_at = lease_expires_at WHERE id = $1", low)
	t.Logf("the other session's own ask for the lower row: %v", askErr)
	holder.Rollback(ctx) //nolint:errcheck
	select {
	case <-done:
	case <-time.After(keeperHang):
		t.Fatal("the worker's commit was never answered")
	}
	waitFor(t, "the commit to have been recorded or the job to have ended", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND (next_segment > $2 OR state <> 'running')", high, nextBefore) == 1
	})
	r.noFailed(t)
	if log := w.log.String(); strings.Contains(log, "40P01") || strings.Contains(strings.ToLower(log), "deadlock") {
		t.Errorf("a deadlock reached the worker's log:\n%s", log)
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up: %v", lines)
	}
}

// A worker that ends a job it still owns writes the row before it ends the
// ffmpeg (DOMAINS 25): a row that says running with no ffmpeg behind it has a
// returning viewer join a job nobody works on. A job paused at its lead whose
// viewers have all seeked away is ended by its loop; the cancel is on Postgres's
// way while the ffmpeg still exists.
//
// Fails with the gate path's two lines swapped (stop, then cancelUnwanted).
func TestAJobLeftBehindIsWrittenCancelledBeforeItsFfmpegIsEnded(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_LEAD_SECONDS=12")
	r, w, ctx := s.r, s.workers[0], context.Background()
	for i, n := range []int{0, 1} {
		if _, err := s.fetch(ctx, s.jobs, r.spec, psid(i), n); err != nil {
			t.Fatal(err)
		}
	}
	var p ffmpegProcess
	waitFor(t, "the job to be paused at its lead", keeperHang, func() bool {
		var ok bool
		p, ok = r.ffmpegOf(w)
		return ok && p.State == "T"
	})
	var existed bool
	done := w.db.OnStatement(isTheCancel, func() { existed = r.alive(p) }, nil)
	// Both viewers go far ahead: the job's are left behind.
	for i, n := range []int{12, 22} {
		attempt, stop := context.WithTimeout(ctx, keeperHang)
		_, err := s.fetch(attempt, s.jobs, r.spec, psid(i), n)
		stop()
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(keeperHang):
		t.Fatal("the worker never wrote the cancel of the job its viewers left behind")
	}
	if !existed {
		t.Error("the ffmpeg of a job its viewers left behind was already ended when the cancel reached Postgres: the row was written after the process was ended")
	}
}

// The same, for a job the keeper ends because nobody watches its spec: the keeper
// writes the cancel on its own connection before it cancels the job, so the
// ffmpeg, which the cancel ends, is still there when the write reaches Postgres.
//
// Fails with no write by the keeper, and with one after the cancel.
func TestAJobNobodyWatchesIsWrittenCancelledByTheKeeperBeforeItsFfmpegIsEnded(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	var p ffmpegProcess
	waitFor(t, "the job to run with its ffmpeg", keeperHang, func() bool {
		var ok bool
		p, ok = r.ffmpegOf(w)
		return ok
	})
	old := r.jobOf(t, r.hash)
	var existed bool
	done := w.keep.OnStatement(isTheCancel, func() { existed = r.alive(p) }, nil)
	r.exec(t, "UPDATE transcode_viewers SET last_seen_at = now() - interval '1 hour' WHERE spec_hash = $1", r.hash)
	waitFor(t, "the job to be cancelled", keeperHang, func() bool { return r.jobState(t, old) == "cancelled" })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not write the cancel on its own connection")
	}
	if !existed {
		t.Error("the ffmpeg was already ended when the keeper's cancel reached Postgres")
	}
}

// A worker whose keeper's connection is refused goes on with its loop: the
// fence before a put is answered at once, as not answered, so the loop turns,
// pauses its ffmpeg at the lead and asks again for the viewers every half
// second, as it does with Postgres there. A fence that waited half a lease for
// the keeper's next attempt held the loop for five seconds, and a copy's
// ffmpeg, which ends the film in a second, ran on past its lead and was gone
// when the loop turned (the merge queue's "ffmpeg cannot be paused here").
//
// Fails with a fence that waits for the keeper's next attempt.
func TestACopyWhoseKeepersConnectionIsRefusedStaysPausedAtItsLeadWithItsFfmpeg(t *testing.T) {
	film(t)
	s := newStack(t, 1, "JELLYSCALE_TEST_LEAD_SECONDS=8")
	r, w, ctx := s.r, s.workers[0], context.Background()
	r.copied(t, "film.mkv")
	// Postgres goes away as the first segment is recorded.
	var once sync.Once
	away := make(chan struct{})
	w.db.OnStatement(isTheSegmentCommit, nil, func() { once.Do(func() { w.away(true); close(away) }) })
	t.Cleanup(w.back)
	go s.fetch(ctx, s.jobs, r.spec, psid(0), 0) //nolint:errcheck
	select {
	case <-away:
	case <-time.After(keeperHang):
		t.Fatal("the worker never recorded a segment")
	}
	const failedRead = "reading the viewers' segments failed"
	failed := func() int { return strings.Count(w.log.String(), failedRead) }
	was := failed()
	// A bound on a hang: the loop reads the viewers every half second, and a build that
	// holds it in the fence does not read again for five.
	waitFor(t, "the loop to look for the viewers twice more while Postgres is away", 3*time.Second, func() bool { return failed() >= was+2 })
	p, ok := r.ffmpegOf(w)
	if !ok || p.State != "T" {
		t.Fatalf("the copy's ffmpeg is %+v (running %v), want it paused at its lead", p, ok)
	}
	if strings.Contains(w.log.String(), "cannot be paused") {
		t.Errorf("the worker could not pause the ffmpeg:\n%s", w.log.String())
	}
	w.back()
	s.allLocked(30*time.Second, w)
	waitFor(t, "the viewer's job to go on", keeperHang, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'") == 1 })
	if q, ok := r.ffmpegOf(w); !ok || q.Pid != p.Pid || q.Start != p.Start {
		t.Errorf("the copy's ffmpeg was %+v while Postgres was away and is %+v (running %v) after it", p, q, ok)
	}
	r.noFailed(t)
}
