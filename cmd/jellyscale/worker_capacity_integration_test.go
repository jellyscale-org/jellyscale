//go:build integration

package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// JS-161 (D-044 as amended by D-062): a worker's capacity is in the reference machine's cores, the unit of the price
// table. A worker that declares nothing counts each processor at the reference speed, with no floor and no benchmark at
// its start or beside its work, registers at once, and says in its log at start and on /metrics what it counts and that
// it is assumed; transcodeCores and transcodeSlots, when declared, are the capacity and are said to be declared; a
// capacity below the dearest row of the price table is said once per start. Real processes, as an operator runs them.
//
// Names the build adds (docs/running.md's metric table has them):
//   log "transcode capacity" at the worker's start, attributes cores (what admission counts) and source (assumed or declared)
//   log "transcode capacity below the dearest row": attributes cost (4.2) and cores (as before JS-161's second round)
//   jellyscale_transcode_cores (the capacity), jellyscale_transcode_capacity_assumed (1 assumed, 0 declared),
//   jellyscale_transcode_capacity_below_dearest_row (1 or 0)
// Gone: the log "transcode speed measured", jellyscale_transcode_worker_speed and
// jellyscale_transcode_speed_measurement_failures_total.
//
// The processors an undeclared worker counts are the ones its process may use: the tests set GOMAXPROCS in the
// environment of the worker they start, which the Go runtime reads, so the count is the same on any host.

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

// capacityLine waits for the worker's one line saying what it counts, and returns it.
func capacityLine(t *testing.T, p *proc) string {
	t.Helper()
	p.until(30*time.Second, "the worker to say what capacity it counts", func() bool { return len(p.recordsOf("transcode capacity")) > 0 })
	time.Sleep(2 * time.Second) // A second line would be here by now.
	lines := p.recordsOf("transcode capacity")
	if len(lines) != 1 {
		t.Fatalf("%d lines say what the worker counts, want one per start\n%s", len(lines), p.log)
	}
	return lines[0]
}

// shimFFmpeg is a PATH whose ffmpeg writes each run's arguments to the returned log and then does what script says.
func shimFFmpeg(t *testing.T, script string) (path, runs string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	runs = filepath.Join(dir, "runs")
	body := "#!/bin/sh\necho \"$*\" >> " + runs + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, runs
}

// workerWith starts a worker process with the config keys and PATH given, and returns it with its metrics address and
// the cluster that holds its row.
func workerWith(t *testing.T, env map[string]string, extra, path string) (*cluster, *proc, string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	c := newCluster(t, false, "60s") // It builds the binaries, so builds is read only after it.
	p, addr := startWorkerOn(t, c, builds[newVersion], extra, path)
	return c, p, addr
}

// startWorkerOn starts one more worker process on the cluster, as a restart does.
func startWorkerOn(t *testing.T, c *cluster, bin, extra, path string) (*proc, string) {
	t.Helper()
	addr := freeAddr(t)
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", "metricsListen": %q, %s
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-x", "transcodeIdle": "60s"}`, addr, extra, c.dir, c.dir)
	p := startProc(t, procSpec{bin: bin, dsn: c.dsn, args: []string{"--roles=worker"}, config: config,
		user: "admin", password: "r-pass", path: path, term: true})
	p.version, p.roles = newVersion, "worker"
	p.until(30*time.Second, "the worker to start", func() bool { return len(p.recordsOf("worker started")) > 0 })
	p.workerID, _ = field(p.recordsOf("worker started")[0], "worker")
	return p, addr
}

