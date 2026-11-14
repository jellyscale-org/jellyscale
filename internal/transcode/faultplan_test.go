package transcode

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The plan of JS-066's fault schedules: which step falls when, on what, for how
// long. It is a function of the seed and nothing else, needs neither Postgres nor
// ffmpeg, and so runs in the unit tier; faults_integration_test.go carries the
// plan out against real processes.

// faultKind is one kind of step, with its length class where the length matters:
// a restart that a lease outlasts and one that outlasts the lease are different
// steps, because the build must do different things for them.
type faultKind string

const (
	// faultKill is SIGKILL of a worker that holds a job's lease.
	faultKill faultKind = "kill"
	// faultPGShort ends every connection of every process and refuses new ones for
	// less than a lease less a heartbeat; faultPGLong for longer than a lease.
	faultPGShort faultKind = "postgres-restart-short"
	faultPGLong  faultKind = "postgres-restart-long"
	// faultStopShort is SIGSTOP then SIGCONT of a worker, for less than a lease less
	// a heartbeat; faultStopLease for longer than a lease; faultStopStall for longer
	// than the stall limit (Worker.Stall).
	faultStopShort faultKind = "stop-short"
	faultStopLease faultKind = "stop-past-lease"
	faultStopStall faultKind = "stop-past-stall"
	// faultDelaySmall slows a worker's database connections by up to a 300 ms round
	// trip; faultDelayLong by so much that the check before a put goes unanswered
	// (half a lease).
	faultDelaySmall faultKind = "delay-small"
	faultDelayLong  faultKind = "delay-past-half-lease"
	// faultViewerStop is Jobs.Stop of a viewer.
	faultViewerStop faultKind = "viewer-stop"
)

var allFaultKinds = []faultKind{faultKill, faultPGShort, faultPGLong, faultStopShort, faultStopLease, faultStopStall, faultDelaySmall, faultDelayLong, faultViewerStop}

// faultTiming is what the workers of a schedule run at. The lease is shorter than
// the 30 s of TestAKilledWorkersJobIsClaimedBeforeItsLeaseRunsOut and longer than
// production's 10: a build that waits a dead worker's lease out claims after it
// whatever the machine, which is what that check needs, while the long steps
// (a restart or a stop that outlasts a lease, one that outlasts the stall limit)
// stay a few tens of seconds. The stall limit is production's.
type faultTiming struct{ Lease, Heartbeat, Stall time.Duration }

var faultClock = faultTiming{Lease: 20 * time.Second, Heartbeat: 3 * time.Second, Stall: 30 * time.Second}

// faultStep is one step of a schedule.
type faultStep struct {
	Kind faultKind
	// At is the planned moment, from the schedule's start; Length is how long the
	// restart, the stop or the delay lasts (0 for a kill or a viewer's stop).
	At, Length time.Duration
	// Target picks among the candidates the step has when it runs, by index: the
	// workers that hold a job's lease for a kill, the workers there are for a stop
	// or a delay, the viewers that may be stopped for a viewer's stop.
	Target int
	// Waited asks for the worker of a job a viewer is waiting on.
	Waited bool
	// Clean marks the step whose surroundings the plan keeps quiet, so that what
	// the build must do for it alone (a replacement claimed before the dead
	// worker's lease ran out) can be checked.
	Clean bool
	// RoundTrip is how long a delay makes one round trip take.
	RoundTrip time.Duration
}

func (s faultStep) String() string {
	extra := ""
	if s.Waited {
		extra += " on the worker of a waited-on job"
	}
	if s.Clean {
		extra += " (clean)"
	}
	if s.RoundTrip > 0 {
		extra += fmt.Sprintf(" round trip %v", s.RoundTrip)
	}
	return fmt.Sprintf("%s target %d at %v for %v%s", s.Kind, s.Target, s.At, s.Length, extra)
}

// faultSchedule is a list of steps that fall in the order of At.
type faultSchedule struct {
	Steps []faultStep
	// Window is how long the steps are spread over; the schedule goes on until the
	// viewers have been served after the last of them.
	Window time.Duration
}

