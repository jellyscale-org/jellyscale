package testenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file is what tests of ffmpeg and ffprobe runs (JS-112) share: stand-ins
// for the tools that are shell scripts, so a test needs no ffmpeg to see how a
// run starts, fails and stops, and counts a process's own children by reading
// /proc, so another task's ffmpeg on the machine never counts.

// Shims writes an executable script for each tool in a new directory and puts
// it in front of PATH for the test. It returns the directory.
func Shims(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// NoTools makes PATH an empty directory for the test: no ffmpeg, no ffprobe.
// The shims' scripts run under /bin/sh, which needs no PATH to start.
func NoTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// Recorder puts shims for the tools in front of PATH that write each start
// (the tool, its arguments one to a line, the working directory and what was
// on its stdin) to a log, and exit 0 with stdout as given. It returns what reads
// the log, with each of the directories named in scrub replaced by its
// placeholder (and the test's own working directory by <pkg>), so a recording
// made in another checkout or run is the same text.
func Recorder(t *testing.T, stdout map[string]string, scrub map[string]string) func() string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "runs")
	scripts := map[string]string{}
	for name, out := range stdout {
		scripts[name] = fmt.Sprintf(`{ echo "tool %s"; for a; do echo "arg $a"; done; echo "cwd $(pwd)"; echo "stdin $(cat)"; echo "--"; } >>%q
printf %%s %q`, name, log, out)
	}
	Shims(t, scripts)
	pkg, _ := os.Getwd()
	return func() string {
		data, _ := os.ReadFile(log)
		text := string(data)
		for dir, placeholder := range scrub {
			text = strings.ReplaceAll(text, dir, placeholder)
		}
		return strings.ReplaceAll(text, pkg, "<pkg>")
	}
}

// Fault puts a stand-in for tool in front of PATH that fails the way kind says,
// and returns the context to run it with. "exit1" writes a line to stderr and
// exits 1; "signal" kills itself with SIGKILL; "context" runs until the context
// ends, which the returned context does the moment the tool has started.
func Fault(t *testing.T, tool, kind string) context.Context {
	t.Helper()
	started := filepath.Join(t.TempDir(), "started")
	script := map[string]string{
		"exit1":   `echo "boom: the file is not a film" >&2; exit 1`,
		"signal":  `kill -9 $$`,
		"context": fmt.Sprintf(`echo $$ >%q; exec sleep 600`, started),
	}[kind]
	if script == "" {
		t.Fatalf("no fault called %q", kind)
	}
	Shims(t, map[string]string{tool: script})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // Only a bound on a hang.
	t.Cleanup(cancel)
	if kind == "context" {
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(started); err == nil {
					cancel()
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	return ctx
}

// Held is stand-ins for tools that note their process id in a file, then wait
// until Release before they run the real tool (or, with none on PATH, exit 0).
type Held struct {
	dir     string
	names   []string
	release string
	path    string
}

// Hold makes the stand-ins in a directory of their own. They are on PATH only
// for a process started with Env, so two worker processes can hold different
// tools in one test.
func Hold(t *testing.T, names ...string) *Held {
	t.Helper()
	h := &Held{dir: t.TempDir(), names: names}
	h.release = filepath.Join(h.dir, "release")
	if err := os.Mkdir(filepath.Join(h.dir, "pids"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		run := "exit 0"
		if real, err := exec.LookPath(name); err == nil {
			run = fmt.Sprintf("exec %q \"$@\"", real)
		}
		script := fmt.Sprintf("#!/bin/sh\necho $$ >%q\nwhile [ ! -e %q ]; do sleep 0.05; done\n%s\n",
			filepath.Join(h.dir, "pids", name+".$$"), h.release, run)
		if err := os.WriteFile(filepath.Join(h.dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.path = h.dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Cleanup(func() {
		h.Release()
		for _, pid := range h.all() {
			syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
		}
	})
	return h
}

// Env is the setting of PATH that puts the stand-ins in front, for a process's
// environment.
func (h *Held) Env() string { return "PATH=" + h.path }

// Release lets every stand-in, those waiting and those to come, run its tool.
func (h *Held) Release() {
	os.WriteFile(h.release, nil, 0o644) //nolint:errcheck
}

func (h *Held) all() []int {
	var pids []int
	files, _ := filepath.Glob(filepath.Join(h.dir, "pids", "*"))
	for _, file := range files {
		if pid, err := strconv.Atoi(strings.TrimLeft(filepath.Ext(file), ".")); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ChildrenOf is the stand-ins that were started by process parent and have not
// ended: only those, whatever else runs on the machine.
func (h *Held) ChildrenOf(parent int) []int {
	var pids []int
	for _, pid := range h.all() {
		if Alive(pid) && ParentOf(pid) == parent {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ParentOf is the process id of pid's parent, or 0.
func ParentOf(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// pid (comm) state ppid ...; comm may hold spaces and brackets.
	rest := string(data[strings.LastIndexByte(string(data), ')')+2:])
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

// Alive says whether pid is a process that runs: not gone, and not a zombie
// waiting for its parent.
func Alive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	rest := string(data[strings.LastIndexByte(string(data), ')')+2:])
	return rest != "" && rest[0] != 'Z' && rest[0] != 'X'
}

// GoneWithin fails the test unless every process in pids has ended within d.
// Pdeathsig is the kernel's, delivered as the parent dies, so d is a bound on
// the reaping and not a wait for work.
func GoneWithin(t *testing.T, d time.Duration, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var left []int
		for _, pid := range pids {
			if Alive(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range left {
				syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
			}
			t.Fatalf("%d of the killed process's children still run %v after the kill: %v", len(left), d, left)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
