//go:build integration

package transcode

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-168 (D-046): a film one worker cannot keep at real time is shared with
// every worker that has room, and the server holds no room back for a viewer who
// has not asked. A helper is queued, and claimed, wherever a worker has exactly
// its cost free after the running jobs and the jobs queued and not yet claimed
// (invariants 58 and 59); a new viewer who finds the cluster full stops a helper
// (preemptHelpers, unchanged).
//
// Today roomForHelper and claim both refuse a helper unless a job of its cost
// still fits somewhere after it, so these checks fail on zero helper rows.
//
// Mutants each must fail: the rule lifted at the insert only (a row that stays
// queued: TestAQueuedHelperIsClaimedByAWorkerWithExactlyItsCostFree); at the claim
// only (no row: TestAHelperIsQueuedWhereExactlyItsCostIsFree); the insert without
// the helpers lock, or reading the ledger before taking it (the contended insert);
// the claim's UPDATE without `state = 'queued'` (the contended claim); a claim that
// takes the oldest queued job whatever it is (the viewer-first checks).

// costFree is the cost of every job these checks run, 0.8 cores: the rig's film at
// 720 lines, and costSpec's "sdr1080".
const costFree = 0.8

// viewedOn is a viewer's running job of the cost on the worker, with a viewer of
// its spec, so that a helper may be asked for it.
func (r *rig) viewedOn(t *testing.T, worker string) (playback.Spec, int64) {
	t.Helper()
	spec, id := r.running(t, "sdr1080", worker)
	r.watchedBy(t, spec, "device")
	return spec, id
}

// askHelpers is what the viewer's job's worker does on a heartbeat, times over,
// for the chunk at segment 12: the helpers it queues, in all.
func (r *rig) askHelpers(t *testing.T, w *Worker, spec playback.Spec, id int64, times int) {
	t.Helper()
	for i := range times {
		// A different chunk each time so that a refusal is the room's and not "the chunk is taken".
		if _, err := w.insertHelper(context.Background(), claimed{ID: id, Hash: spec.Hash(), Spec: spec}, 3+i, 12+4*i); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *rig) helperRowsAll(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper AND state IN ('queued', 'running')")
}

// claimBy is a claim by the worker, which has the cores it says.
func (r *rig) claimBy(t *testing.T, id string, cores float64) (int64, bool) {
	t.Helper()
	job, ok, err := (&Worker{Pool: r.pool, ID: id, Cores: cores}).claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return job.ID, ok
}

// The insert: a watched job judged slow, with exactly a helper's cost free on one
// other worker and nothing free anywhere else, gets a helper row. The room for a
// viewer who has not asked is not counted. Today none, however often it asks.
func TestAHelperIsQueuedWhereExactlyItsCostIsFree(t *testing.T) {
	t.Run("one other worker has exactly its cost free", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 3)
		if n := r.helperRowsAll(t); n != 1 {
			t.Errorf("%d helper rows with exactly one helper's cores free on one other worker, want 1", n)
		}
	})
	t.Run("two other workers have it each: two helpers for one viewer, and no third", func(t *testing.T) {
		r := newRig(t, nil)
		for _, id := range []string{"a", "b", "c"} {
			r.addNode(t, id, costFree, playback.OutputVersion)
		}
		spec, id := r.viewedOn(t, "a")
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 4)
		if n := r.helperRowsAll(t); n != 2 {
			t.Errorf("%d helper rows with the cores of two other workers free, want 2", n)
		}
	})
}

// The claim: a queued helper is claimed by a worker that has exactly its cost
// free. Today never.
func TestAQueuedHelperIsClaimedByAWorkerWithExactlyItsCostFree(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", costFree, playback.OutputVersion)
	r.addNode(t, "b", costFree, playback.OutputVersion)
	r.viewedOn(t, "a")
	helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
	if id, ok := r.claimBy(t, "b", costFree); !ok || id != helper {
		t.Fatalf("claimed %d, %v; want helper %d, which fits the worker exactly", id, ok, helper)
	}
	if got := r.state(t, helper); got != "running" {
		t.Errorf("the helper is %s", got)
	}
}

