//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// The kit for the checks of JS-029: Postgres going away under real worker
// processes and a stream replica. Every process reaches the database through a
// testenv.DBProxy of its own, so a test ends the connections of one process or
// all of them, refuses new ones for as long as it likes, and brings each back
// at its own moment, while the test itself reads Postgres directly.
//
// The workers run at their production lease (10 seconds) and heartbeat (3
// seconds): the criteria are about an outage that ends before a lease runs out
// and one that outlasts it, and a shorter lease would move the line between
// them. The tests that end an outage before the lease does start it just after
// a renewal (afterRenewal), so the outage has most of the lease to end in; none
// of them judges how long anything took.

// putLog is the segment store of a worker process that writes a line for each
// Put it begins, so a test can tell what a worker stored and when. A Put waits
// JELLYSCALE_TEST_PUTDELAY_AFTER_MS after it has stored the object, which is
// the moment between the store and the row that says so: an outage that comes
// there finds the segment stored and its row not updated.
type putLog struct {
	store.Store
	worker, path string
}

func (p *putLog) Put(ctx context.Context, key string, body io.Reader) error {
	if f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%s %d %s\n", p.worker, time.Now().UnixNano(), key)
		f.Close()
	}
	err := p.Store.Put(ctx, key, body)
	if ms, convErr := strconv.Atoi(os.Getenv("JELLYSCALE_TEST_PUTDELAY_AFTER_MS")); convErr == nil {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	return err
}

type put struct {
	worker string
	at     time.Time
	key    string
}

func putsOf(t *testing.T, path string) []put {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var puts []put
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		nanos, _ := strconv.ParseInt(fields[1], 10, 64)
		puts = append(puts, put{fields[0], time.Unix(0, nanos), fields[2]})
	}
	return puts
}

// outageWorker is a worker process whose database connections and liveness
// connection each go through a proxy.
type outageWorker struct {
	id         string
	cmd        *exec.Cmd
	db, live   *testenv.DBProxy
	log        logBuffer
	reaped     chan struct{}
	killedOnce sync.Once
}

func (w *outageWorker) away(refuse bool) {
	for _, p := range []*testenv.DBProxy{w.db, w.live} {
		if refuse {
			p.Away()
		} else {
			p.Sever()
		}
	}
}

func (w *outageWorker) back() {
	w.db.Back()
	w.live.Back()
}

func (w *outageWorker) kill() {
	w.killedOnce.Do(func() { w.cmd.Process.Kill() }) //nolint:errcheck
	<-w.reaped
}

// stack is a rig, worker processes, and a stream replica's pool, each behind a
// proxy of its own.
type stack struct {
	t       *testing.T
	r       *rig
	seg     string
	putlog  string
	workers []*outageWorker
	replica *testenv.DBProxy
	jobs    *Jobs // The stream replica's, through its proxy.
}

func newStack(t *testing.T, workers int, env ...string) *stack {
	t.Helper()
	seg := t.TempDir()
	r := newRig(t, store.NewPosix(seg))
	s := &stack{t: t, r: r, seg: seg, putlog: t.TempDir() + "/puts.log", replica: testenv.NewDBProxy(t, r.dsn)}
	s.jobs = s.replicaJobs()
	for range workers {
		s.addWorker(env...)
	}
	return s
}

// replicaJobs is a stream replica of its own: the pool goes through the replica proxy.
func (s *stack) replicaJobs() *Jobs {
	pool, err := db.Open(context.Background(), s.replica.DSN())
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(pool.Close)
	ctx, cancel := context.WithCancel(context.Background())
	s.t.Cleanup(cancel)
	hub := &Hub{Pool: pool, Log: s.r.log}
	go hub.Run(ctx)
	return &Jobs{Pool: pool, Segments: s.r.segments, Hub: hub, QueueTimeout: 5 * time.Second, WaitTimeout: 120 * time.Second}
}

