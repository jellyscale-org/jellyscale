package transcode

import (
	"math"
	"testing"
	"time"
)

// JS-177 (D-064, D-066, D-070): the estimator that turns a worker's samples into the capacity it keeps. These tests are
// the pure logic: samples in with an injected clock and injected readings (the CPU the worker's ffmpeg processes wanted,
// each job's own speed), the figure out. No process, no database. Real processes and real contention are the checks of
// capacity_learn_integration_test.go and cmd/jellyscale/capacity_learning_integration_test.go.
//
// None of the names below exists yet; they are what the checks imply, and the build adds exactly these (capacity_learn.go):
//
//	type learnJob struct {
//		Price    float64       // playback.Spec.Cost of the job
//		Speed    float64       // ffmpeg's own speed over the sample's window (progress.speedOver), paused time left out
//		Age      time.Duration // since the job's first output, at the sample
//		Paused   bool          // at its lead at any moment of the window
//		Copy     bool          // a copy: left out of the sum, voids nothing
//		Fallback bool          // an HEVC or AV1 fallback (Spec.Recode): its speed is never a sample
//	}
//	type learnSample struct {
//		At         time.Time
//		Allotment  float64 // CPUs the worker may use: cpu.max, else the affinity, else GOMAXPROCS
//		Demand     float64 // CPU-seconds per second the worker's transcode ffmpeg processes wanted over the window (on a CPU plus on a run queue)
//		Jobs       []learnJob
//		JobEnded   bool // a job ended in the window
//		Extraction bool // a non-transcode media tool ran at any moment of the window (D-070)
//		Unreadable bool // the kernel's figures could not be read
//	}
//	func startFigure(allotment, ceiling float64) (figure float64, source string) // ceiling 0 is none
//	func newCapacityEstimator(start float64, source string, lease time.Duration, at time.Time) *capacityEstimator
//	func (e *capacityEstimator) Observe(s learnSample) (void string) // "", "not_cpu_bound", "paused", "mixed_load" or "unreadable"
//	func (e *capacityEstimator) Tick(at time.Time) (rose bool)       // a timed rise, if one is due
//	func (e *capacityEstimator) Figure() float64
//	func (e *capacityEstimator) Source() string // "assumed", "ceiling" or, once a sample has moved it, "learned"
//
// The numbers asserted are the design record's (docs/transcoding.md, "Learning capacity"): a sample every 3 s heartbeat;
// CPU-bound is the CPU wanted at 0.9 of the allotment or more; a job counts from 15 s after its first output; 7 counting
// samples lower the figure to their mean and 3 raise it to their least, with a dead band of 10%; the floor is a quarter of
// the start; the first timed-rise gap is 30 leases (5 minutes at the default 10 s), doubles with each rise a job shows
// wrong, is capped at 7 days, and resets when a rise is confirmed. A sample is one worker, E = sum(price x speed) / 1.2.

var learnEpoch = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const (
	sampleEvery = 3 * time.Second
	i5          = 0.88 // The i5-6500's speed on the 3.2 row (JS-154, JS-177 measurement).
)

// heldBy is the capacity the jobs show: the price they keep at 1.2 times real time.
func heldBy(jobs ...learnJob) float64 {
	sum := 0.0
	for _, j := range jobs {
		if !j.Copy {
			sum += j.Price * j.Speed / 1.2
		}
	}
	return sum
}

func jobAt(price, speed float64) learnJob {
	return learnJob{Price: price, Speed: speed, Age: time.Hour}
}

// starvedSample is a sample of a worker whose processors the ffmpeg wanted all of: 1.5 times the allotment, which a hog-starved
// job read (JS-177 measurement 1.86 and over).
func starvedSample(at time.Time, jobs ...learnJob) learnSample {
	return learnSample{At: at, Allotment: 4, Demand: 6, Jobs: jobs}
}