// A helper still never takes a worker over its cores, counting running jobs and
// jobs queued and not yet claimed, and the cores of another output version count
// for nothing (invariants 58, 59). These pass today and must go on passing once the
// room rule is lifted: the mutant is a ledger that forgets the queued jobs, sums
// two workers' cores, or reads another version's nodes.
func TestAHelperNeverTakesAWorkerOverItsCoresWhateverTheRoomRule(t *testing.T) {
	t.Run("a little less than its cost free on every worker: none queued over several heartbeats, none claimed", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", 0.79, playback.OutputVersion)
		r.addNode(t, "c", 0.75, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 5)
		if n := r.helperRowsAll(t); n != 0 {
			t.Errorf("%d helper rows where no worker has the cost free", n)
		}
		helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		for _, w := range []struct {
			id    string
			cores float64
		}{{"b", 0.79}, {"c", 0.75}} {
			if got, ok := r.claimBy(t, w.id, w.cores); ok {
				t.Errorf("worker %s with %v cores claimed %d", w.id, w.cores, got)
			}
		}
		if got := r.state(t, helper); got != "queued" {
			t.Errorf("the helper is %s", got)
		}
	})
	t.Run("its cost free only as the sum of two workers' cores: none queued, none claimed", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree/2, playback.OutputVersion)
		r.addNode(t, "c", costFree/2, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 5)
		if n := r.helperRowsAll(t); n != 0 {
			t.Errorf("%d helper rows where the cost is free only across two workers", n)
		}
		r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		for _, w := range []string{"b", "c"} {
			if got, ok := r.claimBy(t, w, costFree/2); ok {
				t.Errorf("worker %s claimed %d with half the cost free", w, got)
			}
		}
	})
	t.Run("a helper queued and not yet claimed counts: no second one for the same cores", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 4)
		if n := r.helperRowsAll(t); n != 1 {
			t.Errorf("%d helper rows, the queued one and what the insert added, with cores for one", n)
		}
	})
	t.Run("a queued helper whose cores have since been taken stays unclaimed", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree, playback.OutputVersion)
		r.viewedOn(t, "a")
		helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		r.running(t, "sdr1080", "b") // Another viewer's job took b's cores after the helper was queued.
		if got, ok := r.claimBy(t, "b", costFree); ok {
			t.Errorf("claimed %d over a worker's cores", got)
		}
		if got := r.state(t, helper); got != "queued" {
			t.Errorf("the helper is %s", got)
		}
	})
	t.Run("cores of another output version count for nothing, at the insert and at the claim", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "other", 4, anotherVersion)
		spec, id := r.viewedOn(t, "a")
		r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: costFree}, spec, id, 3)
		if n := r.helperRowsAll(t); n != 0 {
			t.Errorf("%d helper rows on the strength of another version's cores", n)
		}
		r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		if got, ok := r.claimBy(t, "a", costFree); ok {
			t.Errorf("claimed %d with the cores of this version held", got)
		}
	})
}