// A worker that declares nothing counts each processor it may use at the reference machine's speed, with no floor (3
// processors are 3 cores, not 3.2) and no benchmark: its row, its log at start and /metrics all give that number, and
// say it is assumed. The worker says so once per start.
func TestAnUndeclaredWorkerCountsEachProcessorAtTheReferenceSpeedAndSaysItIsAssumed(t *testing.T) {
	for _, procs := range []int{1, 3, 6} {
		t.Run(fmt.Sprint(procs, " processors"), func(t *testing.T) {
			c, p, addr := workerWith(t, map[string]string{"GOMAXPROCS": fmt.Sprint(procs)}, "", os.Getenv("PATH"))
			line := capacityLine(t, p)
			if cores := num(line, "cores"); cores != float64(procs) {
				t.Errorf("the line says %v cores for %d processors: %s", cores, procs, line)
			}
			if src, _ := field(line, "source"); src != "assumed" {
				t.Errorf("an undeclared worker's capacity is %q, want assumed: %s", src, line)
			}
			// What an operator of an overstated worker reads: how many processors were counted, and the setting that corrects it.
			if got := num(line, "processors"); got != float64(procs) {
				t.Errorf("an assumed line gives processors=%v, want %d: %s", got, procs, line)
			}
			if !strings.Contains(line, "transcodeCores") {
				t.Errorf("an assumed line does not name transcodeCores: %s", line)
			}
			if got := rowCores(c, p); got != float64(procs) {
				t.Errorf("the row has %v cores, want %d", got, procs)
			}
			text := metricsText(t, addr)
			if got := series(text, "jellyscale_transcode_cores"); got != float64(procs) {
				t.Errorf("jellyscale_transcode_cores = %v, want %d: the capacity admission counts", got, procs)
			}
			if got := series(text, "jellyscale_transcode_capacity_assumed"); got != 1 {
				t.Errorf("jellyscale_transcode_capacity_assumed = %v, want 1", got)
			}
		})
	}
}

// transcodeCores, and transcodeSlots when cores are not declared, are the capacity whatever the processors; the log and
// /metrics say it was declared, and /metrics shows as capacity what the row carries and admission counts.
func TestDeclaredCoresAndSlotsAreTheRowsCapacityAndSaidToBeDeclared(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  string
		wanted float64
	}{
		{"cores far above the processors", `"transcodeCores": 64,`, 64},
		{"cores far below the processors", `"transcodeCores": 0.5,`, 0.5},
		{"slots with no cores", `"transcodeSlots": 3,`, 3},
		{"cores win over slots", `"transcodeSlots": 5, "transcodeCores": 2.5,`, 2.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p, addr := workerWith(t, map[string]string{"GOMAXPROCS": "4"}, tc.extra, os.Getenv("PATH"))
			line := capacityLine(t, p)
			if src, _ := field(line, "source"); src != "declared" {
				t.Errorf("source=%q, want declared: %s", src, line)
			}
			if cores := num(line, "cores"); cores != tc.wanted {
				t.Errorf("the line says %v cores, want %v: %s", cores, tc.wanted, line)
			}
			// A declared capacity is the operator's own: the line gives no processors and tells nothing to change.
			if _, has := field(line, "processors"); has || strings.Contains(line, "transcodeCores") {
				t.Errorf("a declared line gives the processors or names transcodeCores: %s", line)
			}
			if got := rowCores(c, p); got != tc.wanted {
				t.Errorf("the row has %v cores, want the declared %v\n%s", got, tc.wanted, p.log)
			}
			text := metricsText(t, addr)
			if got := series(text, "jellyscale_transcode_cores"); got != tc.wanted {
				t.Errorf("jellyscale_transcode_cores = %v, want %v", got, tc.wanted)
			}
			if got := series(text, "jellyscale_transcode_capacity_assumed"); got != 0 {
				t.Errorf("jellyscale_transcode_capacity_assumed = %v, want 0 for a declared capacity", got)
			}
		})
	}
}