// hit says whether another step of the schedule falls in the window of step i, from a second
// before it begins to extra after it ends, and counts for cuts.
func (s faultSchedule) hit(i int, extra time.Duration, cuts func(other faultStep) bool) bool {
	me := s.Steps[i]
	from, to := me.At-time.Second, me.At+me.Length+extra
	for j, other := range s.Steps {
		if j == i || !cuts(other) {
			continue
		}
		if earlier(other.At, to) && earlier(from, other.At+other.reach()+time.Second) {
			return true
		}
	}
	return false
}

// workerHit is a worker that a kill, a stop or a delay hit, from one moment to another.
type workerHit struct {
	worker   string
	from, to time.Time
}

// judgedAfterRestart says whether the jobs of a worker are held to what a restart longer than a
// lease does to them: they are unless a step hit that worker from a second before the
// restart began until the jobs were judged. What hit another worker cuts nothing here.
func judgedAfterRestart(hits []workerHit, worker string, began, now time.Time) bool {
	for _, h := range hits {
		if h.worker == worker && h.to.After(began.Add(-time.Second)) && h.from.Before(now) {
			return false
		}
	}
	return true
}

// workerOut says whether the step takes a worker away from the viewers for as
// long as it lasts, and for how long. Two such steps never overlap: the
// schedules keep one running worker with a free slot (a kill is replaced by a new
// process at once, which takes a few seconds to be there).
func (s faultStep) workerOut() (time.Duration, bool) {
	switch s.Kind {
	case faultKill:
		return 8 * time.Second, true
	case faultStopShort, faultStopLease, faultStopStall, faultDelayLong, faultDelaySmall:
		return s.Length + 5*time.Second, true
	}
	return 0, false
}

// isPostgres says whether the step is a restart of Postgres.
func (s faultStep) isPostgres() bool { return s.Kind == faultPGShort || s.Kind == faultPGLong }

// reach is how long after At what the step does is still felt: the length, and
// the seconds a restart takes to end every connection and a worker to be back.
func (s faultStep) reach() time.Duration {
	if out, ok := s.workerOut(); ok {
		return out
	}
	return s.Length + 6*time.Second
}

// quarrels says whether a Clean step c and another step p may not fall together. A clean step is
// one whose own check needs the surroundings quiet: a kill's is that a restart of Postgres does not
// fall within hold before it (a reaper forgets nobody then) nor within a lease after it (the
// replacement is claimed before the lease the dead worker held runs out, and a restart would
// hold the claim up); a restart's is that no worker is taken away and no other restart falls
// while it is judged; a stop's and a delay's is that no restart falls in them.
func quarrels(c, p faultStep, tm faultTiming) bool {
	if !c.Clean {
		return false
	}
	switch {
	case c.isPostgres():
		if _, out := p.workerOut(); out || p.isPostgres() {
			return earlier(p.At, c.At+c.reach()+2*time.Second) && earlier(c.At, p.At+p.reach()+2*time.Second)
		}
	case p.isPostgres():
		after := c.reach()
		before := p.reach()
		if c.Kind == faultKill {
			after, before = tm.Lease, before+liveTiming("hold")
		}
		// A second more than the kill's own window: the stop's and the delay's checks are cut by
		// hit(), which looks a second before the step as well.
		return earlier(p.At, c.At+after+time.Second) && earlier(c.At, p.At+before+2*time.Second)
	}
	return false
}

func (s faultStep) overlaps(o faultStep, margin time.Duration) bool {
	a, aOK := s.workerOut()
	b, bOK := o.workerOut()
	if !aOK || !bOK {
		return false
	}
	return s.At < o.At+b+margin && o.At < s.At+a+margin
}

// lengthOf draws a length in [low, high) at a tenth of a second.
func lengthOf(rng *rand.Rand, low, high time.Duration) time.Duration {
	return low + time.Duration(rng.Int64N(int64((high-low)/(100*time.Millisecond))))*100*time.Millisecond
}

