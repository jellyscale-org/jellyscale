package transcode

import (
	"testing"
	"time"
)

// JS-177, the fix round: what a reviewer's mutants and scenarios showed the first tests let through. Same estimator, same
// injected clock (capacity_learn_test.go has the helpers).

// A job that runs at 3x holds its price three times over, and a worker whose CPUs are all wanted by it is not slow: only a
// job under 1.2x lowers the figure. Mutant: `lowers` without the `slow &&` guard lowers a figure of 4 to 2.0 on 0.8 core jobs
// at 3x.
func TestAHealthyJobOnAWorkerWhoseCPUIsWantedNeverLowersTheFigure(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(60, starvedSample, jobAt(0.8, 3.0)) // E = 2.0, under 0.9 of 4.
	if got := tl.e.Figure(); got != 4 {
		t.Errorf("a healthy job at 3x lowered the figure to %v", got)
	}
}

// A job at exactly 1.2x is held, not slow. Mutant: `Speed <= admissionBound` lowers a figure of 4 on a 2 core job at 1.2x
// (E = 2.0).
func TestAJobAtExactlyTheAdmissionBoundIsNotSlow(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(60, starvedSample, jobAt(2.0, admissionBound))
	if got := tl.e.Figure(); got != 4 {
		t.Errorf("a job at exactly 1.2x lowered the figure to %v", got)
	}
}

// A ceiling equal to the allotment is not below it, so it holds nothing and the start is assumed: the design's source is
// ceiling only where transcodeCores is lower than the CPUs. Mutant: `ceiling <= allotment`.
func TestACeilingEqualToTheAllotmentIsAssumed(t *testing.T) {
	if got, source := startFigure(4, 4); got != 4 || source != "assumed" {
		t.Errorf("startFigure(4, 4) = %v, %q; want 4, assumed", got, source)
	}
}

// failedRise lowers the figure after a timed rise with the job that shows it wrong, and says what the estimator reports.
func (tl *sampleRun) failedRise(gap time.Duration) (refuted int, failed bool) {
	tl.now = tl.now.Add(gap)
	tl.e.Tick(tl.now)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	return tl.e.TakeFailedRise()
}

// Each refuted timed rise is counted, and the count is what puts transcodeCores in the second line of the log. Mutant:
// `failedRises++` removed.
func TestEachRefutedTimedRiseIsCountedSinceTheLastConfirmedOne(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	if _, failed := tl.e.TakeFailedRise(); failed {
		t.Fatal("the first lowering was a refuted rise")
	}
	if refuted, failed := tl.failedRise(5 * time.Minute); !failed || refuted != 1 {
		t.Errorf("first refuted rise: %d, %v; want 1, true", refuted, failed)
	}
	if refuted, failed := tl.failedRise(10 * time.Minute); !failed || refuted != 2 {
		t.Errorf("second refuted rise: %d, %v; want 2, true", refuted, failed)
	}
	if _, failed := tl.e.TakeFailedRise(); failed {
		t.Error("a refuted rise is reported twice")
	}
}

// Three samples at or above 0.9 of the raised figure confirm a rise; two do not. A rise that two samples only began to confirm
// is still refuted by the lowering that follows, which doubles the gap. Mutant: `confirmed >= 1`.
func TestTwoConfirmingSamplesDoNotConfirmARiseAndThreeDo(t *testing.T) {
	confirming := jobAt(3.2, 3.9*admissionBound/3.2) // E = 3.9 of a figure of 4, at 1.17x: held, and slow.
	for _, tc := range []struct {
		samples int
		refuted bool
		gap     time.Duration // The wait before the next rise.
	}{{2, true, 10 * time.Minute}, {3, false, 5 * time.Minute}} {
		tl := newSampleRun(4)
		tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
		tl.now = tl.now.Add(5 * time.Minute)
		if !tl.e.Tick(tl.now) {
			t.Fatal("the first rise was not due")
		}
		tl.feedJobs(tc.samples, starvedSample, confirming)
		tl.feedJobs(7, starvedSample, jobAt(3.2, i5)) // The job that follows is as slow as before.
		if _, failed := tl.e.TakeFailedRise(); failed != tc.refuted {
			t.Errorf("%d confirming samples: the lowering after refuted the rise: %v, want %v", tc.samples, failed, tc.refuted)
		}
		loweredAt := tl.now
		if tl.e.Tick(loweredAt.Add(tc.gap - time.Second)) {
			t.Errorf("%d confirming samples: a rise came before the gap of %v", tc.samples, tc.gap)
		}
		if !tl.e.Tick(loweredAt.Add(tc.gap)) {
			t.Errorf("%d confirming samples: the rise was not due after %v", tc.samples, tc.gap)
		}
	}
}