// idleSample is a sample of a worker whose ffmpeg mostly waited: a slow source or store (measured 0.44 and under).
func idleSample(at time.Time, jobs ...learnJob) learnSample {
	return learnSample{At: at, Allotment: 4, Demand: 1.5, Jobs: jobs}
}

type sampleRun struct {
	e   *capacityEstimator
	now time.Time
}

func newSampleRun(start float64) *sampleRun {
	return &sampleRun{e: newCapacityEstimator(start, "assumed", defaultLease, learnEpoch), now: learnEpoch}
}

// feed observes n samples, one heartbeat apart, built by make from the time of each, and returns the last void reason.
func (tl *sampleRun) feed(n int, make func(at time.Time) learnSample) (void string) {
	for range n {
		tl.now = tl.now.Add(sampleEvery)
		void = tl.e.Observe(make(tl.now))
	}
	return void
}

func (tl *sampleRun) feedJobs(n int, cpu func(time.Time, ...learnJob) learnSample, jobs ...learnJob) string {
	return tl.feed(n, func(at time.Time) learnSample { return cpu(at, jobs...) })
}

func wantNear(t *testing.T, what string, got, want, within float64) {
	t.Helper()
	if math.Abs(got-want) > within {
		t.Errorf("%s: %.4f, want %.4f (within %.4f)", what, got, want, within)
	}
}

// The start is the lesser of the CPUs the worker may use, a container limit counted as written, and the optional ceiling
// (D-064, criteria 4 and 13). A quota is never rounded up: 3.5 is 3.5, not 4.
func TestTheStartFigureIsTheLesserOfTheAllotmentAndTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		name               string
		allotment, ceiling float64
		want               float64
		source             string
	}{
		{"a 3.5 CPU limit counts as written", 3.5, 0, 3.5, "assumed"},
		{"a limit of 0.4 counts as written", 0.4, 0, 0.4, "assumed"},
		{"the processors with no ceiling", 4, 0, 4, "assumed"},
		{"a ceiling below the processors", 8, 3, 3, "ceiling"},
		{"a fractional ceiling below the processors", 8, 2.7, 2.7, "ceiling"},
		{"a ceiling above the processors no longer counts as written", 2, 64, 2, "assumed"},
		{"a ceiling above a fractional limit", 3.5, 8, 3.5, "assumed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, source := startFigure(tc.allotment, tc.ceiling)
			if got != tc.want || source != tc.source {
				t.Errorf("startFigure(%v, %v) = %v, %q; want %v, %q", tc.allotment, tc.ceiling, got, source, tc.want, tc.source)
			}
		})
	}
}

// The unit and the lowering: seven consecutive counting samples of a CPU-bound worker whose jobs run under 1.2x, and not
// six; the figure becomes the mean of the seven estimates, E = price x speed / 1.2. The i5's 0.88x on the 3.2 row is 2.35.
func TestSevenSaturatedSamplesUnder1Point2LowerTheFigureToTheirMeanAndSixDoNot(t *testing.T) {
	tl := newSampleRun(4)
	speeds := []float64{0.84, 0.86, 0.88, 0.88, 0.90, 0.92, 0.88}
	i := 0
	for ; i < 6; i++ {
		tl.feedJobs(1, starvedSample, jobAt(3.2, speeds[i]))
	}
	if got := tl.e.Figure(); got != 4 {
		t.Fatalf("the figure moved to %v after six counting samples; it takes seven", got)
	}
	tl.feedJobs(1, starvedSample, jobAt(3.2, speeds[6]))
	mean := 0.0
	for _, speed := range speeds {
		mean += heldBy(jobAt(3.2, speed)) / float64(len(speeds))
	}
	wantNear(t, "the figure after seven samples", tl.e.Figure(), mean, 1e-9)
	wantNear(t, "the figure against the i5's price x speed / 1.2", tl.e.Figure(), 3.2*i5/1.2, 3.2*i5/1.2/10)
	if got := tl.e.Source(); got != "learned" {
		t.Errorf("source %q after a move, want learned", got)
	}
}

