//go:build integration

package transcode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// JS-177 (D-064, D-066, D-070): a lowered figure rises from evidence and by time, its backoff and its failures are said, a
// worker's own extraction moves nothing it has learned, and a move never restores a row the reaper deleted. The kit and the
// names the build adds are in capacity_learn_integration_test.go.

// converge plays the 4K HDR spec as viewer n until the worker's figure has fallen, and returns the moment it was seen to.
func (r *rig) converge(t *testing.T, w *Worker, n int) (stop func(), at time.Time) {
	t.Helper()
	stop = r.playAs(t, r.costSpec(t, "hdr4k"), n)
	waitFor(t, "the worker's figure to fall", learnHang, func() bool { return r.rowCores(t, w.ID) < 3.6 })
	return stop, time.Now()
}

// Criterion 11: a lowered figure rises from evidence, with no wait. The lease is 30 s, so the timed rise's first gap is 15
// minutes, which no wait here reaches: only a job that shows the worker holds more can raise it. The worker is lowered by a
// 4K HDR job at 0.88x; a 2.0 core job (a 1080p HDR re-encode, which it holds) then runs at 3x, with the CPU starved: E is
// 5, above 1.1 of 2.3, and three such samples raise the figure to its start. A build that rises only by time never does. The
// time the rise takes after a load drops (16 s, 30 s) is the unit test's, at the production cadence with explicit times; the
// worker's cadence is TestTheSamplerTakesOneSampleEveryHeartbeat's.
func TestALoweredFigureRisesFromAJobThatShowsMoreWithNoTimedWait(t *testing.T) {
	set, _ := paced(t, i5Speed)
	r := newRig(t, nil)
	w, _ := r.learner(t, starvedKernel(), func(worker *Worker) { worker.Lease = 30 * time.Second })
	stop, _ := r.converge(t, w, 0)
	lowered := r.rowCores(t, w.ID)
	stop()
	set(3)
	defer r.playAs(t, r.costSpec(t, "hdr1080"), 1)()
	waitFor(t, "the figure to rise on the evidence of a job that holds more", learnHang, func() bool { return r.rowCores(t, w.ID) > lowered*1.1 })
	if got := r.rowCores(t, w.ID); got > 4 {
		t.Errorf("the figure rose to %v, above its start of 4", got)
	}
}

// What the sampler's cadence is, by mechanism (criteria 6, 10, 11, 12): the worker asks the kernel for its processes' demand
// once for each heartbeat, at the production heartbeat of 3 s and lease of 10 s, with a re-encode running. The fifth ask comes
// four heartbeats after the first: at least 4 x 3 s x 0.7 and at most 4 x 3 s x 2. A sampler on a 30 s ticker never reaches a
// fifth ask inside the wait, and one on a 10 s ticker takes 40 s for four intervals; a sampler on a 1 s ticker takes 4 s.
// Nothing signals ffmpeg and nothing depends on its version. The wait bounds a hang (60 s); the interval is the only bound
// and is a multiple of the heartbeat either way, so a loaded runner's late heartbeat does not break it: 2 times is 12 s of lateness.
func TestTheSamplerTakesOneSampleEveryHeartbeat(t *testing.T) {
	paced(t, 0.5)
	r := newRig(t, nil)
	kernel := idleKernel() // Not CPU-bound: the samples are void and the figure stays; only the asking is read.
	r.learner(t, kernel, func(worker *Worker) { worker.Heartbeat, worker.Lease = 3*time.Second, 10*time.Second })
	defer r.playAs(t, r.costSpec(t, "sdr1080"), 0)()
	waitFor(t, "five asks of the kernel", time.Minute, func() bool { return len(kernel.demandTimes()) >= 5 })
	asked := kernel.demandTimes()
	span := asked[4].Sub(asked[0])
	if span < time.Duration(4*3*0.7*float64(time.Second)) || span > 4*3*2*time.Second {
		t.Errorf("the fifth ask came %v after the first, want within 8.4 s and 24 s: four heartbeats of 3 s", span)
	}
	t.Logf("five asks of the kernel in %v", span)
}