func (s *stack) addWorker(env ...string) *outageWorker {
	t := s.t
	t.Helper()
	w := &outageWorker{id: uuid.NewString(), db: testenv.NewDBProxy(t, s.r.dsn), live: testenv.NewDBProxy(t, s.r.dsn), reaped: make(chan struct{})}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER="+w.id, "JELLYSCALE_TEST_SEGMENTS="+s.seg, "JELLYSCALE_TEST_SCRATCH="+s.r.scratch,
		// Without a statement cache every statement sends its text, which is how
		// a proxy can end the connection as one particular statement runs.
		"JELLYSCALE_TEST_DATABASE_URL="+w.db.DSN()+"&default_query_exec_mode=exec", "JELLYSCALE_TEST_LIVENESS="+w.live.Addr(),
		"JELLYSCALE_TEST_SLOTS=1", "JELLYSCALE_TEST_NO_HELPERS=1", "JELLYSCALE_TEST_PUTLOG="+s.putlog)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = &w.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.cmd = cmd
	go func() { cmd.Wait(); close(w.reaped) }() //nolint:errcheck
	t.Cleanup(func() {                          // After the kill below, which runs first.
		if t.Failed() {
			t.Logf("worker %s logged:\n%s", w.id, w.log.String())
		}
	})
	t.Cleanup(func() { w.kill() })
	s.workers = append(s.workers, w)
	waitFor(t, "worker "+w.id+" to hold its lock and register", 30*time.Second, func() bool { return s.r.locked(w.id) })
	return w
}

func (s *stack) worker(id string) *outageWorker {
	for _, w := range s.workers {
		if w.id == id {
			return w
		}
	}
	return nil
}

// away ends the connections of the given workers and of the stream replica
// (replica false leaves the replica alone), refusing new ones when refuse is
// true, and checks the outage is what it claims to be: Postgres no longer
// holds the old connections of any of them, and with it their advisory locks.
func (s *stack) away(refuse bool, replica bool, workers ...*outageWorker) {
	t := s.t
	t.Helper()
	before := s.lockPids()
	for _, w := range workers {
		w.away(refuse)
	}
	if replica {
		if refuse {
			s.replica.Away()
		} else {
			s.replica.Sever()
		}
	}
	s.waitEnded(before, workers)
	if refuse {
		held := s.lockPids()
		for _, w := range workers {
			if _, ok := held[w.id]; ok {
				t.Fatalf("worker %s still holds a liveness lock while its connections are refused", w.id)
			}
		}
	}
}

// waitEnded waits until Postgres no longer has the lock backends the given workers had.
func (s *stack) waitEnded(before map[string]int, workers []*outageWorker) {
	t := s.t
	t.Helper()
	old := map[int]bool{}
	for _, w := range workers {
		if pid, ok := before[w.id]; ok {
			old[pid] = true
		}
	}
	waitFor(t, "Postgres to end the severed connections", 10*time.Second, func() bool {
		rows, err := s.r.pool.Query(context.Background(), "SELECT pid FROM pg_stat_activity")
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var pid int
			if rows.Scan(&pid) == nil && old[pid] {
				return false
			}
		}
		return true
	})
}

// terminate is the third shape of away, and the one a real switchover or a fast
// shutdown makes: Postgres itself ends every backend of the given workers and of
// the stream replica with a FATAL message (57P01, admin_shutdown), which the
// statement running at that moment, or the next one, receives. The proxies
// only close sockets, which a client sees as EOF and no PgError; code that
// takes "Postgres is away" for a connection error alone fails on this. New
// connections are accepted at once.
func (s *stack) terminate(workers ...*outageWorker) {
	t := s.t
	t.Helper()
	before := s.lockPids()
	ctx := context.Background()
	for _, w := range workers {
		// The liveness connection is made from the same URL, so it carries w.db's name.
		if n, err := w.db.Terminate(ctx, s.r.pool); err != nil || n == 0 {
			t.Fatalf("worker %s's backends were not terminated: %d %v", w.id, n, err)
		}
	}
	if _, err := s.replica.Terminate(ctx, s.r.pool); err != nil {
		t.Fatal(err)
	}
	s.waitEnded(before, workers)
}

// back lets the given workers, and the replica when asked, reach Postgres again.
func (s *stack) back(replica bool, workers ...*outageWorker) {
	for _, w := range workers {
		w.back()
	}
	if replica {
		s.replica.Back()
	}
}

