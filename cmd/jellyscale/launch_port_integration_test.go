//go:build integration

package main

// JS-171: a launched process never loses its port. The harness chooses the address (freeAddr) and the process binds it a
// moment later; another socket can take it between the two, which cost a merge-queue run (44983050791: one of the 18 processes of
// TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries exited with "bind: address already in use"). These checks take
// the address with a real socket, as the harness has chosen it and before the process binds, and then read what the test
// is handed.
//
// The jellyscale binary the harness launches is a wrapper (TestLaunchWrapperChild, this test binary run again) that decides,
// per start, whether that start's address is taken: a squatter process (TestLaunchSquatterChild) binds it and answers 204 to
// every request, then the real binary is started on it and fails to bind, which is what the queue run saw; or the start is
// told another process's address instead, one the harness launched. Every start is recorded in a directory, so that a check
// reads how many starts there were and which one served. A start that is not taken replaces the wrapper with the real binary
// (exec), so the process the harness holds is the real one.
//
// What the squatter answers is what a launch that waits for /healthz alone would take for the process's: it lives on after the
// real binary has exited, as the process that took the port does in the queue.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// A plan is what happens to a launched process's starts.
type portPlan struct {
	takes int    // The first starts whose address is taken.
	usurp string // If set, those starts are told this address (another process's) and not the one the harness chose.
}

// startRecord is one start of the real binary as the wrapper saw it.
type startRecord struct {
	pid          int    // The process the harness launched: the wrapper, which is the real binary itself when the start was not taken.
	child        int    // The real binary the wrapper started as its child on a taken start; 0 when the start was not taken.
	chosen, told string // The address the harness chose, and the one the start was told.
}

// pids are the processes of the start, which wrote rows to transcode_nodes under ids that begin with them.
func (r startRecord) pids() []int {
	if r.child != 0 {
		return []int{r.pid, r.child}
	}
	return []int{r.pid}
}

// wrapperFor writes the wrapper for a plan and returns its path. Its starts are recorded in dir.
func wrapperFor(t *testing.T, real, dir string, plan portPlan) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexport JS171_DIR='%s' JS171_REAL='%s' JS171_TAKES=%d JS171_USURP='%s'\nexec '%s' -test.run='^TestLaunchWrapperChild$' -- \"$@\"\n",
		dir, real, plan.takes, plan.usurp, os.Args[0])
	path := filepath.Join(t.TempDir(), "wrapper")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// A squatter outlives the wrapper that started it, as the process that took the port does in the queue; none outlives the test.
	t.Cleanup(func() {
		files, _ := filepath.Glob(filepath.Join(dir, "squatter-*"))
		for _, f := range files {
			if raw, err := os.ReadFile(f); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
					syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
				}
			}
		}
	})
	return path
}

func startsIn(t *testing.T, dir string) []startRecord {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "start-*"))
	sort.Slice(files, func(i, j int) bool {
		return len(files[i]) < len(files[j]) || len(files[i]) == len(files[j]) && files[i] < files[j]
	})
	var out []startRecord
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r startRecord
		if _, err := fmt.Sscanf(string(raw), "%d %s %s", &r.pid, &r.chosen, &r.told); err != nil {
			t.Fatalf("%s: %q: %v", f, raw, err)
		}
		if raw, err := os.ReadFile(strings.Replace(f, "start-", "child-", 1)); err == nil {
			r.child, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		out = append(out, r)
	}
	return out
}

// The role sets that listen, as the 18-process test launches them (api, stream, api,stream) and the default one (all).
var listeningRoles = []string{"api", "stream", "api,stream", "all"}

