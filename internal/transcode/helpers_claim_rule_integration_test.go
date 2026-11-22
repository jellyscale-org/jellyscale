//go:build integration

package transcode

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-168 regression (D-052): a helper is never claimed by a worker running a job
// of its spec that is not a helper (a viewer's from the start, a seek's, a
// failover's), whatever cores the worker has to spare. The one-second fallback is
// only for a worker running only helpers of the spec (a dead helper's replacement).
// A helper is queued only where a worker that may claim it has its cost free.
// A lone worker therefore holds no helper however many cores it has.
//
// The row-level checks put the rows a claim reads and call the claim or the
// insert as the worker's heartbeat does; the whole-system ones run real workers
// (in this process, or as processes for the killed ones) and real ffmpeg.
//
// Mutants each must fail:
//   - the claim's one-second fallback left as it is (NOT EXISTS over any job of the
//     spec, not only a non-helper): TestAHelperIsNeverClaimedByAWorkerRunningAJobOfItsSpecThatIsNotAHelper
//     (every age over a second), the lone-worker and two-worker checks;
//   - the claim refusing a worker that runs any job of the spec, helpers too: the
//     helper-only worker check;
//   - the claim fixed and roomForHelper not (the room counted on the viewers' own
//     worker): the insert checks and the "no helper row in any state" ones;
//   - the rule applied to the spec's workers by spec_hash of the other specs: the
//     other-spec checks.
//
// What shorter timings could hide: none is shortened. The ages are the rows' own
// (created_at), the heartbeat and lease of the in-process workers are the kit's,
// and the whole-system checks watch for the 15 seconds the task names.

// claimAs is a claim by the worker, which has the cores and helpers it says.
func (r *rig) claimAs(t *testing.T, w *Worker) (int64, bool) {
	t.Helper()
	job, ok, err := w.claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return job.ID, ok
}

// startsAt puts a job of the spec at segment n, as a seek's or a failover's is.
func (r *rig) startsAt(t *testing.T, id int64, n int) {
	t.Helper()
	r.exec(t, "UPDATE transcode_jobs SET start_segment = $2, next_segment = $2 WHERE id = $1", id, n)
}

// The claim: whatever the helper's age (under a second, over it, up to the queue
// timeout), the worker's cores (for two, three or many jobs) and what else of the
// spec the worker runs, a worker running a job of the helper's spec that is not a
// helper does not claim it. A control worker with none of the spec claims it, so a
// refusal is the rule's and not an expired row. Today every age over a second is
// claimed.
func TestAHelperIsNeverClaimedByAWorkerRunningAJobOfItsSpecThatIsNotAHelper(t *testing.T) {
	jobs := []struct {
		name  string
		start int
	}{{"a viewer's job from the start", 0}, {"a seek's or a failover's job begun mid-film", 12}}
	for _, job := range jobs {
		for _, age := range []time.Duration{200 * time.Millisecond, 2 * time.Second, 5 * time.Second} {
			for _, jobsOfCores := range []int{2, 3, 8} {
				for _, alsoHelper := range []bool{false, true} {
					if alsoHelper && jobsOfCores == 2 {
						continue // The viewer's job and a helper fill it.
					}
					cores := float64(jobsOfCores) * costFree
					name := fmt.Sprintf("%s, helper %v old, cores for %d jobs, helpers of the spec on the worker too: %v", job.name, age, jobsOfCores, alsoHelper)
					t.Run(name, func(t *testing.T) {
						r := newRig(t, nil)
						r.addNode(t, "a", cores, playback.OutputVersion)
						r.addNode(t, "control", costFree, playback.OutputVersion)
						spec, viewers := r.running(t, "sdr1080", "a")
						r.startsAt(t, viewers, job.start)
						r.watchedBy(t, spec, "device")
						if alsoHelper {
							r.addJob(t, spec, "running", "a", true, 0)
						}
						helper := r.addJob(t, spec, "queued", "", true, age)
						if got, ok := r.claimAs(t, &Worker{Pool: r.pool, ID: "a", Cores: cores}); ok {
							t.Errorf("the worker running the spec's %s claimed job %d", job.name, got)
						}
						if got := r.state(t, helper); got != "queued" {
							t.Errorf("the helper is %s", got)
						}
						if got, ok := r.claimAs(t, &Worker{Pool: r.pool, ID: "control", Cores: costFree}); !ok || got != helper {
							t.Errorf("the control worker with none of the spec claimed %d, %v; want the helper %d", got, ok, helper)
						}
					})
				}
			}
		}
	}
}