// A viewer who has asked goes before a helper, whichever was queued first: with
// cores for one job, the viewer's is the one claimed; with cores for both, the
// viewer's is claimed first. Passes today (a helper is refused outright); it is
// what keeps a lifted rule from claiming the oldest row.
func TestAViewersQueuedJobIsClaimedBeforeAHelperWhicheverWasQueuedFirst(t *testing.T) {
	for _, cores := range []float64{costFree, 2 * costFree} {
		for _, helperOlder := range []bool{true, false} {
			name := strconv.FormatFloat(cores, 'f', -1, 64) + " cores, helper older " + strconv.FormatBool(helperOlder)
			t.Run(name, func(t *testing.T) {
				r := newRig(t, nil)
				r.addNode(t, "a", costFree, playback.OutputVersion)
				r.addNode(t, "b", cores, playback.OutputVersion)
				r.viewedOn(t, "a")
				helperAge, viewerAge := 5*time.Second, time.Second
				if !helperOlder {
					helperAge, viewerAge = viewerAge, helperAge
				}
				helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, helperAge)
				viewer := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", false, viewerAge)
				if id, ok := r.claimBy(t, "b", cores); !ok || id != viewer {
					t.Fatalf("claimed %d, %v; want the viewer's job %d", id, ok, viewer)
				}
				if cores > costFree {
					if id, ok := r.claimBy(t, "b", cores); !ok || id != helper {
						t.Errorf("second claim %d, %v; want the helper %d, which fits after the viewer's job", id, ok, helper)
					}
				} else if got := r.state(t, helper); got != "queued" {
					t.Errorf("the helper is %s with no cores for it", got)
				}
			})
		}
	}
}

// Two slow films whose workers decide at the same moment, with cores for exactly
// one helper in the cluster: exactly one helper row, in each of 10 rounds. The
// interleaving is forced: the test holds the helpers lock, so both decisions are
// waiting for it when it lets go, and a build that does not take it (or reads the
// cores before it) does not wait, or queues two. The lock is the one decision per
// cluster that invariant 58 names. Only the two decisions can wait on a lock here,
// no worker runs.
func TestTwoWorkersDecidingAtOnceQueueOneHelperForCoresForOne(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		r.addNode(t, id, costFree, playback.OutputVersion)
	}
	specA, idA := r.viewedOn(t, "a")
	specB, idB := r.viewedOn(t, "b") // c is the one worker with the cores: one helper in all.
	wa, wb := &Worker{Pool: r.pool, ID: "a", Cores: costFree}, &Worker{Pool: r.pool, ID: "b", Cores: costFree}
	for round := range 10 {
		r.exec(t, "DELETE FROM transcode_jobs WHERE helper")
		holder, err := r.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended('helpers', 0))"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, d := range []struct {
			w    *Worker
			spec playback.Spec
			id   int64
		}{{wa, specA, idA}, {wb, specB, idB}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := d.w.insertHelper(ctx, claimed{ID: d.id, Hash: d.spec.Hash(), Spec: d.spec}, 3, 12); err != nil {
					t.Error(err)
				}
			}()
		}
		waitFor(t, "round "+strconv.Itoa(round)+": both decisions to wait for the helpers lock", 30*time.Second, func() bool { return r.lockWaiters(t) >= 2 })
		if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended('helpers', 0))"); err != nil {
			t.Fatal(err)
		}
		holder.Release()
		wg.Wait()
		if n := r.helperRowsAll(t); n != 1 {
			t.Fatalf("round %d: %d helper rows queued or running with the cores of one worker free, want exactly 1", round, n)
		}
	}
}

// lockWaiters counts the sessions that wait for a lock, other than this one.
func (r *rig) lockWaiters(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()")
}

// One queued helper that two workers both have the cores for is run by exactly
// one of them, in each of 10 rounds. Forced: the test holds the row's lock, so
// both claims are waiting (on the row, or on one another) when it lets go.
func TestAQueuedHelperTwoWorkersHaveTheCoresForIsRunByOne(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		r.addNode(t, id, costFree, playback.OutputVersion)
	}
	r.viewedOn(t, "a")
	for round := range 10 {
		r.exec(t, "DELETE FROM transcode_jobs WHERE helper")
		helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT id FROM transcode_jobs WHERE id = $1 FOR UPDATE", helper); err != nil {
			t.Fatal(err)
		}
		var claims sync.WaitGroup
		results := make(chan bool, 2)
		for _, id := range []string{"b", "c"} {
			claims.Add(1)
			go func() {
				defer claims.Done()
				_, ok, err := (&Worker{Pool: r.pool, ID: id, Cores: costFree}).claim(ctx)
				if err != nil {
					t.Error(err)
				}
				results <- ok
			}()
		}
		waitFor(t, "round "+strconv.Itoa(round)+": both claims to wait", 30*time.Second, func() bool { return r.lockWaiters(t) >= 2 })
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		claims.Wait()
		close(results)
		got := 0
		for ok := range results {
			if ok {
				got++
			}
		}
		if got != 1 {
			t.Fatalf("round %d: %d workers claimed the one queued helper, want exactly 1", round, got)
		}
	}
}

