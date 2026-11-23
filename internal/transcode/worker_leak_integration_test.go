//go:build integration

package transcode

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// JS-158: the kits hold every test to its workers. A test that starts a worker and does not stop it, by the kit's
// stop or by building and running the worker itself, fails itself at its end and names the worker, instead of the next
// test failing with a deadlock. The check runs the test binary again on a test that leaks, which is the only way to
// see a test fail without failing this one (the same way failover_integration_test.go runs a worker).
//
// What this cannot reach: a kit whose stop is skipped is not a test the check can write without changing the kit; the
// leaking tests here start a worker beside the kit, which a guard in the kit sees the same way (a node whose liveness
// lock is held, a session still running), whichever code started it.

const leakEnv = "JELLYSCALE_TEST_JS158_LEAK"

// leakingWorker is a worker built and run by the test itself, never stopped.
func leakingWorker(t *testing.T, r *rig, id string) {
	t.Helper()
	pool, err := db.Open(context.Background(), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	w := &Worker{ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: r.segments, Log: r.log, Media: media.Open,
		Scratch: r.scratch, Slots: 2, Idle: 60 * time.Second, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	w.Hub = &Hub{Pool: pool, Log: r.log}
	ctx := context.Background()
	go w.Hub.Run(ctx)
	go w.Present(ctx, "worker", "", 200*time.Millisecond)
	go w.Run(ctx)
	waitFor(t, "worker "+id+" to register", 30*time.Second, func() bool { return r.locked(id) })
}

// TestJS158TheTestThatLeaksAWorker is the body the check below runs in a process of its own. Run alone it has
// nothing to do, so it passes: the scenario comes from the process that asks for it.
func TestJS158TheTestThatLeaksAWorker(t *testing.T) {
	scenario := os.Getenv(leakEnv)
	if scenario == "" {
		return
	}
	id := "js158-leaked-" + scenario
	switch scenario {
	case "rig":
		leakingWorker(t, newRig(t, nil), id)
	case "scanned":
		leakingWorker(t, newScanned(t, film(t), t.TempDir()).rig, id)
	case "process":
		r := newRig(t, nil)
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER="+id, "JELLYSCALE_TEST_SEGMENTS="+t.TempDir(), "JELLYSCALE_TEST_SCRATCH="+r.scratch)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "worker "+id+" to register", 30*time.Second, func() bool { return r.locked(id) })
	case "stopped":
		newRig(t, nil).worker(t, nil) // Through the kit, which stops it.
	}
}

func TestATestThatLeavesAWorkerRunningFailsItselfAndNamesItWhicheverWayItStartedIt(t *testing.T) {
	for _, scenario := range []string{"rig", "scanned", "process"} {
		t.Run(scenario, func(t *testing.T) {
			out, err := leakingTest(t, scenario)
			if err == nil {
				t.Fatalf("the test that left worker js158-leaked-%s running passed:\n%s", scenario, out)
			}
			// A failure message of the test, not a line of the worker's own log, which also carries its id.
			if !regexp.MustCompile(`(?m)^\s+\S+_test\.go:\d+: .*js158-leaked-` + scenario).MatchString(out) {
				t.Errorf("the test failed, but no failure of the test names worker js158-leaked-%s:\n%s", scenario, out)
			}
		})
	}
	t.Run("a worker the kit stops is no leak", func(t *testing.T) {
		if out, err := leakingTest(t, "stopped"); err != nil {
			t.Errorf("a test whose worker the kit stopped failed: %v\n%s", err, out)
		}
	})
}

func leakingTest(t *testing.T, scenario string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJS158TheTestThatLeaksAWorker$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), leakEnv+"="+scenario)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