// Criteria 12 and 15, and the part of 3 that says nothing starts an ffmpeg to raise or lower the figure: the WIRING of the
// timed rise and its failures, with lower bounds. The schedule itself (5 minutes, doubling, no cap under a day, the reset)
// is the unit test's, with explicit times. A lone worker throttled to the i5's speed, with a lease of 2 s so that the first gap
// is 30 leases, 60 s, and the next 120 s, is asked for the 4K HDR job over and over: the viewer's leaving lets the figure
// rise by time to its start, not before the first gap; the play admitted above it lowers it again, and that failed rise is
// logged with the figure before and after; the next rise comes after at least twice the gap; the second failed rise also
// names transcodeCores as the ceiling that ends the retries. No ffmpeg ran but the jobs' own. Mutants: a rise with no backoff
// (the second gap is 60 s); a failed rise that is not logged, or that names transcodeCores from the first.
//
// What the shorter lease could hide: a lease of 2 s with a heartbeat of 200 ms leaves a cut margin of 100 ms (lease/20), so a
// renewal slower than 1.9 s cuts the job, which a runner whose Postgres stalls for 4 s (docs/testing.md) meets and which would
// fail this test and not the build (a lease of 1 s, with 50 ms, was tried and given up for that); and a gap tied to a
// constant 5 minutes and not to the lease would make the test wait out its bound and fail, which is the point. The sampler's cadence at the production heartbeat is the burst check's and the real starved worker's, not this one.
func TestATimedRiseBacksOffAndEachFailedRiseIsLoggedNamingTheCeilingFromTheSecond(t *testing.T) {
	_, runs := paced(t, i5Speed)
	r := newRig(t, nil)
	w, logs := r.learner(t, starvedKernel(), func(worker *Worker) { worker.Lease = 2 * time.Second })
	const gap = 60 * time.Second // 30 leases of 2 s.
	stop, lowered := r.converge(t, w, 0)
	stop()
	waitFor(t, "the first timed rise", 2*gap, func() bool { return r.rowCores(t, w.ID) == 4 })
	if since := time.Since(lowered); since < gap-time.Second {
		t.Errorf("the figure rose %v after it was lowered, before the first gap of %v", since, gap)
	}
	if got := logLinesWith(logs.String(), "timed rise failed"); len(got) != 0 {
		t.Fatalf("a rise that no job has tested is logged as failed: %v", got)
	}
	// The play admitted above the figure shows it wrong: the figure is lowered again, and the failed rise is logged.
	stop, lowered = r.converge(t, w, 1)
	first := waitLogLines(t, logs, "timed rise failed", 1)
	if len(first) != 1 || !strings.Contains(first[0], "before=") || !strings.Contains(first[0], "after=") {
		t.Fatalf("the first failed rise: %d lines, want one with the figure before and after: %v", len(first), first)
	}
	if strings.Contains(first[0], "transcodeCores") {
		t.Errorf("the first failed rise names the ceiling: %s", first[0])
	}
	stop()
	waitFor(t, "the second timed rise", 3*gap, func() bool { return r.rowCores(t, w.ID) == 4 })
	if since := time.Since(lowered); since < 2*gap-time.Second {
		t.Errorf("the second rise came %v after the figure was lowered again, before twice the first gap, %v", since, 2*gap)
	}
	stop, _ = r.converge(t, w, 2)
	defer stop()
	second := waitLogLines(t, logs, "timed rise failed", 2)
	if len(second) != 2 || !strings.Contains(second[1], "transcodeCores") || !strings.Contains(second[1], "before=") || !strings.Contains(second[1], "after=") {
		t.Fatalf("the second failed rise: %d lines; want two, the second naming transcodeCores with the figure before and after: %v", len(second), second)
	}
	for _, run := range logLinesWith(runs(), "") {
		if strings.Contains(run, "lavfi") || strings.Contains(run, "testsrc") || strings.Contains(run, "nullsrc") {
			t.Errorf("an ffmpeg ran to raise or lower the figure: %s", run)
		}
	}
}

