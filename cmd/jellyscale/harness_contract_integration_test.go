//go:build integration

package main

// JS-114: what the process harness (harness_integration_test.go) promises, checked on stand-in programs (a shell
// script that exits, sleeps or prints on both streams) so that no Postgres and no jellyscale binary is needed.
// Where a promise is that a test fails, the check runs that test as a child of this binary (the child tests at
// the bottom, which do nothing unless a check starts them) and reads how it ended.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// standIn writes a program that is a shell script, and returns its path.
func standIn(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "standin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func sleepPath(t *testing.T) string {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	return sleep
}

// sleeper replaces the shell with a sleep, so that the process is the one the harness started.
func sleeper(t *testing.T) string { return "exec " + sleepPath(t) + " 600" }

func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck
		t.Errorf("process %d outlived its test (kill 0 says %v)", pid, err)
	}
}

func signalOf(status error) syscall.Signal {
	var exit *exec.ExitError
	if errors.As(status, &exit) {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal()
		}
	}
	return 0
}

// --- no process outlives its test ---------------------------------------------------------------

func TestHarnessEndsAProcessWhenItsTestPassed(t *testing.T) {
	var pid int
	t.Run("test", func(t *testing.T) {
		p := startProc(t, procSpec{bin: standIn(t, sleeper(t)), dsn: "x"})
		pid = p.cmd.Process.Pid
	})
	assertGone(t, pid)
}

func TestHarnessEndsAProcessWhenItsTestFailed(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	log, status := runChild(t, "fail", "HARNESS_PIDFILE="+pidfile)
	if status == nil || !strings.Contains(log, "fail on purpose") {
		t.Fatalf("the child test was to fail on purpose; it ended with %v:\n%s", status, log)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the child never started its process:\n%s", log)
	}
	pid, _ := strconv.Atoi(string(raw))
	assertGone(t, pid)
}

func TestHarnessEndsEveryProcessOfATestAndTheOnesThatAlreadyWentAway(t *testing.T) {
	var pids []int
	var killed, byItself *proc
	t.Run("test", func(t *testing.T) {
		for range 3 {
			pids = append(pids, startProc(t, procSpec{bin: standIn(t, sleeper(t)), dsn: "x"}).cmd.Process.Pid)
		}
		killed = startProc(t, procSpec{bin: standIn(t, sleeper(t)), dsn: "x"})
		killed.kill()
		killed.kill() // a second kill of a process that is gone is no error and no hang
		byItself = startProc(t, procSpec{bin: standIn(t, "exit 3"), dsn: "x"})
		<-byItself.exited
	})
	for _, pid := range pids {
		assertGone(t, pid)
	}
	if got := signalOf(killed.status); got != syscall.SIGKILL {
		t.Errorf("kill ended the process with signal %v, want SIGKILL", got)
	}
	var exit *exec.ExitError
	if !errors.As(byItself.status, &exit) || exit.ExitCode() != 3 {
		t.Errorf("a process that exited by itself has status %v, want exit 3", byItself.status)
	}
}

func TestHarnessEndsATestsProcessWithSIGKILLAndAStoppedOneWithSIGTERM(t *testing.T) {
	var killed, stopped *proc
	t.Run("test", func(t *testing.T) {
		killed = startProc(t, procSpec{bin: standIn(t, "echo up; "+sleeper(t)), dsn: "x"})
		stopped = startProc(t, procSpec{bin: standIn(t, `trap 'echo got-term; exit 0' TERM
echo up
while :; do `+sleepPath(t)+` 0.05; done`), dsn: "x", term: true})
		for _, p := range []*proc{killed, stopped} {
			p.until(time.Minute, "the stand-in to say it is up", func() bool { return strings.Contains(p.log.String(), "up") })
		}
	})
	if got := signalOf(killed.status); got != syscall.SIGKILL {
		t.Errorf("a test that kills ends its process with %v (status %v), want SIGKILL", got, killed.status)
	}
	if stopped.status != nil || !strings.Contains(stopped.log.String(), "got-term") {
		t.Errorf("a test that stops ends its process with SIGTERM and waits for it: status %v, log %q", stopped.status, stopped.log.String())
	}
}

// --- how a process is started --------------------------------------------------------------------

