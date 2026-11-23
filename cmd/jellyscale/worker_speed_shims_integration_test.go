//go:build integration

package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// JS-161: what the measurement does with an ffmpeg that is slow, killed, or never ends, with the real process and a shim
// for ffmpeg on its PATH. The locked checks in worker_speed_integration_test.go hold the ordinary run and the missing
// ffmpeg; these hold the rest of the measurement's failure reasons, and that it times the ffmpeg it runs.

// workerWithFFmpeg starts a worker whose ffmpeg is the script, and returns it with its metrics address.
func workerWithFFmpeg(t *testing.T, script string) (*cluster, *proc, string) {
	t.Helper()
	c := newCluster(t, false, "60s")
	addr := freeAddr(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", "metricsListen": %q,
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-x", "transcodeIdle": "60s"}`, addr, c.dir, c.dir)
	p := startProc(t, procSpec{bin: builds[newVersion], dsn: c.dsn, args: []string{"--roles=worker"}, config: config,
		user: "admin", password: "r-pass", path: bin, term: true})
	p.until(30*time.Second, "the worker to start", func() bool { return len(p.recordsOf("worker started")) > 0 })
	p.workerID, _ = field(p.recordsOf("worker started")[0], "worker")
	return c, p, addr
}

// An ffmpeg that is stopped by a signal is a failed measurement with the reason killed, and the worker still counts.
func TestAMeasurementWhoseFFmpegIsKilledSaysSo(t *testing.T) {
	_, p, addr := workerWithFFmpeg(t, `kill -9 $$`)
	p.until(docBound(t)+10*time.Second, "the failed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measurement failed")) > 0 })
	if !regexp.MustCompile(`(?m)^jellyscale_transcode_speed_measurement_failures_total\{reason="killed"\} 1$`).MatchString(metricsText(t, addr)) {
		t.Errorf("/metrics does not show the failure under killed:\n%s", metricsText(t, addr))
	}
}

// An ffmpeg that does not end is stopped by the bound, which is a reason of its own, and the worker registers
// within the documented bound all the same, counting its processors at the reference speed.
func TestAMeasurementThatOverrunsItsBoundSaysSo(t *testing.T) {
	c, p, addr := workerWithFFmpeg(t, `exec /bin/sleep 600`)
	p.until(docBound(t)+10*time.Second, "the overrun to be logged", func() bool { return len(p.recordsOf("transcode speed measurement failed")) > 0 })
	if !regexp.MustCompile(`(?m)^jellyscale_transcode_speed_measurement_failures_total\{reason="timeout"\} 1$`).MatchString(metricsText(t, addr)) {
		t.Errorf("/metrics does not show the failure under timeout:\n%s", metricsText(t, addr))
	}
	p.until(30*time.Second, "the worker to register", func() bool { return rowCores(c, p) > 0 })
	if got, want := rowCores(c, p), float64(runtime.GOMAXPROCS(0)); math.Abs(got-want) > 1e-6 {
		t.Errorf("the row has %v cores after a failed measurement, want the processors at the reference speed, %v", got, want)
	}
}

// The measurement times the ffmpeg it runs: with three seconds added before the real one starts, the speed it finds is
// under the reference's and the row carries its processors times that speed.
func TestAMeasurementTimesTheFFmpegItRuns(t *testing.T) {
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	c, p, _ := workerWithFFmpeg(t, "/bin/sleep 3\nexec "+real+` "$@"`)
	p.until(docBound(t)+10*time.Second, "the speed measurement to be logged", func() bool { return len(p.recordsOf("transcode speed measured")) > 0 })
	p.until(30*time.Second, "the worker to register", func() bool { return rowCores(c, p) > 0 })
	line := p.recordsOf("transcode speed measured")[0]
	slowed := num(line, "speed")
	if !(slowed < 1) {
		t.Errorf("an ffmpeg that takes three seconds longer measured a speed of %v", slowed)
	}
	if want := slowed * float64(runtime.GOMAXPROCS(0)); math.Abs(rowCores(c, p)-want) > 1e-3 {
		t.Errorf("the row has %v cores, want the processors times the speed, %v", rowCores(c, p), want)
	}
}
