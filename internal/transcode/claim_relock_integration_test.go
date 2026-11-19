//go:build integration

package transcode

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-030, claim and re-lock. A worker's claim is a statement: it reads that the
// worker's row is there, and commits later. Between the two the worker may be
// forgotten, stop its jobs, lock again and put its row back, and the claim then
// takes a job under the session that ended, whose context is cancelled. The
// interleaving is forced by holding the claim's statement at a proxy (the
// statement is sent before the row is found gone and reaches Postgres after the
// worker has put its row back), so the checks fail on every run of today's tree.
// A one-line mutant of a right build that they fail: a claim that does not say
// which session it belongs to (what today's `registeredAs` is).

const claimStatement = "UPDATE transcode_jobs SET state = 'running', worker"

// proxiedWorker is an in-process worker whose pool goes through its own proxy
// (every statement in exec mode, so that the proxy sees its text) and whose
// liveness lock goes through another, so the check can hold a claim and refuse a lock.
type proxiedWorker struct {
	w          *Worker
	pool, live *testenv.DBProxy
}

func (r *rig) proxiedWorker(t *testing.T) *proxiedWorker {
	t.Helper()
	p := &proxiedWorker{pool: testenv.NewDBProxy(t, r.dsn), live: testenv.NewDBProxy(t, r.dsn)}
	pool, err := db.Open(context.Background(), p.pool.DSN()+"&default_query_exec_mode=exec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cfg, err := through(r.dsn, p.live.Addr())
	if err != nil {
		t.Fatal(err)
	}
	r.worker(t, func(w *Worker) {
		p.w = w
		w.Pool, w.LivenessConfig = pool, cfg
		w.Slots, w.HelpBelow = 1, 0.001
	})
	return p
}

func (r *rig) forget(t *testing.T, id string) {
	t.Helper()
	r.exec(t, "DELETE FROM transcode_nodes WHERE id = $1", id)
	r.exec(t, "SELECT pg_notify($1, $2)", channel, forgotHint(id)) // The reap's own hint.
}

// A claim sent before the worker's row is found gone and committed after the
// worker has stopped its jobs, locked again and put its row back takes a job
// the worker runs: the job is the worker's, runs to its first segment, is never
// written cancelled, and the viewer is served by that same job row.
func TestAClaimHeldAcrossAForgetAndARelockTakesAJobTheWorkerRuns(t *testing.T) {
	slowAt(t, "2")
	r := newRig(t, nil)
	p := r.proxiedWorker(t)
	id := p.w.ID
	held, release := make(chan struct{}), make(chan struct{})
	p.pool.OnStatement(sentHas(claimStatement), func() { close(held); <-release }, nil)

	j := r.jobs(t)
	type fetched struct {
		body []byte
		err  error
	}
	served := make(chan fetched, 1)
	go func() {
		body, err := r.fetch(t, j, psid(0), 0)
		served <- fetched{body, err}
	}()
	select {
	case <-held:
	case <-time.After(60 * time.Second):
		t.Fatal("the worker never sent its claim")
	}
	oldLock := r.lockPID(id)
	r.forget(t, id)
	waitFor(t, "the worker to lock again and put its row back", 60*time.Second, func() bool {
		pid := r.lockPID(id)
		return pid != 0 && pid != oldLock && r.locked(id)
	})
	close(release) // The claim reaches Postgres now.

	var got fetched
	select {
	case got = <-served:
	case <-time.After(90 * time.Second):
		t.Fatal("the viewer was not served")
	}
	if got.err != nil || len(got.body) == 0 {
		t.Fatalf("the viewer's segment: %d bytes, %v", len(got.body), got.err)
	}
	rows := r.jobRows(t)
	if len(rows) != 1 {
		t.Errorf("%d job rows for the spec, want the one the claim took: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.State == "cancelled" || row.State == "failed" {
			t.Errorf("job %d ended %s", row.ID, row.State)
		}
		if row.Worker != id {
			t.Errorf("job %d is %q's, want the worker's (%s)", row.ID, row.Worker, id)
		}
	}
}

// The bound of a claim, stated: a claim committed before the worker is forgotten
// is one of its jobs and ends with them (cancelled, never failed), and the
// viewer's next request starts the job again; no claim commits while the row is gone.
func TestAClaimCommittedBeforeTheWorkerIsForgottenEndsWithItsJobsAndOneCommittedAfterNeverCommits(t *testing.T) {
	slowAt(t, "2")
	t.Run("committed before the forget: cancelled, and the viewer's request starts it again", func(t *testing.T) {
		r := newRig(t, nil)
		p := r.proxiedWorker(t)
		id := p.w.ID
		committed, release := make(chan struct{}), make(chan struct{})
		// The answer is held after Postgres committed the claim.
		p.pool.OnStatement(sentHas(claimStatement), nil, func() { close(committed); <-release })
		j := r.jobs(t)
		served := make(chan error, 1)
		go func() { _, err := r.fetch(t, j, psid(0), 0); served <- err }()
		select {
		case <-committed:
		case <-time.After(60 * time.Second):
			t.Fatal("the worker never claimed")
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", id); n != 1 {
			t.Fatalf("%d jobs running under the worker when its claim had committed, want 1", n)
		}
		first := r.count(t, "SELECT max(id) FROM transcode_jobs")
		oldLock := r.lockPID(id)
		r.forget(t, id)
		waitFor(t, "the worker to lock again and put its row back", 60*time.Second, func() bool {
			pid := r.lockPID(id)
			return pid != 0 && pid != oldLock && r.locked(id)
		})
		close(release)
		select {
		case err := <-served:
			if err != nil {
				t.Fatalf("the viewer after the worker was forgotten: %v", err)
			}
		case <-time.After(90 * time.Second):
			t.Fatal("the viewer was not served")
		}
		var state string
		r.pool.QueryRow(context.Background(), "SELECT state FROM transcode_jobs WHERE id = $1", first).Scan(&state) //nolint:errcheck
		if state != "cancelled" {
			t.Errorf("the job claimed before the forget ended %s, want cancelled", state)
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); n != 0 {
			t.Errorf("%d jobs failed", n)
		}
		if r.count(t, "SELECT max(id) FROM transcode_jobs") == first {
			t.Error("the viewer's request did not start the job again")
		}
	})

	t.Run("no claim commits while the worker's row is gone", func(t *testing.T) {
		r := newRig(t, nil)
		p := r.proxiedWorker(t)
		id := p.w.ID
		held, release := make(chan struct{}), make(chan struct{})
		p.pool.OnStatement(sentHas(claimStatement), func() { close(held); <-release }, nil)
		j := r.jobs(t)
		go r.fetch(t, j, psid(0), 0) //nolint:errcheck
		select {
		case <-held:
		case <-time.After(60 * time.Second):
			t.Fatal("the worker never sent its claim")
		}
		p.live.Refuse(true) // It cannot lock again, so its row stays gone.
		r.forget(t, id)
		time.Sleep(time.Second)
		close(release)
		time.Sleep(3 * time.Second)
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE worker IS NOT NULL"); n != 0 {
			t.Errorf("a claim committed while the worker's row was gone: %d jobs have a worker", n)
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id); n != 0 {
			t.Errorf("the worker's row is back without its lock")
		}
		p.live.Refuse(false)
	})
}