// lockPids is the Postgres backend that holds each registered worker's liveness lock.
func (s *stack) lockPids() map[string]int {
	rows, err := s.r.pool.Query(context.Background(), `SELECT n.id, l.pid FROM transcode_nodes n JOIN pg_locks l
		ON l.locktype = 'advisory' AND l.granted AND l.objsubid = 2 AND l.classid = `+livenessClass+`
			AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	held := map[string]int{}
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			s.t.Fatal(err)
		}
		held[id] = pid
	}
	return held
}

// allLocked waits until every given worker is registered and holds its lock
// again, and returns who holds it.
func (s *stack) allLocked(timeout time.Duration, workers ...*outageWorker) map[string]int {
	s.t.Helper()
	waitFor(s.t, "every worker to hold its liveness lock and have its row", timeout, func() bool {
		held := s.lockPids()
		for _, w := range workers {
			if _, ok := held[w.id]; !ok || !s.r.locked(w.id) {
				return false
			}
		}
		return true
	})
	return s.lockPids()
}

// afterRenewal returns just after every running job's lease was renewed, so an
// outage that starts now has nearly the whole lease to end in.
func (s *stack) afterRenewal() {
	t := s.t
	t.Helper()
	leases := func() map[int64]time.Time {
		rows, err := s.r.pool.Query(context.Background(), "SELECT id, lease_expires_at FROM transcode_jobs WHERE state = 'running'")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		found := map[int64]time.Time{}
		for rows.Next() {
			var id int64
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				t.Fatal(err)
			}
			found[id] = at
		}
		return found
	}
	first := leases()
	deadline := time.Now().Add(30 * time.Second)
	for {
		now := leases()
		renewed := len(first) > 0
		for id, at := range first {
			if !now[id].After(at) {
				renewed = false
			}
		}
		if renewed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the leases were not renewed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// jobRow is what must be the same before and after an outage that cuts nothing.
type jobRow struct {
	ID            int64
	Worker, State string
	Hash          string
	Start         int
}

func (r *rig) jobRows(t *testing.T) []jobRow {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT id, COALESCE(worker, ''), state, spec_hash, start_segment FROM transcode_jobs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.ID, &j.Worker, &j.State, &j.Hash, &j.Start); err != nil {
			t.Fatal(err)
		}
		found = append(found, j)
	}
	return found
}

// ffmpegProcess is a running ffmpeg of the rig: the process id and start time
// say which process it is, and a pid that was reused would differ in the start.
type ffmpegProcess struct {
	Pid    int
	Parent int
	Start  string
	State  string
}

func (r *rig) ffmpegs() []ffmpegProcess {
	entries, _ := os.ReadDir("/proc")
	var found []ffmpegProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 || !bytes.Contains(raw, []byte(r.scratch)) {
			continue
		}
		if args := strings.Split(string(raw), "\x00"); !strings.HasPrefix(args[0][strings.LastIndex(args[0], "/")+1:], "ffmpeg") {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))
		if len(fields) < 20 {
			continue
		}
		parent, _ := strconv.Atoi(fields[1])
		found = append(found, ffmpegProcess{Pid: pid, Parent: parent, Start: fields[19], State: fields[0]})
	}
	return found
}

// ffmpegOf is the ffmpeg a worker process runs, if any.
func (r *rig) ffmpegOf(w *outageWorker) (ffmpegProcess, bool) {
	for _, p := range r.ffmpegs() {
		if p.Parent == w.cmd.Process.Pid {
			return p, true
		}
	}
	return ffmpegProcess{}, false
}

// segmentsIn lists the segment numbers stored for the spec.
func (r *rig) storedSegments(hash string) []int {
	objects, _ := r.segments.List(context.Background(), "segments/"+hash+"/")
	var numbers []int
	for _, o := range objects {
		var n int
		if _, err := fmt.Sscanf(o.Key[strings.LastIndex(o.Key, "/")+1:], "%d.mp4", &n); err == nil {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	return numbers
}

// firstMissing is the lowest segment number not stored for the spec.
func (r *rig) firstMissing(hash string) int {
	n := 0
	for _, have := range r.storedSegments(hash) {
		if have != n {
			break
		}
		n++
	}
	return n
}

// fetch asks as a viewer of the spec for segment n, through the given replica.
func (s *stack) fetch(ctx context.Context, j *Jobs, spec playback.Spec, ps string, n int) ([]byte, error) {
	hash := spec.Hash()
	v := s.r.viewer(ps)
	if err := j.Watch(ctx, v, hash, n); err != nil {
		return nil, err
	}
	return bytesOf(j.Segment(ctx, spec, hash, v, n, 0))
}

// viewerLoop is a player: it asks for segment after segment, from the first, and
// asks again when an answer is an error, which an outage may cause. Errors are
// counted and not judged; what the replica answers while Postgres is away is
// for the API tier to check.
type viewerLoop struct {
	got, errs atomic.Int64 // Segments served; answers that were errors.
	stop      func()
}

func (s *stack) play(j *Jobs, spec playback.Spec, ps string, from, upTo int) *viewerLoop {
	ctx, cancel := context.WithCancel(context.Background())
	v := &viewerLoop{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := from; n < upTo && ctx.Err() == nil; {
			attempt, stop := context.WithTimeout(ctx, 20*time.Second)
			_, err := s.fetch(attempt, j, spec, ps, n)
			stop()
			if err != nil {
				v.errs.Add(1)
				time.Sleep(200 * time.Millisecond)
				continue
			}
			n++
			v.got.Store(int64(n - from))
		}
	}()
	v.stop = func() { cancel(); <-done }
	s.t.Cleanup(v.stop)
	return v
}

// watchNodes samples the workers' rows until stopped and reports which rows
// were ever gone after they had been there.
type nodeWatch struct {
	mu      sync.Mutex
	gone    map[string][]time.Time // When a row was first seen missing, per worker.
	back    map[string][]time.Time // When it was first seen again.
	present map[string]bool
	stop    func()
}

func (s *stack) watchNodes() *nodeWatch {
	n := &nodeWatch{gone: map[string][]time.Time{}, back: map[string][]time.Time{}, present: map[string]bool{}}
	for _, w := range s.workers {
		n.present[w.id] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			rows, err := s.r.pool.Query(ctx, "SELECT id FROM transcode_nodes")
			if err == nil {
				seen := map[string]bool{}
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						seen[id] = true
					}
				}
				rows.Close()
				now := time.Now()
				n.mu.Lock()
				for id, was := range n.present {
					switch {
					case was && !seen[id]:
						n.present[id] = false
						n.gone[id] = append(n.gone[id], now)
					case !was && seen[id]:
						n.present[id] = true
						n.back[id] = append(n.back[id], now)
					}
				}
				n.mu.Unlock()
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()
	n.stop = func() { cancel(); <-done }
	s.t.Cleanup(n.stop)
	return n
}

// forgotten says which workers' rows were ever missing.
func (n *nodeWatch) forgotten() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var ids []string
	for id, times := range n.gone {
		if len(times) > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

var (
	wentAway = regexp.MustCompile(`(?i)((postgres|database).*(away|unreachable|unavailable|is down|went|gone|lost|cannot be reached|not reachable))|((away|unreachable|unavailable|lost|cannot reach).*(postgres|database))`)
	cameBack = regexp.MustCompile(`(?i)((postgres|database).*(is back|came back|back again|reachable again|recovered|restored|returned|available again|answers again))|((back|again|recovered|restored).*(postgres|database))`)
)

// loggedTheOutage fails the test unless the log has a line that says Postgres
// went away and, after it, one that says it was back.
func loggedTheOutage(t *testing.T, who, log string) {
	t.Helper()
	lines := strings.Split(log, "\n")
	away := -1
	for i, line := range lines {
		if wentAway.MatchString(line) {
			away = i
			break
		}
	}
	if away < 0 {
		t.Errorf("the log of %s never says Postgres went away:\n%s", who, log)
		return
	}
	for _, line := range lines[away+1:] {
		if cameBack.MatchString(line) {
			return
		}
	}
	t.Errorf("the log of %s says Postgres went away (%q) and never that it was back:\n%s", who, lines[away], log)
}

// noFailed fails the test if a job ended failed: Postgres being away is no
// reason for a job to fail, and a failed job makes PlaybackInfo and the
// segment route refuse the spec for 30 seconds.
func (r *rig) noFailed(t *testing.T) {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT id, spec_hash, COALESCE(error, '') FROM transcode_jobs WHERE state = 'failed'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var hash string
		var reason string
		if rows.Scan(&id, &hash, &reason) == nil {
			t.Errorf("job %d of spec %s ended failed: %s", id, hash, reason)
		}
	}
}