// launchTaken launches a replica the way TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries does (launchOn, then ready)
// with the plan applied to its starts.
func launchTaken(t *testing.T, c *libCluster, roles string, plan portPlan) (*proc, string) {
	t.Helper()
	dir := t.TempDir()
	cc := *c
	cc.bin = wrapperFor(t, c.bin, dir, plan)
	p := cc.launchOn(c.dsn, roles, "")
	p.ready()
	return p, dir
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// nodeIDs are the ids in transcode_nodes, where every process of an api, stream or worker role registers under "<pid>-<time>".
func nodeIDs(t *testing.T, c *libCluster) []string {
	t.Helper()
	rows, err := c.pool.Query(context.Background(), "SELECT id FROM transcode_nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func hasPrefixPID(ids []string, pid int) int {
	n := 0
	for _, id := range ids {
		if strings.HasPrefix(id, strconv.Itoa(pid)+"-") {
			n++
		}
	}
	return n
}

// ownsItsPort says that the process the test is handed is the real one that was started last, serving on the address the
// test is handed, and that nothing of the starts that lost their port is in what the test reads of it: not its log, not a process
// of theirs still running, not a row they wrote to transcode_nodes.
func ownsItsPort(t *testing.T, c *libCluster, name string, p *proc, dir string, plan portPlan) {
	t.Helper()
	starts := startsIn(t, dir)
	if len(starts) < plan.takes+1 {
		// The launch did not start the process again. Its exit is what the test would read.
		select {
		case <-p.exited:
		case <-time.After(30 * time.Second):
		}
		t.Fatalf("%s: %d starts of the binary, want %d (its address was taken %d times); the process the test holds (exited %v: %v) has this log:\n%s",
			name, len(starts), plan.takes+1, plan.takes, !p.alive(), p.status, p.log)
	}
	if len(starts) != plan.takes+1 {
		t.Errorf("%s: %d starts of the binary, want %d: a start that did not lose its port was started again", name, len(starts), plan.takes+1)
	}
	last := starts[len(starts)-1]
	if last.pid != p.cmd.Process.Pid || last.told != p.addr {
		t.Errorf("%s: the test holds pid %d on %s, the start that served was pid %d on %s", name, p.cmd.Process.Pid, p.addr, last.pid, last.told)
	}
	for _, s := range starts[:len(starts)-1] {
		if s.told == p.addr {
			t.Errorf("%s: the address %s that was taken is the one the test is handed", name, p.addr)
		}
	}
	// The process answers on its address. ready() may return a few milliseconds before the real binary's listener is up (an
	// all process logs "worker started" first, and "listening" is logged before the bind), so this polls for a bound of ten
	// times what it takes; a squatter's answer is never the one it waits for.
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var answered string
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		answered = ""
		resp, err := client.Get(p.url("/healthz"))
		if err != nil {
			answered = err.Error()
		} else {
			resp.Body.Close()
			if resp.StatusCode == 204 && resp.Header.Get("X-Squatter") == "" {
				answered = ""
				break
			}
			answered = fmt.Sprintf("%d, X-Squatter %q", resp.StatusCode, resp.Header.Get("X-Squatter"))
		}
		if time.Now().After(end) {
			t.Errorf("%s: /healthz on %s was not answered by the process in 10 seconds (the last: %s)", name, p.addr, answered)
			break
		}
	}
	if !p.alive() {
		t.Errorf("%s: the process had exited (%v):\n%s", name, p.status, p.log)
	}
	deadline := time.Now().Add(30 * time.Second)
	for strings.Count(p.log.String(), "msg=listening") == 0 && p.alive() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if testenvTaken(p.log.String()) || strings.Count(p.log.String(), "msg=listening") != 1 {
		t.Errorf("%s: its log is not one process's (want no bind error and one \"listening\" line):\n%s", name, p.log)
	}
	// A start that lost its port is gone, with what it started: the wrapper and its child, which a wrapper killed by SIGKILL
	// leaves to Pdeathsig.
	for _, s := range starts[:len(starts)-1] {
		for _, pid := range s.pids() {
			for end := time.Now().Add(15 * time.Second); alive(pid) && time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			}
			if alive(pid) {
				t.Errorf("%s: process %d of a start that lost its port is still running", name, pid)
				syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
			}
		}
	}
	// What it wrote to the database is the serving process's alone: the row of a start that lost its port is not left beside it. A
	// moment passes first, for a write that was in flight when that start was killed.
	time.Sleep(time.Second)
	// The serving process writes its row once it has measured its speed (JS-161), a few seconds after it listens.
	ids := nodeIDs(t, c)
	for end := time.Now().Add(30 * time.Second); hasPrefixPID(ids, last.pid) == 0 && time.Now().Before(end); ids = nodeIDs(t, c) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := hasPrefixPID(ids, last.pid); n != 1 {
		t.Errorf("%s: transcode_nodes holds %d rows of the serving process %d, want 1: %v", name, n, last.pid, ids)
	}
	for _, s := range starts[:len(starts)-1] {
		for _, pid := range s.pids() {
			if n := hasPrefixPID(ids, pid); n != 0 {
				t.Errorf("%s: transcode_nodes holds %d rows of process %d, a start that lost its port: %v", name, n, pid, ids)
			}
		}
	}
}