// The same for a worker with transcodeHelpers off: its claim of a helper is the
// rule of any worker's, no more and no less.
func TestAWorkerWithHelpersOffFollowsTheSameClaimRule(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", 3*costFree, playback.OutputVersion)
	r.addNode(t, "b", costFree, playback.OutputVersion)
	spec, _ := r.viewedOn(t, "a")
	helper := r.addJob(t, spec, "queued", "", true, 3*time.Second)
	if got, ok := r.claimAs(t, &Worker{Pool: r.pool, ID: "a", Cores: 3 * costFree, NoHelpers: true}); ok {
		t.Errorf("a worker with helpers off, running the viewer's job of the spec, claimed %d", got)
	}
	if got, ok := r.claimAs(t, &Worker{Pool: r.pool, ID: "b", Cores: costFree, NoHelpers: true}); !ok || got != helper {
		t.Errorf("a worker with helpers off and none of the spec claimed %d, %v; want helper %d", got, ok, helper)
	}
}

// The claim that follows at once the worker's own claim of a viewer's queued job
// of the spec: the viewer's job goes first, and the helper stays queued however old.
func TestTheClaimThatFollowsTheClaimOfAViewersJobDoesNotTakeTheSpecsHelper(t *testing.T) {
	for _, age := range []time.Duration{200 * time.Millisecond, 3 * time.Second} {
		t.Run("helper "+age.String()+" old", func(t *testing.T) {
			r := newRig(t, nil)
			r.addNode(t, "a", 3*costFree, playback.OutputVersion)
			spec := r.costSpec(t, "sdr1080")
			r.watchedBy(t, spec, "device")
			viewers := r.addJob(t, spec, "queued", "", false, 3*time.Second)
			helper := r.addJob(t, spec, "queued", "", true, age)
			w := &Worker{Pool: r.pool, ID: "a", Cores: 3 * costFree}
			if got, ok := r.claimAs(t, w); !ok || got != viewers {
				t.Fatalf("claimed %d, %v; want the viewer's job %d first", got, ok, viewers)
			}
			if got, ok := r.claimAs(t, w); ok {
				t.Errorf("the next claim took %d", got)
			}
			if got := r.state(t, helper); got != "queued" {
				t.Errorf("the helper is %s after the worker took the viewer's job of its spec", got)
			}
		})
	}
}

// What JS-168 gave stays: the rule is per spec and binds at the claim. These pass
// today and must go on passing.
func TestTheClaimRuleLeavesWhatJS168GaveEveryWorkerThatRunsNoJobOfTheSpec(t *testing.T) {
	t.Run("a worker with none of the spec claims the helper at once", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree, playback.OutputVersion)
		spec, _ := r.viewedOn(t, "a")
		helper := r.addJob(t, spec, "queued", "", true, 0)
		if got, ok := r.claimBy(t, "b", costFree); !ok || got != helper {
			t.Errorf("claimed %d, %v; want helper %d", got, ok, helper)
		}
	})
	t.Run("a worker running only a viewer's job of another spec claims it at once", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "b", 2*costFree, playback.OutputVersion)
		spec, _ := r.viewedOn(t, "a")
		r.running(t, "sdr1080", "b") // Another spec's.
		helper := r.addJob(t, spec, "queued", "", true, 0)
		if got, ok := r.claimBy(t, "b", 2*costFree); !ok || got != helper {
			t.Errorf("claimed %d, %v; want helper %d", got, ok, helper)
		}
	})
	t.Run("a helper already running is not stopped when its worker claims the viewer's job of its spec", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 3*costFree, playback.OutputVersion)
		spec := r.costSpec(t, "sdr1080")
		r.watchedBy(t, spec, "device")
		running := r.addJob(t, spec, "running", "a", true, 0)
		viewers := r.addJob(t, spec, "queued", "", false, time.Second)
		if got, ok := r.claimBy(t, "a", 3*costFree); !ok || got != viewers {
			t.Fatalf("claimed %d, %v; want the viewer's job %d", got, ok, viewers)
		}
		if got := r.state(t, running); got != "running" {
			t.Errorf("the helper that was running is %s", got)
		}
	})
}

