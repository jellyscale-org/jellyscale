//go:build integration

package transcode

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// The checks of JS-066 on what a Stop cancels: the jobs of the specs of the
// viewers it stopped that nobody else is watching, and nothing else. Today the
// cancel is not scoped to those specs (jobs.go Stop), and it reads the viewers'
// job_id while ensure has inserted a viewer's new job and not yet written it
// there, so a Stop that lands in between cancels the job of a viewer who has
// not stopped.

// otherSpec is a second spec of the rig's film, which other viewers play.
func (r *rig) otherSpec() (playback.Spec, string) {
	other := r.spec
	other.AudioBitrate = 64_000
	return other, other.Hash()
}

// insertJob puts a job row straight in: a state the test wants, without a worker.
func (r *rig) insertJob(t *testing.T, hash, owner, state string, helper bool) int64 {
	t.Helper()
	var id int64
	err := r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version, state, worker, lease_expires_at, helper)
		VALUES ($1, '{}', $2, 0, 0, $3, $4, CASE WHEN $4 = 'running' THEN 'some-worker' END, CASE WHEN $4 = 'running' THEN now() + interval '1 hour' END, $5) RETURNING id`,
		hash, owner, playback.OutputVersion, state, helper).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *rig) jobState(t *testing.T, id int64) string {
	t.Helper()
	var state string
	if err := r.pool.QueryRow(context.Background(), "SELECT state FROM transcode_jobs WHERE id = $1", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// viewerRow writes a viewer's row as a test wants it to read at the moment of a Stop.
func (r *rig) viewerRow(t *testing.T, session, hash string, jobID int64, stopped bool, seenAgo string) {
	t.Helper()
	r.exec(t, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, job_id, stopped, last_seen_at)
		VALUES ($1, $2, $3, 'device-'||$1, $4, $5, now() - $6::interval)`, session, hash, r.user, jobID, stopped, seenAgo)
}

// A Stop cancels no job of a spec that none of the viewers it stopped was on,
// whatever that spec's viewer rows say at that moment: another viewer between two
// jobs (its row names a job that ended, or none), a spec whose viewers have all
// gone quiet, a spec with no row at all.
func TestAStopCancelsNoJobOfASpecItsViewersWereNotOn(t *testing.T) {
	r := newRig(t, nil)
	_, otherHash := r.otherSpec()
	j := r.jobs(t)
	ctx := context.Background()
	rows := []struct {
		name  string
		state string
		rows  func(job int64)
	}{
		{"its viewer's row names a job that ended", "queued", func(job int64) {
			r.viewerRow(t, psid(10), otherHash, r.insertJob(t, otherHash, psid(10), "cancelled", false), false, "0s")
		}},
		{"its viewer's row names no job", "queued", func(job int64) { r.viewerRow(t, psid(10), otherHash, 0, false, "0s") }},
		{"its viewer's row names a job of the spec that is not this one", "running", func(job int64) {
			r.viewerRow(t, psid(10), otherHash, r.insertJob(t, otherHash, psid(11), "running", false), false, "0s")
		}},
		{"its only viewer has not asked for a minute and a half", "running", func(job int64) { r.viewerRow(t, psid(10), otherHash, job, false, "90s") }},
		{"its only viewer was stopped", "running", func(job int64) { r.viewerRow(t, psid(10), otherHash, job, true, "0s") }},
		{"it has no viewer row at all", "queued", func(job int64) {}},
		{"it is a helper that nobody's row mentions", "running", func(job int64) {}},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			r.exec(t, "TRUNCATE transcode_jobs, transcode_viewers")
			// The viewer that stops, on the rig's spec, with a job of its own.
			if err := j.Watch(ctx, r.viewer(psid(0)), r.hash, -1); err != nil {
				t.Fatal(err)
			}
			own := r.insertJob(t, r.hash, psid(0), "queued", false)
			r.exec(t, "UPDATE transcode_viewers SET job_id = $1 WHERE play_session_id = $2", own, psid(0))
			other := r.insertJob(t, otherHash, psid(10), c.state, c.name == "it is a helper that nobody's row mentions")
			c.rows(other)
			if err := j.Stop(ctx, r.user, psid(0), "", ""); err != nil {
				t.Fatal(err)
			}
			if got := r.jobState(t, own); got != "cancelled" {
				t.Errorf("the stopped viewer's own job is %s, want cancelled", got)
			}
			if got := r.jobState(t, other); got != c.state {
				t.Errorf("a Stop of a viewer of another spec left the job of a spec it was not on %s, was %s", got, c.state)
			}
		})
	}
}