// With a worker's claim of a queued helper held at the proxy, a viewer who asks
// is running within the bound, not queued behind that helper, in each of 10 rounds.
// Forced: the claim's UPDATE is held before it reaches Postgres; the viewer's job
// is queued meanwhile (or, in a build that takes the helpers lock for a claim, waits
// for it, which is the state the test waits for), and then the claim goes on. The
// helper of a film nobody has made is 6 segments of a film played at its speed, over
// 20 seconds, so a helper left running holds the worker past the bound.
func TestAViewerWhoAsksAsAQueuedHelperIsBeingClaimedIsRunningNotQueuedBehindIt(t *testing.T) {
	slow(t)
	for round := range 10 {
		ok := t.Run("round "+strconv.Itoa(round), func(t *testing.T) {
			r := newRig(t, nil)
			ctx := context.Background()
			p := r.proxiedWorker(t) // One worker of one core: 0.8 for one job.
			held, release := make(chan struct{}), make(chan struct{})
			p.pool.OnStatement(sentHas(claimStatement), func() { close(held); <-release }, nil)
			helperSpec := r.costSpec(t, "sdr1080")
			r.watchedBy(t, helperSpec, "device")
			r.addJob(t, helperSpec, "queued", "", true, time.Second)
			Notify(ctx, r.pool, jobHint)
			select {
			case <-held:
			case <-time.After(30 * time.Second):
				t.Fatal("the worker never sent its claim of the queued helper, which fits it exactly")
			}
			viewerSpec := r.costSpec(t, "sdr1080")
			j := r.jobs(t)
			asked := make(chan int64, 1)
			go func() {
				v := r.viewer(psid(1))
				if err := j.Watch(ctx, v, viewerSpec.Hash(), 0); err != nil {
					t.Error(err)
				}
				id, _, err := j.ensure(ctx, viewerSpec, viewerSpec.Hash(), psid(1), 0, 0)
				if err != nil {
					t.Error(err)
				}
				asked <- id
			}()
			var viewerJob int64
			waitFor(t, "the viewer's ask to be queued, or to wait for the helpers lock a claim holds", 30*time.Second, func() bool {
				select {
				case viewerJob = <-asked:
					return true
				default:
					return r.count(t, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'") > 0
				}
			})
			close(release)
			if viewerJob == 0 {
				viewerJob = <-asked
			}
			waitFor(t, "the viewer's job to run, not wait behind the helper", 10*time.Second, func() bool { return r.state(t, viewerJob) == "running" })
		})
		if !ok {
			return
		}
	}
}

// Real workers of exactly the cost of one job, throttled to real time (slow), one
// viewer: the checks of the runs that JS-162 measured in jellyfin-web (criteria 3
// and 7). The cluster is the kit's (helpers_left_behind_kit_integration_test.go).
// The bounds are the hang bounds of the kit, not the 10 seconds the criteria give
// (a duration is logged, not judged); what is judged is which worker made what.

// perWorkerPeak is the most running jobs any one worker holds, in a sample.
func (r *rig) perWorkerPeak(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM transcode_jobs WHERE state = 'running' GROUP BY worker) x")
}

// mainJob is the spec's running job that is not a helper, if any.
func (r *rig) mainJob(t *testing.T, hash string) (hrow, bool) {
	t.Helper()
	for _, h := range r.rowsOf(t, hash) {
		if !h.Helper && h.State == "running" {
			return h, true
		}
	}
	return hrow{}, false
}

