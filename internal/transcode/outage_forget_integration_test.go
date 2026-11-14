//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// JS-104, failure 1 (TestEveryJobOfEveryWorkerGoesOnThroughAnOutage/restart in
// merge queue run 43587752815): two live workers had their transcode_nodes rows
// forgotten, and their jobs were cancelled, by an outage that ended before any
// lease did.
//
// What the run's log shows: the first worker's row was already gone when its
// connections came back (the worker found it at 06.65, the replica was back at
// 06.55), nobody else was connected to Postgres but the stream replica, and the
// replica's reap loop (outage_integration_test.go: reaping) runs Reap every
// 100 ms through the replica's pool. The proxies end the workers' connections one
// after another and the replica's last, so a Reap sent in between runs with the
// replica's connection standing, the workers' locks gone, and no failure yet
// seen by the replica's pool, which is what Reap takes for "Postgres was not
// away". It forgot the workers whose connections had ended; the last answer
// was lost with the replica's own connection, so the harness's list of what it
// forgot stayed empty.
//
// The checks below force that interleaving, which a run can only wait for.

// sentHas matches a statement that contains every one of parts.
func sentHas(parts ...string) func([]byte) bool {
	return func(sent []byte) bool {
		for _, p := range parts {
			if !bytes.Contains(sent, []byte(p)) {
				return false
			}
		}
		return true
	}
}

// aJobAndItsOwner starts one job through the replica and waits until it runs and its
// worker's ffmpeg is there. It returns the owner and the others.
func (s *stack) aJobAndItsOwner(t *testing.T) (owner *outageWorker, others []*outageWorker) {
	t.Helper()
	if _, err := s.fetch(context.Background(), s.jobs, s.r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to run on a worker with its ffmpeg", 60*time.Second, func() bool {
		owner = s.worker(s.r.owner(t))
		if owner == nil {
			return false
		}
		_, ok := s.r.ffmpegOf(owner)
		return ok
	})
	for _, w := range s.workers {
		if w != owner {
			others = append(others, w)
		}
	}
	return owner, others
}

// nothingWasForgotten is what every check below ends on, once every process is
// back: every row was there at every sample, the job is the one it was on the
// same worker with the same ffmpeg, and it ended neither cancelled nor failed.
func (s *stack) nothingWasForgotten(t *testing.T, nodes *nodeWatch, before []jobRow, owner *outageWorker, ffmpegBefore ffmpegProcess) {
	t.Helper()
	r := s.r
	s.allLocked(60*time.Second, s.workers...)
	// Every worker has looked for its row by now (a look follows the lock taken again at once).
	time.Sleep(time.Second)
	if ids := nodes.forgotten(); len(ids) > 0 {
		t.Errorf("workers that were alive had their rows forgotten: %v", ids)
	}
	for _, w := range s.workers {
		if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+w.id+"'"); n != 1 {
			t.Errorf("worker %s has %d rows in transcode_nodes, want 1", w.id, n)
		}
	}
	if after := r.jobRows(t); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the jobs changed:\nbefore %+v\nafter  %+v", before, after)
	}
	if p, ok := r.ffmpegOf(owner); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
		t.Errorf("the owner's ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
	}
	r.noFailed(t)
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'cancelled'"); n != 0 {
		t.Errorf("%d jobs were cancelled by an outage that ended before their leases", n)
	}
}