// No benchmark runs, at start or beside the worker's work, declared or not: the ffmpeg on its PATH is never run, so
// there is nothing for a busy host to move, and the series of the measurement are not on /metrics. The shim ffmpeg
// records each run; the worker is given ten seconds, which is longer than the start-up and its first sweeps need.
func TestNoBenchmarkRunsAtStartOrBesideTheWorksWhetherDeclaredOrNot(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{{"undeclared", ""}, {"declared", `"transcodeCores": 2,`}} {
		t.Run(tc.name, func(t *testing.T) {
			path, runs := shimFFmpeg(t, "exit 0")
			_, p, addr := workerWith(t, nil, tc.extra, path)
			time.Sleep(10 * time.Second)
			if raw, _ := os.ReadFile(runs); strings.Contains(string(raw), "lavfi") || strings.Contains(string(raw), "testsrc") {
				t.Errorf("the worker ran a synthetic benchmark:\n%s", raw)
			}
			text := metricsText(t, addr)
			for _, gone := range []string{"jellyscale_transcode_worker_speed", "jellyscale_transcode_speed_measurement_failures_total"} {
				if strings.Contains(text, gone) {
					t.Errorf("/metrics still has %s", gone)
				}
			}
			if len(p.recordsOf("transcode speed measured")) != 0 || len(p.recordsOf("transcode speed measurement failed")) != 0 {
				t.Errorf("the log still speaks of a speed measurement\n%s", p.log)
			}
		})
	}
}

// An undeclared worker registers at once: its row is there whatever its ffmpeg does, here an ffmpeg that never ends, which
// the benchmark of the build before waited for until its bound (15 seconds), so a replacement in a rolling restart
// claims the job a viewer is waiting on within the queue timeout. Ten seconds only bounds a hang: the row is written in
// well under one.
func TestAnUndeclaredWorkerRegistersAtOnceWhateverItsFFmpegDoes(t *testing.T) {
	path, _ := shimFFmpeg(t, "exec /bin/sleep 600")
	c, p, _ := workerWith(t, map[string]string{"GOMAXPROCS": "4"}, "", path)
	p.until(10*time.Second, "the worker's row to carry its capacity", func() bool { return rowCores(c, p) == 4 })
}

