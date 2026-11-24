package transcode

import (
	"testing"
	"time"
)

// JS-177 (D-070, docs/transcoding.md section 5, "The rule"): a figure that no clean sample has set or confirmed may still be
// LOWERED beside the worker's own extraction, and a timed rise is not a clean sample. Same estimator and injected clock as
// capacity_learn_test.go, at the 3 s cadence.

// beside is a starved sample with the worker's own extraction in its window.
func beside(at time.Time, jobs ...learnJob) learnSample {
	s := starvedSample(at, jobs...)
	s.Extraction = true
	return s
}

// A lone i5-class worker with a start of 4 learns 2.35 from a clean 4K run; with no job for 5 minutes the timed rise lifts
// the figure to its start, which no job has tested. A 4K job that then runs starved beside a constant extraction must lower
// it again after seven samples, as it lowers a start. Mutant: a rise by time that leaves the figure settled, so that every
// sample beside the extraction is void and the figure stays 4 for as long as the extraction lasts (1200 samples, an hour,
// were tried by hand).
func TestAFigureRaisedByTimeIsLoweredBesideAnExtraction(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	wantNear(t, "the figure learned with no extraction", tl.e.Figure(), 3.2*i5/1.2, 1e-9)
	tl.now = tl.now.Add(5 * time.Minute)
	if !tl.e.Tick(tl.now) {
		t.Fatal("the timed rise was not due after 5 minutes with no job")
	}
	if got := tl.e.Figure(); got != 4 {
		t.Fatalf("the timed rise took the figure to %v, want its start of 4", got)
	}
	if got := tl.e.Source(); got != "learned" {
		t.Errorf("source %q after a timed rise, want learned (the record names only the start's source and learned)", got)
	}
	tl.feedJobs(7, beside, jobAt(3.2, i5))
	wantNear(t, "the figure raised by time, after seven starved samples beside an extraction", tl.e.Figure(), 3.2*i5/1.2, 1e-9)
	if got := tl.e.Source(); got != "learned" {
		t.Errorf("source %q after it, want learned", got)
	}
}

// A figure that three CLEAN samples confirmed after a timed rise is a tested one, and the held rule applies to it again: the
// same starved samples beside an extraction do not lower it. So the fix cannot be a blanket removal of the flag. A job that
// holds 3.74 of a figure of 4 at 1.15x (price 3.9) is the confirming sample.
func TestAFigureRaisedByTimeAndConfirmedByCleanSamplesIsHeldBesideAnExtraction(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	tl.now = tl.now.Add(5 * time.Minute)
	if !tl.e.Tick(tl.now) {
		t.Fatal("the timed rise was not due after 5 minutes with no job")
	}
	tl.feedJobs(3, starvedSample, jobAt(3.9, 1.15)) // Clean: no extraction in the window.
	if got := tl.e.Figure(); got != 4 {
		t.Fatalf("three samples that confirm the rise moved the figure to %v", got)
	}
	tl.feedJobs(60, beside, jobAt(3.2, i5))
	if got := tl.e.Figure(); got != 4 {
		t.Errorf("starved samples beside an extraction lowered a figure that clean samples had confirmed, to %v", got)
	}
}
