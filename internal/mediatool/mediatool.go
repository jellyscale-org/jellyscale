// Package mediatool runs ffmpeg and ffprobe. It is the one place that starts
// them, so the two rules about stopping hold in one place: a tool is killed when
// its context ends, and a worker that is killed outright leaves no tool behind.
package mediatool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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
	// Stall, when above zero, ends a tool that makes no progress for that long
	// with ErrStalled. Progress is the tool's own reading and writing, as the
	// kernel counts them for the process (Linux only: elsewhere no stall is
	// found), and whatever number Progress returns, for a read the tool makes
	// through a connection, which the kernel does not count as a read.
	Stall    time.Duration
	Progress func() int64
	// Background runs the tool at the lowest priority (nice 19), so that it takes only the cores the jobs beside it
	// leave: a worker's extraction holds no cores in any ledger. A transcode is never started this way.
	Background bool
}

// ErrStalled is the cause of a run ended because the tool made no progress for
// Options.Stall. The context was not the one that ended.
var ErrStalled = errors.New("the tool made no progress")

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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if _, err := exec.LookPath("nice"); o.Background && err == nil {
		tool, args = "nice", append([]string{"-n", "19", tool}, args...)
	}
	cmd := Command(ctx, tool, args...)
	cmd.Dir, cmd.Stdin, cmd.WaitDelay = o.Dir, o.Stdin, o.WaitDelay
	var out, errOut, all bytes.Buffer
	var mu sync.Mutex
	cmd.Stdout, cmd.Stderr = &tee{&mu, &out, &all}, &tee{&mu, &errOut, &all}
	err := Start(cmd)
	if err == nil {
		if o.Stall > 0 {
			watched := make(chan struct{})
			defer close(watched)
			go watchStall(watched, cmd.Process.Pid, o, func() { cancel(ErrStalled) })
		}
		err = cmd.Wait()
	}
	if err != nil && errors.Is(context.Cause(ctx), ErrStalled) {
		err = ErrStalled
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

// watchStall calls stalled once the process has made no progress for o.Stall,
// until done is closed.
func watchStall(done <-chan struct{}, pid int, o Options, stalled func()) {
	tick := time.NewTicker(max(o.Stall/10, 10*time.Millisecond))
	defer tick.Stop()
	last, since := int64(-1), time.Now()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		now := processIO(pid)
		if o.Progress != nil {
			now += o.Progress()
		}
		if now != last {
			last, since = now, time.Now()
		} else if time.Since(since) >= o.Stall {
			stalled()
			return
		}
	}
}

// processIO is the bytes the process has read and written, by the kernel's
// count; a number that changes every call where there is no count, so that no
// process is thought stalled for it.
func processIO(pid int) int64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return time.Now().UnixNano()
	}
	var total int64
	for _, line := range strings.Split(string(data), "\n") {
		if name, value, ok := strings.Cut(line, ": "); ok && (name == "rchar" || name == "wchar") {
			n, _ := strconv.ParseInt(value, 10, 64)
			total += n
		}
	}
	return total
}
