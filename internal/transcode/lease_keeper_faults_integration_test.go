//go:build integration

package transcode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-105, the faults of the lease keeper's own connection and statements. The
// workers run at their production lease (10 s) and heartbeat (3 s): the criteria
// are about the lines a renewal's bound (1.5 s), a retry and the cut (7 s after
// the last renewal) draw, and a shorter lease would move them. Nothing here
// judges how long something took; a hang is bounded by keeperHang.

// With only the pool held, the keeper's connection answers and renews every job's
// lease, so Postgres never sees the lease run out. A job whose loop is stuck on
// the pool (a paused job's look at the furthest segment has no bound of its own)
// makes nothing, holds its ffmpeg and its cores, and is never taken over unless
// the keeper gives it up when the work has not come round for a lease. The job
// is paused at its lead, a replacement serves the next request on another worker
// once the old job stopped being renewed, and the old worker stores nothing more.
func TestAJobWhoseWorkHasNotComeRoundIsGivenUpWhileTheKeeperStillAnswers(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_LEAD_SECONDS=8")
	r, held, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil { // One segment asked for: the job stops at its lead.
		t.Fatal(err)
	}
	old := r.jobOf(t, r.hash)
	var paused ffmpegProcess
	waitFor(t, "the job's ffmpeg to be paused at its lead", keeperHang, func() bool {
		for _, p := range r.ffmpegs() {
			if p.Parent == held.cmd.Process.Pid && p.State == "T" {
				paused = p
			}
		}
		return paused.Pid != 0
	})
	free := s.addWorker("JELLYSCALE_TEST_LEAD_SECONDS=8")
	held.db.Hold() // Only the pool. The keeper's connection and the lock's answer.
	t.Cleanup(held.db.Release)
	began := time.Now()
	if gone, _ := r.goneBefore(t, paused, time.Time{}); !gone {
		t.Fatalf("the ffmpeg of a job paused at its lead is still a process %v after the pool was held while the keeper answered", keeperHang)
	}
	t.Logf("the job's ffmpeg was gone %v after the pool was held (a lease is 10 s)", time.Since(began).Round(100*time.Millisecond))
	// The keeper stopped renewing the job it gave up: its lease runs out, which is what lets a replacement start.
	waitFor(t, "the given-up job's lease to run out", keeperHang, func() bool { return r.leaseRunOut(t, old) })
	if len(interruptedLines(held)) == 0 {
		t.Errorf("the worker did not log that it gave the job up as interrupted:\n%s", held.log.String())
	}
	missing := r.firstMissing(r.hash)
	attempt, stop := context.WithTimeout(ctx, keeperHang)
	defer stop()
	if _, err := s.fetch(attempt, s.jobs, r.spec, psid(0), missing); err != nil {
		t.Fatalf("the viewer's next request, for segment %d, with the old worker's pool held: %v", missing, err)
	}
	var worker string
	if err := r.pool.QueryRow(ctx, "SELECT worker FROM transcode_jobs WHERE spec_hash = $1 AND id > $2 ORDER BY id LIMIT 1", r.hash, old).Scan(&worker); err != nil {
		t.Fatalf("no replacement job after job %d: %v", old, err)
	}
	if worker != free.id {
		t.Errorf("the replacement runs on %q, want the free worker %s", worker, free.id)
	}
	held.db.Release()
	waitFor(t, "the old job to end", keeperHang, func() bool { return r.jobState(t, old) != "running" })
	if got := r.jobState(t, old); got != "cancelled" {
		t.Errorf("the job given up ended %s, want cancelled", got)
	}
	r.noFailed(t)
}

