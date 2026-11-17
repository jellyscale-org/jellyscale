package mediatool_test

// JS-112: one runner for ffmpeg and ffprobe. The names these tests rely on that
// the task implies and the code lacks (the package does not exist yet):
//
//	Run(ctx, tool string, args []string, o Options) (Result, error)
//	Options{Dir string; Stdin io.Reader; Output Output; WaitDelay time.Duration}
//	Output: WarningsAllowed (the zero value) or AnyOutputIsFailure
//	Result{Stdout, Stderr []byte}
//	Command(ctx, tool string, args ...string) *exec.Cmd, and Start(cmd) error,
//	  for a caller that starts and waits itself, as a transcode does
//	FileFailure(err) bool: the run ended in the file's doing
//
// Every test here runs in `go test ./...` with no ffmpeg on PATH: the tools are
// shell scripts (testenv.Shims).

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/mediatool"
	"jellyscale/internal/testenv"
)

func run(t *testing.T, o mediatool.Output, script string) (mediatool.Result, error) {
	t.Helper()
	testenv.Shims(t, map[string]string{"ffmpeg": script})
	return mediatool.Run(context.Background(), "ffmpeg", []string{"-i", "in"}, mediatool.Options{Output: o})
}

// 'Any output is a failure' (subtitles' extraction: a file cut short makes
// ffmpeg complain and exit 0).
func TestAnyOutputIsAFailureNamesTheTextAndIgnoresWhitespace(t *testing.T) {
	for _, c := range []struct {
		name, script string
		fails        bool
	}{
		{"text on stderr", `echo "Truncating packet" >&2`, true},
		{"text on stdout", `echo "Truncating packet"`, true},
		{"nothing", `exit 0`, false},
		{"whitespace only", `printf ' \n\t\n' ; printf '  \n' >&2`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, mediatool.AnyOutputIsFailure, c.script)
			switch {
			case c.fails && err == nil:
				t.Fatal("exit 0 with output succeeded")
			case c.fails && !strings.Contains(err.Error(), "Truncating packet"):
				t.Errorf("the error does not carry the text: %v", err)
			case c.fails && !mediatool.FileFailure(err):
				t.Errorf("complaining and exiting 0 is the file's failure: %v", err)
			case !c.fails && err != nil:
				t.Errorf("failed: %v", err)
			}
		})
	}
}

// 'Warnings allowed' (a probe: text on stderr with exit 0 is not a failure).
func TestWarningsAllowedKeepsTheTextAndFailsOnAnExitStatus(t *testing.T) {
	res, err := run(t, mediatool.WarningsAllowed, `echo '{"a":1}'; echo "Application provided invalid, non monotonically increasing dts" >&2`)
	if err != nil {
		t.Fatalf("exit 0 with a warning failed: %v", err)
	}
	if !strings.Contains(string(res.Stderr), "non monotonically") || strings.TrimSpace(string(res.Stdout)) != `{"a":1}` {
		t.Errorf("stdout %q stderr %q: the caller needs both", res.Stdout, res.Stderr)
	}
	res, err = run(t, mediatool.WarningsAllowed, `echo "no such file" >&2; exit 1`)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("exit 1: %v", err)
	}
	if !strings.Contains(string(res.Stderr), "no such file") && !strings.Contains(err.Error(), "no such file") {
		t.Errorf("the failure lost its text: %v %q", err, res.Stderr)
	}
}

