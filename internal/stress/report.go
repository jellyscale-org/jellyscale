// Package stress reads the numbers of the control-plane stress run: percentiles,
// the first step that is over its budget, whether the harness kept the call
// rate it claims, and the report. The calls themselves are driven from package
// transcode (stress_driver_test.go), which owns the unexported code they need.
package stress

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrRate means the harness made fewer calls than it set out to, or they failed,
// so the numbers measure an easier load than the one they claim.
var ErrRate = errors.New("the stress run could not keep its call rate")

// Op is what one operation did during a step.
type Op struct {
	// Intended is the calls the schedule called for; Calls the ones that
	// finished, Errors the ones among them that failed.
	Intended, Calls, Errors int
	P50, P99                time.Duration
}

// Step is one rung of the ladder.
type Step struct {
	Viewers, Workers int
	Pool             int // The pgx pool size, or 0 if not recorded.
	Ops              map[string]Op
	PostgresCPU      float64 // Percent of one core, averaged over the step; negative when never measured.
	Connections      int     // The most Postgres connections seen.
}

// budgets are the p99 an operation may have before the primary is called
// saturated: Watch and beat are on the path of every request and lease; a reap
// has to finish well inside the second a dead worker's jobs are given.
var budgets = map[string]time.Duration{
	"Watch": 50 * time.Millisecond,
	"beat":  50 * time.Millisecond,
	"Reap":  250 * time.Millisecond,
}

// Percentile is the nearest-rank p-th percentile of samples, without sorting
// the caller's slice. No samples give 0.
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := int(p/100*float64(len(sorted)) + 0.999999999)
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// Degraded returns the index of the first step with an operation over its
// budget, and which. It returns -1 if none is.
func Degraded(steps []Step) (int, string) {
	for i, step := range steps {
		for _, name := range sortedNames(step.Ops) {
			if budget, ok := budgets[name]; ok && step.Ops[name].P99 > budget {
				return i, fmt.Sprintf("%s p99 %v is over its budget of %v", name, step.Ops[name].P99, budget)
			}
		}
	}
	return -1, ""
}

// RateShortfall returns ErrRate, naming the operation, if a step completed less
// than 90% of an operation's intended calls without error.
func RateShortfall(step Step) error {
	for _, name := range sortedNames(step.Ops) {
		op := step.Ops[name]
		if op.Intended > 0 && (op.Calls-op.Errors)*10 < op.Intended*9 {
			return fmt.Errorf("%w: V=%d, %s: %d of %d intended calls finished, %d of them with an error",
				ErrRate, step.Viewers, name, op.Calls, op.Intended, op.Errors)
		}
	}
	return nil
}

func sortedNames(ops map[string]Op) []string {
	names := make([]string, 0, len(ops))
	for name := range ops {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Format is the report: a table per step, then the first degraded one.
func Format(steps []Step) string {
	var out strings.Builder
	for _, step := range steps {
		fmt.Fprintf(&out, "V=%d W=%d", step.Viewers, step.Workers)
		if step.Pool > 0 {
			fmt.Fprintf(&out, " pool=%d", step.Pool)
		}
		// A sample that failed every time reads n/a, never as an idle database.
		cpu, conns := "n/a", "n/a"
		if step.PostgresCPU >= 0 {
			cpu = fmt.Sprintf("%.0f%%", step.PostgresCPU)
		}
		if step.Connections > 0 {
			conns = strconv.Itoa(step.Connections)
		}
		fmt.Fprintf(&out, " cpu=%s conns=%s\n", cpu, conns)
		fmt.Fprintf(&out, "  %-12s %9s %9s %7s %10s %10s\n", "operation", "calls", "intended", "errors", "p50", "p99")
		names := []string{"Watch", "Segment", "beat", "Reap", "claim", "Unavailable", "Notify"}
		for _, name := range sortedNames(step.Ops) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		for _, name := range names {
			op, ok := step.Ops[name]
			if !ok {
				continue
			}
			fmt.Fprintf(&out, "  %-12s %9d %9d %7d %10v %10v\n", name, op.Calls, op.Intended, op.Errors,
				op.P50.Round(10*time.Microsecond), op.P99.Round(10*time.Microsecond))
		}
	}
	if i, why := Degraded(steps); i >= 0 {
		fmt.Fprintf(&out, "first degraded step: V=%d (%s)\n", steps[i].Viewers, why)
	} else {
		fmt.Fprintf(&out, "no step degraded\n")
	}
	return out.String()
}