// Every round trip held 1 s on all of a worker's connections, set before the job
// is claimed: under half a heartbeat (1.5 s), so a renewal still fits its bound,
// and the connect of the keeper's connection takes several round trips, which
// is longer than a renewal's bound and has a bound of its own. The job is one
// row on one worker with one ffmpeg through the whole film, which takes more
// than two leases, and stores every segment. A keeper that gives its
// connect the renewal's bound can never connect, and a job claimed under the
// delay is cut at 7 seconds.
func TestASlowDatabaseSetBeforeTheClaimCostsNoJobItsLeaseOrItsWorker(t *testing.T) {
	slowAt(t, "2")
	// The pool as production opens it (see lease_keeper_integration_test.go). The queue's timeout
	// (6 s in production, counted from when the viewer's request queued the job) is lengthened:
	// a claim under 1 s round trips is more than six of them, which is the queue's rule and not
	// the keeper's.
	s := newStack(t, 1, productionPool, "JELLYSCALE_TEST_QUEUE_TIMEOUT=60s")
	r, w, ctx := s.r, s.workers[0], context.Background()
	count := playback.EncodedLayout(900000000).Count()
	// The pool's and the keeper's connections, before the claim. The lock's connection stays
	// as it was: its looks are bounded at half a second by JS-066 and a worker whose looks
	// all time out claims nothing, which is that task's rule and not the keeper's.
	w.db.Delay(time.Second)
	w.keep.Delay(time.Second)
	t.Cleanup(func() { w.db.Delay(0); w.keep.Delay(0) })
	s.jobs.QueueTimeout = time.Minute // A claim under 1 s round trips is several of them; the viewer's wait for a worker is not what is looked at.
	// The keeper's connection, if it holds one already, is made again, slowly, as the job starts.
	w.keep.Sever()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	s.play(s.jobs, r.spec, psid(0), 1, count)
	var expired atomic.Int64
	var mu sync.Mutex
	seen := map[string]bool{}
	sampling, stopSampling := context.WithCancel(ctx)
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for sampling.Err() == nil {
			var n int
			if r.pool.QueryRow(ctx, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at < now()").Scan(&n) == nil && n > 0 {
				expired.Add(int64(n))
			}
			mu.Lock()
			for _, p := range r.ffmpegs() {
				seen[fmt.Sprint(p.Pid, p.Start)] = true
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	waitFor(t, "the film to be done through a slow database", 6*time.Minute, func() bool { return r.jobState(t, job) != "running" })
	stopSampling()
	<-sampled
	if got := r.jobState(t, job); got != "done" {
		t.Errorf("the job ended %s through a slow database, want done", got)
	}
	if n := expired.Load(); n > 0 {
		t.Errorf("a running job's lease was found run out %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); n != 1 {
		t.Errorf("%d jobs for the spec, want 1: %+v", n, r.jobRows(t))
	}
	mu.Lock()
	if len(seen) != 1 {
		t.Errorf("%d different ffmpeg processes ran, want 1: %v", len(seen), seen)
	}
	mu.Unlock()
	stored := r.storedSegments(r.hash)
	if len(stored) != count || stored[0] != 0 || stored[len(stored)-1] != count-1 {
		t.Errorf("stored segments %v, want 0 to %d", stored, count-1)
	}
	r.noFailed(t)
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up through a slow database: %v", lines)
	}
}

// Under the same 1 s round trips, the keeper's connection alone is severed, twice:
// it connects again (several round trips, with a bound of its own) while the
// pool and the lock's connection answer, and the job goes on as one row, one
// worker and one ffmpeg, its lease in Postgres never found run out, with a
// renewal landing after each sever.
func TestSeveringTheKeepersConnectionAloneUnderASlowDatabaseCostsNoJob(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, productionPool)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	s.play(s.jobs, r.spec, psid(0), 1, playback.EncodedLayout(900000000).Count())
	waitFor(t, "the job to run with its ffmpeg and two segments", keeperHang, func() bool {
		_, ok := r.ffmpegOf(w)
		return ok && r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 2
	})
	w.delay(time.Second) // Set once the job runs: TestASlowDatabaseSetBeforeTheClaim... is the one that sets it before.
	t.Cleanup(func() { w.delay(0) })
	rowsBefore := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(w)
	var expired atomic.Int64
	sampling, stopSampling := context.WithCancel(ctx)
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for sampling.Err() == nil {
			if r.leaseRunOut(t, job) {
				expired.Add(1)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	// Severed once, as the criterion says: at a 1 s round trip a new connection
	// costs about 4 s, and a second sever while the first renewal's answer is in
	// flight leaves no time for a third connect before the cut (lead, JS-105).
	for i := range 1 {
		before, accepted := r.leaseOf(t, job), w.keep.Accepted()
		w.keep.Sever()
		waitFor(t, fmt.Sprintf("the keeper to connect again after sever %d", i+1), keeperHang, func() bool { return w.keep.Accepted() > accepted })
		waitFor(t, fmt.Sprintf("a renewal to land after sever %d", i+1), keeperHang, func() bool { return r.leaseOf(t, job).After(before) })
	}
	stopSampling()
	<-sampled
	if n := expired.Load(); n > 0 {
		t.Errorf("the job's lease was found run out %d times while only the keeper's connection was severed", n)
	}
	if got := r.jobRows(t); fmt.Sprint(got) != fmt.Sprint(rowsBefore) {
		t.Errorf("job rows were %+v and are %+v", rowsBefore, got)
	}
	if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
		t.Errorf("ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave the job up: %v", lines)
	}
	r.noFailed(t)
}

// Postgres away from the worker from 2.9 s after a renewal to 6.2 s after it
// (3.3 s): the heartbeat at 3 s fails, and a keeper that tries again only at the
// next heartbeat (6 s, inside the outage) has its cut at 7 s before the next one
// at 9 s. A failed renewal is tried again at once and then at a short interval, so
// one lands between 6.2 s and 7 s. Every job keeps its row, worker and ffmpeg.
// The job is paused at its lead first, so no fence before a put sends a renewal:
// the renewal the outage is timed from is a heartbeat's, and nothing but the
// keeper's own retry can land a renewal between the failed ones and the cut.
func TestAnOutageFrom2Point9SecondsAfterARenewalFor3Point3SecondsCostsNoJob(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_LEAD_SECONDS=8")
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil { // One segment asked for: the job stops at its lead.
		t.Fatal(err)
	}
	// The job is paused at its lead, as a viewer's job is most of the time. Then no segment waits to be
	// put, so the only renewals are the heartbeat's: a storing job's fence asks the keeper for a renewal
	// sent at once, and each of its asks would be a retry of a failed heartbeat that this outage must
	// not be given (a build that retries nothing, or only at the next heartbeat, cuts the paused job at
	// 7 s: heartbeats at 3 s and 6 s fail, the next is at 9 s).
	waitFor(t, "the job's ffmpeg to be paused at its lead", keeperHang, func() bool {
		p, ok := r.ffmpegOf(w)
		return ok && p.State == "T"
	})
	rowsBefore := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(w)
	sent := make(chan time.Time, 1)
	w.keep.OnStatement(isTheRenewal, func() { sent <- time.Now() }, nil)
	var at time.Time
	select {
	case at = <-sent:
	case <-time.After(keeperHang):
		t.Fatal("the worker sent no renewal")
	}
	time.Sleep(time.Until(at.Add(2900 * time.Millisecond)))
	w.away(true) // Pool, lock and keeper: Postgres is away from this worker.
	time.Sleep(3300 * time.Millisecond)
	w.back()
	waitFor(t, "the worker to hold its lock again", keeperHang, func() bool { return r.locked(w.id) })
	time.Sleep(2 * time.Second)
	if got := r.jobRows(t); fmt.Sprint(got) != fmt.Sprint(rowsBefore) {
		t.Errorf("job rows were %+v and are %+v: an outage of 3.3 s changed a job", rowsBefore, got)
	}
	if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
		t.Errorf("ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", w.id); n != 1 {
		t.Errorf("the worker's row was forgotten")
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up in an outage of 3.3 s: %v", lines)
	}
	r.noFailed(t)
}

// The liveness lock's connection refused for longer than a lease while the keeper's
// and the pool's answer: losing the lock is not losing the lease (DOMAINS 25), and
// the keeper is not on the lock's connection, so it renews through all of it. The
// worker keeps its row and the job goes on with the same ffmpeg; a build with the
// keeper on the lock's connection cuts the job at 7 s.
func TestTheLocksConnectionRefusedForLongerThanALeaseCostsTheKeepersJobNothing(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to run with its ffmpeg and a segment", keeperHang, func() bool {
		_, ok := r.ffmpegOf(w)
		return ok && r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND next_segment >= 1") == 1
	})
	job := r.jobOf(t, r.hash)
	rowsBefore := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(w)
	before := r.leaseOf(t, job)
	w.live.Away() // Only the lock's connection; every other query is answered.
	waitFor(t, "the lock to be gone", 10*time.Second, func() bool { _, held := s.lockPids()[w.id]; return !held })
	time.Sleep(12 * time.Second) // More than a lease.
	if got := r.leaseOf(t, job); !got.After(before) || r.leaseRunOut(t, job) {
		t.Errorf("the job's lease was %v and is %v (run out %v): with the lock's connection refused the keeper did not renew it",
			before.Format("15:04:05.000"), got.Format("15:04:05.000"), r.leaseRunOut(t, job))
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", w.id); n != 1 {
		t.Errorf("the worker was forgotten while only its lock's connection was refused")
	}
	if got := r.jobRows(t); fmt.Sprint(got) != fmt.Sprint(rowsBefore) {
		t.Errorf("job rows were %+v and are %+v", rowsBefore, got)
	}
	if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
		t.Errorf("ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave the job up: %v", lines)
	}
	w.live.Back()
	s.allLocked(30*time.Second, w)
	r.noFailed(t)
}