// planFaults is the whole run: two schedules, the first with the kill, a
// restart and a stop that a lease outlasts, and the second with the ones that it
// does not outlast, so that the run holds every kind and every length of step,
// at least one kill of the worker of a job a viewer waits on, and at least one
// stop of such a worker past its lease. Steps beyond those are drawn from the seed,
// and so are all the moments, the targets and the lengths.
func planFaults(seed uint64, tm faultTiming) []faultSchedule {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	short := func() time.Duration { return lengthOf(rng, 2*time.Second, tm.Lease-tm.Heartbeat-6*time.Second) }
	make := func(kind faultKind) faultStep {
		step := faultStep{Kind: kind, Target: rng.IntN(1000)}
		switch kind {
		case faultPGShort, faultStopShort:
			step.Length = short()
		case faultPGLong:
			step.Length = lengthOf(rng, tm.Lease+2*time.Second, tm.Lease+6*time.Second)
		case faultStopLease:
			step.Length = lengthOf(rng, tm.Lease+2*time.Second, tm.Lease+4*time.Second)
			step.Waited = true
		case faultStopStall:
			step.Length = lengthOf(rng, tm.Stall+3*time.Second, tm.Stall+6*time.Second)
		case faultDelaySmall:
			step.Length = lengthOf(rng, 3*time.Second, 12*time.Second)
			step.RoundTrip = lengthOf(rng, 50*time.Millisecond, 301*time.Millisecond)
		case faultDelayLong:
			step.Length = lengthOf(rng, 12*time.Second, 18*time.Second)
			step.RoundTrip = lengthOf(rng, tm.Lease/2+time.Second, tm.Lease/2+4*time.Second)
		case faultKill:
			step.Waited = rng.IntN(2) == 0
		}
		return step
	}
	extras := func(kinds []faultKind, n int) []faultKind {
		var drawn []faultKind
		for range n {
			drawn = append(drawn, kinds[rng.IntN(len(kinds))])
		}
		return drawn
	}
	// The steps each schedule must hold, then the ones drawn.
	first := append([]faultKind{faultKill, faultKill, faultPGShort, faultStopShort, faultDelaySmall, faultViewerStop},
		extras([]faultKind{faultKill, faultPGShort, faultStopShort, faultDelaySmall, faultViewerStop}, rng.IntN(2))...)
	second := append([]faultKind{faultPGLong, faultStopLease, faultStopStall, faultDelayLong, faultViewerStop},
		extras([]faultKind{faultKill, faultPGShort, faultStopShort, faultDelaySmall, faultViewerStop, faultPGLong}, rng.IntN(2))...)
	windows := []time.Duration{90 * time.Second, 110 * time.Second}
	var schedules []faultSchedule
	for i, kinds := range [][]faultKind{first, second} {
		window := windows[i]
		var steps []faultStep
		clean := map[faultKind]bool{}
		for j, kind := range kinds {
			step := make(kind)
			switch {
			case i == 0 && j == 1:
				// The second kill is the clean one, of the worker of a job a viewer waits on.
				step.Waited, step.Clean = true, true
			case i == 1 && kind == faultStopLease && !clean[kind]:
				// The stop past a lease is clean in the second schedule, so that the worker's stopping
				// its own ffmpeg is judged in every run, with no restart to cut it.
				step.Clean, clean[kind] = true, true
			case i == 0 && !clean[kind] && (kind == faultPGShort || kind == faultStopShort || kind == faultDelaySmall):
				// The first restart, stop and delay of the first schedule are clean too, so that what the
				// build must do for each of them alone is judged in every run.
				step.Clean, clean[kind] = true, true
			}
			steps = append(steps, step)
		}
		var placed []faultStep
		fits := func(step faultStep, bounded bool) bool {
			if bounded && earlier(window, step.At+step.Length+3*time.Second) {
				return false
			}
			for _, other := range placed {
				margin := time.Duration(0)
				if other.Clean || step.Clean {
					margin = 4 * time.Second
				}
				if step.overlaps(other, margin) || quarrels(step, other, tm) || quarrels(other, step, tm) {
					return false
				}
			}
			return true
		}
		// The clean steps first, then the rest around them.
		order := slices.Clone(steps)
		slices.SortStableFunc(order, func(a, b faultStep) int {
			if a.Clean != b.Clean {
				if a.Clean {
					return -1
				}
				return 1
			}
			return 0
		})
		for _, step := range order {
			placedAt := false
			for try := 0; try < 3000 && !placedAt; try++ {
				step.At = lengthOf(rng, 3*time.Second, window-3*time.Second-step.Length)
				if step.Clean {
					step.At = lengthOf(rng, 10*time.Second, window-25*time.Second)
					if step.Kind == faultKill {
						step.At = lengthOf(rng, 25*time.Second, window-25*time.Second)
					}
				}
				if fits(step, true) {
					placedAt = true
				}
			}
			if !placedAt {
				// No room by luck: the first moment where it fits, and the window grows to hold it.
				for step.At = 3 * time.Second; !fits(step, false) && earlier(step.At, time.Hour); step.At += time.Second {
				}
				window = max(window, step.At+step.Length+6*time.Second)
			}
			placed = append(placed, step)
		}
		slices.SortStableFunc(placed, func(a, b faultStep) int { return int(a.At - b.At) })
		schedules = append(schedules, faultSchedule{Steps: placed, Window: window})
	}
	return schedules
}

