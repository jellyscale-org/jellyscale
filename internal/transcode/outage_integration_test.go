//go:build integration

package transcode

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// JS-029: a Postgres failover, seen from the workers. The stream and api roles
// have their own checks in internal/api/outage_integration_test.go.

// reaping runs what an api or stream replica runs while it waits, through the
// replica's own pool (so it is away when the replica is), and remembers every
// worker it forgot.
func (s *stack) reaping(t *testing.T) (forgot func() []string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var forgotten []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if workers, _, _, err := Reap(ctx, s.jobs.Pool, ""); err == nil && len(workers) > 0 {
				mu.Lock()
				forgotten = append(forgotten, workers...)
				mu.Unlock()
			}
			s.jobs.Unavailable(ctx, "unused", "", "") //nolint:errcheck // A PlaybackInfo: it reaps too.
			time.Sleep(100 * time.Millisecond)
		}
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return func() []string {
		stop()
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(forgotten)
	}
}

// Three workers each run a job (a re-encode, a copy, and a re-encode paused at
// its lead) while viewers ask for segments, and Postgres goes away and comes
// back before any lease runs out. In both shapes of away, every job is the same
// row on the same worker with the same ffmpeg, none was started a second time,
// nothing was forgotten that is alive, and none ended failed or cancelled.
//
// The returns are staggered (the replica first, then the workers one by one),
// so a reap by the replica or by the worker that locked first happens while
// the others are still taking their lock again.
func TestEveryJobOfEveryWorkerGoesOnThroughAnOutage(t *testing.T) {
	shapes := []struct {
		name   string
		refuse bool            // New connections are refused until the return.
		hold   time.Duration   // How long they are, before the replica may connect again.
		back   []time.Duration // When each worker may connect again, after the replica.
		// Postgres ends every backend itself, with FATAL 57P01 rather than a closed socket
		// (see stack.terminate), and takes new connections at once.
		terminate bool
	}{
		// Every connection ended, new ones accepted at once: a switchover seen through a Service.
		{"switchover", false, 0, []time.Duration{0, 0, 0}, false},
		// Every connection ended, new ones refused for some seconds: a restart.
		{"restart", true, 1500 * time.Millisecond, []time.Duration{0, time.Second, 2 * time.Second}, false},
		// What a CNPG switchover does to a session: the server says it is shutting down.
		{"terminated by the server", false, 0, []time.Duration{0, 0, 0}, true},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			slowAt(t, "2")
			// Eight seconds of lead pauses the job whose viewer asks for nothing more. The
			// others' viewers ask as soon as they are served, so theirs run on.
			s := newStack(t, 3, "JELLYSCALE_TEST_LEAD_SECONDS=8")
			r := s.r
			enc := r.spec
			r.copied(t, "film.mkv")
			cp := r.spec
			r.spec, r.hash = enc, enc.Hash()
			low := enc
			low.VideoBitrate = 1_000_000

			encViewer := s.play(s.jobs, enc, psid(0), 0, 18)
			cpViewer := s.play(s.jobs, cp, psid(1), 0, 8)
			if _, err := s.fetch(context.Background(), s.jobs, low, psid(2), 0); err != nil {
				t.Fatal(err)
			}
			keep, stopKeeping := context.WithCancel(context.Background())
			t.Cleanup(stopKeeping)
			go func() {
				for keep.Err() == nil {
					s.jobs.Watch(keep, r.viewer(psid(2)), low.Hash(), -1) //nolint:errcheck
					time.Sleep(300 * time.Millisecond)
				}
			}()

			waitFor(t, "each worker to run one job, the third paused at its lead", 60*time.Second, func() bool {
				if encViewer.got.Load() < 3 || cpViewer.got.Load() < 2 {
					return false
				}
				for _, w := range s.workers {
					if r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = '"+w.id+"'") != 1 {
						return false
					}
					if _, ok := r.ffmpegOf(w); !ok {
						return false
					}
				}
				var lowWorker string
				if r.pool.QueryRow(context.Background(), "SELECT COALESCE(worker, '') FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running'", low.Hash()).Scan(&lowWorker) != nil || lowWorker == "" {
					return false
				}
				p, ok := r.ffmpegOf(s.worker(lowWorker))
				return ok && p.State == "T"
			})
			before := r.jobRows(t)
			ffmpegBefore := map[string]ffmpegProcess{}
			for _, w := range s.workers {
				ffmpegBefore[w.id], _ = r.ffmpegOf(w)
			}
			locksBefore := s.lockPids()
			nodes := s.watchNodes()
			forgot := s.reaping(t)
			servedBefore := encViewer.got.Load() + cpViewer.got.Load()

			s.afterRenewal()
			if shape.terminate {
				s.terminate(s.workers...)
			} else {
				s.away(shape.refuse, true, s.workers...)
			}
			time.Sleep(shape.hold)
			s.back(true) // The replica is back first, and looks for dead workers.
			begun := time.Now()
			var returned sync.WaitGroup
			for i, w := range s.workers {
				returned.Add(1)
				go func() {
					defer returned.Done()
					time.Sleep(max(0, shape.back[i]-time.Since(begun)))
					w.back()
				}()
			}
			returned.Wait()

			holders := s.allLocked(30*time.Second, s.workers...)
			for id, pid := range holders {
				if pid == locksBefore[id] {
					t.Errorf("worker %s holds its lock on the backend it held before the outage: the connections were not ended", id)
				}
			}
			// The rows are as they were.
			after := r.jobRows(t)
			if fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("the jobs changed:\nbefore %+v\nafter  %+v", before, after)
			}
			for _, w := range s.workers {
				if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore[w.id].Pid || p.Start != ffmpegBefore[w.id].Start {
					t.Errorf("worker %s's ffmpeg was %+v before the outage and is %+v (running %v) after it", w.id, ffmpegBefore[w.id], p, ok)
				}
			}
			for _, hash := range []string{enc.Hash(), cp.Hash(), low.Hash()} {
				if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = '"+hash+"'"); n != 1 {
					t.Errorf("spec %s has %d jobs, want the one it had", hash, n)
				}
			}
			// The viewers go on, and so does the paused job when one asks for what it can make.
			waitFor(t, "the viewers to be served after the outage", 60*time.Second, func() bool {
				return encViewer.got.Load()+cpViewer.got.Load() >= servedBefore+4
			})
			if _, err := s.fetch(context.Background(), s.jobs, low, psid(2), 3); err != nil {
				t.Errorf("the paused job's next segment: %v", err)
			}
			if after := r.jobRows(t); len(after) != len(before) {
				t.Errorf("a job was started: %+v", after)
			}
			if ids := nodes.forgotten(); len(ids) > 0 {
				t.Errorf("workers that were alive had their rows forgotten: %v", ids)
			}
			if ids := forgot(); len(ids) > 0 {
				t.Errorf("a reap forgot live workers that were taking their lock again: %v", ids)
			}
			for _, spec := range []string{enc.Hash(), cp.Hash(), low.Hash()} {
				if reason, err := s.jobs.Unavailable(context.Background(), spec, "", ""); err != nil || reason != "" {
					t.Errorf("PlaybackInfo would refuse spec %s after the outage: %q %v", spec, reason, err)
				}
			}
			r.noFailed(t)
			if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'cancelled'"); n != 0 {
				t.Errorf("%d jobs were cancelled by an outage that ended before their leases", n)
			}
			if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at <= now()"); n != 0 {
				t.Errorf("%d running jobs have a lease that has run out", n)
			}
			for _, w := range s.workers {
				loggedTheOutage(t, "worker "+w.id, w.log.String())
			}
		})
	}
}

