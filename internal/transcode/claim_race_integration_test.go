//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
)

// The two guards of JS-066's one-owner and one-live-job invariants, run against
// each other at the moment they matter, with connections of their own, many
// rounds, and nothing slowed down: the row lock of a claim (Worker.claim) and
// the per-spec lock of ensure. The randomized schedules meet both guards as well,
// whenever a job dies or a free worker is woken for a queued one; these are the
// same contention with the rest taken away, so that a build without a guard fails
// in every run and not in the runs where the schedule happened to meet it.

// Several free workers woken for one queued job at once: exactly one of them gets
// it, and the row says which.
func TestSeveralFreeWorkersWokenForOneQueuedJobClaimItOnce(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	const workers, rounds = 6, 200
	var all []*Worker
	for i := range workers {
		pool, err := db.Open(ctx, r.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		id := fmt.Sprintf("claimer-%d", i)
		r.exec(t, `INSERT INTO transcode_nodes (id, roles, store, slots, locked, output_version) VALUES ($1, 'worker', '', 1, false, $2)`, id, playback.OutputVersion)
		all = append(all, &Worker{ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Log: r.log})
	}
	for round := range rounds {
		r.exec(t, "UPDATE transcode_jobs SET state = 'cancelled' WHERE state IN ('queued', 'running')")
		job := r.insertJob(t, r.hash, psid(round), "queued", false)
		r.exec(t, "UPDATE transcode_jobs SET spec = $2 WHERE id = $1", job, mustSpecJSON(t, r))
		var start sync.WaitGroup
		start.Add(1)
		var got atomic.Int64
		winners := make(chan string, workers)
		var done sync.WaitGroup
		for _, w := range all {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				claimed, ok, err := w.claim(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if ok && claimed.ID == job {
					got.Add(1)
					winners <- w.ID
				}
			}()
		}
		start.Done()
		done.Wait()
		close(winners)
		var who []string
		for id := range winners {
			who = append(who, id)
		}
		if got.Load() != 1 {
			t.Fatalf("round %d: %d workers claimed one queued job: %v", round, got.Load(), who)
		}
		var worker string
		if err := r.pool.QueryRow(ctx, "SELECT worker FROM transcode_jobs WHERE id = $1", job).Scan(&worker); err != nil {
			t.Fatal(err)
		}
		if worker != who[0] {
			t.Fatalf("round %d: %s claimed the job and the row says %s's", round, who[0], worker)
		}
	}
}

func mustSpecJSON(t *testing.T, r *rig) []byte {
	t.Helper()
	raw, err := json.Marshal(r.spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Two viewers of one spec ask through two replicas for a segment no live job will
// make, at the same moment: one job is started, and both are given it. A spec has
// one live job.
func TestTwoReplicasAskingForAMissingSegmentAtOnceStartOneJob(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	a, b := r.jobs(t), r.jobs(t)
	const rounds = 200
	for round := range rounds {
		r.exec(t, "TRUNCATE transcode_jobs, transcode_viewers")
		// The job that died: expired a moment ago, as a killed worker's does.
		dead := r.insertJob(t, r.hash, psid(0), "running", false)
		r.exec(t, "UPDATE transcode_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1", dead)
		sessions := [2]string{psid(2 * round), psid(2*round + 1)}
		for _, s := range sessions {
			r.viewerRow(t, s, r.hash, dead, false, "0s")
		}
		var start sync.WaitGroup
		start.Add(1)
		ids := make(chan int64, 2)
		var done sync.WaitGroup
		for i, replica := range []*Jobs{a, b} {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				id, _, err := replica.ensure(ctx, r.spec, r.hash, sessions[i], 5, 5)
				if err != nil {
					t.Error(err)
				}
				ids <- id
			}()
		}
		start.Done()
		done.Wait()
		close(ids)
		first, second := <-ids, <-ids
		if live := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state IN ('queued', 'running') AND (state = 'queued' OR lease_expires_at > now())", r.hash); live != 1 {
			t.Fatalf("round %d: %d live jobs for one spec after two replicas asked at once", round, live)
		}
		if first != second {
			t.Fatalf("round %d: the two viewers were given jobs %d and %d", round, first, second)
		}
	}
}