// Criterion 10 (D-070), a learned figure: a sample taken while the worker's own extraction runs neither lowers nor raises
// it. The worker learns 2.3 from a 4K HDR job beside nothing; then an extraction runs for the rest of the test and a 0.8 core
// job at 0.3x (which would lower the figure to its floor) runs; the row stays exactly where it was. The raising case is the
// unit test's (the same branch of the same estimator; a second 25 s here would add nothing the wiring has not shown).
// Mutant: an extraction that lowers a learned figure.
func TestAnExtractionBesideTheJobsNeverMovesAFigureTheWorkerLearned(t *testing.T) {
	set, _ := paced(t, i5Speed)
	r := newRig(t, nil)
	w, _ := r.learner(t, starvedKernel(), nil)
	stop, _ := r.converge(t, w, 0)
	stop()
	learned := r.rowCores(t, w.ID)
	startExtraction(t, 2*time.Minute)
	set(0.3)
	defer r.playAs(t, r.costSpec(t, "sdr1080"), 1)()
	watchFigure(t, r, w, learned, 15*time.Second)
}

// Criteria 1 and 10 (D-070), a figure not yet learned: beside the worker's own extraction a CPU-bound sample still lowers
// it, which a worker that runs extractions for hours (a chapter backlog, a whole-film read) needs to be told it overstates
// itself. The extraction runs first and for the whole of the test, and slows the job to 0.7x (E 1.87 against 2.3 alone). That
// nothing beside an extraction raises it, and that clean samples replace it, are the unit tests' (the same estimator).
func TestAFigureNotYetLearnedIsLoweredBesideAnExtraction(t *testing.T) {
	paced(t, 0.7)
	r := newRig(t, nil)
	w, _ := r.learner(t, starvedKernel(), nil)
	startExtraction(t, 90*time.Second)
	defer r.playAs(t, r.costSpec(t, "hdr4k"), 0)()
	waitFor(t, "the figure to fall beside the extraction", learnHang, func() bool { return r.rowCores(t, w.ID) < 3.6 })
}

// Criterion 16's sentence on the reason, which D-070 asks for: a re-encode under real time while its worker's own extraction
// runs is counted under the slow-job reasons with a reason of its own, once for the job, and logged once, whether or not a
// helper was asked for; and it still asks for a helper as any slow job does (invariant 58). A second worker has the room
// the helper needs. The extraction is this process's, so both workers see it, and the helper's own counts are not asserted:
// the worker that runs the viewer's job is the one read.
func TestAReencodeUnderRealTimeBesideItsWorkersExtractionIsCountedWithItsOwnReasonAndStillAsksForAHelper(t *testing.T) {
	paced(t, 0.5)
	r := newRig(t, nil)
	one, oneLogs := r.learner(t, idleKernel(), nil)
	two, twoLogs := r.learner(t, idleKernel(), nil)
	logsOf := map[string]*logBuffer{one.ID: oneLogs, two.ID: twoLogs}
	startExtraction(t, 3*time.Minute)
	stop := r.playAs(t, r.costSpec(t, "sdr1080"), 0)
	defer stop()
	var host *Worker
	waitFor(t, "the viewer's job to run", learnHang, func() bool {
		for _, w := range []*Worker{one, two} {
			if r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND NOT helper AND worker = $1", w.ID) == 1 {
				host = w
			}
		}
		return host != nil
	})
	counter := `jellyscale_transcode_unhelped_jobs_total{capacity="assumed",reason="own_extraction"}`
	waitFor(t, "the slow job to be counted beside the extraction", learnHang, func() bool { return value(scrape(t, host), counter) >= 1 })
	time.Sleep(3 * time.Second) // Dozens of beats: once for the job, not once for each.
	if got := value(scrape(t, host), counter); got != 1 {
		t.Errorf("the worker counts %v jobs beside its own extraction, want 1", got)
	}
	if got := waitLogLines(t, logsOf[host.ID], "own_extraction", 1); len(got) != 1 {
		t.Errorf("%d log lines for the job beside its extraction, want one: %v", len(got), got)
	}
	waitFor(t, "a helper to be asked for, as for any slow job", learnHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper") > 0
	})
}