// A segment ffmpeg finishes while Postgres is away is stored when it is back,
// and no segment is stored before: the check before every store (invariant 37)
// has to pass first. The job is the same row and ffmpeg throughout.
func TestASegmentFinishedWhileAwayIsStoredOnlyOnceTheCheckPassesAgain(t *testing.T) {
	// A copy too: its segments are gathered from ffmpeg's fragments, which are
	// gone once gathered, so a store that waits must not gather again (JS-029:
	// the restart shape failed a copy job so in the merge queue).
	for _, copied := range []bool{false, true} {
		t.Run(map[bool]string{false: "re-encode", true: "copy"}[copied], func(t *testing.T) {
			storedOnceTheCheckPasses(t, copied)
		})
	}
}

func storedOnceTheCheckPasses(t *testing.T, copied bool) {
	slowAt(t, "8") // A segment every half second.
	s := newStack(t, 1)
	r := s.r
	if copied {
		r.copied(t, "film.mkv")
	}
	w := s.workers[0]
	if _, err := s.fetch(context.Background(), s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a few segments", 30*time.Second, func() bool { return len(r.storedSegments(r.hash)) >= 3 })
	before := r.jobRows(t)
	// A copy's ffmpeg can finish the short film while Postgres is away, so only
	// the re-encode's is followed.
	ffmpegBefore, ok := r.ffmpegOf(w)
	if !ok && !copied {
		t.Fatal("no ffmpeg")
	}
	s.afterRenewal()
	s.away(true, true, w)
	severed := time.Now()
	time.Sleep(300 * time.Millisecond) // A store already past its check may land.
	stored := len(files(t, s.seg))
	time.Sleep(4 * time.Second) // Eight segments' worth of ffmpeg.
	if now := len(files(t, s.seg)); now != stored {
		t.Errorf("%d files were stored while Postgres was away", now-stored)
	}
	for _, p := range putsOf(t, s.putlog) {
		if p.at.After(severed.Add(300*time.Millisecond)) && p.at.Before(time.Now()) {
			t.Errorf("a store began at %v, while Postgres was away (since %v): %s", p.at, severed, p.key)
		}
	}
	if p, ok := r.ffmpegOf(w); !copied && (!ok || p.Pid != ffmpegBefore.Pid) {
		t.Fatalf("ffmpeg was %+v and is %+v (running %v): the outage ended the job", ffmpegBefore, p, ok)
	}
	s.back(true, w)
	// Nothing is lost: the segments made while it was away are stored, from the first, with no gap.
	waitFor(t, "the segments made while Postgres was away to be stored", 60*time.Second, func() bool { return r.firstMissing(r.hash) >= 9 })
	// A copy may have finished its film by now, which ends its job done.
	if after := r.jobRows(t); !copied && fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the job changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if p, ok := r.ffmpegOf(w); !copied && ok && p.Pid != ffmpegBefore.Pid {
		t.Errorf("ffmpeg was started again: %+v", p)
	}
	r.noFailed(t)
	loggedTheOutage(t, "the worker", w.log.String())
}

// An outage that comes between a worker storing a segment and writing that to
// its job, or at any other moment of a job's life, never ends a job failed
// ("the last attempt failed" would refuse the spec for 30 seconds); a job the
// outage does cut short is cancelled, and the viewer's next request is served.
func TestNoJobFailsAndNoSpecIsRefusedBecausePostgresWasAway(t *testing.T) {
	slowAt(t, "4")
	// The store takes 250 ms after it has the object, the moment an outage hits the update that follows.
	s := newStack(t, 2, "JELLYSCALE_TEST_PUTDELAY_AFTER_MS=250")
	r := s.r
	viewer := s.play(s.jobs, r.spec, psid(0), 0, 20)
	rng := rand.New(rand.NewPCG(29, 1))
	seen := len(putsOf(t, s.putlog))
	for round := range 8 {
		// Wait for the next store to begin, then cut every connection while it waits.
		waitFor(t, "a store to begin", 60*time.Second, func() bool { return len(putsOf(t, s.putlog)) > seen })
		time.Sleep(time.Duration(40+rng.IntN(120)) * time.Millisecond)
		seen = len(putsOf(t, s.putlog))
		switch round % 4 {
		case 0: // Every connection ended and refused for a moment: a restart.
			s.away(true, true, s.workers...)
			time.Sleep(time.Duration(500+rng.IntN(1000)) * time.Millisecond)
			s.back(true, s.workers...)
		case 1: // Every connection ended, new ones accepted: a switchover.
			s.away(false, true, s.workers...)
		case 2:
			// Only the connections that run queries, as when a pooler restarts or the pool
			// recycles them while the lock connection stands: the update after a store fails,
			// and nothing else tells the worker it was cut off.
			for _, w := range s.workers {
				w.db.Sever()
			}
			s.replica.Sever()
		case 3:
			// Postgres ends every backend itself: the update after the store meets FATAL 57P01
			// (admin_shutdown), not a closed socket, and so does the next statement of a worker
			// that was between two.
			s.terminate(s.workers...)
		}
		s.allLocked(30*time.Second, s.workers...)
		r.noFailed(t)
		seen = len(putsOf(t, s.putlog))
	}
	// The same moment, found rather than guessed: a row lock held by the test makes the
	// update that records a stored segment wait, and Postgres ends that backend while it
	// does. The statement fails with 57P01 exactly there.
	// A spec the rounds above did not make, so that a job runs for the whole phase.
	fresh := r.spec
	fresh.VideoBitrate = 1_000_000
	freshViewer := s.play(s.jobs, fresh, psid(2), 0, 40)
	waitFor(t, "the fresh spec's job to run", 60*time.Second, func() bool { return freshViewer.got.Load() >= 1 })
	for range 2 {
		if t.Failed() {
			break // The first terminated update already failed a job; the second would find nothing running.
		}
		ctx := context.Background()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Rolled back however the phase ends: a held lock would wedge the workers and the cleanup.
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, "SELECT 1 FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' FOR UPDATE", fresh.Hash()); err != nil {
			t.Fatal(err)
		}
		var pid int
		deadline := time.Now().Add(60 * time.Second)
		for r.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE wait_event_type = 'Lock'
			AND query LIKE 'UPDATE transcode_jobs SET next_segment%' LIMIT 1`).Scan(&pid) != nil {
			if time.Now().After(deadline) {
				rows, _ := r.pool.Query(ctx, "SELECT pid, state, wait_event_type, left(query, 90) FROM pg_stat_activity WHERE query LIKE 'UPDATE%' OR wait_event_type = 'Lock'")
				for rows != nil && rows.Next() {
					var pid int
					var state, kind, query *string
					rows.Scan(&pid, &state, &kind, &query) //nolint:errcheck
					t.Logf("backend %d %v %v: %v", pid, deref(state), deref(kind), deref(query))
				}
				t.Fatal("no worker's update of next_segment waited for the row the test locked")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := r.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
		tx.Rollback(ctx)            //nolint:errcheck
		time.Sleep(2 * time.Second) // The worker has ended or recorded the end of the job by now.
		s.allLocked(30*time.Second, s.workers...)
		r.noFailed(t)
	}
	s.logTranscodes()
	before := viewer.got.Load()
	waitFor(t, "the viewer to be served after the last outage", 90*time.Second, func() bool { return viewer.got.Load() >= before+3 })
	r.noFailed(t)
	if reason, err := s.jobs.Unavailable(context.Background(), r.hash, "", ""); err != nil || reason != "" {
		t.Errorf("PlaybackInfo refuses the spec: %q %v", reason, err)
	}
	// A segment nobody has asked for starts a job at once and is served, not refused.
	if _, err := s.fetch(context.Background(), s.jobs, r.spec, psid(1), 15); err != nil {
		t.Errorf("a segment after the outages: %v", err)
	}
}

// An outage that outlasts a lease: the worker stops that ffmpeg at its first
// heartbeat after the lease ran out, which is while Postgres is still away, and
// stores nothing from it after the lease ran out. The job is cancelled and not
// failed, and the viewer's next request starts it again at the first missing
// segment and is answered with the segment.
func TestAnOutageThatOutlastsTheLeaseStopsFfmpegAndTheNextRequestStartsItAgain(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r := s.r
	w := s.workers[0]
	ctx := context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a few segments", 30*time.Second, func() bool { return r.firstMissing(r.hash) >= 3 })
	s.afterRenewal()
	s.away(true, true, w)
	var leaseEnd time.Time
	if err := r.pool.QueryRow(ctx, "SELECT lease_expires_at FROM transcode_jobs WHERE state = 'running'").Scan(&leaseEnd); err != nil {
		t.Fatal(err)
	}
	// Past the lease and the heartbeat after it, with a long margin: Postgres is back only after ffmpeg is.
	waitFor(t, "ffmpeg to stop while Postgres is away", 120*time.Second, func() bool {
		_, running := r.ffmpegOf(w)
		return !running
	})
	for _, p := range putsOf(t, s.putlog) {
		if p.at.After(leaseEnd) {
			t.Errorf("a segment store began at %v, after the lease ran out at %v: %s", p.at, leaseEnd, p.key)
		}
	}
	stored := len(files(t, s.seg))
	time.Sleep(time.Second)
	s.back(true, w)
	s.allLocked(30*time.Second, w)
	if now := len(files(t, s.seg)); now != stored {
		t.Errorf("%d files were stored after ffmpeg was stopped", now-stored)
	}
	r.noFailed(t)
	missing := r.firstMissing(r.hash)
	body, err := s.fetch(ctx, s.jobs, r.spec, psid(0), missing)
	if err != nil || !isMP4Segment(body) {
		t.Fatalf("the first missing segment (%d) after the outage: %d bytes, %v", missing, len(body), err)
	}
	r.noFailed(t)
	if n := r.count(t, fmt.Sprintf("SELECT count(*) FROM transcode_jobs WHERE start_segment = %d AND id > (SELECT min(id) FROM transcode_jobs)", missing)); n != 1 {
		t.Errorf("%d jobs started at segment %d, the first missing: %+v", n, missing, r.jobRows(t))
	}
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs ORDER BY id LIMIT 1"); state != "cancelled" {
		t.Errorf("the job the outage cut short is %s, want cancelled", state)
	}
}

// A worker killed while Postgres is away is forgotten once Postgres is back,
// and the other worker continues its job where the segments stop; the viewer
// who was waiting on it is answered with the segment.
func TestAWorkerKilledWhilePostgresIsAwayIsForgottenAndAnotherContinuesItsJob(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 2)
	r := s.r
	ctx := context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 1); err != nil {
		t.Fatal(err)
	}
	owner := s.worker(r.owner(t))
	var survivor *outageWorker
	for _, w := range s.workers {
		if w != owner {
			survivor = w
		}
	}
	owned := r.watchOwners(t)
	s.afterRenewal()
	s.away(true, true, s.workers...)
	owner.kill()
	waitFor(t, "the killed worker's ffmpeg to be gone", 10*time.Second, func() bool { return len(r.ffmpegs()) == 0 })
	time.Sleep(300 * time.Millisecond)
	missing := r.firstMissing(r.hash)
	// The viewer asks for the first missing segment while nothing answers.
	answered := make(chan error, 1)
	go func() {
		attempt, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		for {
			body, err := s.fetch(attempt, s.jobs, r.spec, psid(0), missing)
			if err == nil && isMP4Segment(body) {
				answered <- nil
				return
			}
			if attempt.Err() != nil {
				answered <- fmt.Errorf("never answered: %v", err)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	time.Sleep(2 * time.Second)
	s.back(true, s.workers...)
	if err := <-answered; err != nil {
		t.Fatalf("the viewer waiting for segment %d: %v", missing, err)
	}
	waitFor(t, "the killed worker to be forgotten", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+owner.id+"'") == 0
	})
	if n := r.count(t, fmt.Sprintf("SELECT count(*) FROM transcode_jobs WHERE worker = '%s' AND start_segment = %d", survivor.id, missing)); n != 1 {
		t.Errorf("want one job of the surviving worker at segment %d, the first missing: %+v", missing, r.jobRows(t))
	}
	owned.check(t)
	r.noFailed(t)
}

// A worker takes its lock again first and only then looks for its row. Finding
// it gone, because another replica took the worker for dead while it was away,
// it stops every ffmpeg and puts the row back only afterwards, and it stores
// nothing after it was forgotten.
func TestAWorkerThatFindsItsRowGoneStopsFfmpegBeforePuttingTheRowBack(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r := s.r
	w := s.workers[0]
	ctx := context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.ffmpegOf(w); !ok {
		t.Fatal("no ffmpeg")
	}
	s.afterRenewal()
	s.away(true, true, w)
	// What a replica's reap does to a worker whose lock is gone, done by hand because a build may
	// wait before it forgets a worker that could still be taking its lock again.
	forgotAt := time.Now()
	if _, err := r.pool.Exec(ctx, `DELETE FROM transcode_nodes WHERE id = $1`, w.id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE transcode_jobs SET lease_expires_at = LEAST(lease_expires_at, now() + interval '1 second') WHERE worker = $1 AND state = 'running'`, w.id); err != nil {
		t.Fatal(err)
	}
	s.back(true, w)
	var rowBackAfterFfmpeg bool
	var rowBackAt time.Time
	waitFor(t, "the worker to put its row back", 60*time.Second, func() bool {
		present := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+w.id+"'") == 1
		_, running := r.ffmpegOf(w) // Looked at after the row, so a row already there means ffmpeg is already gone.
		if present {
			rowBackAfterFfmpeg, rowBackAt = !running, time.Now()
		}
		return present
	})
	if !rowBackAfterFfmpeg {
		t.Error("the worker's row was back while its ffmpeg still ran")
	}
	s.allLocked(30*time.Second, w)
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == w.id && p.at.After(forgotAt.Add(300*time.Millisecond)) && p.at.Before(rowBackAt) {
			t.Errorf("the worker stored %s while its row was gone", p.key)
		}
	}
	// It takes a job again once the row is back.
	missing := r.firstMissing(r.hash)
	if body, err := s.fetch(ctx, s.jobs, r.spec, psid(0), missing); err != nil || !isMP4Segment(body) {
		t.Errorf("segment %d after the worker came back: %d bytes, %v", missing, len(body), err)
	}
	r.noFailed(t)
}