// A worker running only helpers of the spec claims another once it has been queued
// a second and not before, and a worker with none of the spec has it first. As on
// 30af59c; passes today.
func TestAWorkerRunningOnlyHelpersOfTheSpecClaimsAnotherAfterASecond(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", costFree, playback.OutputVersion)
	r.addNode(t, "helpers", 2*costFree, playback.OutputVersion)
	r.addNode(t, "none", costFree, playback.OutputVersion)
	spec, _ := r.viewedOn(t, "a")
	r.addJob(t, spec, "running", "helpers", true, 0)
	young := r.addJob(t, spec, "queued", "", true, 200*time.Millisecond)
	if got, ok := r.claimBy(t, "helpers", 2*costFree); ok {
		t.Errorf("the helper-only worker claimed %d before the helper had waited a second", got)
	}
	if got, ok := r.claimBy(t, "none", costFree); !ok || got != young {
		t.Fatalf("the worker with none of the spec claimed %d, %v; want %d first", got, ok, young)
	}
	old := r.addJob(t, spec, "queued", "", true, 2*time.Second)
	if got, ok := r.claimBy(t, "helpers", 2*costFree); !ok || got != old {
		t.Errorf("the helper-only worker claimed %d, %v after a second; want %d", got, ok, old)
	}
}

// The insert: a helper is queued only when a worker that may claim it has its cost
// free. Today the room on the viewers' own worker counts.
func TestAHelperIsQueuedOnlyWhereAWorkerThatMayClaimItHasItsCostFree(t *testing.T) {
	rows := func(t *testing.T, r *rig, a string, cores float64, spec playback.Spec, id int64) int {
		r.askHelpers(t, &Worker{Pool: r.pool, ID: a, Cores: cores}, spec, id, 5)
		return r.helperRowsAll(t)
	}
	t.Run("the viewers' worker has cores to spare and is the only worker: none, however often it asks", func(t *testing.T) {
		for _, jobsOfCores := range []int{2, 3, 8} {
			r := newRig(t, nil)
			cores := float64(jobsOfCores) * costFree
			r.addNode(t, "a", cores, playback.OutputVersion)
			spec, id := r.viewedOn(t, "a")
			if n := rows(t, r, "a", cores, spec, id); n != 0 {
				t.Errorf("%d helper rows with cores for %d jobs on the viewers' own lone worker", n, jobsOfCores)
			}
		}
	})
	t.Run("the viewers' worker has cores to spare and the other worker is full: none", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 3*costFree, playback.OutputVersion)
		r.addNode(t, "b", costFree, playback.OutputVersion)
		r.running(t, "sdr1080", "b") // Another spec fills b.
		spec, id := r.viewedOn(t, "a")
		if n := rows(t, r, "a", 3*costFree, spec, id); n != 0 {
			t.Errorf("%d helper rows where the only free cores are on the viewers' worker", n)
		}
	})
	t.Run("two viewers far apart, their jobs on two workers that both have cores to spare: none", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 2*costFree, playback.OutputVersion)
		r.addNode(t, "b", 2*costFree, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		far := r.addJob(t, spec, "running", "b", false, 0)
		r.startsAt(t, far, 40)
		if n := rows(t, r, "a", 2*costFree, spec, id); n != 0 {
			t.Errorf("%d helper rows where both workers run a viewer's job of the spec", n)
		}
	})
	t.Run("the viewers' worker's room is not counted: a worker with none of the spec has the cost for one, so one row, not two", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", 3*costFree, playback.OutputVersion)
		r.addNode(t, "c", costFree, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		if n := rows(t, r, "a", 3*costFree, spec, id); n != 1 {
			t.Errorf("%d helper rows, want the one for worker c", n)
		}
	})
	t.Run("a worker running only helpers of the spec with its cost free may claim it: one row", func(t *testing.T) {
		r := newRig(t, nil)
		r.addNode(t, "a", costFree, playback.OutputVersion)
		r.addNode(t, "h", 2*costFree, playback.OutputVersion)
		spec, id := r.viewedOn(t, "a")
		r.addJob(t, spec, "running", "h", true, 0)
		r.exec(t, "UPDATE transcode_jobs SET start_segment = 36, next_segment = 36 WHERE helper AND worker = 'h'")
		if n := rows(t, r, "a", costFree, spec, id); n != 2 {
			t.Errorf("%d helper rows (the running one and one queued for the helper-only worker), want 2", n)
		}
	})
}