// moves is the race of both shapes below: a viewer whose previous job ended asks
// for a new one (ensure), while Stops of another viewer keep landing. Each round
// is a viewer pair of its own, so no row is shared between rounds. The stopper
// runs as long as ensure does, and once more after it, so the Stop is not only
// before it: it lands between the insert and the write of the viewer's job_id as
// often as the two can be made to meet.
func (r *rig) racesStopAgainstMove(t *testing.T, sameSpec bool) {
	t.Helper()
	mover, stopper := r.jobs(t), r.jobs(t)
	ctx := context.Background()
	moverSpec, moverHash := r.spec, r.hash
	stoppedHash := r.hash
	if !sameSpec {
		_, stoppedHash = r.otherSpec()
	}
	const rounds = 200
	var raced atomic.Int64
	for i := range rounds {
		r.exec(t, "TRUNCATE transcode_jobs, transcode_viewers")
		movingSession, stoppedSession := psid(2*i), psid(2*i+1)
		// The one that moves: its previous job ended, so its row names it.
		previous := r.insertJob(t, moverHash, movingSession, "cancelled", false)
		r.viewerRow(t, movingSession, moverHash, previous, false, "0s")
		// The one that stops, on the same spec or on another, with a job of its own.
		r.viewerRow(t, stoppedSession, stoppedHash, 0, false, "0s")
		if !sameSpec {
			own := r.insertJob(t, stoppedHash, stoppedSession, "queued", false)
			r.exec(t, "UPDATE transcode_viewers SET job_id = $1 WHERE play_session_id = $2", own, stoppedSession)
		}
		done := make(chan struct{})
		stops := make(chan int)
		const stoppers = 4
		for range stoppers {
			go func() {
				n := 0
				for {
					select {
					case <-done:
						stops <- n
						return
					default:
					}
					if err := stopper.Stop(ctx, r.user, stoppedSession, "", ""); err != nil {
						t.Error(err)
						stops <- n
						return
					}
					n++
				}
			}()
		}
		job, _, err := mover.ensure(ctx, moverSpec, moverHash, movingSession, 0, 0)
		close(done)
		for range stoppers {
			raced.Add(int64(<-stops))
		}
		if err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if err := stopper.Stop(ctx, r.user, stoppedSession, "", ""); err != nil {
			t.Fatal(err)
		}
		if got := r.jobState(t, job); got != "queued" {
			t.Fatalf("round %d: the job of a viewer that did not stop is %s after the Stop of another viewer of %s", i, got,
				map[bool]string{true: "its own spec", false: "another spec"}[sameSpec])
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state IN ('queued', 'running')", moverHash); n != 1 {
			t.Fatalf("round %d: %d live jobs for the viewer that moved, want the one it started", i, n)
		}
	}
	t.Logf("%d Stops raced %d moves", raced.Load(), rounds)
}

// Stops of one viewer racing another viewer's move to a new job after its
// previous job ended cancel nothing of that other viewer's, on another spec.
func TestStopsRacingAMoveOnAnotherSpecCancelNothingOfIt(t *testing.T) {
	newRigForRace(t).racesStopAgainstMove(t, false)
}

// On one spec, stopping one of two viewers never cancels the job the other is
// waiting on or last waited on, also while the other is being moved to a new job
// after its previous one ended (DOMAINS 38); the job it is moved to is the one
// its request is answered by.
func TestStopsRacingAMoveOnOneSpecCancelNothingTheOtherWaitsOn(t *testing.T) {
	newRigForRace(t).racesStopAgainstMove(t, true)
}

func newRigForRace(t *testing.T) *rig { return newRig(t, nil) }

// The moment the race is about, held still without assuming how a build writes it: a
// viewer whose previous job ended is being given a new job, and the write that makes
// its row name that job waits on a lock someone else holds. Whether the new job is
// already committed then (the job and the row naming it are written in two steps) or
// not (one transaction) is the build's choice; either way a Stop of another viewer
// that lands there must leave the new job alone, on the same spec and on another.
func TestAStopBetweenAJobsInsertAndTheViewersRowNamingItCancelsNothing(t *testing.T) {
	for _, sameSpec := range []bool{false, true} {
		name := "another spec"
		if sameSpec {
			name = "one spec"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			mover, stopper := r.jobs(t), r.jobs(t)
			ctx := context.Background()
			_, otherHash := r.otherSpec()
			stoppedHash := otherHash
			if sameSpec {
				stoppedHash = r.hash
			}
			previous := r.insertJob(t, r.hash, psid(0), "cancelled", false)
			r.viewerRow(t, psid(0), r.hash, previous, false, "0s")
			r.viewerRow(t, psid(1), stoppedHash, 0, false, "0s")
			// A shared lock on the mover's row: ensure's own shared lock goes through, and any
			// write to the row waits for it.
			hold, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(ctx) //nolint:errcheck
			if _, err := hold.Exec(ctx, "SELECT 1 FROM transcode_viewers WHERE play_session_id = $1 AND spec_hash = $2 FOR SHARE", psid(0), r.hash); err != nil {
				t.Fatal(err)
			}
			moved := make(chan int64, 1)
			go func() {
				id, _, err := mover.ensure(ctx, r.spec, r.hash, psid(0), 0, 0)
				if err != nil {
					t.Error(err)
				}
				moved <- id
			}()
			waitFor(t, "the write that names the new job in the viewer's row to wait on the lock", 30*time.Second, func() bool {
				return r.count(t, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()") >= 1
			})
			if err := stopper.Stop(ctx, r.user, psid(1), "", ""); err != nil {
				t.Fatal(err)
			}
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			id := <-moved
			if got := r.jobState(t, id); got != "queued" {
				t.Errorf("the job of the viewer that moved is %s after a Stop of another viewer of %s landed before its row named the job", got, name)
			}
		})
	}
}