func testenvTaken(log string) bool { return strings.Contains(log, "address already in use") }

// stopsClean ends the process as an orchestrator does and says its exit is the serving process's own: a clean one.
func stopsClean(t *testing.T, name string, p *proc) {
	t.Helper()
	p.stop()
	if p.alive() || p.status != nil {
		t.Errorf("%s: after SIGTERM the exit read is %v (still alive %v), want a clean exit:\n%s", name, p.status, p.alive(), p.log)
	}
}

// forEachListeningRole runs the check once for each role set that listens.
func forEachListeningRole(t *testing.T, check func(t *testing.T, c *libCluster, roles string)) {
	c := newLibCluster(t)
	for _, roles := range listeningRoles {
		t.Run(roles, func(t *testing.T) {
			sub := *c
			sub.t = t // The processes it launches fail this subtest, not the whole test.
			check(t, &sub, roles)
		})
	}
}

// The control: with no address taken a process is started once, which is what the checks below count from, and no role set
// is started again, or killed, for not listening where its address is.
func TestALaunchedProcessWhoseAddressIsFreeIsStartedOnceInEveryRoleSet(t *testing.T) {
	forEachListeningRole(t, func(t *testing.T, c *libCluster, roles string) {
		plan := portPlan{}
		p, dir := launchTaken(t, c, roles, plan)
		ownsItsPort(t, c, "free", p, dir, plan)
		stopsClean(t, "free", p)
	})
}

// A worker alone binds nothing, whatever address its launch chose (run in main.go returns for a process with neither the api nor the
// stream role before it makes the http server, which is the only thing that binds --listen; --metricsListen is a config's own
// address, which a launch does not choose): it is started once, even where a socket holds that address,
// and is not mistaken for a process that lost its port.
func TestAWorkerLaunchedThroughTheHarnessIsStartedOnceEvenWhereItsAddressIsTaken(t *testing.T) {
	c := newLibCluster(t)
	for _, takes := range []int{0, 1} {
		t.Run(fmt.Sprintf("taken %d", takes), func(t *testing.T) {
			dir := t.TempDir()
			cc := *c
			cc.t = t
			cc.bin = wrapperFor(t, c.bin, dir, portPlan{takes: takes})
			p := cc.launchOn(c.dsn, "worker", "")
			p.ready()
			time.Sleep(3 * time.Second) // A launch that judges a worker by its port would end it about now and start it again.
			if starts := startsIn(t, dir); len(starts) != 1 || !p.alive() || p.count("worker started") != 1 {
				t.Errorf("the worker was started %d times (alive %v, logged %d \"worker started\" lines), want once and still running:\n%s", len(starts), p.alive(), p.count("worker started"), p.log)
			}
			stopsClean(t, "worker", p)
		})
	}
}

func TestALaunchedProcessWhoseAddressIsTakenOnceServesOnAnAddressOfItsOwn(t *testing.T) {
	forEachListeningRole(t, func(t *testing.T, c *libCluster, roles string) {
		plan := portPlan{takes: 1}
		p, dir := launchTaken(t, c, roles, plan)
		ownsItsPort(t, c, "taken once", p, dir, plan)
		stopsClean(t, "taken once", p)
	})
}

func TestALaunchedProcessWhoseAddressIsTakenSeveralTimesInARowServesOnAnAddressOfItsOwn(t *testing.T) {
	forEachListeningRole(t, func(t *testing.T, c *libCluster, roles string) {
		plan := portPlan{takes: 3}
		p, dir := launchTaken(t, c, roles, plan)
		ownsItsPort(t, c, "taken three times", p, dir, plan)
		stopsClean(t, "taken three times", p)
	})
}

// The lowest surface of the harness: startProc and until, with no helper of a test between, as every test that launches a listening
// process through it uses them.
func TestAProcessStartedByStartProcAloneSurvivesATakenAddress(t *testing.T) {
	forEachListeningRole(t, func(t *testing.T, c *libCluster, roles string) {
		plan := portPlan{takes: 2}
		dir := t.TempDir()
		p := startProc(t, procSpec{bin: wrapperFor(t, c.bin, dir, plan), dsn: c.dsn, args: []string{"--roles", roles}, listen: true,
			user: "admin", password: "pw-js102", path: os.Getenv("PATH"), term: true})
		p.until(libraryHang, "the process to answer /healthz", func() bool { return p.code("/healthz") == 204 })
		ownsItsPort(t, c, "startProc alone", p, dir, plan)
		stopsClean(t, "startProc alone", p)
	})
}