// While a worker does not hold its lock it claims no new job, however long
// taking the lock again takes, though Postgres answers everything else.
// (This holds before the change too; it guards the change, which moves the
// worker's lock check.)
func TestAWorkerWithoutItsLockClaimsNoJobHoweverLongLockingAgainTakes(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r := s.r
	w := s.workers[0]
	ctx := context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	other := r.spec
	other.VideoBitrate = 900_000
	w.live.Away() // Only the liveness connection; every other query is answered.
	waitFor(t, "the lock to be gone", 10*time.Second, func() bool { _, held := s.lockPids()[w.id]; return !held })
	asked := make(chan struct{})
	go func() {
		attempt, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		close(asked)
		s.fetch(attempt, s.jobs, other, psid(5), 0) //nolint:errcheck
	}()
	<-asked
	waitFor(t, "the second spec's job to be queued", 10*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = '"+other.Hash()+"'") == 1
	})
	for range 40 { // Four seconds of looking.
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = '"+other.Hash()+"' AND worker IS NOT NULL"); n != 0 {
			t.Fatal("the worker claimed a job while it did not hold its lock")
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.live.Back()
	s.allLocked(30*time.Second, w)
}

// A worker that cannot take its lock again while Postgres answers its other
// queries, and that another replica has forgotten, stops its ffmpeg within the
// grace Reap gave its jobs, before a replacement's ffmpeg starts.
func TestAWorkerThatCannotLockAgainAndWasForgottenStopsBeforeTheReplacementStarts(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 2)
	r := s.r
	ctx := context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 1); err != nil {
		t.Fatal(err)
	}
	owner := s.worker(r.owner(t))
	owned := r.watchOwners(t)
	s.afterRenewal()
	owner.live.Away() // The lock cannot be taken again, for as long as the test says.
	// Another replica forgets it: this is Reap as it runs, once the lock is gone.
	waitFor(t, "a reap to forget the worker", 60*time.Second, func() bool {
		Reap(ctx, r.pool, "") //nolint:errcheck // The worker's own look may be the one that forgets it.
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+owner.id+"'") == 0
	})
	missing := r.firstMissing(r.hash)
	body, err := s.fetch(ctx, s.jobs, r.spec, psid(0), missing)
	if err != nil || !isMP4Segment(body) {
		t.Fatalf("segment %d from the replacement: %d bytes, %v", missing, len(body), err)
	}
	if _, running := r.ffmpegOf(owner); running {
		t.Error("the forgotten worker's ffmpeg still runs beside the replacement's")
	}
	if n := r.count(t, fmt.Sprintf("SELECT count(*) FROM transcode_jobs WHERE worker <> '%s' AND start_segment = %d", owner.id, missing)); n != 1 {
		t.Errorf("want the other worker's job at segment %d: %+v", missing, r.jobRows(t))
	}
	owner.live.Back()
	owned.check(t)
	r.noFailed(t)
}