// A worker whose capacity is below the dearest row (4.2, the AV1 fallback of a 4K HDR source) says so once per start,
// in the log, naming the row's cost and the capacity it counts, and on /metrics; a worker at or above it says nothing.
// Judged on the counted capacity, declared or assumed (D-044, D-062); a default worker is no longer floored to the dearest job.
func TestACapacityBelowTheDearestRowIsSaidOncePerStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		procs string
		cores float64
		below bool
	}{
		{"declared 3", `"transcodeCores": 3,`, "8", 3, true},
		{"declared 4.2", `"transcodeCores": 4.2,`, "8", 4.2, false},
		{"declared 16", `"transcodeCores": 16,`, "2", 16, false},
		{"assumed from 3 processors", "", "3", 3, true},
		{"assumed from 8 processors", "", "8", 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, addr := workerWith(t, map[string]string{"GOMAXPROCS": tc.procs}, tc.extra, os.Getenv("PATH"))
			capacityLine(t, p)
			lines := p.recordsOf("transcode capacity below the dearest row")
			gauge := series(metricsText(t, addr), "jellyscale_transcode_capacity_below_dearest_row")
			if !tc.below {
				if len(lines) != 0 || gauge == 1 {
					t.Errorf("a worker counting %v cores says it is below the dearest row: %d lines, gauge %v", tc.cores, len(lines), gauge)
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

// allowedCPUs are the processors this test process may use, as the kernel says (taskset -cp), so that a test pins its
// worker to some of them on a host of any size.
func allowedCPUs(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("taskset", "-cp", fmt.Sprint(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	list := strings.TrimSpace(string(out))
	list = list[strings.LastIndex(list, ":")+1:]
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(list), ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, _ := strconv.Atoi(lo)
		b := a
		if isRange {
			b, _ = strconv.Atoi(hi)
		}
		for c := a; c <= b; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus
}

// A worker that declares nothing and is pinned with taskset counts exactly the processors it may use, however many the
// host has: pinned to 3 CPUs it counts 3, to 2 it counts 2, never the host's processor count (D-062). Unlike the tests
// above, nothing sets GOMAXPROCS: the number comes from the process's affinity as an operator's taskset or cgroup makes it.
func TestAnUndeclaredWorkerPinnedWithTasksetCountsThePinnedProcessors(t *testing.T) {
	cpus := allowedCPUs(t)
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprint("pinned to ", n), func(t *testing.T) {
			if len(cpus) <= n {
				t.Fatalf("pinning to %d of the %d CPUs this test may use shows nothing: run it on a host with more than %d", n, len(cpus), n)
			}
			var pin []string
			for _, c := range cpus[:n] {
				pin = append(pin, fmt.Sprint(c))
			}
			c := newCluster(t, false, "60s") // It builds the binaries, so builds is read only after it.
			wrapper := filepath.Join(t.TempDir(), "jellyscale")
			script := "#!/bin/sh\nexec taskset -c " + strings.Join(pin, ",") + " " + builds[newVersion] + " \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			p, addr := startWorkerOn(t, c, wrapper, "", os.Getenv("PATH"))
			line := capacityLine(t, p)
			if cores := num(line, "cores"); cores != float64(n) {
				t.Errorf("pinned to %d CPUs the worker says %v cores: %s", n, cores, line)
			}
			if got := rowCores(c, p); got != float64(n) {
				t.Errorf("the row has %v cores, want %d", got, n)
			}
			if got := series(metricsText(t, addr), "jellyscale_transcode_cores"); got != float64(n) {
				t.Errorf("jellyscale_transcode_cores = %v, want %d", got, n)
			}
		})
	}
}

// transcodeCores counted as written, fractional: 2.7 is 2.7, not 3 or 4, in the row, the log and /metrics (D-062).
func TestFractionalTranscodeCoresAreCountedAsWritten(t *testing.T) {
	c, p, addr := workerWith(t, map[string]string{"GOMAXPROCS": "4"}, `"transcodeCores": 2.7,`, os.Getenv("PATH"))
	line := capacityLine(t, p)
	if cores := num(line, "cores"); cores != 2.7 {
		t.Errorf("the line says %v cores, want 2.7: %s", cores, line)
	}
	if got := rowCores(c, p); got != 2.7 {
		t.Errorf("the row has %v cores, want 2.7", got)
	}
	if got := series(metricsText(t, addr), "jellyscale_transcode_cores"); got != 2.7 {
		t.Errorf("jellyscale_transcode_cores = %v, want 2.7", got)
	}
}

// A worker restarted with another declaration, or with none, shows the new figure in its row and on /metrics, and the
// gauge that says assumed follows (D-062): the restart of a worker is the operator's way to change its capacity.
func TestARestartedWorkerShowsTheNewCapacityOnItsRowAndOnMetrics(t *testing.T) {
	t.Setenv("GOMAXPROCS", "3")
	c := newCluster(t, false, "60s")
	for _, step := range []struct {
		extra   string
		cores   float64
		assumed float64
	}{{`"transcodeCores": 2,`, 2, 0}, {`"transcodeCores": 5.5,`, 5.5, 0}, {"", 3, 1}, {`"transcodeSlots": 4,`, 4, 0}} {
		p, addr := startWorkerOn(t, c, builds[newVersion], step.extra, os.Getenv("PATH"))
		capacityLine(t, p)
		if got := rowCores(c, p); got != step.cores {
			t.Errorf("%q: the row has %v cores, want %v", step.extra, got, step.cores)
		}
		text := metricsText(t, addr)
		if got := series(text, "jellyscale_transcode_cores"); got != step.cores {
			t.Errorf("%q: jellyscale_transcode_cores = %v, want %v", step.extra, got, step.cores)
		}
		if got := series(text, "jellyscale_transcode_capacity_assumed"); got != step.assumed {
			t.Errorf("%q: jellyscale_transcode_capacity_assumed = %v, want %v", step.extra, got, step.assumed)
		}
		p.stop()
	}
}
