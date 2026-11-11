//go:build integration

package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// These tests kill, stop and cut off real worker processes. A worker is the
// test binary run again (TestMain sees JELLYSCALE_TEST_WORKER), so a SIGKILL
// is a real one and the kernel closes its sockets as it would in production.

// workerProcess is the body of such a process.
func workerProcess(id string) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("worker", id)
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "worker %s: %s: %v\n", id, what, err)
		os.Exit(1)
	}
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		fail("database", err)
	}
	duration := func(name string, fallback time.Duration) time.Duration {
		if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
			return d
		}
		return fallback
	}
	var media store.Cache
	w := &Worker{ID: id, Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Log: log, Media: media.Open,
		Segments: store.NewPosix(os.Getenv("JELLYSCALE_TEST_SEGMENTS")), Scratch: os.Getenv("JELLYSCALE_TEST_SCRATCH"),
		Slots: 2, Idle: time.Minute, Lead: time.Hour,
		Lease: duration("JELLYSCALE_TEST_LEASE", 10*time.Second), Heartbeat: duration("JELLYSCALE_TEST_HEARTBEAT", 3*time.Second)}
	helperSettings(w)
	if addr := os.Getenv("JELLYSCALE_TEST_LIVENESS"); addr != "" {
		if w.LivenessConfig, err = through(dsn, addr); err != nil {
			fail("liveness address", err)
		}
	}
	w.Hub = &Hub{Pool: pool, Log: log}
	go w.Hub.Run(ctx)
	go w.Present(ctx, "worker", "", 200*time.Millisecond)
	if os.Getenv("JELLYSCALE_TEST_SUBTITLES") != "" {
		go w.RunSubtitles(ctx) // As main does; a test of subtitle jobs asks for it.
	}
	w.Run(ctx)
}

// through is the connection config of dsn, to the server at addr instead.
func through(dsn, addr string) (*pgx.ConnConfig, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	u.Host = addr
	return pgx.ParseConfig(u.String())
}

// workerProcess starts a worker process; the test ends it.
func (r *rig) workerProcess(t *testing.T, id string, segmentsDir string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER="+id, "JELLYSCALE_TEST_SEGMENTS="+segmentsDir, "JELLYSCALE_TEST_SCRATCH="+r.scratch)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	reaped := make(chan struct{})
	go func() { cmd.Wait(); close(reaped) }() //nolint:errcheck
	t.Cleanup(func() {
		once.Do(func() { cmd.Process.Signal(syscall.SIGCONT); cmd.Process.Kill() }) //nolint:errcheck
		<-reaped
	})
	waitFor(t, "worker "+id+" to hold its lock and register", 30*time.Second, func() bool { return r.locked(id) })
	return cmd
}

// locked says whether the node has registered as holding its lock.
func (r *rig) locked(id string) bool {
	var n int
	r.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_nodes WHERE id = $1 AND locked", id).Scan(&n) //nolint:errcheck
	return n == 1
}

// owner is the worker of the spec's running job, or "".
func (r *rig) owner(t *testing.T) string {
	t.Helper()
	var worker *string
	r.pool.QueryRow(context.Background(), "SELECT worker FROM transcode_jobs WHERE state = 'running' ORDER BY id DESC LIMIT 1").Scan(&worker) //nolint:errcheck
	if worker == nil {
		return ""
	}
	return *worker
}

// slowAt makes ffmpeg read its input rate times faster than real time.
func slowAt(t *testing.T, rate string) {
	t.Helper()
	film(t)
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + real + " -readrate " + rate + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reference is segments 0 to upTo of an uninterrupted run, by the same command.
func (r *rig) reference(t *testing.T, upTo int) string {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cmd, err := Command(ctx, filepath.Join(film(t), "film.mkv"), r.spec, 0, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reference run", time.Minute, func() bool {
		_, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.m4s", upTo+1)))
		return err == nil
	})
	cancel()
	cmd.Wait() //nolint:errcheck
	return dir
}

// watchOwners samples until the test ends how many ffmpeg run, and which
// workers each job has had. A job is one worker's, and one ffmpeg runs at a
// time for the one spec: two would be two owners.
type owners struct {
	mu        sync.Mutex
	most      int
	workers   map[int64]map[string]bool
	stop      func()
	whenFirst map[int]time.Time // When ffmpeg first ran at each count.
}