// Invariant 25 under contention, with real processes: three workers run one
// spec's job while their lock connections are ended one worker at a time and all
// together, by Postgres (a terminated backend) and by the network (every
// connection of the worker cut), and the other workers and a stream replica
// reap all the while. The old owner's ffmpeg and a replacement's never run at
// once, no job has two workers, no worker stores a segment while its row is
// gone, and each round ends with the job still on its worker and ffmpeg, or
// with that worker's ffmpeg gone.
func TestNoJobHasTwoOwnersWhileLockConnectionsAreEndedOverAndOver(t *testing.T) {
	slowAt(t, "1")
	s := newStack(t, 3)
	r := s.r
	viewer := s.play(s.jobs, r.spec, psid(0), 0, 22)
	waitFor(t, "the job to run", 60*time.Second, func() bool { return viewer.got.Load() >= 1 && r.owner(t) != "" })
	owned := r.watchOwners(t)
	nodes := s.watchNodes()
	forgot := s.reaping(t)
	rng := rand.New(rand.NewPCG(25, 2))
	type round struct {
		name string
		do   func()
	}
	pick := func() *outageWorker { return s.workers[rng.IntN(len(s.workers))] }
	rounds := []round{
		{"one lock backend terminated", func() { terminateQuietly(r, pick().id) }},
		{"every lock backend terminated", func() {
			for _, w := range s.workers {
				terminateQuietly(r, w.id)
			}
		}},
		{"one worker's connections cut", func() { w := pick(); w.away(false) }},
		{"every worker's and the replica's connections cut", func() {
			for _, w := range s.workers {
				w.away(false)
			}
			s.replica.Sever()
		}},
		{"the owner's lock backend terminated", func() { terminateQuietly(r, r.owner(t)) }},
		{"every connection cut and refused for a second", func() {
			for _, w := range s.workers {
				w.away(true)
			}
			s.replica.Away()
			time.Sleep(time.Second)
			s.back(true, s.workers...)
		}},
	}
	for _, round := range rounds {
		s.allLocked(60*time.Second, s.workers...)
		waitFor(t, "a running job", 60*time.Second, func() bool { return r.owner(t) != "" })
		jobBefore := mustScalar(t, r, "SELECT id || ' ' || worker FROM transcode_jobs WHERE state = 'running' ORDER BY id DESC LIMIT 1")
		var pidsBefore []ffmpegProcess
		for _, w := range s.workers {
			if p, ok := r.ffmpegOf(w); ok {
				pidsBefore = append(pidsBefore, p)
			}
		}
		t.Logf("round %q", round.name)
		round.do()
		s.allLocked(60*time.Second, s.workers...)
		before := viewer.got.Load()
		waitFor(t, "the viewer to be served after "+round.name, 90*time.Second, func() bool { return viewer.got.Load() > before })
		jobAfter := ""
		if r.owner(t) != "" {
			jobAfter = mustScalar(t, r, "SELECT id || ' ' || worker FROM transcode_jobs WHERE state = 'running' ORDER BY id DESC LIMIT 1")
		}
		if jobAfter != jobBefore {
			// Another job, or another worker's: the old ffmpeg must be gone.
			for _, old := range pidsBefore {
				for _, now := range r.ffmpegs() {
					if now.Pid == old.Pid && now.Start == old.Start {
						t.Errorf("after %s the job went from %q to %q but ffmpeg %d of the old one still runs", round.name, jobBefore, jobAfter, old.Pid)
					}
				}
			}
		}
	}
	viewer.stop()
	owned.check(t)
	r.noFailed(t)
	// No worker stored a segment while its row was gone (a store that began as the row was
	// deleted is the check before it passing a moment earlier, so a quarter second is allowed).
	nodes.stop()
	nodes.mu.Lock()
	for id, gone := range nodes.gone {
		for i, at := range gone {
			until := time.Now()
			if i < len(nodes.back[id]) {
				until = nodes.back[id][i]
			}
			for _, p := range putsOf(t, s.putlog) {
				if p.worker == id && p.at.After(at.Add(250*time.Millisecond)) && p.at.Before(until) {
					t.Errorf("worker %s stored %s at %v, %v after its row was forgotten and before it came back", id, p.key, p.at, p.at.Sub(at))
				}
			}
		}
	}
	nodes.mu.Unlock()
	t.Logf("rows forgotten during the rounds: %v; forgotten by the replica's reap: %v", nodes.forgotten(), forgot())
}