// faultSeed is the seed of a run: the one the environment names (the replay of a
// run, JELLYSCALE_TEST_FAULT_SEED), or else a new one.
func faultSeed(env string) (uint64, error) {
	if env != "" {
		return strconv.ParseUint(strings.TrimSpace(env), 10, 64)
	}
	var raw [8]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(raw[:]), nil
}

func describeSchedule(seed uint64, i int, s faultSchedule) string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d schedule %d (%v window, %d steps):", seed, i, s.Window, len(s.Steps))
	for k, step := range s.Steps {
		fmt.Fprintf(&b, "\n  step %d: %v", k, step)
	}
	return b.String()
}

// The steps are a function of the seed alone: built twice from one seed they are
// the same list, and from another seed they are not.
func TestFaultStepsAreAFunctionOfTheSeedAlone(t *testing.T) {
	for _, seed := range []uint64{1, 2, 99, 1 << 40} {
		a, b := planFaults(seed, faultClock), planFaults(seed, faultClock)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("seed %d planned twice gave two lists:\n%v\n%v", seed, a, b)
		}
	}
	if fmt.Sprint(planFaults(1, faultClock)) == fmt.Sprint(planFaults(2, faultClock)) {
		t.Error("seeds 1 and 2 planned the same steps")
	}
}

// With no seed given a run draws a new one; the one named by the environment is
// the one used.
func TestFaultSeedIsDrawnAnewUnlessTheEnvironmentNamesOne(t *testing.T) {
	a, err := faultSeed("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := faultSeed("")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("two runs with no seed drew %d both times", a)
	}
	if got, err := faultSeed("12345"); err != nil || got != 12345 {
		t.Errorf("the seed named by the environment: got %d, %v", got, err)
	}
	if _, err := faultSeed("not a number"); err == nil {
		t.Error("a seed that is not a number was accepted")
	}
}