// Criterion 8 and invariant 25: a move writes the worker's own row and never restores one the reaper deleted. The move is
// held where a test can make it lose the race: the test locks the worker's row while its job runs with the CPU not wanted
// (so no sample counts), then makes the CPU wanted, and waits until the worker's statement that writes the new cores is
// itself waiting for the row (asked of the worker's own sessions, by application name); it then deletes the row, as a reaper
// that forgot the worker does, and lets the statement go. The row that is there afterwards, if any, is not the old session's:
// a move that inserts or upserts writes the old session back, and the worker is forgotten no more. Mutant: the move as an
// INSERT ... ON CONFLICT DO UPDATE.
func TestAMoveNeverRestoresARowTheReaperDeleted(t *testing.T) {
	paced(t, i5Speed)
	r := newRig(t, nil)
	kernel := idleKernel()
	w, _ := r.learner(t, kernel, nil)
	stop := r.playAs(t, r.costSpec(t, "hdr4k"), 0)
	defer stop()
	_, _, old, found := r.nodeRow(t, w.ID)
	if !found || old == "" {
		t.Fatalf("the worker's row has no session: found %v, %q", found, old)
	}
	ctx := context.Background()
	holder, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck
	if _, err := holder.Exec(ctx, "SELECT 1 FROM transcode_nodes WHERE id = $1 FOR UPDATE", w.ID); err != nil {
		t.Fatal(err)
	}
	kernel.wants(6) // The CPU is wanted from here: the samples count, and the move is on its way.
	app := "rig-worker-" + w.ID
	waitFor(t, "the worker's statement that writes its new cores to wait for the row", learnHang, func() bool {
		return r.count(t, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'
			AND query ~* '^\s*(update|insert into) transcode_nodes' AND query ILIKE '%cores%'`, app) > 0
	})
	if _, err := holder.Exec(ctx, "DELETE FROM transcode_nodes WHERE id = $1", w.ID); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker's waiting statement to end", learnHang, func() bool {
		return r.count(t, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'
			AND query ~* '^\s*(update|insert into) transcode_nodes' AND query ILIKE '%cores%'`, app) == 0
	})
	if _, _, session, found := r.nodeRow(t, w.ID); found && session == old {
		t.Errorf("the move wrote the row of the session the reaper forgot back into the table")
	}
}

// D-066 limit 4: a worker that cannot read the kernel's CPU figures says so once at start, in the log and on /metrics, and
// learns nothing: its figure stays its start, its source stays assumed, whatever its jobs do.
func TestAWorkerThatCannotReadTheKernelSaysSoOnceAtStartAndLearnsNothing(t *testing.T) {
	paced(t, 0.5)
	r := newRig(t, nil)
	kernel := starvedKernel()
	kernel.err = errors.New("no scheduler accounting here")
	w, logs := r.learner(t, kernel, nil)
	if got := value(scrape(t, w), `jellyscale_transcode_capacity_unreadable`); got != 1 {
		t.Errorf("/metrics says unreadable %v at start, want 1", got)
	}
	stop := r.playAs(t, r.costSpec(t, "sdr1080"), 0)
	defer stop()
	watchFigure(t, r, w, 4, 30*time.Second)
	if got := logLinesWith(logs.String(), "transcode capacity cannot be learned"); len(got) != 1 {
		t.Errorf("%d lines say the worker cannot read the kernel's figures, want one for the start: %v", len(got), got)
	}
	if got := value(scrape(t, w), `jellyscale_transcode_capacity_source{source="assumed"}`); got != 1 {
		t.Errorf("the source is not assumed (%v)", got)
	}
}

// Criterion 14: a smaller move than a fifth is not logged. A move is at least the 10% dead band, so the smaller one is a
// lowering of 16.7%, from a start of 3.2 to 2.67 (a 3.2 core job at 1.0x): in the row and not in the log. A 5% move cannot be
// made, the band holds it. That one beyond a fifth is logged, once, with the figure after, is the first check's (4 to 2.3, one
// line). Mutants: every move logged; one line for each sample that counts.
func TestAMoveWithinAFifthOfTheFigureLastLoggedIsNotLogged(t *testing.T) {
	paced(t, 1.0)
	r := newRig(t, nil)
	kernel := &fakeKernel{cpus: 3.2, demand: 6}
	w, logs := r.learner(t, kernel, nil)
	defer r.playAs(t, r.costSpec(t, "hdr4k"), 0)()
	waitFor(t, "the figure to fall by a sixth", learnHang, func() bool { return r.rowCores(t, w.ID) < 3.0 })
	if first := r.rowCores(t, w.ID); first < 0.8*3.2 {
		t.Fatalf("the first move took the figure to %v, more than a fifth under 3.2: the check wants a smaller one", first)
	}
	time.Sleep(2 * time.Second) // The line, if any, is written with the move.
	if got := logLinesWith(logs.String(), "transcode capacity moved"); len(got) != 0 {
		t.Fatalf("a move of 17%% is logged: %v", got)
	}
}