// A replica holds no lock, and the only thing it runs that reads nodes is
// PlaybackInfo's Unavailable. In the moments the workers' connections end one
// after another and its own is still there, with no failure seen yet by its
// pool, it forgets nobody and cuts no lease (no replica code path deletes a
// transcode_nodes row: TestNoReplicaCodePathForgetsAWorker says so of the source).
// Two shapes of "one after another": every worker's connection has ended, and
// all but one has (the last ends within the same half second).
//
// This is a characterization: it passes on a build that forgets nobody, and
// fails on one whose replica deletes rows. The workers run at their production
// lease and heartbeat (see outage_kit).
func TestAReplicaForgetsNobodyWhileTheConnectionsEndOneByOne(t *testing.T) {
	for _, lastStillHolds := range []bool{false, true} {
		t.Run(fmt.Sprintf("the last worker still holds its lock %v", lastStillHolds), func(t *testing.T) {
			slowAt(t, "2")
			s := newStack(t, 3)
			r := s.r
			ctx := context.Background()
			owner, others := s.aJobAndItsOwner(t)
			gone, holder := append([]*outageWorker{owner}, others[0]), others[1]
			if !lastStillHolds {
				gone, holder = append(gone, holder), nil
			}
			before := r.jobRows(t)
			ffmpegBefore, _ := r.ffmpegOf(owner)
			nodes := s.watchNodes()
			s.reaping(t)
			s.afterRenewal()

			s.away(true, false, gone...)
			for i := 0; i < 5; i++ { // Every look a replica makes in the moment, with its connection standing.
				if _, err := s.jobs.Unavailable(ctx, r.hash, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			if holder != nil {
				holder.away(true) // Within the same half second, before the holder's own two looks.
			}
			s.replica.Sever()
			for _, w := range gone {
				if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+w.id+"'"); n != 1 {
					t.Errorf("worker %s's row is gone: %d", w.id, n)
				}
			}
			if left := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at > now() + interval '3 seconds'"); left != 1 {
				t.Errorf("the running job's lease was cut: %d running jobs have more than 3 s left, want 1", left)
			}
			s.back(true, s.workers...)
			s.nothingWasForgotten(t, nodes, before, owner, ffmpegBefore)
		})
	}
}

// A worker that is still connected when the others' locks are gone, because its
// own connections end within 0.5 s of theirs, forgets nobody and cuts no lease:
// its two looks (0.5 s apart) are real, so by its second the locks of all are
// gone, itself included, and a snapshot in which it cannot see its own lock
// forgets nobody (reap: lockHeld of self). This is a characterization of the
// look as it stands; a stagger beyond 0.5 s would need a longer confirmation
// gap, which costs failover time and is the lead's to decide, not this check's.
func TestAWorkerWhoseConnectionsEndWithinHalfASecondOfTheOthersForgetsNobody(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 3)
	r := s.r
	owner, others := s.aJobAndItsOwner(t)
	gone, holder := []*outageWorker{owner, others[0]}, others[1]
	before := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(owner)
	nodes := s.watchNodes()
	s.afterRenewal()

	s.away(true, false, gone...)
	time.Sleep(450 * time.Millisecond)
	holder.away(true)
	for _, w := range gone {
		if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+w.id+"'"); n != 1 {
			t.Errorf("worker %s's row is gone: %d", w.id, n)
		}
	}
	if left := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at > now() + interval '1 second'"); left != 1 {
		t.Errorf("the running job's lease was cut or run out: %d running jobs have more than 1 s left, want 1", left)
	}
	s.back(true, s.workers...)
	s.nothingWasForgotten(t, nodes, before, owner, ffmpegBefore)
}

// A worker that really is dead is forgotten as before, and its job's lease is
// cut so another worker may take it: killed while Postgres is up, by a live
// worker's two looks; killed during an outage, once every process is back and
// the outage is over (a live worker may not forget it while its own lock is
// being taken again: the table's hold).
func TestAWorkerThatIsReallyDeadIsForgottenAndItsJobsLeaseCut(t *testing.T) {
	for _, duringOutage := range []bool{false, true} {
		t.Run(fmt.Sprintf("killed during an outage %v", duringOutage), func(t *testing.T) {
			slowAt(t, "2")
			s := newStack(t, 3)
			r := s.r
			owner, _ := s.aJobAndItsOwner(t)
			var job int64
			if err := r.pool.QueryRow(context.Background(), "SELECT id FROM transcode_jobs WHERE state = 'running'").Scan(&job); err != nil {
				t.Fatal(err)
			}
			s.afterRenewal()
			if duringOutage {
				s.away(true, true, s.workers...)
				owner.kill()
				time.Sleep(time.Second)
				s.back(true, s.workers...)
			} else {
				owner.kill()
			}
			// A hang bound: the table's hold, two looks and the lease are well inside it.
			waitFor(t, "the dead worker to be forgotten", 90*time.Second, func() bool {
				return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+owner.id+"'") == 0
			})
			if left := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND state = 'running' AND lease_expires_at <= now() + interval '2 seconds'", job); left != 1 {
				// Another worker may have taken the job over already, which is as good.
				if state := r.jobState(t, job); state == "running" {
					t.Errorf("the dead worker's job %d is still running with a lease that is not cut", job)
				}
			}
			for _, w := range s.workers {
				if w != owner {
					if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = '"+w.id+"'"); n != 1 {
						t.Errorf("live worker %s has %d rows", w.id, n)
					}
				}
			}
		})
	}
}