// holder is a real process the harness launched that keeps its address, in this go test process or in another one beside it.
func holder(t *testing.T, c *libCluster, beside bool) string {
	t.Helper()
	if !beside {
		p := c.launchOn(c.dsn, "api", "")
		p.ready()
		return p.addr
	}
	file, stop := filepath.Join(t.TempDir(), "addr"), filepath.Join(t.TempDir(), "stop")
	t.Setenv("JS171_HOLDER_ADDR", file)
	t.Setenv("JS171_HOLDER_STOP", stop)
	t.Setenv("JS171_BIN", c.bin)
	t.Setenv("JS171_DSN", c.dsn)
	other := startProc(t, procSpec{bin: os.Args[0], dsn: "x", args: []string{"-test.run=^TestLaunchHolderChild$", "-test.v"}, path: os.Getenv("PATH")})
	// The child test ends itself when the stop file is there, and its own cleanups end its process.
	t.Cleanup(func() {
		os.WriteFile(stop, nil, 0o644) //nolint:errcheck
		select {
		case <-other.exited:
		case <-time.After(time.Minute):
		}
	})
	other.until(libraryHang, "the process beside this one to have its address", func() bool { _, err := os.Stat(file); return err == nil })
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(raw))
}

func TestALaunchedProcessWhoseAddressIsTakenByAnotherProcessTheHarnessLaunchedServesOnAnAddressOfItsOwn(t *testing.T) {
	forEachListeningRole(t, func(t *testing.T, c *libCluster, roles string) {
		for _, beside := range []bool{false, true} {
			t.Run(map[bool]string{false: "in this go test process", true: "in one beside it"}[beside], func(t *testing.T) {
				c := *c
				c.t = t
				plan := portPlan{takes: 1, usurp: holder(t, &c, beside)}
				p, dir := launchTaken(t, &c, roles, plan)
				ownsItsPort(t, &c, "taken by a launched process", p, dir, plan)
				stopsClean(t, "taken by a launched process", p)
			})
		}
	})
}

// All 18 processes launched before any is waited for, as TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries does: 6 api,
// 6 stream and 6 api,stream, with the address of several taken: three of them once (the first of each role set), three three times
// in a row, one of each of two role sets by a process of this go test process, and one by a process beside it.
func TestEighteenProcessesLaunchedAtOnceServeEachOnItsOwnAddressWhenSeveralLoseTheirPort(t *testing.T) {
	c := newLibCluster(t)
	inside, beside := holder(t, c, false), holder(t, c, true)
	roleOf := func(i int) string { return []string{"api", "stream", "api,stream"}[i/6] }
	plans := make([]portPlan, 18)
	for i := range plans {
		switch i {
		case 0, 6, 12:
			plans[i] = portPlan{takes: 1}
		case 1, 7, 13:
			plans[i] = portPlan{takes: 3}
		case 2, 8:
			plans[i] = portPlan{takes: 1, usurp: inside}
		case 14:
			plans[i] = portPlan{takes: 1, usurp: beside}
		}
	}
	procs := make([]*proc, len(plans))
	dirs := make([]string, len(plans))
	for i, plan := range plans {
		dirs[i] = t.TempDir()
		cc := *c
		cc.bin = wrapperFor(t, c.bin, dirs[i], plan)
		procs[i] = cc.launchOn(c.dsn, roleOf(i), "")
	}
	for _, p := range procs {
		p.ready()
	}
	for i, p := range procs {
		ownsItsPort(t, c, "process "+strconv.Itoa(i)+" ("+roleOf(i)+")", p, dirs[i], plans[i])
	}
	seen := map[string]int{}
	for i, p := range procs {
		if j, ok := seen[p.addr]; ok {
			t.Errorf("processes %d and %d serve on the same address %s", j, i, p.addr)
		}
		seen[p.addr] = i
	}
	var stopped sync.WaitGroup
	for _, p := range procs {
		stopped.Add(1)
		go func() { defer stopped.Done(); p.stop() }()
	}
	stopped.Wait()
	for i, p := range procs {
		if p.alive() || p.status != nil {
			t.Errorf("process %d: the exit read is %v, want a clean exit", i, p.status)
		}
	}
}

// --- a start that fails for another reason is not started again, and a port taken at every start is not waited on for ever -------

