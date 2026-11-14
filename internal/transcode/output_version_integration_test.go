//go:build integration

package transcode

import (
	"context"
	"fmt"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-082: every job row records the OutputVersion of what queued it, and a
// worker claims only rows of the version it makes. These tests use one name the
// task implies and the build must add: the column transcode_jobs.output_version
// (integer, NOT NULL, DEFAULT 4: what a replica built before the column writes
// is version 4, the last output version before rows recorded one). A worker
// here is of playback.OutputVersion, the only version one process can be; the
// worker of another version is a row of version OutputVersion+1, and the
// cross-version behaviour with two builds is in cmd/jellyscale.

// anotherVersion is a version the in-process worker does not make.
const anotherVersion = playback.OutputVersion + 1

// insertQueued inserts a queued job row of the rig's spec with the version
// given, created age ago, and returns its id.
func (r *rig) insertQueued(t *testing.T, version int, age time.Duration, helper bool) int64 {
	t.Helper()
	var id int64
	err := r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, created_at)
		VALUES ($1, '{}', $2, 0, 0, $3, $4, now() - make_interval(secs => $5)) RETURNING id`,
		r.hash, fmt.Sprintf("owner-%d-%d", version, time.Now().UnixNano()), helper, version, age.Seconds()).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *rig) versionOf(t *testing.T, id int64) int {
	t.Helper()
	return r.count(t, "SELECT output_version FROM transcode_jobs WHERE id = $1", id)
}

// A viewer's job records the OutputVersion of the replica that queued it.
func TestAViewersJobRecordsTheOutputVersionOfTheReplicaThatQueuedIt(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	id, _, err := j.ensure(context.Background(), r.spec, r.hash, psid(0), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.versionOf(t, id); got != playback.OutputVersion {
		t.Errorf("the job's output_version is %d, the replica's OutputVersion is %d", got, playback.OutputVersion)
	}
}

// A replica built before this release inserts no version: the row is accepted
// after the migration and is version 4. A worker that makes 4 claims it, and a
// worker of a later version does not (here: claims it exactly when the
// in-process worker's version is 4; the later-version side with real
// processes is in cmd/jellyscale).
func TestAJobWrittenWithNoVersionCountsAsVersion4(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	var id int64
	err := r.pool.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment)
		VALUES ($1, '{}', 'old-replica', 0, 0) RETURNING id`, r.hash).Scan(&id)
	if err != nil {
		t.Fatalf("a job row with no version, as a replica built before this release writes it, is refused: %v", err)
	}
	if got := r.versionOf(t, id); got != 4 {
		t.Errorf("the job a replica wrote with no version is version %d, want 4", got)
	}
	// An old row can be a helper as well.
	err = r.pool.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper)
		VALUES ($1, '{}', 'helper', 8, 8, true) RETURNING id`, r.hash).Scan(&id)
	if err != nil || r.versionOf(t, id) != 4 {
		t.Errorf("a helper row with no version: %v", err)
	}

	if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('w', 2)"); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: r.pool, ID: "w"}
	_, ok, err := w.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := playback.OutputVersion == 4; ok != want {
		t.Errorf("a worker of version %d claimed the version 4 job: %v, want %v", playback.OutputVersion, ok, want)
	}
}

// A worker never claims a job of another version, helper or not, queued or
// expired.
func TestAWorkerNeverClaimsAJobOfAnotherVersion(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		helper bool
		age    time.Duration
	}{
		{"a queued job", false, 0},
		{"a queued helper", true, 2 * time.Second},
		{"a job that has expired", false, time.Hour},
		{"a helper that has expired", true, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			id := r.insertQueued(t, anotherVersion, tc.age, tc.helper)
			// Slots to spare, so that a helper would be claimable.
			if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('w', 4)"); err != nil {
				t.Fatal(err)
			}
			w := &Worker{Pool: r.pool, ID: "w"}
			if got, ok, err := w.claim(ctx); err != nil || ok {
				t.Fatalf("a worker of version %d claimed job %d of version %d: %+v %v %v", playback.OutputVersion, id, anotherVersion, got, ok, err)
			}
			if state := mustScalar(t, r, fmt.Sprintf("SELECT state || ' ' || COALESCE(worker, '-') FROM transcode_jobs WHERE id = %d", id)); state != "queued -" {
				t.Errorf("the job is %q after the claim, want it left queued with no worker", state)
			}
		})
	}
}

// Jobs of another version ahead of a worker's own in the queue do not hold it
// up: the worker's own is claimed at once, not when the others expire.
func TestJobsOfAnotherVersionAheadInTheQueueDoNotHoldUpAWorker(t *testing.T) {
	ctx := context.Background()
	for _, ahead := range []int{1, 2, 7} {
		t.Run(fmt.Sprintf("%d ahead", ahead), func(t *testing.T) {
			r := newRig(t, nil)
			var others []int64
			for i := 0; i < ahead; i++ {
				others = append(others, r.insertQueued(t, anotherVersion, 0, false))
			}
			own := r.insertQueued(t, playback.OutputVersion, 0, false)
			if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('w', 4)"); err != nil {
				t.Fatal(err)
			}
			w := &Worker{Pool: r.pool, ID: "w"}
			got, ok, err := w.claim(ctx)
			if err != nil || !ok || got.ID != own {
				t.Fatalf("the claim with %d jobs of another version ahead: %+v %v %v, want job %d", ahead, got, ok, err, own)
			}
			for _, id := range others {
				if state := mustScalar(t, r, fmt.Sprintf("SELECT state FROM transcode_jobs WHERE id = %d", id)); state != "queued" {
					t.Errorf("job %d of another version is %s, want it left queued for a worker of its version", id, state)
				}
			}
			// The head of the queue must not be a lock the worker is stuck behind
			// either: a second claim finds nothing, and does not claim another's.
			if got, ok, err := w.claim(ctx); err != nil || ok {
				t.Errorf("a second claim took %+v %v %v, want nothing", got, ok, err)
			}
		})
	}
}

// A helper records the version of the job it helps, and a slow job's helper
// is claimed by a worker of that version: in a cluster of one version it still
// starts on another worker.
func TestAHelperRecordsTheVersionOfTheJobItHelps(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 2, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	r.playInBackground(t, j, psid(0))
	var helper job
	waitFor(t, "a helper to be making segments", time.Minute, func() bool {
		var ok bool
		helper, ok = r.helperMaking(t)
		return ok
	})
	rows := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND output_version <> $2", r.hash, playback.OutputVersion)
	if rows != 0 {
		t.Errorf("%d of the spec's jobs have a version other than %d, the version of the job the helper helps", rows, playback.OutputVersion)
	}
	if got := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND helper AND output_version = $2", helper.ID, playback.OutputVersion); got != 1 {
		t.Errorf("job %d, the helper at work, does not record version %d", helper.ID, playback.OutputVersion)
	}
}

// Slot accounting counts the version in question only. These tests name a
// second column the build must add in the same migration:
// transcode_nodes.output_version (integer, NOT NULL, DEFAULT 4, written by
// Register as playback.OutputVersion, so a pre-release worker counts as 4).
// The tests set it by hand, as a worker of another build would register.

func (r *rig) insertNode(t *testing.T, id string, slots, version int) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), "INSERT INTO transcode_nodes (id, slots, output_version) VALUES ($1, $2, $3)", id, slots, version); err != nil {
		t.Fatalf("a node of version %d: %v", version, err)
	}
}

// insertRunning inserts a job of the spec hash running on a live node.
func (r *rig) insertRunning(t *testing.T, hash, worker string, version int, helper bool) int64 {
	t.Helper()
	var id int64
	err := r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, state, worker, lease_expires_at)
		VALUES ($1, '{}', $2, 12, 13, $3, $4, 'running', $5, now() + interval '1 minute') RETURNING id`,
		hash, fmt.Sprintf("owner-%s-%d", hash, time.Now().UnixNano()), helper, version, worker).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The worker of the viewer's version is full of a viewer and a helper, and a
