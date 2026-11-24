//go:build integration

package transcode

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-177 (D-064, D-066), criteria 8 and 9: each worker's figure is its own, and the share is 0.65 of the price free. Every
// 3.2 core job here has a small output (cheapOutput): two real 4K tone mapped encodes at once, one for each worker, compete for
// the CPUs of a 2-CPU runner and each made about 0.55x, not the 0.88x the pacing sets, so each worker learned 1.46 and not 2.3
// (the merge queue). With the small output the speed is the pacing's on any host. The kit and the names the build adds are in
// capacity_learn_integration_test.go.

// An unthrottled worker beside a throttled one keeps its start figure, and a move writes only the worker's own row: two
// workers each run one 4K HDR job at 0.88x; the one whose CPU is starved learns 2.3, the one whose CPU is not wanted learns
// nothing and stays at its 4, exactly, for as long as the other's move takes and after. Mutants: one figure for the cluster;
// a move written to every worker's row.
func TestEachWorkersFigureIsItsOwnAndAMoveWritesOnlyItsOwnRow(t *testing.T) {
	paced(t, i5Speed)
	r := newRig(t, nil)
	noHelpers := func(w *Worker) { w.NoHelpers = true } // So that each worker's sample is its own job's alone.
	throttled, _ := r.learner(t, starvedKernel(), noHelpers)
	roomy, _ := r.learner(t, idleKernel(), noHelpers)
	for n := range 2 {
		stop := r.playAs(t, cheapOutput(t, r.costSpec(t, "hdr4k")), n)
		defer stop()
	}
	for _, w := range []*Worker{throttled, roomy} {
		if got := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", w.ID); got != 1 {
			t.Fatalf("worker %s runs %d jobs, want one each of the two 3.2 core jobs", w.ID, got)
		}
	}
	waitFor(t, "the throttled worker's figure to fall", learnHang, func() bool { return r.rowCores(t, throttled.ID) < 3.6 })
	if got := r.rowCores(t, roomy.ID); got != 4 {
		t.Fatalf("the other worker's row has %v cores after the first one's move, want its start of 4", got)
	}
	watchFigure(t, r, roomy, 4, 10*time.Second)
}

// The unit and the share (D-066): two undeclared workers each throttled to the i5's speed learn about 2.3 from a 4K HDR job
// each. Once both have, the 4K HDR job is admitted shared (each of the two has 0.65 of its 3.2 price, 2.08, free), and is
// played: the viewer's job on one worker and a helper on the other, which a literal 0.78 of the price (2.5) refuses and
// which a refusal for want of one worker holding it would not start. Mutants: a share still read at 0.78; a figure
// learned by only one worker.
func TestTwoWorkersThrottledToTheI5AreAdmittedTheFourKPlaySharedOnceConvergedAndPlayItWithAHelper(t *testing.T) {
	paced(t, i5Speed)
	r := newRig(t, nil)
	// Each learns from a job of its own, the two started at once: each worker then has 0.8 free (a share is 2.08), so neither
	// can host the other's helper, and each sample is its job's alone. (Started one after the other, the first job's helper
	// would land on the second worker and take its cores.)
	a, _ := r.learner(t, starvedKernel(), nil)
	b, _ := r.learner(t, starvedKernel(), nil)
	j := r.jobs(t)
	specs := []playback.Spec{cheapOutput(t, r.costSpec(t, "hdr4k")), cheapOutput(t, r.costSpec(t, "hdr4k"))}
	stops, errs := make([]func(), 2), make([]error, 2)
	var started sync.WaitGroup
	for n := range specs {
		started.Add(1)
		go func() {
			defer started.Done()
			stops[n], errs[n] = r.playErr(j, specs[n], n)
		}()
	}
	started.Wait()
	for n, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stops[n])
	}
	for _, w := range []*Worker{a, b} {
		waitFor(t, "worker "+w.ID+" to learn its figure", learnHang, func() bool { return r.rowCores(t, w.ID) < 3.6 })
	}
	figures := []float64{r.rowCores(t, a.ID), r.rowCores(t, b.ID)}
	t.Logf("the two workers learned %.3f and %.3f (a 3.2 core job at 0.88x holds 2.347)", figures[0], figures[1])
	for _, f := range figures {
		if f < 2.08 || f > 2.7 {
			t.Fatalf("figures %v: each worker holds about 2.3 of the 3.2 price at 0.88x, between the share (2.08) and 2.7", figures)
		}
	}
	for _, stop := range stops {
		stop()
	}
	waitFor(t, "both jobs to end", learnHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
	shared := cheapOutput(t, r.costSpec(t, "hdr4k"))
	admission, err := r.jobs(t).Admit(context.Background(), shared, r.user, deviceOf(2))
	if err != nil {
		t.Fatal(err)
	}
	if admission.Reason != "" || admission.How != AdmittedShared || len(admission.Capacities) != 2 {
		t.Fatalf("a 4K HDR play two workers of %v keep together was judged %+v, want admitted shared", figures, admission)
	}
	for _, c := range admission.Capacities {
		if math.Abs(c-figures[0]) > 1e-6 && math.Abs(c-figures[1]) > 1e-6 {
			t.Errorf("the capacities counted, %v, are not the workers' learned %v", admission.Capacities, figures)
		}
	}
	stop := r.playAs(t, shared, 2)
	defer stop()
	var host string
	// Only helper rows that are queued or running count: between the first play's stop and the second's a helper can fit on the
	// other worker at the 0.65 share and leave a cancelled row, which is the converge phase's and not the shared play's.
	waitFor(t, "the viewer's job to run on one worker and its helper on the other", learnHang, func() bool {
		var hosts, helpers int
		for _, w := range []*Worker{a, b} {
			if r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND NOT helper AND worker = $1", w.ID) == 1 {
				hosts, host = hosts+1, w.ID
			}
			if r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND helper AND worker = $1", w.ID) == 1 {
				helpers++
			}
		}
		return hosts == 1 && helpers == 1 && r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper AND state IN ('queued', 'running') AND worker = $1", host) == 0
	})
	// The helper's chunk is the viewer's to read: segments 4 to 7 come, from the other worker's job.
	for n := 4; n < 8; n++ {
		if _, err := bytesOf(j.Segment(context.Background(), shared, shared.Hash(), r.viewerOf(2), n, 0)); err != nil {
			t.Fatalf("segment %d of the shared play: %v", n, err)
		}
	}
}
