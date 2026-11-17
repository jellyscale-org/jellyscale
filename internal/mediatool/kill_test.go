package mediatool_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/mediatool"
	"jellyscale/internal/testenv"
)

// A process killed with SIGKILL leaves no tool it started behind, however the
// tool was run. The process is this test binary run again (TestMain sees
// JELLYSCALE_TEST_MEDIATOOL), so the kill is a real one; the tool is a script
// that writes its pid to a file and sleeps, and only the pids whose parent is
// the killed process are counted, so another task's ffmpeg on the machine
// neither passes nor fails these.

const modeEnv = "JELLYSCALE_TEST_MEDIATOOL"

func TestMain(m *testing.M) {
	if mode := os.Getenv(modeEnv); mode != "" {
		child(mode)
		return
	}
	os.Exit(m.Run())
}

// child is the body of the killed process: it runs the tool the way its mode
// says, and never returns until it is killed (the tool sleeps 10 minutes).
func child(mode string) {
	ctx := context.Background()
	var err error
	switch mode {
	case "warnings":
		_, err = mediatool.Run(ctx, "ffmpeg", []string{"-i", "in"}, mediatool.Options{Output: mediatool.WarningsAllowed})
	case "any output":
		_, err = mediatool.Run(ctx, "ffmpeg", []string{"-i", "in"}, mediatool.Options{Output: mediatool.AnyOutputIsFailure})
	case "held": // Ends with exit 0 when its tool is let go.
		// -version succeeds whether the stand-in or a real ffmpeg on PATH runs once the tool is let go.
		if _, err = mediatool.Run(ctx, "ffmpeg", []string{"-version"}, mediatool.Options{}); err == nil {
			os.Exit(0)
		}
	case "stdin":
		reader, _ := io.Pipe() // Nothing is ever written, as when a stream is still coming.
		_, err = mediatool.Run(ctx, "ffmpeg", nil, mediatool.Options{Stdin: reader})
	case "directory":
		_, err = mediatool.Run(ctx, "ffmpeg", nil, mediatool.Options{Dir: os.TempDir()})
	case "started and waited by the caller":
		cmd := mediatool.Command(ctx, "ffmpeg", "-i", "in")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err = mediatool.Start(cmd); err == nil {
			err = cmd.Wait()
		}
	default:
		err = fmt.Errorf("no mode %q", mode)
	}
	fmt.Fprintln(os.Stderr, "the tool ended without a kill:", err)
	os.Exit(3)
}

func TestAKilledProcessLeavesNoChildForEachWayAToolIsRun(t *testing.T) {
	for _, mode := range []string{"warnings", "any output", "stdin", "directory", "started and waited by the caller"} {
		t.Run(mode, func(t *testing.T) {
			held := testenv.Hold(t, "ffmpeg")
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), modeEnv+"="+mode, held.Env())
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() { cmd.Wait(); close(exited) }()          //nolint:errcheck
			t.Cleanup(func() { cmd.Process.Kill(); <-exited }) //nolint:errcheck

			var children []int
			deadline := time.Now().Add(30 * time.Second) // Only a bound on a hang.
			for len(children) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the process never ran its tool: %s", stderr.String())
				}
				time.Sleep(10 * time.Millisecond)
				children = held.ChildrenOf(cmd.Process.Pid)
			}
			if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			testenv.GoneWithin(t, time.Second, children...)
		})
	}
}

// Only the killed process's children go: a second process that runs the same
// tool at the same time keeps it, and its run finishes.
func TestAKilledProcessLeavesAnotherProcessesToolRunning(t *testing.T) {
	held := testenv.Hold(t, "ffmpeg")
	var procs [2]*exec.Cmd
	for i := range procs {
		procs[i] = exec.Command(os.Args[0])
		procs[i].Env = append(os.Environ(), modeEnv+"=held", held.Env())
		procs[i].Stdout = io.Discard
		var stderr bytes.Buffer
		procs[i].Stderr = &stderr
		if err := procs[i].Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { procs[i].Process.Kill(); procs[i].Wait() }) //nolint:errcheck
	}
	var first, second []int
	deadline := time.Now().Add(30 * time.Second)
	for len(first) == 0 || len(second) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the processes never ran their tools")
		}
		time.Sleep(10 * time.Millisecond)
		first, second = held.ChildrenOf(procs[0].Process.Pid), held.ChildrenOf(procs[1].Process.Pid)
	}
	procs[0].Process.Signal(syscall.SIGKILL) //nolint:errcheck
	testenv.GoneWithin(t, time.Second, first...)
	for _, pid := range second {
		if !testenv.Alive(pid) {
			t.Fatalf("the other process's tool %d died with the first process", pid)
		}
	}
	held.Release()
	if err := procs[1].Wait(); err != nil {
		t.Errorf("the other process's run did not finish: %v", err)
	}
}