// A renewal counts only once Postgres has confirmed it, one table at a time. A
// worker keeps a re-encode and, for a burn-in job that waits for its track, the
// track's extraction (a subtitle job, which hangs). The extraction's renewal is
// made to fail (a trigger that raises on its lease being moved) and then to block
// past its bound (its table locked), each for longer than a lease. The transcode
// jobs' leases in Postgres keep moving, their ffmpeg and their rows stay, and
// no job is cut: the keeper's cut and Postgres agree, so no renewal that
// Postgres rolled back was counted (a batch of two tables in one transaction, a
// confirmation read from a rolled-back result), and none that Postgres kept was
// lost. The extraction itself is cut, a heartbeat before its lease can lapse.
func TestARenewalCountsOnlyWhenConfirmedSoASubtitleTablesFaultCostsTheTranscodeJobNothing(t *testing.T) {
	for _, mode := range []string{"fails", "blocks"} {
		t.Run(mode, func(t *testing.T) {
			testenv.FFmpeg(t)
			testenv.Font(t)
			film(t)
			shimDir := t.TempDir()
			real := testenv.FFmpeg(t)
			begun := filepath.Join(shimDir, "begun")
			// The extraction, the run that writes track files, waits for good.
			script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *track.vtt*) touch %q; while true; do sleep 0.1 >/dev/null 2>&1; done;; esac\nexec %q -readrate 1 \"$@\"\n", begun, real)
			if err := os.WriteFile(filepath.Join(shimDir, "ffmpeg"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			s := newStack(t, 1, "JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_LEAD_SECONDS=8")
			r, w, ctx := s.r, s.workers[0], context.Background()
			r.useBurnFilm(t)
			plain := r.spec
			burn := r.spec
			index := 2
			burn.Subtitle = &index
			if _, err := s.fetch(ctx, s.jobs, plain, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			go s.fetch(ctx, s.jobs, burn, psid(1), 0) //nolint:errcheck
			waitFor(t, "the extraction to run", keeperHang, func() bool { _, err := os.Stat(begun); return err == nil })
			// The re-encode is paused at its lead (one segment asked for), so it stores nothing and asks for no
			// fence during the looking below: the only renewals that can move its lease are the heartbeat's.
			// Were it storing, every segment's fence would send the transcode table's statement alone, which
			// commits, and a keeper that counts the heartbeat's rolled-back batch would pass unseen. The
			// burn-in job's own fate is not judged: its track is the one that is never extracted.
			waitFor(t, "the re-encode to be paused at its lead", keeperHang, func() bool {
				p, ok := r.ffmpegOf(w)
				return ok && p.State == "T"
			})
			encode := r.jobOf(t, plain.Hash())
			ffmpegBefore, _ := r.ffmpegOf(w)
			if ffmpegBefore.Pid == 0 {
				t.Fatal("the re-encode has no ffmpeg")
			}
			var key string
			if err := r.pool.QueryRow(ctx, "SELECT key FROM subtitle_jobs WHERE state = 'running' AND worker = $1", w.id).Scan(&key); err != nil {
				t.Fatalf("no running subtitle job on the worker: %v", err)
			}

			switch mode {
			case "fails":
				r.exec(t, `CREATE FUNCTION refuse_lease() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'the lease may not be moved'; END $$`)
				r.exec(t, `CREATE TRIGGER refuse_lease BEFORE UPDATE ON subtitle_jobs FOR EACH ROW
					WHEN (OLD.state = 'running' AND NEW.lease_expires_at IS DISTINCT FROM OLD.lease_expires_at) EXECUTE FUNCTION refuse_lease()`)
			case "blocks":
				tx, err := r.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { tx.Rollback(context.Background()) }) //nolint:errcheck
				if _, err := tx.Exec(ctx, "LOCK TABLE subtitle_jobs IN ACCESS EXCLUSIVE MODE"); err != nil {
					t.Fatal(err)
				}
			}
			if w.keep.Accepted() == 0 {
				t.Error("the lease keeper has no connection of its own: nothing came through the keeper's proxy")
			}
			start := time.Now()
			leases := map[int64]time.Time{encode: r.leaseOf(t, encode)}
			advanced := map[int64]int{}
			// Two leases (20 s) of looking, by the clock: the re-encode's lease keeps moving and is never run out.
			for time.Since(start) < 22*time.Second {
				for id, was := range leases {
					if now := r.leaseOf(t, id); now.After(was) {
						advanced[id]++
						leases[id] = now
					}
					if r.leaseRunOut(t, id) {
						t.Errorf("job %d's lease was found run out while only the subtitle table's renewal failed", id)
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			for id := range leases {
				if advanced[id] < 3 {
					t.Errorf("job %d's lease moved %d times in 22 s with the job paused, want at least 3: the heartbeat's transcode renewals are not confirmed apart from the subtitle table's", id, advanced[id])
				}
				if got := r.jobState(t, id); got != "running" {
					t.Errorf("job %d is %s, want running: the subtitle table's renewal cut it", id, got)
				}
			}
			if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
				t.Errorf("the re-encode's ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
			}
			// The extraction itself is cut when its own lease would lapse: no ffmpeg of its extraction remains.
			for _, p := range r.ffmpegs() {
				if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.Pid)); err == nil && strings.Contains(string(raw), "track.vtt") {
					t.Errorf("the extraction's ffmpeg %d is still a process 22 s after its renewals stopped being confirmed", p.Pid)
				}
			}
		})
	}
}

// Real contention on a worker's rows: a worker with two jobs while another
// session takes both rows in the opposite order, 200 times, as a reap's cut of
// a worker's jobs and a viewer's cancel do (each a statement over several rows).
// Postgres aborts one of two statements that wait for each other, after
// deadlock_timeout (lowered here for the database of this test, so that it is
// not the pause that is looked at). The keeper sends its ids in ascending order
// and retries at once when it is the one aborted, so no renewal is lost, no job
// is cut, no error reaches a viewer, and the worker's log has no failed renewal
// that was not the one abort. The heartbeat is 150 ms so that renewals meet the
// other session's locks; the lease keeps the production ratio to it.
func TestTwoJobsOfAWorkerAgainstOtherStatementsOverBothRowsLoseNoRenewalAndFailNoRequest(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_LEASE=1500ms", "JELLYSCALE_TEST_HEARTBEAT=150ms")
	r, w, ctx := s.r, s.workers[0], context.Background()
	r.exec(t, `DO $$ BEGIN EXECUTE format('ALTER DATABASE %I SET deadlock_timeout = ''20ms''', current_database()); END $$`)
	other := r.spec
	other.VideoBitrate = 1_200_000
	count := playback.EncodedLayout(900000000).Count()
	viewerA := s.play(s.jobs, r.spec, psid(0), 0, count)
	viewerB := s.play(s.jobs, other, psid(1), 0, count)
	waitFor(t, "both jobs to run", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", w.id) == 2
	})
	var ids []int64
	rows, err := r.pool.Query(ctx, "SELECT id FROM transcode_jobs WHERE worker = $1 ORDER BY id DESC", w.id) // Descending: the keeper's order reversed.
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id) //nolint:errcheck
		ids = append(ids, id)
	}
	rows.Close()
	rowsBefore := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(w)
	aborted := 0
	for round := range 200 {
		for attempt := 0; ; attempt++ {
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var failed error
			for _, id := range ids {
				if _, failed = tx.Exec(ctx, "UPDATE transcode_jobs SET lease_expires_at = lease_expires_at WHERE id = $1", id); failed != nil {
					break
				}
				time.Sleep(10 * time.Millisecond) // The second row is wanted while the first is held.
			}
			if failed != nil {
				tx.Rollback(ctx) //nolint:errcheck
				if strings.Contains(failed.Error(), "40P01") && attempt < 5 {
					aborted++
					continue
				}
				t.Fatalf("round %d: the other session's own statement: %v", round, failed)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	t.Logf("the other session was the one Postgres aborted %d times in 200 rounds", aborted)
	time.Sleep(time.Second)
	for _, id := range ids {
		if r.leaseRunOut(t, id) {
			t.Errorf("job %d's lease ran out under contention", id)
		}
	}
	if got := r.jobRows(t); fmt.Sprint(got) != fmt.Sprint(rowsBefore) {
		t.Errorf("job rows were %+v and are %+v", rowsBefore, got)
	}
	if p, ok := r.ffmpegOf(w); ok && ffmpegBefore.Pid != 0 && p.Pid != ffmpegBefore.Pid {
		t.Errorf("an ffmpeg was started again: %+v", p)
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up under contention: %v", lines)
	}
	if strings.Contains(w.log.String(), "40P01") || strings.Contains(strings.ToLower(w.log.String()), "deadlock") {
		t.Errorf("a deadlock reached the worker's log: a renewal aborted by Postgres is retried at once and logged as nothing:\n%s", w.log.String())
	}
	if e := viewerA.errs.Load() + viewerB.errs.Load(); e != 0 {
		t.Errorf("%d viewer requests failed", e)
	}
	r.noFailed(t)
}