// terminateQuietly ends the backend that holds a worker's lock, if it holds one: a
// worker that is just taking it again is as good a victim.
func terminateQuietly(r *rig, id string) {
	r.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = `+livenessClass+` AND objsubid = 2 AND objid::bigint = hashtext($1)::bigint & 4294967295`, id) //nolint:errcheck
}

// The row that says a segment was stored is written after the store. If the
// connection that writes it ends as the statement runs, the segment is stored
// and its row is not: the job is not failed for it (a failed job refuses its
// spec for 30 seconds) and the viewer is served. Only the pool's connections
// end here, not the lock's, so nothing else tells the worker it was cut off, and
// the next statement finds Postgres at once.
func TestAStoreWhoseRowCannotBeWrittenDoesNotFailTheJob(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r := s.r
	w := s.workers[0]
	viewer := s.play(s.jobs, r.spec, psid(0), 0, 8)
	waitFor(t, "the first segment", 60*time.Second, func() bool { return viewer.got.Load() >= 1 })
	for range 2 {
		tripped := w.db.SeverOn("UPDATE transcode_jobs SET next_segment")
		select {
		case <-tripped:
		case <-time.After(60 * time.Second):
			t.Fatal("the worker never wrote the row of a stored segment")
		}
		time.Sleep(2 * time.Second) // The worker has ended or recorded the end of the job by now.
		r.noFailed(t)
	}
	waitFor(t, "the viewer to be served", 90*time.Second, func() bool { return viewer.got.Load() >= 6 })
	s.logTranscodes()
	r.noFailed(t)
	if reason, err := s.jobs.Unavailable(context.Background(), r.hash, "", ""); err != nil || reason != "" {
		t.Errorf("PlaybackInfo refuses the spec: %q %v", reason, err)
	}
}

// logTranscodes shows how each job ended, for the reader of a failure.
func (s *stack) logTranscodes() {
	for _, w := range s.workers {
		for _, line := range strings.Split(w.log.String(), "\n") {
			if strings.Contains(line, "transcode ") || strings.Contains(line, "upload") {
				s.t.Log(line[:min(len(line), 300)])
			}
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Postgres ends the three workers' lock connections one after another, 100 ms
// apart (a switchover reaches each node's connections when it gets to them).
// The first to lose its lock sees the others still holding theirs, and later
// ones see it back: nobody takes anyone for dead, and no ffmpeg changes.
func TestWorkersWhoseLockBackendsAreEndedOneByOneKeepEveryFfmpeg(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 3)
	r := s.r
	enc := r.spec
	r.copied(t, "film.mkv")
	cp := r.spec
	r.spec, r.hash = enc, enc.Hash()
	low := enc
	low.VideoBitrate = 1_000_000
	s.play(s.jobs, enc, psid(0), 0, 40)
	s.play(s.jobs, cp, psid(1), 0, 40)
	s.play(s.jobs, low, psid(2), 0, 40)
	waitFor(t, "each worker to run one job", 60*time.Second, func() bool {
		for _, w := range s.workers {
			if _, ok := r.ffmpegOf(w); !ok {
				return false
			}
		}
		return true
	})
	before := r.jobRows(t)
	ffmpegBefore := map[string]ffmpegProcess{}
	for _, w := range s.workers {
		ffmpegBefore[w.id], _ = r.ffmpegOf(w)
	}
	locksBefore := s.lockPids()
	nodes := s.watchNodes()
	for _, w := range s.workers {
		r.terminate(t, w.id)
		time.Sleep(100 * time.Millisecond)
	}
	holders := s.allLocked(30*time.Second, s.workers...)
	for id, pid := range holders {
		if pid == locksBefore[id] {
			t.Errorf("worker %s holds its lock on the backend it held before: it was not ended", id)
		}
	}
	// Longer than a look and a confirmation, so a forgetting would have happened.
	time.Sleep(3 * liveTiming("grace"))
	if after := r.jobRows(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the jobs changed:\nbefore %+v\nafter  %+v", before, after)
	}
	for _, w := range s.workers {
		if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore[w.id].Pid || p.Start != ffmpegBefore[w.id].Start {
			t.Errorf("worker %s's ffmpeg was %+v and is %+v (running %v)", w.id, ffmpegBefore[w.id], p, ok)
		}
	}
	if ids := nodes.forgotten(); len(ids) > 0 {
		t.Errorf("workers that were alive had their rows forgotten: %v", ids)
	}
}

// Every backend of a worker and of a replica ends, and a viewer's PlaybackInfo
// comes while the worker's lock is still gone: the worker is taking it again,
// so the replica waits a moment for it and counts the worker, and it forgets no
// row (it may have missed this very outage).
func TestAReplicaAsksAboutAWorkerWhoseLockIsGoneAndDeletesNothing(t *testing.T) {
	s := newStack(t, 1)
	r := s.r
	w := s.workers[0]
	waitFor(t, "the worker to register", 30*time.Second, func() bool { return r.locked(w.id) })
	nodes := s.watchNodes()
	before := s.lockPids()
	w.away(true) // New connections are refused until it is back.
	if _, err := s.replica.Terminate(context.Background(), r.pool); err != nil {
		t.Fatal(err)
	}
	s.waitEnded(before, s.workers)
	if _, held := s.lockPids()[w.id]; held {
		t.Fatal("the worker still holds its lock")
	}
	time.AfterFunc(300*time.Millisecond, w.back)
	var reason string
	for attempt := 0; ; attempt++ {
		var err error
		if reason, err = s.jobs.Unavailable(context.Background(), r.hash, "", ""); err == nil {
			break
		} else if attempt == 20 {
			t.Fatal(err) // The replica's own connections were ended too.
		}
	}
	if reason != "" {
		t.Errorf("PlaybackInfo refused with a live worker taking its lock again: %q", reason)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_nodes"); n != 1 {
		t.Errorf("%d rows in transcode_nodes, want the worker's", n)
	}
	s.allLocked(30*time.Second, s.workers...)
	if ids := nodes.forgotten(); len(ids) > 0 {
		t.Errorf("rows were forgotten: %v", ids)
	}
}
