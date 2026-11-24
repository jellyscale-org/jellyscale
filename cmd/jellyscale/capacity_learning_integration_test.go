//go:build integration

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// JS-177 (D-064, D-066, D-070): a worker learns what it holds from the jobs it runs, with real processes and the kernel's
// own figures. The estimator's arithmetic is internal/transcode/capacity_learn_test.go and the worker's wiring, with the
// kernel's figures injected, is capacity_learn_integration_test.go there; what only a real process can show is here: that
// a worker under a container CPU limit counts it as written, and that competing processes lower the figure by the kernel's
// own scheduler accounting, at the production heartbeat and in the time the design states. The negatives (a slow read, a
// slow upload, a pause, a burst) are on the injected kernel in internal/transcode, where the guarantee is the same and no busy
// loop shares the machine with other tests.
//
// The names the build adds, besides those listed in those files: the log "transcode capacity moved" (attributes before,
// after, source), jellyscale_transcode_capacity_source{source="assumed"|"learned"|"ceiling"} on /metrics (1 for the source in
// force), and the source word "ceiling" in the line "transcode capacity" at start for a worker held below its processors by
// transcodeCores.
//
// Competing load is made with real processes: `yes` pinned to the worker's CPUs. The worker itself runs at nice 19, so that
// whatever the machine's speed the hogs take nearly all of the CPU the worker's ffmpeg wants, and its job falls far under
// real time on a machine of any speed (nothing here asserts how slow). The worker is pinned with taskset to at most four of
// the CPUs this test may use, as the criterion's 4 CPUs: pinned, it counts 4 processors and starts at 4.

// cpusToPin is the CPUs a test pins its worker to: up to four of those it may use, and never fewer than two.
func cpusToPin(t *testing.T) []int {
	t.Helper()
	cpus := allowedCPUs(t)
	if len(cpus) < 2 {
		t.Fatalf("this test needs at least 2 CPUs to pin a worker to and compete with it on; this process may use %d", len(cpus))
	}
	return cpus[:min(4, len(cpus))]
}

func cpuList(cpus []int) string {
	var list []string
	for _, c := range cpus {
		list = append(list, strconv.Itoa(c))
	}
	return strings.Join(list, ",")
}

// startHogs runs perCPU busy loops on each CPU, pinned there, until stop is called or the test ends.
func startHogs(t *testing.T, cpus []int, perCPU int) (stop func()) {
	t.Helper()
	var hogs []*exec.Cmd
	for _, cpu := range cpus {
		for range perCPU {
			hog := exec.Command("taskset", "-c", strconv.Itoa(cpu), "sh", "-c", "while :; do :; done")
			if err := hog.Start(); err != nil {
				t.Fatal(err)
			}
			hogs = append(hogs, hog)
		}
	}
	var stopped bool
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		for _, hog := range hogs {
			hog.Process.Kill() //nolint:errcheck
			hog.Wait()         //nolint:errcheck
		}
	}
	t.Cleanup(stop)
	return stop
}