// The other's request costs it no restart: with a worker making the job, two
// viewers of one spec wait on it, one stops, and the other is answered by the very
// job it was waiting on, which is still the spec's only job.
func TestAStopOfOneOfTwoViewersCostsTheOtherNoRestart(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	a, b := r.jobs(t), r.jobs(t)
	if _, err := r.fetch(t, a, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(t, b, psid(1), 0); err != nil {
		t.Fatal(err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Fatalf("%d jobs for two viewers of one spec", n)
	}
	job := r.count(t, "SELECT id FROM transcode_jobs")
	answered := make(chan error, 1)
	go func() { _, err := r.fetch(t, b, psid(1), 2); answered <- err }()
	for range 20 {
		if err := a.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-answered; err != nil {
		t.Fatalf("the viewer that did not stop: %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Errorf("%d jobs after the stop, want the one", n)
	}
	if state := r.jobState(t, int64(job)); state != "running" {
		t.Errorf("the job the other viewer waited on is %s", state)
	}
	if n := r.count(t, "SELECT job_id FROM transcode_viewers WHERE play_session_id = $1", psid(1)); n != job {
		t.Errorf("the viewer that did not stop watches job %d, not job %d", n, job)
	}
}

// A Stop still cancels at once every job of the stopped viewers' specs that no
// viewer who has not stopped is watching: the viewer's own, a job it joined and
// did not start, a queued one, and a helper when its spec's last viewer stops,
// so the slot is free for the next job; and not the job another viewer of the
// spec is watching, nor one of a spec the stop did not touch.
func TestAStopCancelsAtOnceWhatNobodyWatchesAnyMore(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	_, otherHash := r.otherSpec()
	r.exec(t, "TRUNCATE transcode_jobs, transcode_viewers")

	// Spec A, two viewers: X stops, Y stays.
	own := r.insertJob(t, r.hash, psid(0), "running", false)    // X started it.
	joined := r.insertJob(t, r.hash, psid(9), "running", false) // Z started it; X joined it, and Z has gone.
	queued := r.insertJob(t, r.hash, psid(0), "queued", false)
	helper := r.insertJob(t, r.hash, psid(0), "running", true)
	yours := r.insertJob(t, r.hash, psid(1), "running", false) // Y's.
	r.viewerRow(t, psid(0), r.hash, joined, false, "0s")
	r.viewerRow(t, psid(1), r.hash, yours, false, "0s")
	r.viewerRow(t, psid(9), r.hash, 0, true, "0s")
	// Spec B has its own viewer and job, untouched.
	b := r.insertJob(t, otherHash, psid(5), "running", false)
	r.viewerRow(t, psid(5), otherHash, b, false, "0s")
	// Spec C's last viewer has a helper and nothing else.
	cHash := fmt.Sprintf("%032x", 99)
	cHelper := r.insertJob(t, cHash, psid(7), "running", true)
	r.viewerRow(t, psid(7), cHash, 0, false, "0s")

	if err := j.Stop(ctx, r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]int64{"the job X started": own, "the job X joined and did not start": joined, "X's queued job": queued} {
		if got := r.jobState(t, id); got != "cancelled" {
			t.Errorf("%s is %s after X stopped, want cancelled", name, got)
		}
	}
	if got := r.jobState(t, yours); got != "running" {
		t.Errorf("Y's job is %s after X stopped", got)
	}
	// A helper belongs to every viewer of its spec: Y still has the spec, so it stays.
	if got := r.jobState(t, helper); got != "running" {
		t.Errorf("the helper of a spec Y still watches is %s after X stopped", got)
	}
	if got := r.jobState(t, b); got != "running" {
		t.Errorf("spec B's job is %s after a Stop of a viewer of spec A", got)
	}
	if err := j.Stop(ctx, r.user, psid(1), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.jobState(t, helper); got != "cancelled" {
		t.Errorf("the helper is %s after the last viewer of its spec stopped", got)
	}
	if got := r.jobState(t, yours); got != "cancelled" {
		t.Errorf("Y's job is %s after Y stopped", got)
	}
	if got := r.jobState(t, cHelper); got != "running" {
		t.Errorf("spec C's helper is %s though no viewer of spec C stopped", got)
	}
	if err := j.Stop(ctx, r.user, psid(7), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.jobState(t, cHelper); got != "cancelled" {
		t.Errorf("a helper is %s when its spec's last viewer stops, want cancelled", got)
	}
}