// Two equal workers, each with the cores of one job: a helper runs on the other
// worker, makes a segment of its chunk before the viewer's own job reaches it, and
// the viewer is served that segment. Today no helper ever starts.
func TestAHelperOnTheOtherEqualWorkerMakesASegmentTheViewerIsServed(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, cores: costFree})
	sc := r.forwardScene(t, c, "running")
	main, ok := r.mainJob(t, sc.s.hash)
	if !ok || main.Worker == sc.helper.Worker {
		t.Fatalf("the helper %+v is not on the other worker than the viewer's job %+v", sc.helper, main)
	}
	if main.Next > sc.helper.Start || !r.storedOn(sc.s.hash, sc.helper.Start) {
		t.Fatalf("segment %d of the helper's chunk is not stored by the helper before the viewer's job (at %d) reaches it", sc.helper.Start, main.Next)
	}
	body, err := r.fetch(t, c.j, psid(0), sc.helper.Start)
	if err != nil || !isMP4Segment(body) {
		t.Fatalf("the viewer was not served the helper's segment %d: %d bytes, %v", sc.helper.Start, len(body), err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if n := r.perWorkerPeak(t); n > 1 {
			t.Fatalf("a worker with the cores of one job runs %d: %+v", n, r.rowsOf(t, sc.s.hash))
		}
	}
}

// Three equal workers, each with the cores of one job: two helpers run at the same
// moment for one viewer, on the two other workers, and no third job. Today one.
func TestThreeEqualWorkersRunTwoHelpersForOneViewerAtTheSameMoment(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second, cores: costFree})
	c.viewerAt(t, psid(0), 0)
	s := r.mainStream()
	defer func() {
		if t.Failed() {
			t.Logf("jobs of the spec: %+v", r.rowsOf(t, s.hash))
		}
	}()
	waitFor(t, "two helpers running, on two workers, beside the viewer's job", 90*time.Second, func() bool {
		workers := map[string]bool{}
		for _, h := range r.helpersOf(t, s.hash) {
			if h.State == "running" {
				workers[h.Worker] = true
			}
		}
		main, ok := r.mainJob(t, s.hash)
		return ok && len(workers) == 2 && !workers[main.Worker]
	})
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if n := len(running(r.jobsOf(t, s.hash))); n > 3 || r.perWorkerPeak(t) > 1 {
			t.Fatalf("%d jobs running on three workers of one job each: %+v", n, r.rowsOf(t, s.hash))
		}
	}
}

// helperAhead is a running helper of the spec for a chunk past the chunk of
// segment n, on a worker other than the one running the job that starts at n.
func (r *rig) helperAhead(t *testing.T, s lbStream, n int) (hrow, bool) {
	t.Helper()
	seekJob := -1
	var onWorker string
	for _, h := range r.rowsOf(t, s.hash) {
		if !h.Helper && h.State == "running" && h.Start == n {
			seekJob, onWorker = int(h.ID), h.Worker
		}
	}
	if seekJob < 0 {
		return hrow{}, false
	}
	for _, h := range r.helpersOf(t, s.hash) {
		if h.State == "running" && h.Start >= (n/4+1)*4 && h.Worker != onWorker {
			return h, true
		}
	}
	return hrow{}, false
}

// Two workers throttled to real time, with the cores of one job each: before a seek
// to the middle a helper runs on the other worker; after it a helper for a chunk
// ahead of the new place runs on the worker the seek's job is not on, and the helper
// left behind is gone (JS-167). Today no helper starts at all.
func TestAfterASeekTheHelperIsForTheNewPlaceOnTheOtherWorker(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, cores: costFree})
	sc := r.forwardScene(t, c, "running")
	sk := r.seek(t, c.j, sc.s, psid(0), 12)
	defer func() {
		if t.Failed() {
			t.Logf("jobs of the spec: %+v", r.rowsOf(t, sc.s.hash))
		}
	}()
	var ahead hrow
	within(t, sk.at.Add(60*time.Second), "a helper for a chunk ahead of segment 12 running on the worker that the seek's job is not on", func() bool {
		var ok bool
		ahead, ok = r.helperAhead(t, sc.s, 12)
		return ok
	})
	t.Logf("a helper for the chunk at %d running %v after the seek's first request", ahead.Start, time.Since(sk.at).Round(100*time.Millisecond))
	if got := r.rowOf(t, sc.helper.ID).State; got == "running" {
		t.Errorf("the helper left behind at %d is still running", sc.helper.Start)
	}
	sk.answered(t)
}

