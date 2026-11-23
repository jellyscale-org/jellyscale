//go:build integration

package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// JS-161 (D-044): a worker measures its own speed against the reference machine at each start, with nothing declared
// by the operator; the result is logged once per start and shown on /metrics; a measurement that fails is logged with
// its reason and shown on /metrics, and never falls back silently to the processor count; transcodeCores and
// transcodeSlots, when declared, are the capacity whatever the measurement says; a capacity below the dearest row of
// the price table is said once per start. Real processes, as an operator runs them.
//
// Names the build adds (docs/running.md's metric table has them, and a docs check holds the table to these):
//   log "transcode speed measured" (logfmt, as the process writes it): attributes speed (per processor, 1 is the reference) and cores (the capacity)
//   log "transcode speed measurement failed": attribute reason
//   log "transcode capacity below the dearest row": attributes cost (4.2) and cores
//   jellyscale_transcode_worker_speed, jellyscale_transcode_cores (the capacity), jellyscale_transcode_speed_measurement_failures_total{reason},
//   jellyscale_transcode_capacity_below_dearest_row (1 or 0)
// The bound of the measurement is the number of seconds docs/running.md states in "measurement ... within N seconds".
//
// Not checked here, for want of the reference machine: that a worker at the reference speed counts its processors
// within the documented tolerance (the unit checks hold the arithmetic; the figure is measured by the build on the
// reference host and recorded in docs/transcoding.md).

var measurementBound = regexp.MustCompile(`measurement[^.]*within (\d+) seconds`)

func docBound(t *testing.T) time.Duration {
	t.Helper()
	body, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	m := measurementBound.FindStringSubmatch(strings.ToLower(strings.Join(strings.Fields(string(body)), " ")))
	if m == nil {
		t.Fatal("docs/running.md states no bound for the speed measurement (\"measurement ... within N seconds\")")
	}
	n, _ := strconv.Atoi(m[1])
	return time.Duration(n) * time.Second
}

func metricsText(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// series is the value of the first line that starts with the series, or NaN.
func series(text, name string) float64 {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, name) && !strings.HasPrefix(line, "#") {
			if f := strings.Fields(line); len(f) == 2 {
				if v, err := strconv.ParseFloat(f[1], 64); err == nil {
					return v
				}
			}
		}
	}
	return math.NaN()
}

// recordsOf are the process's log lines (logfmt) whose message is msg.
func (p *proc) recordsOf(msg string) []string { return logLines(p, msg) }

// num is a number attribute of a line, or NaN.
func num(line, key string) float64 {
	v, _ := field(line, key)
	f, err := strconv.ParseFloat(strings.Trim(v, `"`), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func rowCores(c *cluster, p *proc) float64 {
	var cores float64
	c.pool.QueryRow(c.t.Context(), "SELECT cores::float8 FROM transcode_nodes WHERE id = $1", p.workerID).Scan(&cores) //nolint:errcheck
	return cores
}

// A worker with nothing declared measures its speed at each of its starts, within the documented bound from the
// process's start to its capacity in its row; says so once, with the speed and the capacity; and shows both on
// /metrics. The capacity is a count of reference cores, not the processor count floored to 3.2.
func TestAWorkerMeasuresItsSpeedAtEachStartAndSaysSoOnce(t *testing.T) {
	bound := docBound(t)
	for start := range 3 {
		t.Run(fmt.Sprintf("start %d", start), func(t *testing.T) {
			c := newCluster(t, false, "60s")
			addr := freeAddr(t)
			c.extra = fmt.Sprintf(`"metricsListen": %q,`, addr)
			p := c.launch(newVersion, "worker")
			p.until(bound+10*time.Second, "the speed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measured")) > 0 })
			lines := p.recordsOf("transcode speed measured")
			if len(lines) == 0 {
				t.Fatal("no line")
			}
			want := num(lines[0], "cores")
			c.waitReady(p)
			// The capacity is in the row within the bound, timed from the process's start.
			p.until(5*time.Second, "the measured capacity to reach the worker's row", func() bool { return math.Abs(rowCores(c, p)-want) < 1e-6 })
			if took := time.Since(p.begun); took > bound+5*time.Second { // 5 s: the test's own polling and the process's start-up.
				t.Errorf("the capacity reached the row %v after the process started, the documented bound is %v", took, bound)
			}
			time.Sleep(2 * time.Second)
			lines = p.recordsOf("transcode speed measured")
			if len(lines) != 1 {
				t.Fatalf("%d lines say the speed was measured, want one per start\n%s", len(lines), p.log)
			}
			speed, cores := num(lines[0], "speed"), num(lines[0], "cores")
			if !(speed > 0) || !(cores > 0) {
				t.Fatalf("the line gives speed %v and cores %v: %s", speed, cores, lines[0])
			}
			if got := rowCores(c, p); math.Abs(got-cores) > 1e-6 {
				t.Errorf("the row has %v cores, the line says %v", got, cores)
			}
			text := metricsText(t, addr)
			if got := series(text, "jellyscale_transcode_worker_speed"); math.Abs(got-speed) > 1e-6 {
				t.Errorf("jellyscale_transcode_worker_speed = %v, the line says %v", got, speed)
			}
			if got := series(text, "jellyscale_transcode_cores"); math.Abs(got-cores) > 1e-6 {
				t.Errorf("jellyscale_transcode_cores = %v, the line says %v", got, cores)
			}
		})
	}
}