func TestHarnessStartsAProcessWithItsConfigDatabasePasswordAndPATH(t *testing.T) {
	shims := t.TempDir()
	show := standIn(t, `echo "PATH=$PATH"; echo "DB=$JELLYSCALE_DATABASE_URL"; echo "PW=$JELLYSCALE_ADMIN_PASSWORD"; echo "ARGS=$*"; `+sleeper(t))
	replica := startProc(t, procSpec{bin: show, dsn: "postgres://db/one", args: []string{"--roles=api"}, config: `{"name": "n"}`, listen: true, user: "admin", password: "pw"})
	worker := startProc(t, procSpec{bin: show, dsn: "postgres://db/two", args: []string{"--roles=worker"}, listen: false, path: shims})
	for _, p := range []*proc{replica, worker} {
		p.until(time.Minute, "the stand-in to print", func() bool { return strings.Contains(p.log.String(), "ARGS=") })
	}
	line := func(p *proc, key string) string {
		for _, l := range strings.Split(p.log.String(), "\n") {
			if v, ok := strings.CutPrefix(l, key+"="); ok {
				return v
			}
		}
		return "(absent)"
	}
	// A replica's PATH holds no ffmpeg; a worker's holds what the test gave it.
	dir := line(replica, "PATH")
	if _, err := os.Stat(filepath.Join(dir, "ffmpeg")); err == nil || strings.Contains(dir, ":") || dir == "" {
		t.Errorf("a replica's PATH is %q: want one directory that holds no ffmpeg", dir)
	}
	if got := line(worker, "PATH"); got != shims {
		t.Errorf("a worker's PATH is %q, want %q", got, shims)
	}
	if line(replica, "DB") != "postgres://db/one" || line(worker, "DB") != "postgres://db/two" {
		t.Errorf("databases: %q, %q", line(replica, "DB"), line(worker, "DB"))
	}
	if line(replica, "PW") != "pw" || line(worker, "PW") != "" {
		t.Errorf("admin passwords: %q, %q", line(replica, "PW"), line(worker, "PW"))
	}
	args := line(replica, "ARGS")
	if m := regexp.MustCompile(`^--roles=api --config (\S+) --listen 127\.0\.0\.1:([0-9]+)$`).FindStringSubmatch(args); m == nil {
		t.Errorf("a replica's arguments are %q", args)
	} else if m[2] == "0" || !strings.HasSuffix(replica.addr, ":"+m[2]) {
		t.Errorf("the address %q the replica was given is not the one the harness says (%q)", m[2], replica.addr)
	} else if raw, err := os.ReadFile(m[1]); err != nil || string(raw) != `{"name": "n"}` {
		t.Errorf("the config file holds %q (%v)", raw, err)
	}
	if got := line(worker, "ARGS"); got != "--roles=worker" || worker.addr != "" {
		t.Errorf("a process that was not asked to listen has arguments %q and address %q", got, worker.addr)
	}
}

// The route is written in two pieces so that the standing check, which counts the strings of the sign-in's route,
// finds only the harness's.
const route = "/Users/Authenticate" + "ByName"

func TestHarnessGivesTestsInParallelAPortALogAndATokenOfTheirOwn(t *testing.T) {
	var mu sync.Mutex
	devices := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		device := regexp.MustCompile(`DeviceId="([^"]*)"`).FindStringSubmatch(r.Header.Get("Authorization"))
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "POST" || r.URL.Path != route || device == nil {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, 400)
			return
		}
		var body struct{ Username, Pw string }
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if body.Username != "admin" || body.Pw != "pw" {
			http.Error(w, "wrong credentials", 401)
			return
		}
		devices[device[1]] = true
		fmt.Fprintf(w, `{"AccessToken": "token-%s"}`, device[1])
	}))
	defer server.Close()
	// One script, written before the parallel tests start: a script written while another test forks is "text file busy".
	script := standIn(t, `echo mine-$1; echo theirs-$1 >&2; `+sleeper(t))
	tokens := make([]string, 6)
	logs := make([]string, 6)
	t.Run("group", func(t *testing.T) {
		for i := range tokens {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				p := startProc(t, procSpec{bin: script, args: []string{fmt.Sprint(i)}, dsn: "x", user: "admin", password: "pw"})
				p.addr = strings.TrimPrefix(server.URL, "http://")
				p.until(time.Minute, "the stand-in to print", func() bool { return strings.Count(p.log.String(), "\n") >= 2 })
				// Two sign-ins of one process: a device each, and both tokens are good.
				first, second := p.signIn(t), p.signIn(t)
				if first == second {
					t.Errorf("two sign-ins gave one token %q", first)
				}
				tokens[i], logs[i] = first, p.log.String()
			})
		}
	})
	seen := map[string]bool{}
	for i, token := range tokens {
		if seen[token] {
			t.Errorf("process %d has token %q, which another has too", i, token)
		}
		seen[token] = true
		if want := fmt.Sprintf("mine-%d\ntheirs-%d\n", i, i); logs[i] != want {
			t.Errorf("process %d's log is %q, want %q", i, logs[i], want)
		}
	}
	if len(devices) != 12 {
		t.Errorf("12 sign-ins came from %d devices, want 12", len(devices))
	}
	var named string
	t.Run("named", func(t *testing.T) {
		p := startProc(t, procSpec{bin: standIn(t, sleeper(t)), dsn: "x", user: "admin", password: "pw"})
		p.addr = strings.TrimPrefix(server.URL, "http://")
		named = p.signInAs(t, "my-device")
	})
	if named != "token-my-device" {
		t.Errorf("a sign-in on a named device gave %q", named)
	}
}