// One worker that holds exactly the viewer's job and one larger (a job and a half's
// cores, so that a helper fits and no room is left after it: JS-162's 2+6 at 3.2 cores a job), the viewer's job on the smaller: a helper runs on the larger worker before
// the seek, and a helper for the new place runs on the other worker after it,
// whichever worker the seek's job lands on.
func TestAHelperRunsOnTheLargerWorkerAndAfterASeekOnWhicheverIsNotTheSeeksJobs(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	var small, large *Worker
	options := func(cores float64, into **Worker) func(*Worker) {
		return func(w *Worker) {
			*into = w
			w.Cores, w.Slots, w.ChunkSegments, w.Lead = cores, 1, 4, 60*time.Second
			w.Heartbeat, w.Lease = 3*time.Second, 10*time.Second
		}
	}
	r.worker(t, options(costFree, &small))
	j := r.jobs(t)
	s := r.mainStream()
	if _, err := r.fetch(t, j, psid(0), 0); err != nil { // Only the small worker exists: the viewer's job is on it.
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	r.worker(t, options(1.5*costFree, &large))
	defer func() {
		if t.Failed() {
			t.Logf("small %s, large %s; jobs of the spec: %+v", small.ID, large.ID, r.rowsOf(t, s.hash))
		}
	}()
	waitFor(t, "a helper running on the larger worker", 90*time.Second, func() bool {
		for _, h := range r.helpersOf(t, s.hash) {
			if h.State == "running" && h.Worker == large.ID && h.Next > h.Start {
				return true
			}
		}
		return false
	})
	sk := r.seek(t, j, s, psid(0), 12)
	within(t, sk.at.Add(60*time.Second), "a helper for a chunk ahead of segment 12 running on the worker the seek's job is not on", func() bool {
		_, ok := r.helperAhead(t, s, 12)
		return ok
	})
	sk.answered(t)
}

// A second viewer arriving while helpers hold every core of every worker (the
// behaviour D-046 leaves as it is until JS-161, recorded and not yet changed): its
// transcode is offered, its job starts by stopping the fewest helpers of one
// worker, two arriving at once both start, and no viewer's own job is stopped.
// Passes today; it is what a lifted room rule, which fills the cluster with
// helpers, must not break.
func TestWithHelpersHoldingEveryCoreASecondViewerStartsByStoppingTheFewestHelpers(t *testing.T) {
	full := func(t *testing.T) (r *rig, viewer int64, helpers []int64) {
		t.Helper()
		r = newRig(t, nil)
		r.addNode(t, "a", 3*costFree, playback.OutputVersion)
		r.addNode(t, "b", 3*costFree, playback.OutputVersion)
		_, viewer = r.viewedOn(t, "a")
		for _, w := range []string{"a", "a", "b", "b", "b"} {
			helpers = append(helpers, r.addJob(t, r.costSpec(t, "sdr1080"), "running", w, true, 0))
		}
		return r, viewer, helpers
	}
	stopped := func(t *testing.T, r *rig, ids []int64) (n int, workers map[string]bool) {
		t.Helper()
		workers = map[string]bool{}
		for _, id := range ids {
			if r.state(t, id) == "cancelled" {
				n++
				workers[mustScalar(t, r, "SELECT worker FROM transcode_jobs WHERE id = "+strconv.FormatInt(id, 10))] = true
			}
		}
		return n, workers
	}
	ask := func(t *testing.T, r *rig, spec playback.Spec, device int) {
		t.Helper()
		r.wantOffered(t, spec)
		if _, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), psid(device), 0, 0); err != nil {
			t.Error(err)
		}
	}
	for name, kind := range map[string]string{"the same cost": "sdr1080", "a cheaper one": "copy"} {
		t.Run(name+": one helper of one worker", func(t *testing.T) {
			r, viewer, helpers := full(t)
			ask(t, r, r.costSpec(t, kind), 1)
			if n, _ := stopped(t, r, helpers); n != 1 {
				t.Errorf("%d helpers were stopped for a job that one frees room for", n)
			}
			if got := r.state(t, viewer); got != "running" {
				t.Errorf("a viewer's job is %s", got)
			}
		})
	}
	t.Run("a dearer job takes the helpers of one worker it needs, not of both", func(t *testing.T) {
		r, _, helpers := full(t)
		ask(t, r, r.costSpec(t, "sdr4k"), 1) // 1.6 cores: two helpers' worth.
		if n, workers := stopped(t, r, helpers); n != 2 || len(workers) != 1 {
			t.Errorf("%d helpers stopped on %d workers for a 1.6 core job, want 2 on one", n, len(workers))
		}
	})
	t.Run("two viewers at once both start, each over a helper, and no viewer's job is stopped", func(t *testing.T) {
		r, viewer, helpers := full(t)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ask(t, r, r.costSpec(t, "sdr1080"), 10+i)
			}()
		}
		close(start)
		wg.Wait()
		if n, _ := stopped(t, r, helpers); n != 2 {
			t.Errorf("%d helpers stopped for two viewers, want 2", n)
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE NOT helper AND state = 'queued'"); n != 2 {
			t.Errorf("%d new viewer jobs queued, want both", n)
		}
		if got := r.state(t, viewer); got != "running" {
			t.Errorf("a viewer's job is %s", got)
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE NOT helper AND state = 'cancelled'"); n != 0 {
			t.Errorf("%d viewer jobs were stopped", n)
		}
	})
}