// loneWorker runs a slow film for a viewer on one worker (the change says its
// cores), watches 15 seconds, longer than the one-second wait and the queue
// timeout, and judges that no helper row exists in any state and one ffmpeg runs.
func loneWorker(t *testing.T, change func(*Worker), viewers int) {
	t.Helper()
	slow(t)
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) {
		w.ChunkSegments, w.Lead = 4, 60*time.Second
		if change != nil {
			change(w)
		}
	})
	j := r.jobs(t)
	for i := range viewers {
		if _, err := r.fetch(t, j, psid(i), 0); err != nil {
			t.Fatalf("viewer %d's first segment: %v", i, err)
		}
		r.keepWatching(t, j, psid(i))
	}
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		rows := r.rowsOf(t, r.hash)
		if len(rows) != 1 || rows[0].Helper {
			t.Fatalf("a lone worker has %d job rows for %d viewers of one spec, want the one that is not a helper: %+v", len(rows), viewers, rows)
		}
		if runs := r.runs(); len(runs) > 1 {
			t.Fatalf("%d ffmpeg processes on one worker: %+v", len(runs), runs)
		}
	}
}

// With one worker nothing changes, whatever its cores: the rig's default worker
// (cores for exactly two jobs, which is the regression), three, and many. Today
// the default worker's helper is claimed by the viewers' own worker after a second.
func TestALoneWorkerHoldsNoHelperRowAndOneFFmpegWhateverItsCores(t *testing.T) {
	t.Run("the rig's default worker, cores for two jobs", func(t *testing.T) { loneWorker(t, nil, 1) })
	t.Run("cores for three jobs", func(t *testing.T) { loneWorker(t, func(w *Worker) { w.Cores = 3 * costFree }, 1) })
	t.Run("cores for sixteen jobs", func(t *testing.T) { loneWorker(t, func(w *Worker) { w.Cores = 16 * costFree }, 1) })
	t.Run("two viewers of one spec on cores for three jobs have one job row", func(t *testing.T) {
		loneWorker(t, func(w *Worker) { w.Cores = 3 * costFree }, 2)
	})
}

// With transcodeHelpers off a worker queues no helper row, whatever cores the
// workers have free. Passes today.
func TestWorkersWithHelpersOffQueueNoHelperRowWhateverCoresAreFree(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, cores: 4 * costFree, noHelpers: true})
	c.viewerAt(t, psid(0), 0)
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if rows := r.helpersOf(t, r.hash); len(rows) > 0 {
			t.Fatalf("helper rows with helpers off: %+v", rows)
		}
	}
}