// A sample whose estimate is within 10% of the figure either way neither lowers nor raises it, however many come: the dead
// band that the run-to-run 3% of E sits inside (price 2.0, start 2.0: E is 1.667 x speed).
func TestTheDeadBandIsTenPercentEitherWay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		speed float64
		moved bool
	}{
		{"E at 0.917 of the figure", 1.10, false}, // 2.0 x 1.10 / 1.2 = 1.833
		{"E at 0.875 of the figure", 1.05, true},  // 2.0 x 1.05 / 1.2 = 1.750
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := newSampleRun(2)
			tl.feedJobs(40, starvedSample, jobAt(2, tc.speed))
			if moved := tl.e.Figure() != 2; moved != tc.moved {
				t.Errorf("figure %v after 40 samples at E = %.3f of a figure of 2: moved %v, want %v", tl.e.Figure(), heldBy(jobAt(2, tc.speed)), moved, tc.moved)
			}
		})
	}
	// Above: a job at 1.2x or more whose E is 1.1 of the figure or under confirms it; it does not raise it.
	tl := newSampleRun(2)
	tl.feedJobs(40, starvedSample, jobAt(2, 1.3)) // E = 2.167, 1.083 of 2: inside the band.
	if got := tl.e.Figure(); got != 2 {
		t.Errorf("a figure of 2 moved to %v on samples inside the dead band above it", got)
	}
}

// A slow job with the CPU not wanted is a slow read, a slow store or a hung ffmpeg, not a slow CPU: void, whatever its
// speed, however many samples. The line is 0.9 of the allotment: 0.89 is void, 0.91 counts (D-064, JS-177 measurement 1:
// waiting reads at 0.44 and under, starved at 1.86 and over). A speed-only estimator lowers on the first.
func TestASlowJobWithTheCPUNotWantedChangesNothingAndTheKneeIsNinetyPercentOfTheAllotment(t *testing.T) {
	tl := newSampleRun(4)
	for _, speed := range []float64{0.9, 0.5, 0.1, 0.0} {
		void := tl.feedJobs(30, idleSample, jobAt(3.2, speed))
		if void != "not_cpu_bound" {
			t.Errorf("a job at %.1fx with the CPU idle: void %q, want not_cpu_bound", speed, void)
		}
	}
	if got := tl.e.Figure(); got != 4 {
		t.Fatalf("the figure moved to %v for slow jobs that waited", got)
	}
	below := newSampleRun(4)
	below.feed(30, func(at time.Time) learnSample {
		return learnSample{At: at, Allotment: 4, Demand: 3.56, Jobs: []learnJob{jobAt(3.2, i5)}} // 0.89 of 4
	})
	if got := below.e.Figure(); got != 4 {
		t.Errorf("wanted at 0.89 of the allotment moved the figure to %v; it is not CPU-bound", got)
	}
	above := newSampleRun(4)
	above.feed(7, func(at time.Time) learnSample {
		return learnSample{At: at, Allotment: 4, Demand: 3.64, Jobs: []learnJob{jobAt(3.2, i5)}} // 0.91 of 4
	})
	wantNear(t, "wanted at 0.91 of the allotment, seven samples", above.e.Figure(), 3.2*i5/1.2, 1e-9)
}

