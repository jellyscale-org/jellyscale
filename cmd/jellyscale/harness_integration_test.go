//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/testenv"
)

// JS-114: the one place these tests start a jellyscale process, read its log, ask the kernel for a port and sign in.

// logSink is a log that a process (on stdout and stderr at once) and the test share.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// records are the lines that are JSON objects, as a slog JSON handler writes them.
func (l *logSink) records() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		var record map[string]any
		if len(line) > 0 && json.Unmarshal(line, &record) == nil {
			out = append(out, record)
		}
	}
	return out
}

// freeAddr is an address on this machine that the kernel chose and no socket holds just now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// procSpec is how a process starts.
type procSpec struct {
	bin, dsn       string
	args           []string // Without --listen and --config.
	config         string   // The text of the config file; "" is no file.
	listen         bool     // It gets --listen on a free address.
	user, password string   // Its admin (the password goes in the environment); "" is none.
	path           string   // Its PATH; "" is a directory that holds no ffmpeg, as a replica's in production.
	term           bool     // The test's end stops it with SIGTERM and waits, not SIGKILL.
}

// proc is one running jellyscale process.
type proc struct {
	t        *testing.T
	cmd      *exec.Cmd
	addr     string // "" when it was not asked to listen.
	log      *logSink
	exited   chan struct{}
	status   error // What Wait said, once exited is closed.
	user     string
	password string
	term     bool
	token    string // Of a test that signs in once.
	id       string // What it calls itself in session_sockets.replica, once a socket on it has been seen.
	version  int    // Of the build, for the tests that run two.
	roles    string
	workerID string
	spec     procSpec   // With the config's path in its arguments.
	dir      string     // Where its config is.
	begun    time.Time  // When the running start was made.
	unbound  time.Time  // When the running start was first seen to say it listens with no socket listening.
	lost     [][2]int64 // The windows (UnixNano) of the starts that lost their port.
}

// startProc starts the process and returns without waiting for it. However the test ends, the process does not
// outlive it. A process that loses its port (see until) is started again on another address; p.addr, p.cmd, p.log and
// p.exited are those of the start that is running.
func startProc(t *testing.T, s procSpec) *proc {
	t.Helper()
	testenv.NoWorkersLeft(t, s.dsn) // First, so it runs after the process is stopped: its worker is gone then (JS-158).
	p := &proc{t: t, spec: s, dir: t.TempDir(), user: s.user, password: s.password, term: s.term}
	p.spec.args = append([]string(nil), s.args...)
	if s.config != "" {
		path := filepath.Join(p.dir, "config.json")
		if err := os.WriteFile(path, []byte(s.config), 0o644); err != nil {
			t.Fatal(err)
		}
		p.spec.args = append(p.spec.args, "--config", path)
	}
	p.begin()
	t.Cleanup(func() {
		if p.term {
			p.stop()
		} else {
			p.kill()
		}
	})
	return p
}

// begin makes one start of the process, on an address of its own if it listens.
func (p *proc) begin() {
	p.t.Helper()
	s := p.spec
	args := append([]string(nil), s.args...)
	p.addr = ""
	if s.listen {
		p.addr = freeAddr(p.t)
		args = append(args, "--listen", p.addr)
	}
	path := s.path
	if path == "" {
		path = p.dir
	}
	p.log, p.exited, p.status = &logSink{}, make(chan struct{}), nil
	cmd := exec.Command(s.bin, args...)
	cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+s.dsn, "PATH="+path)
	if s.password != "" {
		cmd.Env = append(cmd.Env, "JELLYSCALE_ADMIN_PASSWORD="+s.password)
	}
	cmd.Stdout, cmd.Stderr = p.log, p.log
	p.begun, p.unbound = time.Now(), time.Time{}
	if err := cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	p.cmd = cmd
	exited := p.exited
	go func() { p.status = cmd.Wait(); close(exited) }()
}