// transcodeCores, when declared, is the capacity whatever the measurement says; so is transcodeSlots when cores are
// not declared. The measurement still runs and is still reported.
func TestDeclaredCoresAndSlotsAreTheRowsCapacityWhateverTheWorkerMeasured(t *testing.T) {
	for _, tc := range []struct {
		name   string
		slots  int
		extra  string
		wanted float64
	}{
		{"cores far above any measurement", 0, `"transcodeCores": 64,`, 64},
		{"cores far below any measurement", 0, `"transcodeCores": 0.5,`, 0.5},
		{"slots with no cores", 3, "", 3},
		{"cores win over slots", 5, `"transcodeCores": 2.5,`, 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, false, "60s")
			addr := freeAddr(t)
			c.slots, c.extra = tc.slots, tc.extra+fmt.Sprintf(`"metricsListen": %q,`, addr)
			p := c.start(newVersion, "worker")
			p.until(docBound(t)+10*time.Second, "the speed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measured")) > 0 })
			if got := rowCores(c, p); got != tc.wanted {
				t.Errorf("the row has %v cores, want the declared %v\n%s", got, tc.wanted, p.log)
			}
			if got := series(metricsText(t, addr), "jellyscale_transcode_cores"); got != tc.wanted {
				t.Errorf("jellyscale_transcode_cores = %v, want %v", got, tc.wanted)
			}
		})
	}
}

// A measurement that fails (no ffmpeg on the worker's PATH) is logged with its reason and shown on /metrics, and the
// worker does not fall back silently to its processor count: the row says what docs/running.md says a failed
// measurement counts as, which is not the 3.2 floor either way (a test of the doc's words is in internal/transcode).
func TestAFailedMeasurementIsLoggedWithItsReasonAndShownOnMetrics(t *testing.T) {
	c := newCluster(t, false, "60s")
	addr := freeAddr(t)
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", "metricsListen": %q,
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-x", "transcodeIdle": "60s"}`, addr, c.dir, c.dir)
	p := startProc(t, procSpec{bin: builds[newVersion], dsn: c.dsn, args: []string{"--roles=worker"}, config: config,
		user: "admin", password: "r-pass", path: filepath.Join(c.dir, "nobin"), term: true})
	p.until(docBound(t)+10*time.Second, "the failed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measurement failed")) > 0 })
	lines := p.recordsOf("transcode speed measurement failed")
	if len(lines) != 1 {
		t.Fatalf("%d lines say the measurement failed, want one\n%s", len(lines), p.log)
	}
	if reason, _ := field(lines[0], "reason"); strings.Trim(reason, `"`) == "" {
		t.Errorf("the line gives no reason: %s", lines[0])
	}
	if !strings.Contains(lines[0], "level=WARN") && !strings.Contains(lines[0], "level=ERROR") {
		t.Errorf("the failure is not logged at Warn or Error: %s", lines[0])
	}
	text := metricsText(t, addr)
	if !regexp.MustCompile(`(?m)^jellyscale_transcode_speed_measurement_failures_total\{reason="[^"]+"\} 1$`).MatchString(text) {
		t.Errorf("/metrics does not show the failure once under its reason:\n%s", text)
	}
	if got := series(text, "jellyscale_transcode_cores"); !(got > 0) {
		t.Errorf("jellyscale_transcode_cores = %v: a worker that failed to measure counts as something", got)
	}
}

// A worker whose capacity is below the dearest row (4.2, the AV1 fallback of a 4K HDR source) says so once per start,
// in the log, naming the row's cost and its own capacity, and on /metrics; a worker at or above it says nothing.
func TestACapacityBelowTheDearestRowIsSaidOncePerStart(t *testing.T) {
	for _, tc := range []struct {
		cores float64
		below bool
	}{{3, true}, {4.2, false}, {16, false}} {
		t.Run(fmt.Sprint(tc.cores, " cores"), func(t *testing.T) {
			c := newCluster(t, false, "60s")
			addr := freeAddr(t)
			c.extra = fmt.Sprintf(`"transcodeCores": %v, "metricsListen": %q,`, tc.cores, addr)
			p := c.start(newVersion, "worker")
			p.until(docBound(t)+10*time.Second, "the speed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measured")) > 0 })
			time.Sleep(2 * time.Second)
			lines := p.recordsOf("transcode capacity below the dearest row")
			gauge := series(metricsText(t, addr), "jellyscale_transcode_capacity_below_dearest_row")
			if !tc.below {
				if len(lines) != 0 || gauge == 1 {
					t.Errorf("a worker with %v cores says it is below the dearest row: %d lines, gauge %v", tc.cores, len(lines), gauge)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("%d lines, want one\n%s", len(lines), p.log)
			}
			if cost := num(lines[0], "cost"); math.Abs(cost-4.2) > 1e-9 {
				t.Errorf("the line names cost %v, want 4.2: %s", cost, lines[0])
			}
			if cores := num(lines[0], "cores"); math.Abs(cores-tc.cores) > 1e-9 {
				t.Errorf("the line names cores %v, want %v: %s", cores, tc.cores, lines[0])
			}
			if gauge != 1 {
				t.Errorf("jellyscale_transcode_capacity_below_dearest_row = %v, want 1", gauge)
			}
		})
	}
}