// What is the file's failure: an exit status above 0, and only that. Under both
// policies, without ffmpeg on PATH.
func TestOnlyAnExitStatusAboveZeroIsTheFilesFailure(t *testing.T) {
	for _, policy := range []struct {
		name   string
		output mediatool.Output
	}{{"warnings allowed", mediatool.WarningsAllowed}, {"any output", mediatool.AnyOutputIsFailure}} {
		t.Run(policy.name, func(t *testing.T) {
			t.Run("exit status 1", func(t *testing.T) {
				_, err := run(t, policy.output, `echo "Invalid data found when processing input" >&2; exit 1`)
				if err == nil || !mediatool.FileFailure(err) {
					t.Errorf("not the file's failure: %v", err)
				}
			})
			t.Run("exit status 183", func(t *testing.T) {
				_, err := run(t, policy.output, `exit 183`)
				if err == nil || !mediatool.FileFailure(err) {
					t.Errorf("not the file's failure: %v", err)
				}
			})
			t.Run("killed by a signal", func(t *testing.T) {
				_, err := run(t, policy.output, `kill -9 $$`)
				if err == nil || mediatool.FileFailure(err) {
					t.Errorf("the OOM killer's doing taken for the file's: %v", err)
				}
			})
			t.Run("context ended", func(t *testing.T) {
				ctx := testenv.Fault(t, "ffmpeg", "context")
				_, err := mediatool.Run(ctx, "ffmpeg", nil, mediatool.Options{Output: policy.output})
				if err == nil || mediatool.FileFailure(err) {
					t.Errorf("a run that was cut short is not the file's failure: %v", err)
				}
			})
			t.Run("tool missing from PATH", func(t *testing.T) {
				testenv.NoTools(t)
				_, err := mediatool.Run(context.Background(), "ffmpeg", nil, mediatool.Options{Output: policy.output})
				if err == nil || mediatool.FileFailure(err) {
					t.Errorf("no ffmpeg is not the file's failure: %v", err)
				}
				var missing *exec.Error
				if !errors.As(err, &missing) {
					t.Errorf("the error does not say the tool is missing: %T %v", err, err)
				}
			})
			t.Run("no space left on device", func(t *testing.T) {
				_, err := run(t, policy.output, `echo "Error writing trailer: No space left on device" >&2; exit 1`)
				if err == nil || mediatool.FileFailure(err) {
					t.Errorf("a full disk is not the file's failure: %v", err)
				}
			})
		})
	}
	t.Run("no space left on device with exit 0", func(t *testing.T) {
		_, err := run(t, mediatool.AnyOutputIsFailure, `echo "No space left on device" >&2`)
		if err == nil || mediatool.FileFailure(err) {
			t.Errorf("a full disk is not the file's failure, even when ffmpeg exits 0: %v", err)
		}
	})
}

// A child is started on a thread that lives as long as the process: Pdeathsig
// is the kernel's answer to the thread that forked ending, so a goroutine that
// locks its OS thread and exits (which ends the thread) must not end a run that
// another goroutine started. 50 children of 2 seconds each, started while 50
// such goroutines end, all exit 0.
//
// Mutant: start the child from the calling goroutine without a thread of its
// own (exec.Cmd.Start with Pdeathsig set, as before the change): the children
// whose forking thread a locked goroutine then ends are killed (checked on
// this machine with a plain exec.Cmd, see the report).
func TestARunIsNotCutShortByGoroutinesThatLockTheirThreadAndExit(t *testing.T) {
	testenv.Shims(t, map[string]string{"ffmpeg": `exec sleep 2`})
	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = mediatool.Run(context.Background(), "ffmpeg", nil, mediatool.Options{})
		}()
		// Each locked goroutine is created as a run starts, and takes the thread
		// the scheduler has at hand, which may be the one that just forked.
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			runtime.Gosched()
			time.Sleep(time.Duration(i) * 30 * time.Millisecond)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
}

// The path a transcode takes (Command, Start, the caller's Wait) is not cut
// short when the goroutine that called Start ends with its thread locked. The
// race is forced, not waited for: the goroutine locks its own thread, starts the
// tool and returns without unlocking, so the thread it called Start on exits at
// once. A build that forks on the caller's thread, with or without a lock
// around Start (Pdeathsig then fires as that thread exits), has its tool killed
// by SIGKILL on every run; a build that forks on a thread of mediatool's own
// does not. The goroutine that waits is another one, as the transcode's is.
//
// Mutants that must fail every run: exec.Cmd.Start on the calling goroutine with
// Pdeathsig set; the same with runtime.LockOSThread/UnlockOSThread around Start.
func TestACommandStartedByAGoroutineWhoseThreadExitsIsNotKilled(t *testing.T) {
	testenv.Shims(t, map[string]string{"ffmpeg": `exec sleep 1`})
	// This goroutine holds a thread, so the one that starts the tool is never on
	// the process's main thread, which does not exit when its goroutine does
	// (without this a mutant survived 1 run in 8 at GOMAXPROCS=2).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	started := make(chan *exec.Cmd)
	startErr := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // Never unlocked: the thread ends with the goroutine.
		cmd := mediatool.Command(context.Background(), "ffmpeg", "-i", "in")
		startErr <- mediatool.Start(cmd)
		started <- cmd
	}()
	cmd := <-started
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the tool did not run to its end after the goroutine that started it ended: %v", err)
	}
}