func (r *rig) watchOwners(t *testing.T) *owners {
	o := &owners{workers: map[int64]map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			n := len(r.ffmpegProcesses())
			rows, err := r.pool.Query(ctx, "SELECT id, COALESCE(worker, '') FROM transcode_jobs WHERE state = 'running'")
			o.mu.Lock()
			o.most = max(o.most, n)
			for err == nil && rows.Next() {
				var id int64
				var worker string
				if rows.Scan(&id, &worker) == nil {
					if o.workers[id] == nil {
						o.workers[id] = map[string]bool{}
					}
					o.workers[id][worker] = true
				}
			}
			o.mu.Unlock()
			if err == nil {
				rows.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	o.stop = func() { cancel(); <-done }
	t.Cleanup(o.stop)
	return o
}

// check fails if a second ffmpeg ever ran beside the first, or a job was ever
// running under two workers.
func (o *owners) check(t *testing.T) {
	t.Helper()
	o.stop()
	if o.most > 1 {
		t.Errorf("%d ffmpeg ran at once for one spec", o.most)
	}
	for id, workers := range o.workers {
		if len(workers) > 1 {
			t.Errorf("job %d was running under %d workers: %v", id, len(workers), workers)
		}
	}
}

// A worker killed in the middle of a film is replaced by the other, which goes
// on at the first missing segment within seconds, not when the dead one's lease
// runs out, and the segments are those of a run that was never interrupted.
func TestAKilledWorkerIsReplacedWithinSeconds(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	const last = 7
	reference := r.reference(t, last)
	ffmpeg := testenv.FFmpeg(t) // Before the shim, which would slow the check of the result.
	slowAt(t, "2")
	a := r.workerProcess(t, "worker-a", segments)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if got := r.owner(t); got != "worker-a" {
		t.Fatalf("the job is worker-a's, not %q", got)
	}
	r.workerProcess(t, "worker-b", segments)
	owned := r.watchOwners(t)

	var killed time.Time
	var after time.Duration
	for n := 1; n <= last; n++ {
		if n == 3 {
			// The viewer has segment 2 and now waits for 3, which a worker is making.
			killed = time.Now()
			a.Process.Kill() //nolint:errcheck
		}
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
		if n == 3 {
			after = time.Since(killed)
		}
	}
	t.Logf("SIGKILL to the replacement's first segment: %v", after.Round(10*time.Millisecond))
	// Logged, not judged: a slower runner takes longer on a right build. That the
	// replacement does not wait out the dead worker's lease is checked by
	// TestAKilledWorkersJobIsClaimedBeforeItsLeaseRunsOut (mechanism_integration_test.go).
	if got := r.owner(t); got != "worker-b" {
		t.Errorf("the job is %q's, want worker-b's", got)
	}
	owned.check(t)
	// A re-encode is not the same bytes twice, even from the start (x264's rate
	// control depends on timing), so splice is what is compared, as the seam
	// test does: the init segment's bytes, and every frame and audio packet at
	// its time, with the decoder content with it.
	stored := t.TempDir()
	for n := 0; n <= last; n++ {
		body, err := os.ReadFile(filepath.Join(segments, SegmentKey(r.hash, n)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stored, fmt.Sprintf("%d.m4s", n)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	init := readFile(t, filepath.Join(segments, SegmentKey(r.hash, -1)))
	if !bytes.Equal(init, readFile(t, filepath.Join(reference, "init.mp4"))) {
		t.Error("the init segment differs from an uninterrupted run's")
	}
	initFile := filepath.Join(stored, "init.mp4")
	if err := os.WriteFile(initFile, init, 0o644); err != nil {
		t.Fatal(err)
	}
	whole := join(t, filepath.Join(reference, "init.mp4"), segmentFiles(reference, 0, last+1)...)
	spliced := join(t, initFile, segmentFiles(stored, 0, last+1)...)
	compare(t, "video", times(t, whole, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
	// The last segment is left out, as in the seam test.
	end := tsOffset + float64(last*4)
	compare(t, "audio", before(times(t, whole, "a:0", "packet=pts_time"), end), before(times(t, spliced, "a:0", "packet=pts_time"), end), 0.0005)
	if warnings := bytes.TrimSpace([]byte(run(t, ffmpeg, "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-"))); len(warnings) > 0 {
		t.Errorf("decoder warnings:\n%s", warnings)
	}
}

// terminate ends the backend that holds the node's liveness lock.
func (r *rig) terminate(t *testing.T, id string) {
	t.Helper()
	var ended bool
	err := r.pool.QueryRow(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = `+livenessClass+` AND objsubid = 2 AND objid::bigint = hashtext($1)::bigint & 4294967295`, id).Scan(&ended)
	if err != nil || !ended {
		t.Fatalf("ending the liveness backend of %s: %v (ended %v)", id, err, ended)
	}
}

// A worker whose lock goes while it lives stops its ffmpeg at once, so that
// nobody takes the job over a running one; a worker then continues it, and
// the first is back once it holds the lock again.
func TestAWorkerThatLosesItsLockStopsBeforeAnyoneTakesItsJobs(t *testing.T) {
	slowAt(t, "2")
	r := newRig(t, nil)
	r.worker(t, nil)
	r.worker(t, nil)
	waitFor(t, "both workers to register", 10*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE locked") == 2
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	owner := r.owner(t)
	owned := r.watchOwners(t)
	first := r.count(t, "SELECT max(id) FROM transcode_jobs")
	r.terminate(t, owner)
	terminated := time.Now()
	waitFor(t, "the old worker's ffmpeg to stop", 5*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	t.Logf("backend terminated to ffmpeg gone: %v", time.Since(terminated).Round(time.Millisecond))
	if _, err := r.fetch(t, j, psid(0), 3); err != nil {
		t.Fatal(err)
	}
	// Either worker may have it now: the one that lost its lock is back in
	// milliseconds, and takes jobs again like any other.
	if r.owner(t) == "" {
		t.Error("no worker has the job")
	}
	if r.count(t, "SELECT max(id) FROM transcode_jobs") == first {
		t.Error("the old job was not replaced")
	}
	if reason := mustScalar(t, r, fmt.Sprintf("SELECT COALESCE(error, '') FROM transcode_jobs WHERE id = %d", first)); !bytes.Contains([]byte(reason), []byte("liveness lock")) {
		t.Errorf("the old job ended with %q, want the lost lock named", reason)
	}
	waitFor(t, "the old worker to hold a lock again and register", 10*time.Second, func() bool { return r.locked(owner) })
	owned.check(t)
}

// refusing is a stand-in for the network between a worker and Postgres, which
// can drop connections and refuse new ones, or go silent and keep the server's
// end open as a host that died does.
type proxy struct {
	ln      net.Listener
	target  string
	refuse  atomic.Bool
	mu      sync.Mutex
	servers []net.Conn
	keep    bool          // Leave the server's end open when the client's closes.
	quiet   atomic.Bool   // Leave the client's end open when the server's closes.
	delay   time.Duration // Added to every chunk, each way.
}

// pipe copies from src to dst, each chunk delay late.
func (p *proxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			time.Sleep(p.delay)
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func newProxy(t *testing.T, dsn string, keep bool) *proxy {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: u.Host, keep: keep}
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			if p.refuse.Load() {
				client.Close()
				continue
			}
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.servers = append(p.servers, server)
			p.mu.Unlock()
			go func() {
				p.pipe(server, client)
				if !p.keep {
					server.Close()
				}
			}()
			go func() {
				p.pipe(client, server)
				if !p.quiet.Load() {
					client.Close()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, server := range p.servers {
			server.Close()
		}
	})
	return p
}

// cut ends the server's end of every connection and tells the client nothing:
// what Postgres does to a vanished host's backend on its keepalives, or a
// pooler to a server connection it recycles.
func (p *proxy) cut() {
	p.quiet.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, server := range p.servers {
		server.Close()
	}
}

func (p *proxy) addr() string { return p.ln.Addr().String() }

// A worker that lost its lock and cannot take it again has no row and claims
// nothing, so nobody counts on it; it is back when the lock is.
func TestAWorkerWithoutItsLockClaimsNothingAndHasNoRow(t *testing.T) {
	slowAt(t, "2")
	r := newRig(t, nil)
	p := newProxy(t, r.dsn, false)
	cfg, err := through(r.dsn, p.addr())
	if err != nil {
		t.Fatal(err)
	}
	var id string
	r.worker(t, func(w *Worker) { w.LivenessConfig = cfg; id = w.ID })
	waitFor(t, "the worker to register", 10*time.Second, func() bool { return r.locked(id) })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	p.refuse.Store(true)
	r.terminate(t, id)
	waitFor(t, "ffmpeg to stop", 5*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	waitFor(t, "the worker's row to go", 3*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id) == 0
	})
	// A new job, which the only worker must not take while its lock is gone.
	go r.fetchAt(t, j, psid(1), 12, 12) //nolint:errcheck
	for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id) != 0 {
			t.Fatal("a worker without its lock has a node row")
		}
		if n := len(r.ffmpegProcesses()); n != 0 {
			t.Fatalf("a worker without its lock runs %d ffmpeg", n)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'queued'"); n != 1 {
		t.Fatalf("%d jobs queued, want the one nobody may take", n)
	}
	p.refuse.Store(false)
	waitFor(t, "the worker to lock again and take the job", 10*time.Second, func() bool {
		return r.locked(id) && len(r.ffmpegProcesses()) == 1
	})
}

// A worker that is stopped for longer than the pause the reaper would forgive
// is not dead: its socket and lock stand, and its job stays its own.
func TestAStoppedWorkerIsNotReaped(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	slowAt(t, "2")
	w := r.workerProcess(t, "worker-a", segments)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'")
	owned := r.watchOwners(t)
	// A viewer waiting on a later segment is what reaps, once a second.
	waited := make(chan error, 1)
	go func() { _, err := r.fetch(t, j, psid(0), 3); waited <- err }()
	time.Sleep(500 * time.Millisecond)
	w.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if _, _, _, err := Reap(context.Background(), r.pool, ""); err != nil {
			t.Fatal(err)
		}
		if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = 'worker-a'") != 1 {
			t.Fatal("a stopped worker was forgotten")
		}
	}
	w.Process.Signal(syscall.SIGCONT) //nolint:errcheck
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Errorf("%d jobs, want the one that was stopped with its worker", n)
	}
	if got := r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'"); got != job {
		t.Errorf("job %d is running, want %d", got, job)
	}
	owned.check(t)
}

// A job paused at its lead holds its lock and its lease like any other.
func TestAJobPausedAtItsLeadIsNotReaped(t *testing.T) {
	slowAt(t, "2")
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) { w.Lead = 8 * time.Second })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	stop := r.keepWatching(t, j, psid(0))
	defer stop()
	waitFor(t, "the job to pause at its lead", 20*time.Second, func() bool { return r.paused() })
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if _, _, _, err := Reap(context.Background(), r.pool, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at > now()"); n != 1 || !r.paused() {
		t.Fatalf("the paused job was reaped: %d running with a lease, paused %v", n, r.paused())
	}
}

// Host death, or a network that drops everything, closes no connection: the
// server holds the lock. Then the lease is all there is, and the job is taken
// over when it runs out, no sooner and no later.
func TestAWorkerWhoseConnectionStaysOpenIsReplacedAtLeaseExpiry(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	slowAt(t, "2")
	p := newProxy(t, r.dsn, true)
	const lease = 4 * time.Second
	a := r.workerProcess(t, "worker-a", segments, "JELLYSCALE_TEST_LIVENESS="+p.addr(),
		"JELLYSCALE_TEST_LEASE="+lease.String(), "JELLYSCALE_TEST_HEARTBEAT=500ms")
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	first := r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'")
	r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = lease, 500*time.Millisecond })
	owned := r.watchOwners(t)
	go r.fetch(t, j, psid(0), 2) //nolint:errcheck
	time.Sleep(300 * time.Millisecond)
	killed := time.Now()
	a.Process.Kill() //nolint:errcheck
	var took time.Duration
	waitFor(t, "another worker to take the job", 30*time.Second, func() bool {
		if r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id > $1 AND state = 'running'", first) == 1 {
			took = time.Since(killed)
			return true
		}
		return false
	})
	t.Logf("SIGKILL with the lock still held to the replacement job: %v (lease %v)", took.Round(10*time.Millisecond), lease)
	// The lease was renewed at most a heartbeat before the kill.
	if took < lease-time.Second {
		t.Errorf("the job was taken over %v after the kill, before its %v lease could run out", took, lease)
	}
	// That it is taken over at all is the wait above, which gives up after 30 s.
	owned.check(t)
}

// A killed worker's slots stop counting at once, not when its row is 10
// seconds old.
func TestAKilledWorkersSlotsStopCountingAtOnce(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	a := r.workerProcess(t, "worker-a", segments)
	j := r.jobs(t)
	if reason, err := j.Unavailable(context.Background(), r.hash, r.user, "device"); err != nil || reason != "" {
		t.Fatalf("with a worker up: %q, %v", reason, err)
	}
	a.Process.Kill() //nolint:errcheck
	killed := time.Now()
	waitFor(t, "the dead worker to stop counting", 5*time.Second, func() bool {
		reason, err := j.Unavailable(context.Background(), r.hash, r.user, "device")
		return err == nil && reason == "no transcode worker is running"
	})
	t.Logf("SIGKILL to no worker counted: %v", time.Since(killed).Round(10*time.Millisecond))
}

// The key is the two-int form of the lock, matched on an unsigned oid.
func TestReapKeepsALiveNodeWhoseHashIsNegative(t *testing.T) {
	r := newRig(t, nil)
	var id string
	if err := r.pool.QueryRow(context.Background(), "SELECT 'node-' || s FROM generate_series(1, 200) s WHERE hashtext('node-' || s) < 0 LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	cfg := r.pool.Config().ConnConfig
	live, err := holdLiveness(context.Background(), cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	r.pool.Exec(context.Background(), "INSERT INTO transcode_nodes (id, roles, slots, locked) VALUES ($1, 'worker', 1, true)", id) //nolint:errcheck
	if _, _, _, err := Reap(context.Background(), r.pool, ""); err != nil {
		t.Fatal(err)
	}
	if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id) != 1 {
		t.Fatal("a live node whose id hashes negative was reaped")
	}
	live.Close()
	if _, _, _, err := Reap(context.Background(), r.pool, ""); err != nil {
		t.Fatal(err)
	}
	if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id) != 0 {
		t.Fatal("a node that let go of its lock was not reaped")
	}
}

// pg_locks lists the cluster's locks, and another database's, however it is
// used, is not this one's.
func TestAnotherDatabasesLockDoesNotKeepANodeAlive(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	name := "other_" + fmt.Sprint(time.Now().UnixNano())
	r.pool.Exec(ctx, "CREATE DATABASE "+name)                                               //nolint:errcheck
	t.Cleanup(func() { r.pool.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") }) //nolint:errcheck
	u, err := url.Parse(r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	live, err := holdLiveness(ctx, cfg, "gone")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, roles, slots, locked) VALUES ('gone', 'worker', 1, true)") //nolint:errcheck
	if _, _, _, err := Reap(ctx, r.pool, ""); err != nil {
		t.Fatal(err)
	}
	if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = 'gone'") != 0 {
		t.Fatal("a lock held in another database kept a dead node alive")
	}
}

// A lock that goes while the connection stands, which no closed socket tells
// the worker, is found by looking: a worker whose lock Postgres does not show
// stops its ffmpeg, before the other worker may take its job, and takes the
// lock again. The look runs at lockCheck whatever the rig's heartbeat.
func TestAWorkerWhoseLockIsGoneWithItsConnectionOpenStopsAndLocksAgain(t *testing.T) {
	// Slow enough that no segment is stored, and so no lease checked, for
	// seconds, and a production heartbeat: only the look at pg_locks is left
	// to stop the worker in time.
	slowAt(t, "0.5")
	r := newRig(t, nil)
	p := newProxy(t, r.dsn, false)
	cfg, err := through(r.dsn, p.addr())
	if err != nil {
		t.Fatal(err)
	}
	// Helpers are a second, rightful ffmpeg for the spec, and this test counts
	// ffmpeg to see the old owner gone before the job moves: no job here is
	// slow enough to be helped (its speed is about 0.2x).
	noHelp := func(w *Worker) { w.HelpBelow = 0.001 }
	var id string
	r.worker(t, func(w *Worker) {
		noHelp(w)
		w.LivenessConfig = cfg
		w.Lease, w.Heartbeat = 10*time.Second, 3*time.Second
		id = w.ID
	})
	waitFor(t, "the worker to register", 10*time.Second, func() bool { return r.locked(id) })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	first := r.count(t, "SELECT max(id) FROM transcode_jobs")
	r.worker(t, noHelp)
	waitFor(t, "the other worker to register", 10*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE locked") == 2
	})
	owned := r.watchOwners(t)
	// At a random moment, not one that lines up with the worker's checks
	// (its heartbeat and its registration are both 3 seconds).
	delay := time.Duration(rand.Int64N(int64(3 * time.Second)))
	t.Logf("cutting the lock %v into the worker's 3 s cycle", delay.Round(time.Millisecond))
	time.Sleep(delay)
	p.cut()
	cut := time.Now()
	waitFor(t, "ffmpeg to stop", 5*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	// Reap may give the job to the other worker reapGrace after it sees the
	// lock gone, which can be at once: ffmpeg must be gone by then.
	// That is owned.check below: never two ffmpeg at once, and no job under two
	// workers. The time is logged.
	t.Logf("lock gone to ffmpeg gone: %v (the grace is %v)", time.Since(cut).Round(time.Millisecond), reapGrace)
	// A viewer waiting on a later segment reaps the worker and has the job
	// queued again, for whichever worker holds its lock.
	if _, err := r.fetch(t, j, psid(0), 3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the worker to hold a lock again", 10*time.Second, func() bool {
		return r.locked(id) && r.count(t, "SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND classid = "+livenessClass+" AND objsubid = 2") == 2
	})
	owned.check(t)
	if reason := mustScalar(t, r, fmt.Sprintf("SELECT COALESCE(error, '') FROM transcode_jobs WHERE id = %d", first)); !bytes.Contains([]byte(reason), []byte("liveness lock")) {
		t.Errorf("the old job ended with %q, want the lost lock named", reason)
	}
}

// files is every stored file with its modification time.
func files(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	found := map[string]time.Time{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && info.Mode().IsRegular() {
			found[path] = info.ModTime()
		}
		return nil
	})
	return found
}

// A worker whose lease is gone stores nothing, whatever the reason: here its
// heartbeat is a minute away, so only the check before each store can notice.
func TestAWorkerWhoseLeaseIsGoneStoresNothing(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	slowAt(t, "2")
	r.worker(t, func(w *Worker) { w.Heartbeat = time.Minute })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.pool.Exec(context.Background(), "UPDATE transcode_jobs SET lease_expires_at = now() - interval '1 second' WHERE state = 'running'") //nolint:errcheck
	time.Sleep(500 * time.Millisecond)                                                                                                    // Past an upload that began before.
	stored := files(t, segments)
	waitFor(t, "ffmpeg to stop", 15*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	if now := files(t, segments); len(now) != len(stored) {
		t.Errorf("%d files stored after the lease was gone, %d before", len(now), len(stored))
	}
}

// A worker stopped for longer than its lease keeps its ffmpeg, and another
// worker takes the job. When it goes on it must not store what it made.
func TestAWorkerStoppedPastItsLeaseOverwritesNothingWhenItGoesOn(t *testing.T) {
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	slowAt(t, "2")
	const lease = 3 * time.Second
	a := r.workerProcess(t, "worker-a", segments, "JELLYSCALE_TEST_LEASE="+lease.String(), "JELLYSCALE_TEST_HEARTBEAT=500ms")
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	first := r.count(t, "SELECT max(id) FROM transcode_jobs")
	r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = lease, 500*time.Millisecond })
	a.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
	// ffmpeg, a process of its own, goes on making segments in worker-a's scratch.
	if _, err := r.fetch(t, j, psid(0), 3); err != nil {
		t.Fatal(err)
	}
	if r.count(t, "SELECT max(id) FROM transcode_jobs") == first || r.owner(t) == "worker-a" {
		t.Fatal("the stopped worker still has the job")
	}
	time.Sleep(time.Second)
	stored := files(t, segments)
	a.Process.Signal(syscall.SIGCONT) //nolint:errcheck
	waitFor(t, "worker-a's ffmpeg to be stopped", 10*time.Second, func() bool {
		return len(r.ffmpegProcesses()) <= 1
	})
	time.Sleep(time.Second)
	now := files(t, segments)
	for name, was := range stored {
		if !now[name].Equal(was) {
			t.Errorf("%s was written again after worker-a went on", filepath.Base(name))
		}
	}
	// The job that took over started at 3; worker-a had made the ones before it.
	for n := 1; n < 3; n++ {
		if _, err := os.Stat(filepath.Join(segments, SegmentKey(r.hash, n))); err == nil {
			if _, was := stored[filepath.Join(segments, SegmentKey(r.hash, n))]; !was {
				t.Errorf("worker-a stored segment %d after its lease was gone", n)
			}
		}
	}
}

// A worker whose row is deleted was taken for dead by a replica that found
// its lock gone, and that replica has cut its jobs' leases short. Putting the
// row back would let the worker renew them, so it stops its jobs instead, and
// registers again only once it holds a lock again.
func TestAWorkerWhoseRowIsDeletedStopsItsJobs(t *testing.T) {
	slowAt(t, "0.5") // No segment, so no check before a store, for seconds.
	r := newRig(t, nil)
	var id string
	// Registration and heartbeat far apart, so only the quarter-second look
	// can stop ffmpeg within the grace.
	r.worker(t, func(w *Worker) { w.Lease, w.Heartbeat = time.Minute, 30*time.Second; id = w.ID })
	waitFor(t, "the worker to register", 10*time.Second, func() bool { return r.locked(id) })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	first := r.count(t, "SELECT max(id) FROM transcode_jobs")
	if _, err := r.pool.Exec(context.Background(), "DELETE FROM transcode_nodes WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	deleted := time.Now()
	waitFor(t, "ffmpeg to stop", 5*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	t.Logf("row deleted to ffmpeg gone: %v (the grace is %v)", time.Since(deleted).Round(time.Millisecond), reapGrace)
	waitFor(t, "the job to end", 5*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND state = 'running'", first) == 0
	})
	if reason := mustScalar(t, r, fmt.Sprintf("SELECT state || ': ' || COALESCE(error, '') FROM transcode_jobs WHERE id = %d", first)); !bytes.Contains([]byte(reason), []byte("liveness lock")) {
		t.Errorf("the old job ended with %q, want the lost lock named", reason)
	}
	waitFor(t, "the worker to register again", 10*time.Second, func() bool { return r.locked(id) })
}

// A database that does not answer the check before a store cuts the job
// short; it does not fail it, which would refuse the spec for 30 seconds.
func TestAnOwnerCheckThatErrsInterruptsTheJob(t *testing.T) {
	r := newRig(t, nil)
	w := &Worker{ID: "worker-x", Pool: r.pool, Log: r.log}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := w.upload(ctx, claimed{ID: 1}, 0, "no-file")
	if !errors.Is(err, errInterrupted) {
		t.Fatalf("upload with no answer from the database: %v, want it interrupted", err)
	}
}

// A worker whose row is gone was taken for dead, and Reap has cut its jobs'
// leases short: it may not renew one, claim one, or store a segment, which is
// what keeps it from stretching that grace. With its row back it may.
func TestAWorkerWithoutItsRowRenewsClaimsAndStoresNothing(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	var running int64
	if err := r.pool.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ('h', '{}', 'o', 0, 0, 'running', 'worker-x', now() + interval '10 seconds') RETURNING id`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment) VALUES ('h2', '{}', 'o2', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	w := &Worker{ID: "worker-x", Pool: r.pool, Log: r.log, Lease: 10 * time.Second}
	job := claimed{ID: running, Hash: "h"}

	if alive, _, err := w.beat(ctx, job); err != nil || alive {
		t.Errorf("beat without a row: alive %v, %v; want the job lost", alive, err)
	}
	if _, ok, err := w.claim(ctx); err != nil || ok {
		t.Errorf("claim without a row: claimed %v, %v; want nothing", ok, err)
	}
	if err := w.upload(ctx, job, 0, "no-file"); !errors.Is(err, errInterrupted) {
		t.Errorf("upload without a row: %v; want it interrupted before the store", err)
	}

	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ('worker-x', 'worker', '', 2, true)`); err != nil {
		t.Fatal(err)
	}
	if alive, _, err := w.beat(ctx, job); err != nil || !alive {
		t.Errorf("beat with its row: alive %v, %v; want the job kept", alive, err)
	}
	if _, ok, err := w.claim(ctx); err != nil || !ok {
		t.Errorf("claim with its row: claimed %v, %v; want the queued job", ok, err)
	}
}

// Reap gives a dead worker's jobs reapGrace to end, not a lease that is gone at
// once: that second is what a worker that lost its lock while it lives needs to
// stop its ffmpeg before another takes the job.
func TestReapLeavesADeadWorkersJobsAGraceToEnd(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ('dead-x', 'worker', '', 2, true)`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := r.pool.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ('h', '{}', 'o', 0, 0, 'running', 'dead-x', now() + interval '10 seconds') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var startedAt, finishedAt time.Time
	if err := r.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&startedAt); err != nil {
		t.Fatal(err)
	}
	workers, jobs, _, err := Reap(ctx, r.pool, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&finishedAt); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(workers, []string{"dead-x"}) || !slices.Equal(jobs, []int64{id}) {
		t.Fatalf("Reap forgot %v and cut %v, want [dead-x] and [%d]", workers, jobs, id)
	}
	// The lease was cut to a grace after the reap: not gone at once (it ends after
	// the moment before Reap ran) and not left at its 10 s (it ends within a grace
	// of the moment after). Both moments are the database's clock, read in order,
	// so the runner's speed does not enter.
	var lease time.Time
	if err := r.pool.QueryRow(ctx, "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if !lease.After(startedAt) {
		t.Error("the dead worker's job lease was gone at once: its worker has no time to stop ffmpeg")
	}
	if lease.After(finishedAt.Add(reapGrace)) {
		t.Errorf("the dead worker's job lease was left at more than the %v grace after the reap", reapGrace)
	}
}

// Two node ids that hash alike share the lock: the second waits, claiming
// nothing and saying why, until the first lets go. Never two holders.
func TestTwoNodesWhoseIdsHashAlikeNeverBothHoldTheLock(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	var first, second string
	// About ten pairs collide among 300,000 ids; any one will do.
	if err := r.pool.QueryRow(ctx, `SELECT ids[1], ids[2] FROM (
			SELECT array_agg('node-' || g) AS ids FROM generate_series(1, 300000) g GROUP BY hashtext('node-' || g) HAVING count(*) > 1 LIMIT 1) c`).Scan(&first, &second); err != nil {
		t.Fatal(err)
	}
	cfg := r.pool.Config().ConnConfig
	a, err := holdLiveness(ctx, cfg, first)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := holdLiveness(ctx, cfg, second); err == nil {
		b.Close()
		a.Close()
		t.Fatalf("%s and %s hash alike, and both took the lock", first, second)
	} else if !strings.Contains(err.Error(), "same hash") {
		t.Errorf("the second node was refused with %q, want the shared hash named", err)
	}
	a.Close()
	b, err := holdLiveness(ctx, cfg, second)
	if err != nil {
		t.Fatalf("the second node could not take the lock once the first let go: %v", err)
	}
	b.Close()
}

// A database a round trip of 300 ms away is slow, not gone: the worker
// registers and stays registered, though a look at the dead may time out.
func TestAWorkerRegistersOverASlowDatabase(t *testing.T) {
	r := newRig(t, nil)
	p := newProxy(t, r.dsn, false)
	p.delay = 150 * time.Millisecond
	u, err := url.Parse(r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = p.addr()
	slow, err := db.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slow.Close)
	var id string
	r.worker(t, func(w *Worker) { w.Pool, w.Heartbeat = slow, 3*time.Second; id = w.ID })
	waitFor(t, "the worker to register over the slow database", 15*time.Second, func() bool { return r.locked(id) })
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if !r.locked(id) {
			t.Fatal("the worker's row went while the database was only slow")
		}
	}
}

// A node that never took a lock (an api or stream process, or a worker of the
// version before locks during a rolling upgrade) is not dead for want of one:
// Reap leaves its row and its jobs' leases alone, and only its lease decides.
func TestReapLeavesANodeThatTookNoLockAlone(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots) VALUES ('old-worker', 'worker', '', 2)`); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := r.pool.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ('h', '{}', 'o', 0, 0, 'running', 'old-worker', now() + interval '10 seconds') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var leaseBefore time.Time
	if err := r.pool.QueryRow(ctx, "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", id).Scan(&leaseBefore); err != nil {
		t.Fatal(err)
	}
	workers, jobs, _, err := Reap(ctx, r.pool, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 0 || len(jobs) != 0 {
		t.Errorf("Reap forgot %v and cut %v; a node that took no lock is not dead", workers, jobs)
	}
	if r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = 'old-worker'") != 1 {
		t.Error("the row of a node that took no lock was deleted")
	}
	var leaseAfter time.Time
	if err := r.pool.QueryRow(ctx, "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", id).Scan(&leaseAfter); err != nil {
		t.Fatal(err)
	}
	if !leaseAfter.Equal(leaseBefore) {
		t.Errorf("the job of a node that took no lock had its lease cut from %v to %v", leaseBefore, leaseAfter)
	}
}