// What voids a sample is told apart by cause (the design's table). Each is void with its reason, from CPU-bound samples of a
// slow job that would otherwise lower the figure, and moves nothing however long it lasts.
func TestASampleIsVoidForAPausedJobAYoungOrEndedJobAndAFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample func(at time.Time) learnSample
		void   string
	}{
		{"a job paused at its lead at any moment of the window", func(at time.Time) learnSample {
			paused := jobAt(0.8, 5)
			paused.Paused = true
			return starvedSample(at, paused, jobAt(3.2, i5))
		}, "paused"},
		{"a job 14 s from its first output", func(at time.Time) learnSample {
			young := jobAt(3.2, i5)
			young.Age = 14 * time.Second
			return starvedSample(at, young)
		}, "mixed_load"},
		{"a job that ended in the window", func(at time.Time) learnSample {
			s := starvedSample(at, jobAt(3.2, i5))
			s.JobEnded = true
			return s
		}, "mixed_load"},
		{"an HEVC or AV1 fallback jobAt, beside a job that would count", func(at time.Time) learnSample {
			fallback := jobAt(3.2, 1.3) // Measured 33% to 52% above the x264 rows: its CPU is not what its price says.
			fallback.Fallback = true
			return starvedSample(at, fallback, jobAt(3.2, i5))
		}, "mixed_load"},
		{"the kernel's figures cannot be read", func(at time.Time) learnSample {
			s := starvedSample(at, jobAt(3.2, i5))
			s.Unreadable = true
			return s
		}, "unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tl := newSampleRun(4)
			if void := tl.feed(40, tc.sample); void != tc.void {
				t.Errorf("void %q, want %q", void, tc.void)
			}
			if got := tl.e.Figure(); got != 4 {
				t.Errorf("the figure moved to %v on void samples", got)
			}
			if got := tl.e.Source(); got != "assumed" {
				t.Errorf("source %q after void samples, want assumed", got)
			}
		})
	}
}

// A job counts from 15 s after its first output: 14 s is void and 15 s counts (the measured length of a job's warm-up and
// of the young restarts of a store that holds puts for a lease).
func TestAJobCountsFromFifteenSecondsAfterItsFirstOutput(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		moved bool
	}{{14*time.Second + 900*time.Millisecond, false}, {15 * time.Second, true}} {
		tl := newSampleRun(4)
		job := jobAt(3.2, i5)
		job.Age = tc.age
		tl.feedJobs(7, starvedSample, job)
		if moved := tl.e.Figure() != 4; moved != tc.moved {
			t.Errorf("a job %v from its first output: moved %v, want %v (figure %v)", tc.age, moved, tc.moved, tl.e.Figure())
		}
	}
}

// A copy (price 0.05) is left out of the sum and voids nothing: the figure is the re-encode's E alone.
func TestACopyBesideAReencodeIsLeftOutOfTheSumAndVoidsNothing(t *testing.T) {
	tl := newSampleRun(4)
	copied := learnJob{Price: 0.05, Speed: 7, Age: 3 * time.Second, Copy: true}
	tl.feedJobs(7, starvedSample, copied, jobAt(3.2, i5))
	wantNear(t, "the figure with a young copy beside the 3.2 re-encode", tl.e.Figure(), 3.2*i5/1.2, 1e-9)
}

// The floor is a quarter of the start: a bad sample cannot zero a worker.
func TestTheFigureNeverGoesBelowAQuarterOfItsStart(t *testing.T) {
	for _, start := range []float64{4, 0.8} {
		tl := newSampleRun(start)
		tl.feedJobs(30, starvedSample, jobAt(3.2, 0.02)) // E = 0.053
		wantNear(t, "the figure after a hundred times too slow a job", tl.e.Figure(), start/4, 1e-9)
	}
}

// A lowered figure rises from evidence, with no wait: three consecutive samples of jobs that run at 1.2x or more, whose E
// is above 1.1 of the figure, and the figure becomes the least of their estimates, never above its start.
func TestAFigureRisesFromEvidenceAfterThreeSamplesToTheLeastEstimateAndNeverAboveItsStart(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5)) // 2.35
	lowered := tl.e.Figure()
	estimates := []float64{3.0, 3.2, 2.8}
	for i, estimate := range estimates {
		price := 2.0
		tl.feedJobs(1, starvedSample, jobAt(price, estimate*1.2/price)) // All at 1.8x and over.
		if i < 2 && tl.e.Figure() != lowered {
			t.Fatalf("the figure moved to %v after %d rising samples; it takes three", tl.e.Figure(), i+1)
		}
	}
	wantNear(t, "the figure after three rising samples", tl.e.Figure(), 2.8, 1e-9)

	capped := newSampleRun(4)
	capped.feedJobs(7, starvedSample, jobAt(3.2, i5))
	capped.feedJobs(10, starvedSample, jobAt(3.2, 4.0)) // E = 10.7.
	if got := capped.e.Figure(); got != 4 {
		t.Errorf("the figure rose to %v; it never exceeds its start of 4", got)
	}
}