// besideExtraction makes a sample one taken while the worker's own extraction runs.
func besideExtraction(cpu func(time.Time, ...learnJob) learnSample, jobs ...learnJob) func(time.Time) learnSample {
	return func(at time.Time) learnSample {
		s := cpu(at, jobs...)
		s.Extraction = true
		return s
	}
}

// A timed rise does not lift a figure while a job runs beside the worker's own extraction (D-070): those samples are void for a
// settled figure, so the clock of the gap would otherwise run out under a figure that the running job still confirms, and
// admit plays the worker cannot hold for as long as the extraction lasts. The clock holds while the extraction is in a sample's
// window and the gap resumes after it. Mutant: no hold in Observe (the rise comes 5 minutes after the lowering).
func TestATimedRiseWaitsOutAnExtractionBesideARunningJob(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(tl *sampleRun) // Leaves the figure lowered.
	}{
		{"a figure learned from clean samples", func(tl *sampleRun) { tl.feedJobs(7, starvedSample, jobAt(3.2, i5)) }},
		{"a figure lowered beside an extraction", func(tl *sampleRun) { tl.feed(7, besideExtraction(starvedSample, jobAt(3.2, i5))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := newSampleRun(4)
			tc.start(tl)
			if tl.e.Figure() >= 4 {
				t.Fatal("the figure was not lowered")
			}
			lowered := tl.e.Figure()
			// An hour of an extraction beside the job that still runs slowly: no rise.
			for range 1200 {
				tl.feed(1, besideExtraction(starvedSample, jobAt(3.2, i5)))
				if tl.e.Tick(tl.now) {
					t.Fatalf("a timed rise came %v after the lowering, beside the worker's own extraction", tl.now.Sub(learnEpoch))
				}
			}
			if got := tl.e.Figure(); got != lowered {
				t.Fatalf("the figure moved from %v to %v beside an extraction", lowered, got)
			}
			// The extraction ends. The gap runs from the last sample it was in, and no job has tested the figure since.
			endedAt := tl.now
			if tl.e.Tick(endedAt.Add(5*time.Minute - time.Second)) {
				t.Error("the rise came before the gap had run after the extraction")
			}
			if !tl.e.Tick(endedAt.Add(5 * time.Minute)) {
				t.Error("the rise was not due a gap after the extraction")
			}
		})
	}
}

// The design record's rule (docs/transcoding.md, section 4) is that a lowered figure rises by time when no sample has counted
// for the gap, and a sample void for a reason other than the worker's own extraction (a paused job, a young one, a slow read)
// is not a test of the figure: it does not hold the clock. This pins that the hold above is the extraction's alone.
func TestSamplesVoidForOtherReasonsDoNotHoldTheTimedRise(t *testing.T) {
	paused := jobAt(3.2, i5)
	paused.Paused = true
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	for range 120 { // 6 minutes, longer than the gap.
		tl.feedJobs(1, starvedSample, paused)
	}
	if !tl.e.Tick(tl.now) {
		t.Error("a job paused for longer than the gap held the figure down; the record's rule is that only a counting sample does")
	}
}

// An extraction cannot move a figure that clean samples have confirmed at its start: three samples at or above 0.9 of it settle it,
// as a move would. Mutant: `settled` set only by a move, which lets a CPU-bound 1.0x sample beside an extraction lower a start that
// an hour of jobs at 1.3x held (3.5 to 2.67, source learned).
func TestAFigureCleanSamplesConfirmedAtItsStartIsNotLoweredByAnExtraction(t *testing.T) {
	tl := newSampleRun(3.5)
	tl.feedJobs(1200, starvedSample, jobAt(3.2, 1.3)) // An hour: E = 3.47, inside the band of 3.5.
	tl.feed(60, besideExtraction(starvedSample, jobAt(3.2, 1.0)))
	if got, source := tl.e.Figure(), tl.e.Source(); got != 3.5 || source != "assumed" {
		t.Errorf("the figure is %v from %q, want 3.5 assumed: an extraction moved a figure clean samples confirmed", got, source)
	}
}
