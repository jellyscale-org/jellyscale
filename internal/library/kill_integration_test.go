//go:build integration

package library_test

// JS-112: a worker killed with SIGKILL while its scan has ffprobe running on
// four files at once (probeConcurrency) leaves none of the ffprobe processes it
// started. The worker is this test binary run again (TestMain sees
// JELLYSCALE_TEST_SCAN), so the kill is a real one. Only the killed process's
// own children are counted, by their parent: another task's ffprobe on the
// machine neither passes nor fails the test. A second process scans another
// library at the same time, with its own held ffprobe, and keeps it.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/testenv"
)

const scanEnv = "JELLYSCALE_TEST_SCAN"

func TestMain(m *testing.M) {
	if which := os.Getenv(scanEnv); which != "" {
		scanProcess(which)
		return
	}
	os.Exit(m.Run())
}

// libraries are the two libraries of the test: the process scans the one named.
func libraries(dirs string) []library.Config {
	d := strings.Split(dirs, ",")
	return []library.Config{
		{Name: "doomed", Kind: "movies", Media: "file://" + d[0], Assets: "file://" + d[1]},
		{Name: "bystander", Kind: "movies", Media: "file://" + d[2], Assets: "file://" + d[3]},
	}
}

// scanProcess is the body of a worker: it scans the library it is told to and
// says how many films failed.
func scanProcess(name string) {
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Getenv("JELLYSCALE_TEST_DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		os.Exit(1)
	}
	libs, err := library.Sync(ctx, pool, libraries(os.Getenv("JELLYSCALE_TEST_SCAN_DIRS")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		os.Exit(1)
	}
	scanner := &library.Scanner{Pool: pool, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), Grace: time.Hour, MaxLostShare: 1}
	for _, lib := range libs {
		if lib.Name == name {
			failed, err := scanner.ScanCounting(ctx, lib)
			fmt.Printf("failed=%d err=%v\n", failed, err)
			if err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	fmt.Fprintln(os.Stderr, "no library", name)
	os.Exit(1)
}

func TestAKilledWorkersScanLeavesNoFfprobeAndAnotherWorkersKeepsRunning(t *testing.T) {
	testenv.FFmpeg(t)
	pool := openDB(t)
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()}
	// Eight files, twice the probes that run at once: four are running when the
	// worker is killed, and the rest have not started.
	for i := range 8 {
		name := fmt.Sprintf("Film %d (200%d)", i, i)
		if err := os.MkdirAll(filepath.Join(dirs[0], name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirs[0], name, name+".mkv"), []byte("not a film"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clip(t, filepath.Join(dirs[2], "Other (2010)", "Other (2010).mkv"))
	if _, err := library.Sync(context.Background(), pool, libraries(strings.Join(dirs, ","))); err != nil {
		t.Fatal(err)
	}

	start := func(name string, held *testenv.Held) (*exec.Cmd, *bytes.Buffer, <-chan error) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), scanEnv+"="+name, "JELLYSCALE_TEST_SCAN_DIRS="+strings.Join(dirs, ","), held.Env())
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { cmd.Process.Kill() }) //nolint:errcheck
		return cmd, &out, done
	}
	doomedTools, bystanderTools := testenv.Hold(t, "ffprobe"), testenv.Hold(t, "ffprobe")
	doomed, doomedOut, _ := start("doomed", doomedTools)
	bystander, bystanderOut, bystanderDone := start("bystander", bystanderTools)

	var running, others []int
	deadline := time.Now().Add(60 * time.Second) // Only a bound on a hang.
	for len(running) < 4 || len(others) < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("ffprobe running under the first worker: %v, under the second: %v\n%s\n%s", running, others, doomedOut, bystanderOut)
		}
		time.Sleep(10 * time.Millisecond)
		running, others = doomedTools.ChildrenOf(doomed.Process.Pid), bystanderTools.ChildrenOf(bystander.Process.Pid)
	}
	// The scan holds the fifth probe back until one of the four ends, and none ends.
	time.Sleep(200 * time.Millisecond)
	if n := len(doomedTools.ChildrenOf(doomed.Process.Pid)); n != 4 {
		t.Fatalf("%d ffprobe running under the worker, want probeConcurrency (4)", n)
	}

	if err := doomed.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	testenv.GoneWithin(t, time.Second, running...)
	for _, pid := range others {
		if !testenv.Alive(pid) {
			t.Fatalf("the second worker's ffprobe %d died with the first worker", pid)
		}
	}

	bystanderTools.Release()
	select {
	case err := <-bystanderDone:
		if err != nil || !strings.Contains(bystanderOut.String(), "failed=0") {
			t.Fatalf("the second worker's scan: %v\n%s", err, bystanderOut)
		}
	case <-time.After(2 * time.Minute): // Only a bound on a hang.
		t.Fatalf("the second worker's scan did not finish:\n%s", bystanderOut)
	}
	if n := scalar[int](t, pool, "SELECT count(*) FROM items i JOIN libraries l ON l.id = i.library_id WHERE l.name = 'bystander' AND i.type = 'Movie'"); n != 1 {
		t.Errorf("the second worker's library has %d films, want 1", n)
	}
}