// childOutcome runs TestLaunchPortChild in the mode and says how it ended and what it said.
func childOutcome(t *testing.T, c *libCluster, mode, roles, dir string) (out string, failed bool) {
	t.Helper()
	t.Setenv("JS171_MODE", mode)
	t.Setenv("JS171_ROLES", roles)
	t.Setenv("JS171_BIN", c.bin)
	t.Setenv("JS171_DSN", c.dsn)
	t.Setenv("JS171_ATTEMPTS", dir)
	p := startProc(t, procSpec{bin: os.Args[0], dsn: "x", args: []string{"-test.run=^TestLaunchPortChild$", "-test.v"}, path: os.Getenv("PATH")})
	select {
	case <-p.exited:
	case <-time.After(3 * time.Minute):
		t.Fatalf("the child test (%s, %s) is still running after three minutes: a launch that hangs:\n%s", mode, roles, p.log)
	}
	return p.log.String(), p.status != nil
}

func TestAProcessThatExitsForAnotherReasonIsStartedOnceAndFailsItsTestAtOnceWithItsLog(t *testing.T) {
	c := newLibCluster(t)
	for _, mode := range []string{"refused-config", "unreachable-database"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			out, failed := childOutcome(t, c, mode, "api", dir)
			if !failed || !strings.Contains(out, "--- FAIL: TestLaunchPortChild") {
				t.Errorf("the launch of a process that exits for another reason did not fail its test:\n%s", out)
			}
			if starts := startsIn(t, dir); len(starts) != 1 {
				t.Errorf("a process that exited for another reason was started %d times, want once: %+v", len(starts), starts)
			}
			if testenvTaken(out[strings.Index(out, "--- FAIL"):]) {
				t.Errorf("a refusal other than a taken port is reported as one:\n%s", out)
			}
			want := map[string]string{"refused-config": "config", "unreachable-database": "connect"}[mode]
			if !strings.Contains(strings.ToLower(out), want) {
				t.Errorf("the failure does not carry the process's log (want %q in it):\n%s", want, out)
			}
		})
	}
}

// maxStarts is the most starts a launch whose address is taken every time may make before its test fails. docs/testing.md states
// the build's own number, which is no larger.
const maxStarts = 10

// A process that exits on a taken address (api) and one that stays alive on it (stream) both end in a failed test that names the
// address, in a few starts, and not in a launch that waits for ever (libraryHang is 90 seconds, and the child is cut at three minutes).
func TestAProcessWhoseAddressIsTakenAtEveryStartFailsItsTestWithinAFewStartsNamingTheAddress(t *testing.T) {
	c := newLibCluster(t)
	for _, roles := range []string{"api", "stream"} {
		t.Run(roles, func(t *testing.T) {
			dir := t.TempDir()
			out, failed := childOutcome(t, c, "always-taken", roles, dir)
			starts := startsIn(t, dir)
			if !failed || !strings.Contains(out, "--- FAIL: TestLaunchPortChild") {
				t.Fatalf("a launch whose address is taken at every start did not fail its test (%d starts):\n%s", len(starts), out)
			}
			if len(starts) < 2 || len(starts) > maxStarts {
				t.Errorf("%d starts, want more than one and at most %d", len(starts), maxStarts)
			}
			last := starts[len(starts)-1].told
			named := false
			for _, line := range regexp.MustCompile(`(?m)^\s+\S+_test\.go:\d+: .*$`).FindAllString(out, -1) {
				named = named || strings.Contains(line, last)
			}
			if !named {
				t.Errorf("the failure's own message does not name the address %s that was taken at the last start:\n%s", last, out)
			}
		})
	}
}

// TestLaunchPortChild is what the two checks above run as a child. Without a mode it does nothing.
func TestLaunchPortChild(t *testing.T) {
	mode := os.Getenv("JS171_MODE")
	if mode == "" {
		return
	}
	c := &libCluster{t: t, bin: os.Getenv("JS171_BIN"), dsn: os.Getenv("JS171_DSN")}
	config, dsn, plan := "", c.dsn, portPlan{}
	switch mode {
	case "refused-config":
		config = `{"libraries": [`
	case "unreachable-database":
		dsn = "postgres://nobody:none@127.0.0.1:1/none"
	case "always-taken":
		plan.takes = 1000
	default:
		t.Fatalf("no such mode %q", mode)
	}
	cc := *c
	cc.bin = wrapperFor(t, c.bin, os.Getenv("JS171_ATTEMPTS"), plan)
	p := cc.launchOn(dsn, os.Getenv("JS171_ROLES"), config)
	p.ready()
}