// Jobs that all run at 1.2x or more whose prices add up above 1.1 of the figure also show the worker holds more, with the
// CPU not wanted: a shared play's own job counts. The figure becomes the prices' sum, never above the start.
func TestJobsAt1Point2OrMoreWhosePricesExceedTheFigureRaiseItWithTheCPUNotWanted(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5)) // 2.35
	tl.feedJobs(3, idleSample, jobAt(2.0, 1.5), jobAt(1.6, 1.5))
	wantNear(t, "the figure after jobs of 3.6 cores of price at 1.5x", tl.e.Figure(), 3.6, 1e-9)
}

// A sample that does not count never raises the figure: a slow job with the CPU idle beside a big price is void.
func TestASampleThatDoesNotCountNeverRaisesTheFigure(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	lowered := tl.e.Figure()
	tl.feedJobs(30, idleSample, jobAt(3.2, 0.9), jobAt(2.0, 3.0)) // One job slow with the CPU idle: void, whatever the prices.
	if got := tl.e.Figure(); got != lowered {
		t.Errorf("a void sample moved the figure from %v to %v", lowered, got)
	}
	paused := jobAt(3.2, 2)
	paused.Paused = true
	tl.feedJobs(30, starvedSample, paused, jobAt(3.2, 3))
	if got := tl.e.Figure(); got != lowered {
		t.Errorf("a window with a paused job moved the figure from %v to %v", lowered, got)
	}
}

// The seven samples that lower the figure must follow each other: a counting sample inside the dead band between two runs
// of low ones breaks the run, so a worker that is slow in turns is not lowered by the turns added up.
func TestSevenLowSamplesMustFollowEachOther(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(6, starvedSample, jobAt(3.2, i5))
	tl.feedJobs(1, starvedSample, jobAt(3.9, 1.15)) // E = 3.74: inside the band of a figure of 4, and under 1.2x.
	tl.feedJobs(6, starvedSample, jobAt(3.2, i5))
	if got := tl.e.Figure(); got != 4 {
		t.Errorf("six low samples, one inside the band and six low ones moved the figure to %v; the seven must be consecutive", got)
	}
}

// loadBurst is a neighbour's load of a set length: a job whose window overlaps it by overlap seconds of the ten has that share
// of its speed taken, fall being how much of it the loadBurst takes at full overlap (1 stops the job).
type loadBurst struct {
	begin, end time.Time
	fall       float64
}

func (b loadBurst) speed(at time.Time) float64 {
	from := at.Add(-10 * time.Second)
	if from.Before(b.begin) {
		from = b.begin
	}
	until := at
	if b.end.Before(until) {
		until = b.end
	}
	overlap := until.Sub(from).Seconds()
	return i5 * (1 - b.fall*max(overlap, 0)/10)
}

// play runs the worker through the burst: one sample every 3 s for 120 s, at the production cadence with explicit times, and
// returns the lowest figure it held, how long after the burst ended the figure first stood 10% above that lowest, and the
// figure 30 s after the burst ended.
func (b loadBurst) play(held float64) (lowest float64, firstRise time.Duration, thirtyAfter float64) {
	tl := &sampleRun{e: newCapacityEstimator(held, "assumed", defaultLease, learnEpoch), now: learnEpoch}
	lowest = held
	for tl.now.Before(learnEpoch.Add(120 * time.Second)) {
		tl.feed(1, func(at time.Time) learnSample { return starvedSample(at, jobAt(3.2, b.speed(at))) })
		if tl.e.Figure() > lowest*1.1 && lowest < held*0.9 && firstRise == 0 && !tl.now.Before(b.end) {
			firstRise = tl.now.Sub(b.end)
		}
		lowest = min(lowest, tl.e.Figure())
		if !tl.now.Before(b.end.Add(30*time.Second)) && thirtyAfter == 0 {
			thirtyAfter = tl.e.Figure()
		}
	}
	return lowest, firstRise, thirtyAfter
}