// Every run holds each kind and each length of step, a kill and a stop past its
// lease of the worker of a job a viewer waits on, steps that overlap, and keeps a
// free worker: no two steps that take a worker away overlap.
func TestEveryPlannedRunHoldsEachKindAndLengthAndKeepsAWorkerFree(t *testing.T) {
	tm := faultClock
	for seed := uint64(0); seed < 300; seed++ {
		schedules := planFaults(seed, tm)
		var steps []faultStep
		for _, s := range schedules {
			if len(s.Steps) == 0 {
				t.Fatalf("seed %d: an empty schedule", seed)
			}
			for _, step := range s.Steps {
				if earlier(step.At+step.Length, 0) || !earlier(step.At+step.Length, s.Window+time.Second) {
					t.Errorf("seed %d: %v runs past the window of %v", seed, step, s.Window)
				}
			}
			steps = append(steps, s.Steps...)
		}
		for _, kind := range allFaultKinds {
			if !slices.ContainsFunc(steps, func(s faultStep) bool { return s.Kind == kind }) {
				t.Errorf("seed %d: no %s step", seed, kind)
			}
		}
		if !slices.ContainsFunc(steps, func(s faultStep) bool { return s.Kind == faultKill && s.Waited }) {
			t.Errorf("seed %d: no kill of the worker of a job a viewer waits on", seed)
		}
		if !slices.ContainsFunc(steps, func(s faultStep) bool { return s.Kind == faultStopLease && s.Waited }) {
			t.Errorf("seed %d: no stop past the lease of the worker of a job a viewer waits on", seed)
		}
		for _, s := range steps {
			switch s.Kind {
			case faultPGShort, faultStopShort:
				if !earlier(s.Length, tm.Lease-tm.Heartbeat) {
					t.Errorf("seed %d: %v is not shorter than a lease less a heartbeat", seed, s)
				}
			case faultPGLong, faultStopLease:
				if earlier(s.Length, tm.Lease+time.Second) {
					t.Errorf("seed %d: %v is not longer than a lease", seed, s)
				}
			case faultStopStall:
				if earlier(s.Length, tm.Stall+time.Second) {
					t.Errorf("seed %d: %v is not longer than the stall limit", seed, s)
				}
			case faultDelaySmall:
				if s.RoundTrip <= 0 || earlier(300*time.Millisecond, s.RoundTrip) {
					t.Errorf("seed %d: %v is not a round trip of up to 300 ms", seed, s)
				}
			case faultDelayLong:
				if earlier(s.RoundTrip, tm.Lease/2) {
					t.Errorf("seed %d: %v does not leave the check before a put unanswered", seed, s)
				}
			}
		}
		for _, sched := range schedules {
			// The check of a stop past a lease is never cut by a restart of Postgres, in any seed.
			for i, st := range sched.Steps {
				if st.Kind == faultStopLease && (!st.Clean || sched.hit(i, 5*time.Second, func(o faultStep) bool { return o.isPostgres() })) {
					t.Errorf("seed %d: %v is cut by a restart of Postgres, so its check is not judged", seed, st)
				}
			}
			for i, a := range sched.Steps {
				for _, b := range sched.Steps[i+1:] {
					if a.overlaps(b, 0) {
						t.Errorf("seed %d: %v and %v take two workers away at once", seed, a, b)
					}
				}
			}
			// Each clean step has its surroundings quiet.
			for i, c := range sched.Steps {
				for j, p := range sched.Steps {
					if i != j && quarrels(c, p, tm) {
						t.Errorf("seed %d: %v and %v fall together, and the first is clean", seed, c, p)
					}
				}
			}
		}
	}
}

// earlier is a < b for durations and times alike.
func earlier[T time.Duration | time.Time](a, b T) bool {
	switch x := any(a).(type) {
	case time.Duration:
		return x.Nanoseconds() < any(b).(time.Duration).Nanoseconds()
	case time.Time:
		return x.Before(any(b).(time.Time))
	}
	return false
}

// A restart longer than a lease is judged job by job: the jobs of a worker that a kill, a stop
// or a delay hit are left out, and every other worker's are judged whatever else falls beside
// the restart, so the plan cannot cut the check away whole.
func TestARestartLongerThanALeaseJudgesEveryJobOfAWorkerNotHit(t *testing.T) {
	began := time.Now()
	hits := []workerHit{
		{"stopped", began.Add(5 * time.Second), began.Add(40 * time.Second)},
		{"killed", began.Add(-30 * time.Second), began.Add(24 * time.Hour)},
		{"delayed", began.Add(-30 * time.Second), began.Add(-10 * time.Second)},
	}
	now := began.Add(30 * time.Second)
	for worker, want := range map[string]bool{"stopped": false, "killed": false, "delayed": true, "untouched": true} {
		if got := judgedAfterRestart(hits, worker, began, now); got != want {
			t.Errorf("a job of worker %q judged: %v, want %v", worker, got, want)
		}
	}
}