// wrapper is a script that runs the build with prefix in front of it, as the taskset wrapper of the pinned-worker check does.
func wrapper(t *testing.T, prefix string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jellyscale")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec "+prefix+" "+builds[newVersion]+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// startWrapped starts a worker of the build behind the wrapper, with its metrics on an address of its own, and returns it
// with that address once it is registered.
func (c *cluster) startWrapped(bin string) (*proc, string) {
	c.t.Helper()
	addr := freeAddr(c.t)
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", "metricsListen": %q,
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-w", "transcodeIdle": "60s", "transcodeLead": %q,
 "libraries": [{"name": "Movies", "kind": "movies", "media": "file://%s/media", "assets": "file://%s/assets"}]}`,
		addr, c.dir, c.dir, c.lead, c.dir, c.dir)
	p := startProc(c.t, procSpec{bin: bin, dsn: c.dsn, args: []string{"--roles=worker"}, config: config,
		user: "admin", password: "r-pass", path: filepath.Join(c.dir, "bin") + ":" + os.Getenv("PATH"), term: true})
	p.version, p.roles = newVersion, "worker"
	c.procs = append(c.procs, p)
	c.waitReady(p)
	return p, addr
}

// A worker in a container with a CPU limit of 3.5 starts at 3.5, as written, not at 4, which is what the Go runtime
// counts (it rounds the limit up): the row, the log and /metrics all say 3.5, and the source is assumed. A worker that
// rounds a quota up overstates itself by the half CPU the operator did not give it. Docker is the test's container
// runtime, as it is for Postgres; the container is the Ubuntu 24.04 image test/ffmpeg61.sh uses, and the build runs in it
// as the user running the test, over the test's own directories.
func TestAWorkerUnderAContainerCPULimitOfThreeAndAHalfStartsAtThreeAndAHalf(t *testing.T) {
	c := newCluster(t, false, "60s")
	name := fmt.Sprintf("js177-limit-%d", os.Getpid())
	t.Cleanup(func() { exec.Command("docker", "rm", "-fv", name).Run() }) //nolint:errcheck
	tmp := os.TempDir()
	bin := filepath.Join(t.TempDir(), "jellyscale")
	script := fmt.Sprintf("#!/bin/sh\nexec docker run --rm -i --init --name %s --network host --cpus 3.5 --user %d:%d -e JELLYSCALE_DATABASE_URL -e JELLYSCALE_ADMIN_PASSWORD -v %s:%s ubuntu:24.04 %s \"$@\"\n",
		name, os.Getuid(), os.Getgid(), tmp, tmp, builds[newVersion])
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p, addr := c.startWrapped(bin)
	line := capacityLine(t, p)
	if cores := num(line, "cores"); cores != 3.5 {
		t.Errorf("a worker under a CPU limit of 3.5 says %v cores: %s", cores, line)
	}
	if src, _ := field(line, "source"); src != "assumed" {
		t.Errorf("source %q, want assumed: %s", src, line)
	}
	if got := rowCores(c, p); got != 3.5 {
		t.Errorf("the row has %v cores, want 3.5", got)
	}
	if got := series(metricsText(t, addr), "jellyscale_transcode_cores"); got != 3.5 {
		t.Errorf("jellyscale_transcode_cores = %v, want 3.5", got)
	}
}

// startPlay is a viewer of the film through an api and stream replica, who asks for segment after segment as it comes.
func (c *cluster) startPlay(api *proc) (*playing, func()) {
	c.t.Helper()
	p := c.startPlayback(api, api)
	return p, c.keepPlaying(p)
}

// moved are the lines that say the worker's capacity moved.
func moved(p *proc) []string { return p.recordsOf("transcode capacity moved") }

// Criteria 6 and 14, with competing processes, at the production heartbeat and lease: a worker pinned to 4 CPUs whose CPUs
// are taken by busy loops runs its job under 1.2x with its processors wanted, which the kernel's scheduler accounting shows.
// Its figure falls from 4, never below a quarter of it; the job it is running goes on (the same row, still running,
// nothing cancelled); the row, /metrics and the log say what the figure is and that it was learned.
//
// The time it takes is asserted, because the design states it (docs/transcoding.md, section 4): the job's first counting
// sample is 15 s after its first output and the seventh 18 s later, so the figure cannot move less than 30 s after the claim
// (a lower bound that no machine breaks), and it moves within 45 s, which the check bounds at 120 s, 2.7 times that: a
// sampler on a 30 s ticker needs 210 s, and one on the heartbeat 35 to 40 s. The 120 s is the least multiple that still
// fails a 30 s ticker, and it is a bound on a hang, not a bar: a closer one (1.5 times, 67 s) would be a duration measured on
// the machine that decides (docs/testing.md), and a runner whose heartbeats are late would break it; a ticker of 10 s (about
// 85 s) passes it, and is the cadence check's in internal/transcode, by mechanism. The 30 s lower bound has no such risk: no
// machine samples before the 15 s a job waits and the seven samples after it. The claim is the first moment the row is seen
// running on the worker, polled every 100 ms.
//
// This is the one test that makes real contention (and the container test the one that reads a real cgroup): it pins busy
// loops to the CPUs that other tests of its shard use. It belongs in test/alone.txt, which runs it serially after the shard's
// other processes have ended: about a minute of one shard's job that nothing else shares.
//
// The contention is tuned so that the job is slow on any host and never starved: only the worker's ffmpeg is niced (10, by a
// shim on its PATH: the worker's own heartbeats must not starve), the worker is pinned to up to four of the CPUs this process
// may use, and n+1 busy loops at nice 0 run on each of the n CPUs (3 on 2 CPUs, 5 on 4). With the loops per CPU growing with
// the CPUs the job's total share is about the same on both, which a constant count does not give (the job ran 2 times faster
// on 4 CPUs under the same loops per CPU). At nice 19 against 3 loops a CPU ffmpeg made no segment at all and the 30 s stall
// check failed the job; at nice 5 or 8 it ran at 1.0x or faster and the figure never fell. Measured on this machine (16
// CPUs, ffmpeg 9.0.2, the test run under taskset), claim to first segment, steady speed, claim to the figure's move:
//
//	2 CPUs (taskset -c 0,1), 3 loops a CPU: 10.8 s, 8.3 s, 8.7 s; 0.60x in each; 35.5 s in each (three runs)
//	4 CPUs (taskset -c 0-3), 5 loops a CPU: 8.8 s; 0.75x; 35.4 s (one run; 4 loops: 7.4 s, 0.86x; 6 loops: 11.1 s, 0.63x)
//
// The job went on every time, making a segment every 5 to 9 s at the longest (never near the 30 s stall), and the figure fell
// 35 to 39 s after the claim: after the 15 s a job waits and seven samples.
func TestAWorkerStarvedByCompetingProcessesLowersItsFigureAndEndsNoJob(t *testing.T) {
	cpus := cpusToPin(t)
	start := float64(len(cpus))
	niceFFmpeg, hogsPerCPU := 10, len(cpus)+1
	c := newCluster(t, false, "3600s")
	// Only the worker's ffmpeg is niced: the worker's own heartbeats must not starve, and a stall is the job's, not the process's.
	realFFmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	shim := fmt.Sprintf("#!/bin/sh\nexec nice -n %d %s \"$@\"\n", niceFFmpeg, realFFmpeg)
	if err := os.WriteFile(filepath.Join(c.dir, "bin", "ffmpeg"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	worker, addr := c.startWrapped(wrapper(t, "taskset -c "+cpuList(cpus)))
	api := c.start(newVersion, "api,stream")
	if got := rowCores(c, worker); got != start {
		t.Fatalf("the worker pinned to %d CPUs registered %v cores, want %v", len(cpus), got, start)
	}
	startHogs(t, cpus, hogsPerCPU)
	p, stop := c.startPlay(api)
	defer stop()
	var job row
	var claimed time.Time
	waitUntil(t, "the viewer's job to be claimed", func() bool {
		var ok bool
		if job, ok = c.rowOf(p); ok && job.Worker == worker.workerID && job.State == "running" {
			claimed = time.Now()
			return true
		}
		return false
	})
	// What the job did while it waited: when it made its first segment, the longest it went without one, its speed.
	var firstSegment, lastSegment, longestGap time.Duration
	segments, speed := 0, 0.0
	deadline := claimed.Add(120 * time.Second)
	for rowCores(c, worker) >= 0.9*start {
		if time.Now().After(deadline) {
			t.Fatalf("the figure had not fallen 120 s after the job was claimed (the design says about 45 s); %d segments, first at %v, longest gap %v, speed %.2f\n%s", segments, firstSegment, longestGap, speed, worker.log)
		}
		var next int
		var encodeSpeed float64
		if err := c.pool.QueryRow(t.Context(), "SELECT next_segment, COALESCE(encode_speed, 0) FROM transcode_jobs WHERE id = $1", job.ID).Scan(&next, &encodeSpeed); err == nil {
			if now := time.Since(claimed); next > segments {
				if segments == 0 {
					firstSegment = now
				}
				longestGap = max(longestGap, now-lastSegment)
				segments, lastSegment = next, now
			}
			if encodeSpeed > 0 {
				speed = encodeSpeed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	took := time.Since(claimed)
	t.Logf("nice %d, %d hogs per CPU on %d CPUs: the figure fell %v after the claim; first segment %v, %d segments, longest gap %v, speed %.2fx", niceFFmpeg, hogsPerCPU, len(cpus), took, firstSegment, segments, longestGap, speed)
	if took < 30*time.Second {
		t.Errorf("the figure fell %v after the job was claimed: its first counting sample is 15 s after its first output and the seventh 18 s later", took)
	}
	got := rowCores(c, worker)
	if got < start/4-1e-9 {
		t.Errorf("the figure fell to %v, below the floor of a quarter of its start, %v", got, start/4)
	}
	// Lowering the figure ends no running job.
	after, ok := c.rowOf(p)
	if !ok || after.ID != job.ID || after.State != "running" {
		t.Errorf("the viewer's job was %+v and is %+v", job, after)
	}
	for _, j := range c.rows() {
		if j.State == "cancelled" || j.State == "failed" {
			t.Errorf("job %d is %s after the worker's figure was lowered", j.ID, j.State)
		}
	}
	text := metricsText(t, addr)
	if series(text, "jellyscale_transcode_cores") != rowCores(c, worker) || series(text, `jellyscale_transcode_capacity_source{source="learned"}`) != 1 {
		t.Errorf("/metrics does not give the row's %v cores learned: cores %v, learned %v", rowCores(c, worker),
			series(text, "jellyscale_transcode_cores"), series(text, `jellyscale_transcode_capacity_source{source="learned"}`))
	}
	if lines := moved(worker); len(lines) != 1 || !strings.Contains(lines[0], "before="+strconv.FormatFloat(start, 'f', -1, 64)) {
		t.Errorf("%d lines say the capacity moved, want one with the figure before (%v) and after: %v", len(lines), start, lines)
	}
}