// What a neighbour's burst of load does (criterion 10), at the production cadence: a burst of b seconds lowers the samples
// whose 10 s window it overlaps: a job stopped by it reads 10% low once 1 s of a window overlaps, so samples are lowered for
// b + 8 s; the measured depth (the job falls to 0.37x) needs 1.4 s of overlap, so b + 7.2 s. Seven samples take 18 s, so a
// burst of 8 s or less cannot move the figure at any depth or phase, and 10 s at the measured depth cannot either
// (docs/transcoding.md, section 3). One of 16 s that stops the job lowers it at every phase; and after the load drops
// (section 4, criterion 11) the figure first rises within 16 s and is within a tenth of what the worker holds within 30 s.
// The worker holds 2.35, the i5's, and starts there. This is the burst's whole arithmetic: it is not repeated with a real
// ffmpeg stopped, which depends on the ffmpeg version (6.1 keeps its read budget in wall time while stopped, invariant 57, so
// a stop does not lower its speed after it continues). The sampler's cadence, which these explicit times assume, is the
// worker's check (TestTheSamplerTakesOneSampleEveryHeartbeat).
func TestAShortBurstOfLoadMovesNothingAndALongOneLowersTheFigureAndItRisesAgain(t *testing.T) {
	held := 3.2 * i5 / 1.2
	phases := []time.Duration{0, 500 * time.Millisecond, time.Second, 1500 * time.Millisecond, 2 * time.Second, 2500 * time.Millisecond}
	for _, tc := range []struct {
		name  string
		long  time.Duration
		fall  float64
		moves bool
	}{
		{"8 s that stops the job", 8 * time.Second, 1.0, false},
		{"8 s at the measured depth", 8 * time.Second, 0.63, false},
		{"10 s at the measured depth", 10 * time.Second, 0.63, false},
		{"16 s that stops the job", 16 * time.Second, 1.0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, phase := range phases {
				begin := learnEpoch.Add(40*time.Second + phase)
				lowest, firstRise, thirtyAfter := loadBurst{begin: begin, end: begin.Add(tc.long), fall: tc.fall}.play(held)
				if !tc.moves && lowest < held*0.9 {
					t.Errorf("phase %v: the figure fell to %.3f of a held %.3f; a burst this short moves it by no more than the 10%% margin", phase, lowest, held)
				}
				if tc.moves && lowest >= held*0.9 {
					t.Errorf("phase %v: a burst that stopped the job for %v left the figure at %.3f of a held %.3f", phase, tc.long, lowest, held)
				}
				if tc.moves && (firstRise == 0 || firstRise > 16*time.Second) {
					t.Errorf("phase %v: the figure first rose %v after the load dropped, want within 16 s", phase, firstRise)
				}
				if tc.moves && thirtyAfter < held*0.9 {
					t.Errorf("phase %v: 30 s after the load dropped the figure is %.3f of a held %.3f, want it within a tenth", phase, thirtyAfter, held)
				}
			}
		})
	}
}

// The unreadable kernel: a worker that cannot read the scheduler's figures learns nothing, however its jobs run, and its
// source stays what it started as (D-066 limit 4: it says so once at start, which the process checks).
func TestAWorkerThatCannotReadTheKernelLearnsNothing(t *testing.T) {
	tl := &sampleRun{e: newCapacityEstimator(4, "assumed", defaultLease, learnEpoch), now: learnEpoch}
	void := tl.feed(60, func(at time.Time) learnSample {
		s := starvedSample(at, jobAt(3.2, 0.3))
		s.Unreadable = true
		return s
	})
	if void != "unreadable" || tl.e.Figure() != 4 || tl.e.Source() != "assumed" {
		t.Errorf("void %q, figure %v, source %q; want unreadable, 4, assumed", void, tl.e.Figure(), tl.e.Source())
	}
}

