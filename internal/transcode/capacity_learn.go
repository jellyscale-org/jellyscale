package transcode

import "time"

// JS-177 (D-064, D-066, D-070): a worker learns what it holds from the jobs it runs. Capacity is the price a worker keeps at
// 1.2 times real time, E = sum(price x speed) / 1.2, in the unit the prices and the admission bound are in. The design, with
// its measurements and the reasons for each number, is in docs/transcoding.md ("Learning capacity").
//
// The estimator is the arithmetic only: a sample goes in, with the time it was taken at, and the figure comes out. It reads
// no clock and touches no process, so the worker's sampler (capacity_sampler.go) and the unit tests drive it alike.

const (
	// admissionBound is the speed, in film seconds per second, a job must make to be held (D-044).
	admissionBound = 1.2
	// cpuKnee is the share of its processors that a worker's ffmpeg must want for a slow job to be the CPU's doing: a job that
	// waits for a read or a store wants 0.44 or less of them, one that is starved 1.86 or more (docs/transcoding.md).
	cpuKnee = 0.9
	// deadBand is the margin either way inside which a sample neither lowers nor raises the figure: E repeats to 3% from run
	// to run, and the x264 rows sit within 14% of their median.
	deadBand = 0.1
	// samplesToLower consecutive low samples lower the figure, to their mean. A burst of load of 8 s or less cannot make so
	// many (a sample is a 10 s window taken every 3 s).
	samplesToLower = 7
	// samplesToRise consecutive samples that show more raise it, to the least of them.
	samplesToRise = 3
	// jobMatures is how long after its first output a job's speed counts: the length of a job's warm-up, and of the young
	// restarts of a store that holds puts for a lease.
	jobMatures = 15 * time.Second
	// floorOfStart is the least a figure falls to, as a share of its start: a bad sample cannot zero a worker.
	floorOfStart = 0.25
	// firstGapLeases is the first wait before a lowered figure that no job has tested rises by time; it doubles each time
	// a job shows the rise wrong, up to longestGap.
	firstGapLeases = 30
	longestGap     = 7 * 24 * time.Hour
)

// learnJob is a running job as one sample sees it.
type learnJob struct {
	Price    float64       // playback.Spec.Cost of the job.
	Speed    float64       // ffmpeg's own speed over the sample's window, paused time left out.
	Age      time.Duration // Since the job's first output, at the sample.
	Paused   bool          // At its lead at any moment of the window.
	Copy     bool          // A copy: left out of the sum, voids nothing.
	Fallback bool          // An HEVC or AV1 fallback: its CPU is not what its price says, so its speed is never a sample.
}

// learnSample is what the worker saw over one window.
type learnSample struct {
	At         time.Time
	Allotment  float64 // CPUs the worker may use.
	Demand     float64 // CPU-seconds per second its transcode ffmpeg processes wanted: on a CPU plus on a run queue.
	Jobs       []learnJob
	JobEnded   bool // A job ended in the window.
	Extraction bool // The worker's own extraction ran at any moment of the window (D-070).
	Unreadable bool // The kernel's figures could not be read.
}

// What a sample is void for. A void sample moves nothing and leaves the streaks as they were.
const (
	voidNotCPUBound = "not_cpu_bound"
	voidPaused      = "paused"
	voidMixedLoad   = "mixed_load"
	voidUnreadable  = "unreadable"
)

// startFigure is the figure a worker begins from: the CPUs it may use, counted as written (3.5 is 3.5), or the ceiling
// when that is lower. A ceiling above the CPUs is not counted as written. ceiling 0 is none.
func startFigure(allotment, ceiling float64) (figure float64, source string) {
	if ceiling > 0 && ceiling < allotment {
		return ceiling, "ceiling"
	}
	return allotment, "assumed"
}

// streak is a run of consecutive samples that point the same way.
type streak struct {
	n          int
	sum, least float64
}

func (s *streak) add(estimate float64) {
	if s.n == 0 || estimate < s.least {
		s.least = estimate
	}
	s.n++
	s.sum += estimate
}

func (s *streak) mean() float64 { return s.sum / float64(s.n) }

func (s *streak) reset() { *s = streak{} }

type capacityEstimator struct {
	start, figure float64
	source        string
	// moved is whether a sample has moved the figure, and settled whether one with no extraction in its window has:
	// only a figure that has not been settled is lowered beside an extraction.
	moved, settled bool

	low, rise streak
	confirmed int // Consecutive samples at or above 0.9 of the figure since a timed rise.

	// A timed rise returns the figure to its start when no job has tested the lowered one for gap. pendingRise is
	// the rise a job has not yet confirmed or refuted. failedRises counts the refuted ones since the last confirmation.
	firstGap, gap time.Duration
	anchor        time.Time // The later of the last move and the last sample that counted.
	pendingRise   bool
	failedRises   int
	failedNow     bool
}

func newCapacityEstimator(start float64, source string, lease time.Duration, at time.Time) *capacityEstimator {
	gap := firstGapLeases * lease
	return &capacityEstimator{start: start, figure: start, source: source, firstGap: gap, gap: gap, anchor: at}
}

func (e *capacityEstimator) Figure() float64 { return e.figure }