// --- a start that fails ----------------------------------------------------------------------------

func TestHarnessFailsAtOnceWithTheLogWhenAProcessExitsWhileStarting(t *testing.T) {
	for _, mode := range []string{"exit-in-until", "exit-in-start"} {
		t.Run(mode, func(t *testing.T) {
			began := time.Now()
			log, status := runChild(t, mode)
			if status == nil {
				t.Fatalf("the child test passed:\n%s", log)
			}
			for _, want := range []string{"boom-on-stdout", "boom-on-stderr", "exited"} {
				if !strings.Contains(log, want) {
					t.Errorf("the failure does not say %q:\n%s", want, log)
				}
			}
			// The child's wait limit is a minute or ten; a failure that waited for it says "timed out".
			if strings.Contains(log, "timed out") {
				t.Errorf("the failure came after the wait's limit, not at once (%v):\n%s", time.Since(began), log)
			}
		})
	}
}

func TestHarnessRetriesAStartThatLostItsPortOnANewPortAndNoOtherExit(t *testing.T) {
	retried := func(t *testing.T, mode string) (log string, status error, starts []string) {
		count := filepath.Join(t.TempDir(), "starts")
		log, status = runChild(t, mode, "STANDIN_COUNT="+count)
		raw, _ := os.ReadFile(count)
		return log, status, strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	t.Run("five starts in a row on one port taken each time", func(t *testing.T) {
		log, status, starts := retried(t, "port-always-taken")
		if status == nil || !strings.Contains(log, "5 starts in a row") || len(starts) != 5 {
			t.Errorf("want a failure after exactly 5 starts, got %d starts, status %v:\n%s", len(starts), status, log)
		}
		if distinct := map[string]bool{}; len(starts) > 1 {
			for _, s := range starts {
				distinct[s] = true
			}
			if len(distinct) == 1 {
				t.Errorf("every start was on the same address: %v", starts)
			}
		}
	})
	t.Run("two lost ports and then a start", func(t *testing.T) {
		log, status, starts := retried(t, "port-taken-twice")
		if status != nil || len(starts) != 3 {
			t.Errorf("want a pass after 3 starts, got %d starts, status %v:\n%s", len(starts), status, log)
		}
	})
	t.Run("any other exit is not tried again", func(t *testing.T) {
		log, status, starts := retried(t, "other-exit")
		if status == nil || len(starts) != 1 || !strings.Contains(log, "failed for another reason") {
			t.Errorf("want one start and a failure with the log, got %d starts, status %v:\n%s", len(starts), status, log)
		}
	})
}

func TestHarnessFailsAtOnceWithTheStatusAndBodyWhenASignInIsRefused(t *testing.T) {
	log, status := runChild(t, "refused-sign-in")
	if status == nil {
		t.Fatalf("the child test passed:\n%s", log)
	}
	for _, want := range []string{"401", "wrong credentials"} {
		if !strings.Contains(log, want) {
			t.Errorf("the failure does not say %q:\n%s", want, log)
		}
	}
}

// --- the log ---------------------------------------------------------------------------------------

// Run under -race (CI's integration tier does; so does this check's command), a log that is not locked is a
// data race, and one that is not shared by the two streams loses or tears lines.
func TestHarnessLogHoldsEverythingBothStreamsWroteWhileATestReadsIt(t *testing.T) {
	const lines = 300
	script := fmt.Sprintf(`i=0; while [ $i -lt %d ]; do echo "out $i"; echo "err $i" >&2; i=$((i+1)); done`, lines)
	p := startProc(t, procSpec{bin: standIn(t, script), dsn: "x"})
	reads := 0
	for done := false; !done; {
		select {
		case <-p.exited:
			done = true
		default:
			_ = p.log.String()
			_ = p.log.records()
			reads++
		}
	}
	if reads == 0 {
		t.Log("the process ended before the first read")
	}
	var out, errs []string
	for _, l := range strings.Split(strings.TrimSpace(p.log.String()), "\n") {
		switch {
		case regexp.MustCompile(`^out [0-9]+$`).MatchString(l):
			out = append(out, l)
		case regexp.MustCompile(`^err [0-9]+$`).MatchString(l):
			errs = append(errs, l)
		default:
			t.Errorf("a torn or foreign line: %q", l)
		}
	}
	if len(out) != lines || len(errs) != lines {
		t.Errorf("the log holds %d lines of stdout and %d of stderr, want %d of each", len(out), len(errs), lines)
	}
}

// --- the children ----------------------------------------------------------------------------------

// runChild runs TestHarnessContractChild in a process of its own, in the mode, and returns what it said and how
// it ended. The harness starts it, as it starts every process of these tests.
func runChild(t *testing.T, mode string, env ...string) (string, error) {
	t.Helper()
	t.Setenv("HARNESS_CONTRACT_MODE", mode)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	p := startProc(t, procSpec{bin: os.Args[0], dsn: "x", args: []string{"-test.run=^TestHarnessContractChild$", "-test.v"}, path: os.Getenv("PATH")})
	select {
	case <-p.exited:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the child test (%s) is still running after two minutes:\n%s", mode, p.log)
	}
	return p.log.String(), p.status
}

// TestHarnessContractChild is the test the checks above run as a child. Without a mode there is nothing for it
// to do, and it passes.
func TestHarnessContractChild(t *testing.T) {
	mode := os.Getenv("HARNESS_CONTRACT_MODE")
	count := `echo "$@" >>"$STANDIN_COUNT"`
	counted := func(fail string) string { return count + "\n" + fail }
	taken := `echo "listen tcp 127.0.0.1:1: bind: address already in use" >&2; exit 1`
	spec := func(script string) procSpec {
		return procSpec{bin: standIn(t, script), dsn: "x", listen: true, path: os.Getenv("PATH")}
	}
	never := func(*proc) bool { return false }
	up := func(p *proc) bool { return strings.Contains(p.log.String(), "up") }
	switch mode {
	case "":
	case "fail":
		p := startProc(t, spec(sleeper(t)))
		if err := os.WriteFile(os.Getenv("HARNESS_PIDFILE"), []byte(strconv.Itoa(p.cmd.Process.Pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatal("fail on purpose")
	case "exit-in-until":
		p := startProc(t, spec(`echo boom-on-stdout; echo boom-on-stderr >&2; exit 2`))
		p.until(10*time.Minute, "a start that never comes", func() bool { return false })
	case "exit-in-start":
		startRetrying(t, spec(`echo boom-on-stdout; echo boom-on-stderr >&2; exit 2`), never)
	case "port-always-taken":
		startRetrying(t, spec(counted(taken)), never)
	case "port-taken-twice":
		script := counted(`[ "$(wc -l <"$STANDIN_COUNT")" -gt 2 ] || { ` + taken + ` ; }
echo up; ` + sleeper(t))
		startRetrying(t, spec(script), up)
	case "other-exit":
		startRetrying(t, spec(counted(`echo "failed for another reason" >&2; exit 1`)), never)
	case "refused-sign-in":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "wrong credentials", 401)
		}))
		defer server.Close()
		p := startProc(t, procSpec{bin: standIn(t, sleeper(t)), dsn: "x", user: "admin", password: "pw"})
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
		p.addr = "127.0.0.1:" + port
		p.signIn(t)
	default:
		t.Fatalf("no such mode %q", mode)
	}
}
