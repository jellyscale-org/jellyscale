//go:build integration

package api

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-158: as internal/transcode's worker_leak_integration_test.go, for the fixtures of this package: a test that
// starts a worker and does not stop it fails itself at its end and names the worker, instead of the next test's
// TRUNCATE (newEnvOn's, which is where run 45041377896 and run 44855263307 failed) meeting its writes.

const leakEnv = "JELLYSCALE_TEST_JS158_LEAK"

// TestJS158TheTestThatLeaksAWorker is the body the check below runs in a process of its own; run alone it has nothing
// to do, so it passes.
func TestJS158TheTestThatLeaksAWorker(t *testing.T) {
	scenario := os.Getenv(leakEnv)
	if scenario == "" {
		return
	}
	id := "js158-leaked-" + scenario
	switch scenario {
	case "env":
		newEnv(t)
		leakingWorker(t, id)
	case "stopped":
		languagesEnv(t, true) // Its worker is the fixture's, which stops it.
	}
}

// leakingWorker is a worker built and run by the test itself, never stopped.
func leakingWorker(t *testing.T, id string) {
	t.Helper()
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	log := slog.New(slog.DiscardHandler)
	w := &transcode.Worker{ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: store.NewPosix(t.TempDir()), Log: log,
		Media: media.Open, Scratch: t.TempDir(), Slots: 2, Idle: time.Minute, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	w.Hub = &transcode.Hub{Pool: pool, Log: log}
	ctx := context.Background()
	go w.Hub.Run(ctx)
	go w.Present(ctx, "worker", "", 200*time.Millisecond)
	go w.Run(ctx)
	waitUntil(t, "the worker to register", func() bool {
		return scalar[int](t, pool, "SELECT count(*)::int FROM transcode_nodes WHERE id = '"+id+"'") == 1
	})
}

func TestATestThatLeavesAWorkerRunningFailsItselfAndNamesIt(t *testing.T) {
	out, err := leakingTest(t, "env")
	if err == nil {
		t.Fatalf("the test that left worker js158-leaked-env running passed:\n%s", out)
	}
	if !regexp.MustCompile(`(?m)^\s+\S+_test\.go:\d+: .*js158-leaked-env`).MatchString(out) {
		t.Errorf("the test failed, but no failure of the test names worker js158-leaked-env:\n%s", out)
	}
	if out, err := leakingTest(t, "stopped"); err != nil {
		t.Errorf("a test whose worker its fixture stopped failed: %v\n%s", err, out)
	}
}

func leakingTest(t *testing.T, scenario string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJS158TheTestThatLeaksAWorker$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), leakEnv+"="+scenario)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