// A claim that overlaps only a re-lock (the lock's connection ended and taken
// again, the row never gone) costs no job: with the lock's connection ended
// over and over while jobs are queued and claimed, every queued job is claimed by
// one worker and runs, none ends cancelled, none has two owners, and a worker
// still claims nothing while it does not hold its lock. ffmpeg is a shim that
// sleeps: what is asked is who claims, not what is encoded.
func TestALocksConnectionEndedOverAndOverCostsNoQueuedJobItsClaim(t *testing.T) {
	film(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\necho $$ >> "+filepath.Join(dir, "runs")+"\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newRig(t, nil)
	var ids []string
	for range 2 {
		r.worker(t, func(w *Worker) { w.Slots, w.HelpBelow = 1, 0.001; ids = append(ids, w.ID) })
	}
	const rounds = 200
	stop := make(chan struct{})
	endLocks := func() { // The lock's connection of each worker ended.
		for _, id := range ids {
			r.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND classid = `+livenessClass+
				` AND objsubid = 2 AND objid::bigint = hashtext($1)::bigint & 4294967295`, id) //nolint:errcheck
		}
	}
	owners := map[int64]map[string]bool{}
	var mu sync.Mutex
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			rows, err := r.pool.Query(context.Background(), "SELECT id, COALESCE(worker, '') FROM transcode_jobs WHERE state = 'running'")
			if err == nil {
				mu.Lock()
				for rows.Next() {
					var id int64
					var w string
					if rows.Scan(&id, &w) == nil {
						if owners[id] == nil {
							owners[id] = map[string]bool{}
						}
						owners[id][w] = true
					}
				}
				mu.Unlock()
				rows.Close()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	for round := range rounds {
		hash := fmt.Sprintf("relock-%03d", round)
		// The job starts in a state no worker claims, and becomes queued only once its viewer's row
		// exists: a worker's ticker that claimed it between the two inserts would see no viewer and cancel it.
		job := r.insertJob(t, hash, psid(round), "done", false)
		r.exec(t, "UPDATE transcode_jobs SET spec = $2 WHERE id = $1", job, mustSpecJSON(t, r))
		r.viewerRow(t, psid(round), hash, job, false, "0s")
		r.exec(t, "UPDATE transcode_jobs SET state = 'queued' WHERE id = $1", job)
		Notify(context.Background(), r.pool, jobHint)
		// The claim that the hint wakes and the end of the locks meet: neither waits for the other.
		time.Sleep(time.Duration(rand.IntN(15)) * time.Millisecond)
		endLocks()
		waitFor(t, fmt.Sprintf("round %d: the queued job to be claimed and run", round), 60*time.Second, func() bool {
			return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND state = 'running' AND worker IS NOT NULL", job) == 1
		})
		// The job is done with: the test ends it, with a reason of its own.
		r.exec(t, "UPDATE transcode_jobs SET state = 'done', ended_at = now() WHERE id = $1 AND state = 'running'", job)
	}
	close(stop)
	<-sampled
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'cancelled' OR state = 'failed'"); n != 0 {
		t.Errorf("%d of %d jobs ended cancelled or failed although only a lock's connection ended", n, rounds)
	}
	for id, who := range owners {
		if len(who) > 1 {
			t.Errorf("job %d had %d owners: %v", id, len(who), who)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'queued'"); n != 0 {
		t.Errorf("%d jobs were never claimed", n)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "runs")); len(raw) == 0 {
		t.Error("no ffmpeg ran")
	}
}