// --- the wrapper, the squatter and the holder: this test binary run again ---------------------------------------------------------

// TestLaunchWrapperChild is the jellyscale binary of these checks. Run by the wrapper script, it records its start, and then
// takes the start's address (or tells it another's) and runs the real binary as a child, or becomes the real binary.
func TestLaunchWrapperChild(t *testing.T) {
	dir := os.Getenv("JS171_DIR")
	if dir == "" {
		return
	}
	runtime.LockOSThread() // Pdeathsig below is tied to the thread that starts the child.
	args := os.Args[1:]
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	real := os.Getenv("JS171_REAL")
	takes, _ := strconv.Atoi(os.Getenv("JS171_TAKES"))
	chosen, at, ok := testenv.ListenArg(args)
	if !ok {
		t.Fatal("no listen address in the arguments")
	}
	told, n := chosen, 0
	for {
		n++
		f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("start-%d", n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		taken := n <= takes
		if taken && os.Getenv("JS171_USURP") != "" {
			told = os.Getenv("JS171_USURP")
		}
		fmt.Fprintf(f, "%d %s %s", os.Getpid(), chosen, told)
		f.Close()
		break
	}
	if n > takes {
		syscall.Exec(real, append([]string{real}, args...), os.Environ()) //nolint:errcheck
		t.Fatal("exec of the real binary failed")
	}
	if os.Getenv("JS171_USURP") == "" {
		// The squatter is started through a shell that leaves at once, so that it is not this process's descendant: a launch that
		// asks who owns the address must find a process that is not the one it launched.
		ready := filepath.Join(dir, fmt.Sprintf("squatter-%d-ready", n))
		null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		sh, err := os.StartProcess("/bin/sh", []string{"sh", "-c", `"$0" -test.run='^TestLaunchSquatterChild$' </dev/null >/dev/null 2>&1 &`, os.Args[0]},
			&os.ProcAttr{Env: append(os.Environ(), "JS171_SQUAT="+chosen, "JS171_READY="+ready, "JS171_PIDFILE="+filepath.Join(dir, fmt.Sprintf("squatter-%d", n))), Files: []*os.File{null, null, null}})
		if err != nil {
			t.Fatal(err)
		}
		sh.Wait() //nolint:errcheck
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the squatter never took the address")
			}
		}
	}
	args = append([]string(nil), args...)
	args[at] = told
	child, err := os.StartProcess(real, append([]string{real}, args...), &os.ProcAttr{Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys: &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("child-%d", n)), []byte(strconv.Itoa(child.Pid)), 0o644) //nolint:errcheck
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range term {
			child.Signal(sig) //nolint:errcheck
		}
	}()
	state, err := child.Wait()
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(state.ExitCode())
}

// TestLaunchSquatterChild holds an address with a real socket for four minutes (longer than any wait of a launch, which a check
// cuts at three), so that the child's bind meets it however late the child gets to it on a loaded machine, and the address stays
// foreign for as long as a launch looks at it. The checks' cleanup ends it.
func TestLaunchSquatterChild(t *testing.T) {
	addr := os.Getenv("JS171_SQUAT")
	if addr == "" {
		return
	}
	stop, err := testenv.Squat(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	os.WriteFile(os.Getenv("JS171_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o644) //nolint:errcheck
	os.WriteFile(os.Getenv("JS171_READY"), nil, 0o644)                                 //nolint:errcheck
	time.Sleep(4 * time.Minute)
}

// TestLaunchHolderChild is a go test process beside the one that runs the checks: it launches a real process through the same
// harness, says its address in a file and keeps it until the stop file is there.
func TestLaunchHolderChild(t *testing.T) {
	file := os.Getenv("JS171_HOLDER_ADDR")
	if file == "" {
		return
	}
	c := &libCluster{t: t, bin: os.Getenv("JS171_BIN"), dsn: os.Getenv("JS171_DSN")}
	p := c.launchOn(c.dsn, "api", "")
	p.ready()
	if err := os.WriteFile(file, []byte(p.addr), 0o644); err != nil {
		t.Fatal(err)
	}
	for stop := os.Getenv("JS171_HOLDER_STOP"); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(stop); err == nil {
			return
		}
		if !p.alive() {
			t.Fatalf("the process exited:\n%s", p.log)
		}
	}
}