// With helpers off a worker asks for none, and still runs a helper that a worker
// with them on asked for: the claim of a worker with transcodeHelpers off is the
// same as any other (today refused for the room rule).
func TestAWorkerWithHelpersOffClaimsAHelperAnotherWorkerAskedFor(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", costFree, playback.OutputVersion)
	r.addNode(t, "b", costFree, playback.OutputVersion)
	r.viewedOn(t, "a")
	helper := r.addJob(t, r.costSpec(t, "sdr1080"), "queued", "", true, 2*time.Second)
	got, ok, err := (&Worker{Pool: r.pool, ID: "b", Cores: costFree, NoHelpers: true}).claim(context.Background())
	if err != nil || !ok || got.ID != helper {
		t.Errorf("claimed %d, %v, %v; want helper %d", got.ID, ok, err, helper)
	}
}

// With one worker nothing changes: a lone worker that holds no second job of the
// cost beside the viewer's queues no helper row (JS-162's 2-CPU worker counted at
// 3.2 cores holding a 4K tone mapped job; a 4 core worker holding one), and runs
// one ffmpeg. Passes today.
func TestALoneWorkerWithNoRoomBesideTheViewersJobQueuesNoHelper(t *testing.T) {
	for _, cores := range []float64{3.2, 4} {
		t.Run(strconv.FormatFloat(cores, 'f', -1, 64)+" cores, a 4K tone mapped job", func(t *testing.T) {
			r := newRig(t, nil)
			r.addNode(t, "a", cores, playback.OutputVersion)
			spec := r.costSpec(t, "hdr4k")
			id := r.addJob(t, spec, "running", "a", false, 0)
			r.watchedBy(t, spec, "device")
			r.askHelpers(t, &Worker{Pool: r.pool, ID: "a", Cores: cores}, spec, id, 5)
			if n := r.helperRowsAll(t); n != 0 {
				t.Errorf("%d helper rows on a lone %v core worker holding a 3.2 core job", n, cores)
			}
		})
	}
	t.Run("one ffmpeg for a slow viewer on a worker of one job", func(t *testing.T) {
		slow(t)
		r := newRig(t, nil)
		c := r.cluster(t, clusterOpts{n: 1, lead: 60 * time.Second, cores: costFree})
		c.viewerAt(t, psid(0), 0)
		for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if n := len(r.rowsOf(t, r.hash)); n != 1 {
				t.Fatalf("%d jobs for one slow viewer on one worker: %+v", n, r.rowsOf(t, r.hash))
			}
			if runs := r.runs(); len(runs) > 1 {
				t.Fatalf("%d ffmpeg processes on one worker: %+v", len(runs), runs)
			}
		}
	})
}