// Source is where the figure comes from: what the worker started from until a sample has moved it, then "learned".
func (e *capacityEstimator) Source() string {
	if e.moved {
		return "learned"
	}
	return e.source
}

// TakeFailedRise reports, once, that the last move was a lowering that refuted a timed rise, and how many have been
// refuted since one was confirmed.
func (e *capacityEstimator) TakeFailedRise() (refuted int, failed bool) {
	failed, e.failedNow = e.failedNow, false
	return e.failedRises, failed
}

// Observe takes one sample and says why it was void, if it was.
func (e *capacityEstimator) Observe(s learnSample) (void string) {
	// The clock of a timed rise (Tick) runs only while nothing stops the worker from learning. A sample taken while its own
	// extraction runs is void for a settled figure and teaches an unsettled one only a lowering, so it cannot say that
	// no job has tested the figure: it holds the clock, and the gap resumes when the extraction has been out of the sample's
	// window. Samples void for any other reason leave the clock alone, as the design record has it: a rise is due when no
	// sample has counted for the gap (docs/transcoding.md, section 4).
	if s.Extraction {
		e.anchor = s.At
	}
	if s.Unreadable {
		return voidUnreadable
	}
	var jobs []learnJob
	for _, job := range s.Jobs {
		if !job.Copy {
			jobs = append(jobs, job)
		}
	}
	if void := voidOf(jobs, s.JobEnded); void != "" {
		return void
	}
	cpuBound := s.Demand >= cpuKnee*s.Allotment
	slow, price, held := false, 0.0, 0.0
	for _, job := range jobs {
		slow = slow || job.Speed < admissionBound
		price += job.Price
		held += job.Price * job.Speed / admissionBound
	}
	switch {
	case slow && !cpuBound:
		return voidNotCPUBound
	case !cpuBound:
		held = price // Every job holds its price at least; the speed of a job that did not want the CPU is no capacity.
	}
	beside := s.Extraction
	if beside && e.settled {
		return ""
	}
	lowers := slow && held < (1-deadBand)*e.figure
	rises := held > (1+deadBand)*e.figure
	if beside && !lowers {
		e.low.reset()
		return ""
	}
	if !beside {
		e.anchor = s.At
	}
	if held >= (1-deadBand)*e.figure && !lowers {
		e.confirmed++
	} else {
		e.confirmed = 0
	}
	// Three clean samples that hold the figure settle it, a start too: from then on an extraction cannot move it.
	if e.confirmed >= samplesToRise {
		e.settled = true
	}
	switch {
	case lowers:
		e.rise.reset()
		e.low.add(held)
		if e.low.n >= samplesToLower {
			e.lower(e.low.mean(), s)
		}
	case rises:
		e.low.reset()
		e.rise.add(held)
		if e.rise.n >= samplesToRise {
			e.raise(e.rise.least, s.At)
		}
	default:
		e.low.reset()
		e.rise.reset()
	}
	if e.pendingRise && e.confirmed >= samplesToRise {
		e.confirmRise()
	}
	return ""
}

// voidOf says why the jobs of a sample cannot be read as the worker's capacity, or "".
func voidOf(jobs []learnJob, ended bool) string {
	if len(jobs) == 0 {
		return voidMixedLoad
	}
	void := ""
	for _, job := range jobs {
		switch {
		case job.Paused:
			return voidPaused
		case job.Fallback || job.Age < jobMatures:
			void = voidMixedLoad
		}
	}
	if ended {
		void = voidMixedLoad
	}
	return void
}

func (e *capacityEstimator) lower(mean float64, s learnSample) {
	e.low.reset()
	figure := max(mean, floorOfStart*e.start)
	if figure >= e.figure {
		return
	}
	if e.pendingRise {
		e.gap = min(2*e.gap, longestGap)
		e.failedRises++
		e.failedNow = true
	} else {
		e.gap, e.failedRises = e.firstGap, 0
	}
	e.pendingRise = false
	e.figure, e.moved, e.anchor = figure, true, s.At
	e.settled = e.settled || !s.Extraction
}

func (e *capacityEstimator) raise(least float64, at time.Time) {
	e.rise.reset()
	figure := min(least, e.start)
	if figure <= e.figure {
		return
	}
	e.figure, e.moved, e.settled, e.anchor = figure, true, true, at
	e.confirmRise()
}

// confirmRise is a rise that jobs showed right: the next lowering starts the gaps again.
func (e *capacityEstimator) confirmRise() {
	e.pendingRise, e.confirmed = false, 0
	e.gap, e.failedRises = e.firstGap, 0
}

// Tick makes a timed rise if one is due: the figure is lowered, no sample has counted for the gap, so it returns to its
// start for a job to test.
func (e *capacityEstimator) Tick(at time.Time) (rose bool) {
	if e.figure >= e.start || at.Sub(e.anchor) < e.gap {
		return false
	}
	// A rise by time is no clean sample: the start it returns to has not been tested, so it is not held against an
	// extraction (it may be lowered beside one, never raised) until three clean samples confirm it, or a rise from evidence.
	e.figure, e.anchor, e.settled = e.start, at, false
	e.pendingRise, e.confirmed = true, 0
	e.low.reset()
	e.rise.reset()
	return true
}