// The viewers' worker has cores to spare and the other worker is full (a row of
// another spec holds it): no helper row however often the job asks, one ffmpeg
// across chunk starts, and every segment asked for is served. Today the viewers'
// worker claims its own job's helper after a second.
func TestTheViewersWorkerWithCoresToSpareAndTheOtherFullQueuesNoHelperAndServesEverySegment(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.addNode(t, "full", costFree, playback.OutputVersion)
	r.running(t, "sdr1080", "full")
	r.worker(t, func(w *Worker) { w.ChunkSegments, w.Lead, w.Cores = 4, 60*time.Second, 3*costFree })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if rows := r.rowsOf(t, r.hash); len(rows) != 1 {
			t.Fatalf("%d job rows of the spec, want the viewer's one: %+v", len(rows), rows)
		}
		if runs := r.runs(); len(runs) > 1 {
			t.Fatalf("%d ffmpeg processes: %+v", len(runs), runs)
		}
	}
	// Segments across the chunk starts at 4 and 8 are made by the same ffmpeg.
	for _, n := range []int{1, 4, 5, 8} {
		if body, err := r.fetch(t, j, psid(0), n); err != nil || !isMP4Segment(body) {
			t.Fatalf("segment %d: %d bytes, %v", n, len(body), err)
		}
	}
	if rows := r.rowsOf(t, r.hash); len(rows) != 1 {
		t.Errorf("%d job rows of the spec after the segments, want one: %+v", len(rows), rows)
	}
}

// Two workers with cores for three jobs each and one slow film, the viewers' job on
// one: in each of 10 runs, watched until the other worker runs two helpers at once,
// every helper that runs is on the other worker and the viewers' worker runs exactly
// one job of the spec throughout.
func TestTheViewersWorkerRunsOnlyItsOwnJobWhileTheOtherRunsTwoHelpers(t *testing.T) {
	slow(t)
	for round := range 10 {
		t.Run("run "+strconv.Itoa(round+1), func(t *testing.T) {
			r := newRig(t, nil)
			c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second, cores: 3 * costFree})
			c.viewerAt(t, psid(0), 0)
			var violation string
			judge := func() (twoOnTheOther bool) {
				rows := r.rowsOf(t, r.hash)
				main, ok := r.mainJob(t, r.hash)
				if !ok {
					return false
				}
				others := 0
				for _, h := range rows {
					if h.State != "running" || h.ID == main.ID {
						continue
					}
					if h.Worker == main.Worker && violation == "" {
						violation = fmt.Sprintf("the viewers' worker %s runs job %d of the spec beside its own: %+v", main.Worker, h.ID, rows)
					}
					if h.Worker != main.Worker && h.Helper {
						others++
					}
				}
				return others >= 2
			}
			waitFor(t, "the other worker to run two helpers at once", 90*time.Second, func() bool { return judge() || violation != "" })
			for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
				judge()
			}
			if violation != "" {
				t.Error(violation)
			}
		})
	}
}

// killedHelper plays the slow film to its end on three or two worker processes
// with cores for two jobs each, kills the worker of a running helper that is not
// the viewers' worker, and returns the rows, the dead helper's row and the
// viewers' job's.
func (r *rig) killedHelper(t *testing.T, n int) (rows []madeBy, victim, main madeBy, set *helperFleet, ended <-chan error) {
	t.Helper()
	// With three workers each has cores for three jobs, so the third worker can take
	// the replacement while it runs a helper or two of its own; with two, cores for two.
	cores := "1.6"
	if n == 3 {
		cores = "2.6"
	}
	set = r.workers(t, n, 2, 6, r.ownStore(t), "JELLYSCALE_TEST_CORES="+cores)
	j := r.jobs(t)
	ended = r.playToTheEnd(t, j, psid(0))
	var onViewers string
	waitFor(t, "a helper to be encoding a chunk a whole chunk past the viewers' job, on a worker other than the viewers', while the third has a job's cores free", 90*time.Second, func() bool {
		rows := r.rows(t)
		running := map[string]int{}
		for _, row := range rows {
			if row.State == "running" {
				running[row.Worker]++
			}
		}
		for _, row := range rows {
			if !row.Helper && row.State == "running" {
				main = row
			}
		}
		for _, row := range rows {
			if row.Helper && row.Worker == main.Worker && onViewers == "" && main.Worker != "" {
				onViewers = fmt.Sprintf("helper %d ran on the viewers' worker: %+v", row.ID, rows)
			}
			if row.Helper && row.State == "running" && row.Next > row.Start && main.Worker != "" && row.Worker != main.Worker && row.Start >= main.Next+6 {
				// Forced, not waited for: the viewers' job cannot reach the dead chunk
				// before the replacement is queued, and the third worker (when there is
				// one) has a job's cores free, so under the claim rule the replacement
				// is a helper row on the third worker, whatever the interleaving.
				third := n == 2
				for _, id := range set.ids {
					if id != main.Worker && id != row.Worker && running[id] <= 1 {
						third = true
					}
				}
				if third {
					victim = row
					return true
				}
			}
		}
		return false
	})
	if onViewers != "" {
		t.Error(onViewers)
	}
	set.kill(victim.Worker)
	return r.rows(t), victim, main, set, ended
}