// D-070: a sample taken while the worker's own extraction runs neither lowers nor raises a learned figure. Learned is a
// figure that a sample with no extraction in its window has set.
func TestASampleBesideAnExtractionNeverMovesALearnedFigure(t *testing.T) {
	beside := func(cpu func(time.Time, ...learnJob) learnSample, jobs ...learnJob) func(time.Time) learnSample {
		return func(at time.Time) learnSample {
			s := cpu(at, jobs...)
			s.Extraction = true
			return s
		}
	}
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5)) // Learned at 2.35, with no extraction.
	learned := tl.e.Figure()
	tl.feed(60, beside(starvedSample, jobAt(3.2, 0.3)))             // Far lower than the figure: a lowering, if it counted.
	tl.feed(60, beside(starvedSample, jobAt(3.2, 2.0)))             // Far higher, all at over 1.2x: a rise, if it counted.
	tl.feed(60, beside(idleSample, jobAt(2.0, 2.0), jobAt(3.2, 2))) // And by the sum of prices.
	if got := tl.e.Figure(); got != learned {
		t.Errorf("samples beside an extraction moved a learned figure from %v to %v", learned, got)
	}
	if got := tl.e.Source(); got != "learned" {
		t.Errorf("source %q, want learned", got)
	}
}

// D-070 and the design's rule for a figure not yet learned (a start, or one lowered only beside an extraction): a CPU-bound
// sample beside an extraction may lower it, because a worker that runs extractions for hours would otherwise overstate
// itself for all of them; it is never raised beside one; and the clean samples that follow replace it in either direction,
// up to the start.
func TestAFigureNotYetLearnedIsLoweredBesideAnExtractionNeverRaisedThereAndReplacedByCleanSamples(t *testing.T) {
	beside := func(jobs ...learnJob) func(time.Time) learnSample {
		return func(at time.Time) learnSample {
			s := starvedSample(at, jobs...)
			s.Extraction = true
			return s
		}
	}
	tl := newSampleRun(4)
	tl.feed(7, beside(jobAt(3.2, 0.7))) // E = 1.87: the extraction took a fifth of the speed.
	lowered := tl.e.Figure()
	wantNear(t, "a start lowered beside an extraction", lowered, 3.2*0.7/1.2, 1e-9)
	tl.feed(60, beside(jobAt(3.2, 2.0)))
	if got := tl.e.Figure(); got != lowered {
		t.Fatalf("a figure lowered beside an extraction was raised to %v beside one", got)
	}
	// The extraction ends: clean samples at the truth, 2.35, above the figure the extraction had taken it to.
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	wantNear(t, "after seven clean samples above it", tl.e.Figure(), 3.2*i5/1.2, 3.2*i5/1.2/10)
	// And clean samples below the extraction's own figure lower it past it.
	low := newSampleRun(4)
	low.feed(7, beside(jobAt(3.2, 0.7)))
	low.feedJobs(7, starvedSample, jobAt(3.2, 0.4))
	wantNear(t, "after seven clean samples below it", low.e.Figure(), 3.2*0.4/1.2, 1e-9)
}

