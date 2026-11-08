package stress

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests pin down how the control-plane stress run reads its numbers: the
// percentiles, the first degraded step, the loud failure when the harness could
// not keep the intended call rate, and the report. The package is the pure part
// of the run (internal/transcode/stress_integration_test.go drives the calls),
// so a wrong threshold or a quiet shortfall shows up in the unit tier.
// They reference names the task implies (Percentile, Op, Step, Degraded,
// RateShortfall, ErrRate, Format) and do not compile until the package exists.

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

// ok is an operation that kept its rate and was fast.
func ok(calls int) Op { return Op{Intended: calls, Calls: calls, P50: ms(1), P99: ms(5)} }

func step(viewers int, change func(ops map[string]Op)) Step {
	ops := map[string]Op{}
	for _, name := range []string{"Watch", "Segment", "beat", "Reap", "claim", "Unavailable", "Notify"} {
		ops[name] = ok(1000)
	}
	if change != nil {
		change(ops)
	}
	return Step{Viewers: viewers, Workers: viewers / 4, Ops: ops, PostgresCPU: 120, Connections: 40}
}

func TestPercentileIsNearestRank(t *testing.T) {
	var samples []time.Duration
	for i := 100; i >= 1; i-- { // Unsorted on purpose.
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	if got := Percentile(samples, 50); got != 50*time.Millisecond {
		t.Errorf("p50 of 1..100 ms: %v", got)
	}
	if got := Percentile(samples, 99); got != 99*time.Millisecond {
		t.Errorf("p99 of 1..100 ms: %v", got)
	}
	if got := Percentile(samples, 100); got != 100*time.Millisecond {
		t.Errorf("p100: %v", got)
	}
	if got := Percentile([]time.Duration{7 * time.Millisecond}, 99); got != 7*time.Millisecond {
		t.Errorf("one sample: %v", got)
	}
	if got := Percentile(nil, 99); got != 0 {
		t.Errorf("no samples: %v", got)
	}
	if samples[0] != 100*time.Millisecond {
		t.Error("Percentile sorted the caller's slice")
	}
}

func TestDegradedNamesTheFirstStepOverTheBudget(t *testing.T) {
	steps := []Step{
		step(50, nil),
		step(100, nil),
		step(200, func(o map[string]Op) { op := o["Watch"]; op.P99 = ms(50.1); o["Watch"] = op }),
		step(400, func(o map[string]Op) { op := o["beat"]; op.P99 = ms(900); o["beat"] = op }),
	}
	i, why := Degraded(steps)
	if i != 2 {
		t.Fatalf("first degraded step %d, want 2 (V=200): %s", i, why)
	}
	if !strings.Contains(why, "Watch") {
		t.Errorf("the reason does not name the operation: %q", why)
	}
}

func TestDegradedBudgetsAreInclusiveAndPerOperation(t *testing.T) {
	// Exactly on the budget is within it.
	at := step(50, func(o map[string]Op) {
		w, b, r := o["Watch"], o["beat"], o["Reap"]
		w.P99, b.P99, r.P99 = ms(50), ms(50), ms(250)
		o["Watch"], o["beat"], o["Reap"] = w, b, r
	})
	if i, why := Degraded([]Step{at}); i != -1 {
		t.Errorf("p99 exactly at the budget counted as degraded: %s", why)
	}
	// A reap is judged against 250 ms, not 50: 200 ms is fine for it.
	slowReap := step(50, func(o map[string]Op) { op := o["Reap"]; op.P99 = ms(200); o["Reap"] = op })
	if i, why := Degraded([]Step{slowReap}); i != -1 {
		t.Errorf("a reap at 200 ms counted as degraded: %s", why)
	}
	overReap := step(50, func(o map[string]Op) { op := o["Reap"]; op.P99 = ms(251); o["Reap"] = op })
	if i, why := Degraded([]Step{overReap}); i != 0 || !strings.Contains(why, "Reap") {
		t.Errorf("a reap at 251 ms: step %d, %q", i, why)
	}
	// The other operations have no budget: a slow Unavailable alone is reported
	// in the table but is not the ceiling.
	slowOther := step(50, func(o map[string]Op) { op := o["Unavailable"]; op.P99 = ms(900); o["Unavailable"] = op })
	if i, why := Degraded([]Step{slowOther}); i != -1 {
		t.Errorf("Unavailable has no budget but counted as degraded: %s", why)
	}
	if i, _ := Degraded(nil); i != -1 {
		t.Errorf("no steps: %d", i)
	}
}

func TestRateShortfallFailsWhenTheHarnessFellBehind(t *testing.T) {
	if err := RateShortfall(step(50, nil)); err != nil {
		t.Fatalf("a step that kept its rate: %v", err)
	}
	// 90% of the intended calls is the line: a harness that makes less than that
	// measured an easier load than the one it claims.
	edge := step(50, func(o map[string]Op) { op := o["Watch"]; op.Intended, op.Calls = 1000, 900; o["Watch"] = op })
	if err := RateShortfall(edge); err != nil {
		t.Errorf("90%% of the intended calls: %v", err)
	}
	behind := step(50, func(o map[string]Op) { op := o["Watch"]; op.Intended, op.Calls = 1000, 899; o["Watch"] = op })
	err := RateShortfall(behind)
	if !errors.Is(err, ErrRate) {
		t.Fatalf("89.9%% of the intended calls: %v", err)
	}
	if !strings.Contains(err.Error(), "Watch") {
		t.Errorf("the error does not name the operation: %v", err)
	}
	// Calls that failed are not calls that kept the rate.
	failing := step(50, func(o map[string]Op) {
		op := o["beat"]
		op.Intended, op.Calls, op.Errors = 1000, 1000, 500
		o["beat"] = op
	})
	if err := RateShortfall(failing); !errors.Is(err, ErrRate) {
		t.Errorf("half of the beats failed: %v", err)
	}
	// An operation nobody intended to call (no jobs yet) is not a shortfall.
	idle := step(50, func(o map[string]Op) { o["claim"] = Op{} })
	if err := RateShortfall(idle); err != nil {
		t.Errorf("an operation with nothing intended: %v", err)
	}
}

func TestFormatReportsEveryStepAndOperation(t *testing.T) {
	steps := []Step{step(50, nil), step(100, func(o map[string]Op) { op := o["Watch"]; op.P99 = ms(80); o["Watch"] = op })}
	out := Format(steps)
	for _, want := range []string{"V=50", "V=100", "W=12", "W=25", "p50", "p99", "Watch", "Segment", "beat", "Reap", "claim", "Unavailable", "Notify", "errors", "cpu", "conns"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "first degraded step: V=100") {
		t.Errorf("the report does not name the first degraded step:\n%s", out)
	}
	clean := Format([]Step{step(50, nil)})
	if !strings.Contains(clean, "no step degraded") {
		t.Errorf("a ladder that held says nothing of it:\n%s", clean)
	}
}