// worker of another version is idle: the next viewer preempts the helper,
// since the idle slots are no slot for its job.
func TestAnIdleWorkerOfAnotherVersionIsNoSlotForAViewerSoAHelperIsPreempted(t *testing.T) {
	r := newRig(t, nil)
	r.insertNode(t, "own", 2, playback.OutputVersion)
	r.insertNode(t, "other", 2, anotherVersion)
	r.insertRunning(t, "specA", "own", playback.OutputVersion, false)
	helper := r.insertRunning(t, "specA", "own", playback.OutputVersion, true)
	j := r.jobs(t)
	if _, _, err := j.ensure(context.Background(), r.spec, r.hash, psid(0), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := mustScalar(t, r, fmt.Sprintf("SELECT state || ' ' || COALESCE(error, '') FROM transcode_jobs WHERE id = %d", helper)); got != "cancelled a new viewer needed its slot" {
		t.Errorf("the helper is %q, want it cancelled for the new viewer: the only slots a job of version %d may use are held", got, playback.OutputVersion)
	}
}

// The other way round: a helper of another version, on a worker of its own
// version, holds nothing of the viewer's slots, and is not cancelled for a
// viewer that has a slot.
func TestAHelperOfAnotherVersionIsNotPreemptedForAViewerWithASlot(t *testing.T) {
	r := newRig(t, nil)
	r.insertNode(t, "own", 2, playback.OutputVersion)
	r.insertNode(t, "other", 1, anotherVersion)
	r.insertRunning(t, "specB", "other", anotherVersion, false) // Fills the other worker's slot.
	helper := r.insertRunning(t, "specB", "other", anotherVersion, true)
	j := r.jobs(t)
	if _, _, err := j.ensure(context.Background(), r.spec, r.hash, psid(0), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := mustScalar(t, r, fmt.Sprintf("SELECT state FROM transcode_jobs WHERE id = %d", helper)); got != "running" {
		t.Errorf("a helper of version %d is %s: a viewer with a free slot on its own version's worker took nothing from it", anotherVersion, got)
	}
}

// A queued helper of the worker's version is claimed only while a slot of its
// version stays free after it, however many idle slots another version has.
func TestAHelperIsNotClaimedForASlotOfAnotherVersion(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.insertNode(t, "own", 2, playback.OutputVersion)
	r.insertNode(t, "other", 2, anotherVersion)
	r.insertRunning(t, "specA", "own", playback.OutputVersion, false)
	helper := r.insertQueued(t, playback.OutputVersion, 2*time.Second, true)
	w := &Worker{Pool: r.pool, ID: "own"}
	if got, ok, err := w.claim(ctx); err != nil || ok {
		t.Fatalf("the worker claimed helper %d (%+v %v %v) with no slot of its version left to spare after it", helper, got, ok, err)
	}
}

// A helper is queued only while a slot of its version stays free after it:
// with the worker of the job's version at one viewer and one spare slot, and a
// worker of another version idle, no helper is queued.
func TestNoHelperIsQueuedOnTheStrengthOfAnotherVersionsIdleSlots(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	dir := r.ownStore(t)
	r.insertNode(t, "idle-other", 4, anotherVersion)
	r.workers(t, 1, 2, 6, dir) // The version of this build; one viewer leaves one slot.
	j := r.jobs(t)
	r.playInBackground(t, j, psid(0))
	waitFor(t, "the viewer's job to be running", time.Minute, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND NOT helper") == 1
	})
	// A helper needs two free slots of its version (one for itself, one for the
	// next viewer); there is one. The record of what happens is the rows; the
	// wait only gives a helper the chance to appear.
	time.Sleep(10 * time.Second)
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper"); n != 0 {
		t.Errorf("%d helper rows: a helper was queued for the idle slots of a worker of version %d, which cannot take it", n, anotherVersion)
	}
}

// Unavailable refuses a viewer's job when every slot of its version is taken,
// though a worker of another version has slots free.
func TestUnavailableCountsOnlySlotsOfTheViewersVersion(t *testing.T) {
	r := newRig(t, nil)
	r.insertNode(t, "own", 1, playback.OutputVersion)
	r.insertNode(t, "other", 3, anotherVersion)
	r.insertRunning(t, "specA", "own", playback.OutputVersion, false)
	j := r.jobs(t)
	reason, err := j.Unavailable(context.Background(), r.hash, r.user, "device")
	if err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Errorf("a transcode is offered with every slot of version %d taken and only another version's slots free; the viewer would wait queueTimeout for a 503", playback.OutputVersion)
	}
}