// A dead helper's replacement, three workers with cores for two jobs each: it
// begins at the dead helper's first missing segment on the third worker, never on
// the viewers' worker, no segment is made twice, and the viewer gets every segment.
func TestADeadHelpersReplacementIsOnTheThirdWorkerNeverTheViewersAndMakesNoSegmentTwice(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	_, victim, main, _, ended := r.killedHelper(t, 3)
	theViewerPlaysOn(t, ended, func() []madeBy { return r.rows(t) })
	rows := r.rows(t)
	var dead madeBy
	for _, row := range rows {
		if row.ID == victim.ID {
			dead = row
		}
	}
	chunkEnd := (victim.Start/6 + 1) * 6
	var replacement *madeBy
	for _, row := range rows {
		if row.ID > victim.ID && row.Helper && row.Start >= victim.Start && row.Start < chunkEnd && row.Next > row.Start {
			replacement = &row
			break
		}
	}
	if replacement == nil {
		t.Fatalf("no replacement made segments of the dead helper's chunk %d to %d: %+v", victim.Start, chunkEnd-1, rows)
	}
	if replacement.Worker == victim.Worker || replacement.Worker == main.Worker {
		t.Errorf("the replacement is on worker %s; want the third (not the dead %s, not the viewers' %s): %+v", replacement.Worker, victim.Worker, main.Worker, rows)
	}
	if replacement.Start < dead.Next {
		t.Errorf("the replacement began at %d, but the dead helper had made up to %d", replacement.Start, dead.Next-1)
	}
	for n := victim.Start; n < replacement.Start; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d, before the replacement's start %d, was never made: it did not begin at the first missing segment", n, replacement.Start)
		}
	}
	for _, row := range rows {
		if row.Helper && row.Worker == main.Worker {
			t.Errorf("helper %d ran on the viewers' worker: %+v", row.ID, rows)
		}
	}
	if twice := madeTwice(rows); len(twice) > 0 {
		t.Errorf("segments made twice: %v: %+v", twice, rows)
	}
	for n := 0; n < filmSegments; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d was never made", n)
		}
	}
}

// Only the viewers' worker is left with cores (two workers with cores for two jobs
// each, the helper's worker killed): once the dead worker no longer counts, no
// helper of the spec is queued, none is ever claimed by the viewers' worker, and
// the viewer is served every segment, none refused or failed. Today the viewers'
// worker takes the replacement.
func TestWithOnlyTheViewersWorkerLeftNoHelperIsQueuedOrClaimedAndTheViewerIsServed(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	_, victim, main, _, ended := r.killedHelper(t, 2)
	waitFor(t, "the dead worker to stop counting", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1 AND seen_at > now() - interval '15 seconds'", victim.Worker) == 0
	})
	newest := r.count(t, "SELECT COALESCE(max(id), 0) FROM transcode_jobs WHERE helper")
	theViewerPlaysOn(t, ended, func() []madeBy { return r.rows(t) })
	rows := r.rows(t)
	for _, row := range rows {
		if row.Helper && row.Worker == main.Worker {
			t.Errorf("helper %d ran on the viewers' worker: %+v", row.ID, rows)
		}
		if row.Helper && int(row.ID) > newest {
			t.Errorf("helper %d was queued after the dead worker stopped counting, with only the viewers' worker left: %+v", row.ID, rows)
		}
		if row.State == "failed" {
			t.Errorf("job %d failed: %+v", row.ID, rows)
		}
	}
	for n := 0; n < filmSegments; n++ {
		if !r.stored(n) {
			t.Errorf("segment %d was never made", n)
		}
	}
}