// The timed rise and its backoff (criterion 12, D-066 (2), the design's schedule): a lowered figure that no job has tested
// rises to its start once the gap has passed, from the lowering. The first gap is 30 leases, 5 minutes at the default. A rise
// that the next job shows wrong (seven counting samples lower it again) doubles the gap before the next, with no cap under
// 24 hours (the tenth rise waits 2560 minutes) and a cap of 7 days. The rises are due 5, 15.75, 36.5, 77.25 ... minutes
// after the first lowering when each play lowers the figure 45 s after the rise; here the lowering follows the rise by
// seven samples, 21 s.
func TestATimedRiseBacksOffByDoublingFromFiveMinutesAndNeverUnderADay(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	if tl.e.Figure() >= 4 {
		t.Fatal("the first lowering did not happen")
	}
	gaps := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute,
		320 * time.Minute, 640 * time.Minute, 1280 * time.Minute, 2560 * time.Minute, 5120 * time.Minute, 7 * 24 * time.Hour, 7 * 24 * time.Hour}
	for n, gap := range gaps {
		loweredAt := tl.now
		if tl.e.Tick(loweredAt.Add(gap - time.Second)) {
			t.Fatalf("rise %d came %v after the figure was lowered, before the gap of %v", n+1, gap-time.Second, gap)
		}
		tl.now = loweredAt.Add(gap)
		if !tl.e.Tick(tl.now) {
			t.Fatalf("rise %d was not due %v after the figure was lowered", n+1, gap)
		}
		if got := tl.e.Figure(); got != 4 {
			t.Fatalf("rise %d took the figure to %v, want its start of 4", n+1, got)
		}
		// The play admitted above the figure: the job shows it wrong.
		tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
		if tl.e.Figure() >= 4 {
			t.Fatalf("after rise %d the figure was not lowered again by seven samples", n+1)
		}
	}
}

// A rise that a job confirms (three counting samples at or above 0.9 of the raised figure) resets the gap to its first
// value, and so does a lowering that was not a failed rise.
func TestAConfirmedRiseResetsTheGap(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	for _, gap := range []time.Duration{5 * time.Minute, 10 * time.Minute} { // Two failed rises: the gap is 20 minutes now.
		tl.now = tl.now.Add(gap)
		if !tl.e.Tick(tl.now) {
			t.Fatalf("the rise after %v was not due", gap)
		}
		tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	}
	tl.now = tl.now.Add(20 * time.Minute)
	if !tl.e.Tick(tl.now) {
		t.Fatal("the third rise, after 20 minutes, was not due")
	}
	// This time the job holds the raised figure: E = 3.9 of 4 at 1.17x, three samples.
	tl.feedJobs(3, starvedSample, jobAt(3.2, 3.9*1.2/3.2))
	if tl.e.Figure() != 4 {
		t.Fatalf("the figure is %v after a rise that the jobs confirmed", tl.e.Figure())
	}
	// Later the worker slows (a neighbour moves in for good) and is lowered: that is not a failed rise, so the gap is 5 minutes.
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	loweredAt := tl.now
	if tl.e.Tick(loweredAt.Add(5*time.Minute - time.Second)) {
		t.Fatal("a rise came before the first gap")
	}
	if !tl.e.Tick(loweredAt.Add(5 * time.Minute)) {
		t.Errorf("the gap after a confirmed rise and a lowering was not the first, 5 minutes: the backoff was not reset")
	}
}

// Counting samples that confirm a lowered figure keep it lowered however long they go on: the timed rise is only for a
// figure no job has tested.
func TestAFigureThatRunningJobsConfirmIsNeverRaisedByTime(t *testing.T) {
	tl := newSampleRun(4)
	tl.feedJobs(7, starvedSample, jobAt(3.2, i5))
	lowered := tl.e.Figure()
	// 40 minutes of samples a sampleEvery apart that sit inside the band of the lowered figure: the first gap is 5 minutes.
	for range 800 {
		tl.feedJobs(1, starvedSample, jobAt(3.2, i5))
		if tl.e.Tick(tl.now) {
			t.Fatalf("a timed rise came %v after the lowering while jobs went on confirming the figure", tl.now.Sub(learnEpoch))
		}
	}
	if got := tl.e.Figure(); math.Abs(got-lowered) > lowered/10 {
		t.Errorf("the figure is %v, was %v", got, lowered)
	}
}