// A lone viewer is no slower to start or to seek. With a helper holding the other
// of two workers (heartbeats of 10 s, the lease 33 s), a seek's job is claimed before
// any job's next heartbeat could free a worker and inside the 5 second queue timeout
// (4 seconds): the helper is stopped at once by the hint, not left to end by itself,
// and the segment is served, not refused. How long the segment takes is the film's
// speed (about 6 seconds read at real time), which is logged and not judged. Today no
// helper holds the other worker, so the setup fails.
func TestASeekIntoAClusterAHelperHoldsIsServedBeforeAnyHeartbeat(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, heartbeat: 10 * time.Second, cores: costFree})
	sc := r.forwardScene(t, c, "running")
	sk := r.seek(t, c.j, sc.s, psid(0), 12)
	// The seek's job is claimed, on the worker the helper held or the one the viewer's
	// old job was on, before any heartbeat (10 s) or the queue timeout (5 s) could
	// have moved a cluster of two workers that are both full.
	within(t, sk.at.Add(4*time.Second), "the seek's job to be claimed", func() bool {
		for _, h := range r.rowsOf(t, sc.s.hash) {
			if !h.Helper && h.Start == 12 && h.State != "queued" {
				return true
			}
		}
		return false
	})
	select {
	case err := <-sk.done:
		if err != nil {
			t.Errorf("the seek's segment request failed: %v", err)
		}
		sk.done <- err
		t.Logf("the seek's first segment was served %v after the request", time.Since(sk.at).Round(100*time.Millisecond))
	case <-time.After(60 * time.Second):
		t.Errorf("the seek's segment was not served within 60 seconds: %+v", r.rowsOf(t, sc.s.hash))
	}
	if !r.storedOn(sc.s.hash, 12) {
		t.Error("segment 12 is not stored")
	}
}

// A start on an idle cluster claims the viewer's job first: whenever a helper row
// exists the viewer's job is already running and older than it, so the helper is
// asked for by a job that has begun and never takes the worker the viewer's job
// wants. (A helper is asked for only once the job has measured its speed, which
// the job's own worker decides; the rows cannot say when that was, so what is
// judged is the order.) Passes today.
func TestAStartOnAnIdleClusterClaimsTheViewersJobBeforeAnyHelper(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, cores: costFree})
	s := r.mainStream()
	sk := r.seek(t, c.j, s, psid(0), 0)
	for served := false; !served; {
		rows := r.rowsOf(t, s.hash)
		for _, h := range rows {
			if !h.Helper {
				continue
			}
			if main, ok := r.mainJob(t, s.hash); !ok || main.ID > h.ID {
				t.Fatalf("a helper was queued with the viewer's job not running, or newer: %+v", rows)
			}
		}
		select {
		case err := <-sk.done:
			sk.done <- err
			if err != nil {
				t.Fatalf("the first segment: %v", err)
			}
			served = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, ok := r.mainJob(t, s.hash); !ok {
		t.Errorf("no running job of the viewer's: %+v", r.rowsOf(t, s.hash))
	}
}