// until waits for ready, polling; the process exiting first fails the test at once with its log. Its address is chosen before
// it binds, so another socket can take it first (merge-queue run 44983050791): a start that says it could not bind, or whose
// address is held by a process that is not its own, is ended and started again on a new address, at most launchStarts starts
// in all, and nothing of it is left for the test to read.
func (p *proc) until(limit time.Duration, what string, ready func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if p.lostPort() {
			p.restart()
			continue
		}
		bound := true // Nothing is known to hold its address that should not, or it binds none.
		if p.binds() {
			switch p.owner() {
			case ownerForeign:
				p.restart()
				continue
			case ownerNone:
				bound = false
				if p.unbound.IsZero() {
					p.unbound = time.Now()
				} else if time.Since(p.unbound) > bindGrace {
					if strings.Contains(p.log.String(), "msg=listening") {
						p.restart() // It says it listens and nothing does: a start that failed to bind and is stuck on its way out.
						continue
					}
					bound = true // It binds nothing a socket shows, as a stand-in for the binary does not.
				}
			}
		}
		if bound && ready() {
			p.forgetLostStarts()
			return
		}
		if !p.alive() {
			if p.lostPort() { // It said so and exited after the read at the top of this loop.
				p.restart()
				continue
			}
			p.t.Fatalf("the process exited (%v) while waiting for %s:\n%s", p.status, what, p.log)
		}
		if time.Now().After(deadline) {
			if p.lostPort() {
				p.restart()
				continue
			}
			p.t.Fatalf("timed out after %v waiting for %s:\n%s", limit, what, p.log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// startRetrying starts the process and waits until ready says it is up, for a minute at most. The address comes
// from freeAddr, which closes its listener before the binary binds the port a moment later; with dozens of
// processes and pools opening connections from the same ephemeral range, another socket can take it in
// between. That is the test's race, not the server's: a process that exited with "address already in use" is
// started again on a new port, in five starts at most. Any other exit fails the test at once with the log.
func startRetrying(t *testing.T, s procSpec, ready func(*proc) bool) *proc {
	t.Helper()
	const starts = 5
	for attempt := 1; ; attempt++ {
		p := startProc(t, s)
		deadline := time.Now().Add(time.Minute)
		for p.alive() {
			if ready(p) {
				return p
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out after a minute waiting for the process to have started:\n%s", p.log)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !strings.Contains(p.log.String(), "address already in use") {
			t.Fatalf("the process exited (%v) while starting:\n%s", p.status, p.log)
		}
		if attempt == starts {
			t.Fatalf("--listen was taken by another socket in %d starts in a row", attempt)
		}
	}
}

func (p *proc) url(path string) string { return "http://" + p.addr + path }

func (p *proc) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *proc) sigterm() { p.cmd.Process.Signal(syscall.SIGTERM) } //nolint:errcheck

func (p *proc) kill() {
	p.cmd.Process.Kill() //nolint:errcheck
	<-p.exited
}

// stop ends the process as an orchestrator does, with SIGTERM, and kills it if it has not gone after a minute.
func (p *proc) stop() {
	if !p.alive() {
		return
	}
	p.sigterm()
	select {
	case <-p.exited:
	case <-time.After(time.Minute):
		p.kill()
	}
}

// signIn is one device's sign-in as the admin, on a device of its own: a token that stays valid beside the
// others'.
func (p *proc) signIn(t *testing.T) string {
	t.Helper()
	return p.signInAs(t, uuid.NewString())
}

// signInAs is signIn on a device the test names, for the one that must find it again. A failed sign-in fails
// the test at once with the status and body.
func (p *proc) signInAs(t *testing.T, device string) string {
	t.Helper()
	status, raw := p.authenticate(t, device, p.user, p.password)
	var reply struct{ AccessToken string }
	if status != 200 || json.Unmarshal(raw, &reply) != nil || reply.AccessToken == "" {
		t.Fatalf("sign-in: %d %s", status, raw)
	}
	return reply.AccessToken
}

// authenticate signs a user in on a device and gives the whole answer, for a test of what a sign-in returns.
func (p *proc) authenticate(t *testing.T, device, user, password string) (int, []byte) {
	t.Helper()
	got, err := p.signInReply(device, user, password)
	if err != nil {
		t.Fatal(err)
	}
	return got.status, got.body
}

// signInReply is one sign-in attempt with the headers the test adds (name, value pairs), whole: status, headers and
// body. A transport error is returned, so that attempts made from goroutines can be counted.
type signInReply struct {
	status int
	header http.Header
	body   []byte
}

// signInPath is the sign-in route, spelled once in this package's tests (JS-114).
const signInPath = "/Users/AuthenticateByName"

func (p *proc) signInReply(device, user, password string, headers ...string) (signInReply, error) {
	return p.signInReplyAt(signInPath, device, user, password, headers...)
}

// signInReplyAt is signInReply at the path the test names: the route in another letter case, or with a trailing slash.
func (p *proc) signInReplyAt(path, device, user, password string, headers ...string) (signInReply, error) {
	body, _ := json.Marshal(map[string]string{"Username": user, "Pw": password})
	return p.signInBody(path, device, body, headers...)
}

// signInBody posts the bytes the test names, JSON or not.
func (p *proc) signInBody(path, device string, body []byte, headers ...string) (signInReply, error) {
	request, err := http.NewRequest("POST", p.url(path), bytes.NewReader(body))
	if err != nil {
		return signInReply{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="c", Device="d", DeviceId="%s", Version="1"`, device))
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return signInReply{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return signInReply{response.StatusCode, response.Header, raw}, nil
}
