// Package mediatool runs ffmpeg and ffprobe. It is the one place that starts
// them, so the two rules about stopping hold in one place: a tool is killed when
// its context ends, and a worker that is killed outright leaves no tool behind.
package mediatool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Output says what a tool's text means.
type Output int

const (
	// WarningsAllowed: text with exit status 0 is not a failure (a probe's
	// warnings); the caller has it in the Result.
	WarningsAllowed Output = iota
	// AnyOutputIsFailure: a tool that writes anything but whitespace has failed,
	// whatever its exit status (a file cut short makes ffmpeg complain and exit 0).
	AnyOutputIsFailure
)

// Options are how Run starts a tool. The zero value runs it in the current
// directory with no stdin, allowing warnings.
type Options struct {
	Dir    string
	Stdin  io.Reader
	Output Output
	// WaitDelay is how long Run waits, after the context has ended, for a
	// process the tool started to let go of its output.
	WaitDelay time.Duration
}

// Result is what the tool wrote. Combined is both, interleaved as they came.
type Result struct {
	Stdout, Stderr, Combined []byte
}

// Error is a failed run. Its text is the cause's, so a caller that adds the
// tool's output to it writes what it always did. Err is nil when the tool exited
// 0 but wrote text under AnyOutputIsFailure.
type Error struct {
	Err    error
	Output string // What the tool wrote, trimmed

	cut bool // The context had ended
}

func (e *Error) Error() string {
	if e.Err == nil {
		return "the tool wrote: " + e.Output
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// FileFailure says whether a failed run is the file's doing: the tool ran to the
// end and said so, by an exit status above 0 or by writing text under
// AnyOutputIsFailure. A run that was cut short (the context, a signal such as
// the OOM killer's, no tool on PATH) or that ran out of disk says nothing about
// the file.
func FileFailure(err error) bool {
	var failed *Error
	if !errors.As(err, &failed) || failed.cut || strings.Contains(failed.Output, "No space left on device") {
		return false
	}
	if failed.Err == nil {
		return true
	}
	var exited *exec.ExitError
	return errors.As(failed.Err, &exited) && exited.ExitCode() > 0 // -1 when a signal killed it.
}

// Command is the tool's command, killed when ctx ends and, where the system
// allows, when this process dies. Start it with Start, never with cmd.Start.
func Command(ctx context.Context, tool string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, tool, args...)
	dieWithParent(cmd)
	return cmd
}

// The kernel's death signal (Pdeathsig) fires when the thread that forked the
// child exits, not the process. So every child is forked by one goroutine that
// holds its thread for as long as the process lives, and no other goroutine
// that locks its thread and ends can take a tool with it.
var (
	forkOnce sync.Once
	forks    = make(chan fork)
)

type fork struct {
	cmd   *exec.Cmd
	reply chan error
}

// Start starts a command made by Command.
func Start(cmd *exec.Cmd) error {
	forkOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			for f := range forks {
				f.reply <- f.cmd.Start()
			}
		}()
	})
	reply := make(chan error, 1)
	forks <- fork{cmd, reply}
	return <-reply
}

// Run runs the tool to its end.
func Run(ctx context.Context, tool string, args []string, o Options) (Result, error) {
	cmd := Command(ctx, tool, args...)
	cmd.Dir, cmd.Stdin, cmd.WaitDelay = o.Dir, o.Stdin, o.WaitDelay
	var out, errOut, all bytes.Buffer
	var mu sync.Mutex
	cmd.Stdout, cmd.Stderr = &tee{&mu, &out, &all}, &tee{&mu, &errOut, &all}
	err := Start(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	res := Result{out.Bytes(), errOut.Bytes(), all.Bytes()}
	text := strings.TrimSpace(all.String())
	if err != nil || (o.Output == AnyOutputIsFailure && text != "") {
		return res, &Error{Err: err, Output: text, cut: ctx.Err() != nil}
	}
	return res, nil
}

// tee writes to a buffer of its own and to the one both streams share.
type tee struct {
	mu        *sync.Mutex
	own, both *bytes.Buffer
}

func (t *tee) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.own.Write(p)
	return t.both.Write(p)
}
