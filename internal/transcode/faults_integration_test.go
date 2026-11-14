//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-066: the lease and failover invariants under randomized faults, with real
// processes. A schedule (faultplan_test.go) is carried out against three or four
// worker processes of one output version, two stream replicas and five viewers
// playing forward on two specs, while an observer reads the database every few
// milliseconds and holds every row it sees to the invariants. Nothing here judges
// a duration: what is checked is what the database and the processes recorded
// (rows, lease and claim times, process ids, the put log, the workers' logs), and
// the longest a wait may take is a bound on a hang.
//
// The workers run with a lease of faultClock.Lease and a heartbeat of
// faultClock.Heartbeat. What a lease shorter than the 30 s of the check of a
// dead worker's lease could hide: a build that waits the lease out would claim
// at 20 s, which is still after the replacement was due, and the claim is
// compared with the lease the database holds, not with a time the test chose.
//
// The seed is printed with the steps when a schedule starts, and every failure
// names the invariant broken, the seed, and the step it followed. A seed replays
// the steps, in the same order, with the same targets and lengths; the interleaving
// of the processes is not replayed (JELLYSCALE_TEST_FAULT_SEED, docs/testing.md).
// JELLYSCALE_TEST_FAULT_SOAK=n runs the same test n times with a new seed each,
// for hunting flakes.

const (
	// faultSegments is how many segments a viewer asks for at most; the film is
	// 150 s, 38 segments of 4 s, and a viewer that has all of them has played it.
	faultSegments = 34
	// faultHang bounds a wait that must end: a request, a drain, a step that waits
	// for something to be back. A right build ends them in seconds or in the tens of seconds
	// a long stop takes; a wrong one never does.
	faultHang = 150 * time.Second
	// faultWorkers is how many worker processes run: a killed one is replaced by a new process.
	faultWorkers = 3
)

// chaosFilm makes the film the viewers play: 150 s, long enough that a viewer playing
// at the speed of the film is not at its end when the schedule is.
func chaosFilm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testenv.FFmpeg(t)
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=150",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=150",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-g", "96", "-c:a", "aac", filepath.Join(dir, "film.mkv"))
	return dir
}

type chaosWorker struct {
	*outageWorker
	spawned int
	killed  atomic.Bool
	stopped atomic.Bool
}

type chaosReplica struct {
	proxy *testenv.DBProxy
	jobs  *Jobs
}

// chaosViewer is a player of one spec through one replica.
type chaosViewer struct {
	index     int
	spec      playback.Spec
	hash      string
	session   string
	replica   int
	group     *chaosGroup
	stoppable bool
	stopped   atomic.Bool
	inRequest atomic.Bool
	served    atomic.Int64 // Segments served, in order, from the first.
	done      chan struct{}
}

// chaosGroup keeps viewers of one spec in step: each asks for segment n only once
// every viewer of the group that has not stopped is done with n-1.
type chaosGroup struct {
	mu    sync.Mutex
	cond  *sync.Cond
	ready map[*chaosViewer]int
	ctx   context.Context
}

func newChaosGroup(ctx context.Context, members ...*chaosViewer) *chaosGroup {
	g := &chaosGroup{ready: map[*chaosViewer]int{}, ctx: ctx}
	g.cond = sync.NewCond(&g.mu)
	for _, m := range members {
		g.ready[m] = 0
		m.group = g
	}
	go func() { <-ctx.Done(); g.cond.Broadcast() }()
	return g
}

// arrive blocks until every viewer of the group that is still playing is ready for n.
func (g *chaosGroup) arrive(v *chaosViewer, n int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready[v] = n
	g.cond.Broadcast()
	for g.ctx.Err() == nil && !v.stopped.Load() {
		behind := false
		for m, at := range g.ready {
			if m != v && !m.stopped.Load() && at < n {
				behind = true
			}
		}
		if !behind {
			return true
		}
		g.cond.Wait()
	}
	return false
}

func (g *chaosGroup) leave() {
	g.mu.Lock()
	g.cond.Broadcast()
	g.mu.Unlock()
}

type chaosViolation struct {
	invariant, detail, after string
}

type putEvent struct {
	worker, key string
	at          time.Time
}

type delayWindow struct {
	worker     string
	from, to   time.Time
	roundTrip  time.Duration
	longerThan bool // A round trip too long for the check before a put to be answered.
}

// jobSnap is one row of transcode_jobs as the observer saw it.
type jobSnap struct {
	ID                         int64
	Hash, Owner, State, Worker string
	Helper                     bool
	Lease, Created             time.Time
	Start, Next                int
	Error                      string
}

type viewerSnap struct {
	Session, Hash string
	Job           int64
	Stopped       bool
	Seen          time.Time
}

type validity struct {
	at      time.Time
	valid   bool
	invalid time.Time // When the job stopped being the worker's to put for, as far as this sample shows.
}

// chaos is one schedule being carried out.
type chaos struct {
	t        *testing.T
	seed     uint64
	index    int
	plan     faultSchedule
	r        *rig
	s        *stack
	ctx      context.Context
	cancel   context.CancelFunc
	started  time.Time
	replicas [2]*chaosReplica
	viewers  []*chaosViewer
	hashes   [2]string
	specs    [2]playback.Spec

	mu         sync.Mutex
	workers    []*chaosWorker
	pgAway     bool
	pgEnded    time.Time
	current    string // The step that began last.
	violations []chaosViolation
	claimant   map[int64]string
	lastJobs   map[int64]jobSnap
	valid      map[int64][]validity
	created    map[int64]jobSnap
	rowGone    map[string][]time.Time
	rowSeen    map[string]bool
	delays     []delayWindow
	hits       []workerHit          // The workers a step took away or hurt, and when.
	judged     map[string]int       // How many jobs each kind of check judged.
	noLive     map[string]time.Time // Since when a spec a viewer waits on has had no job queued or running with a lease.
}

func (c *chaos) violate(invariant, format string, args ...any) {
	c.mu.Lock()
	v := chaosViolation{invariant, fmt.Sprintf(format, args...), c.current}
	c.violations = append(c.violations, v)
	c.mu.Unlock()
	c.t.Logf("INVARIANT BROKEN: %s (seed %d, schedule %d, after step %q): %s", v.invariant, c.seed, c.index, v.after, v.detail)
	c.cancel() // A schedule that has broken an invariant has nothing more to show.
}

func (c *chaos) spawn() (*chaosWorker, error) {
	t := c.t
	w := &outageWorker{id: uuid.NewString(), db: testenv.NewDBProxy(t, c.r.dsn), live: testenv.NewDBProxy(t, c.r.dsn), reaped: make(chan struct{})}
	cmd := execWorker(w, c.s, "JELLYSCALE_TEST_LEASE="+faultClock.Lease.String(), "JELLYSCALE_TEST_HEARTBEAT="+faultClock.Heartbeat.String(),
		"JELLYSCALE_TEST_QUEUE_TIMEOUT=20s")
	cmd.Stderr = &w.log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w.cmd = cmd
	go func() { cmd.Wait(); close(w.reaped) }() //nolint:errcheck
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker %s logged:\n%s", w.id, w.log.String())
		}
	})
	t.Cleanup(func() { cmd.Process.Signal(syscall.SIGCONT); w.kill() }) //nolint:errcheck
	c.mu.Lock()
	cw := &chaosWorker{outageWorker: w, spawned: len(c.workers)}
	c.workers = append(c.workers, cw)
	c.mu.Unlock()
	return cw, nil
}

// execWorker is the command of a worker process of the stack, with its own two proxies.
func execWorker(w *outageWorker, s *stack, env ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER="+w.id, "JELLYSCALE_TEST_SEGMENTS="+s.seg, "JELLYSCALE_TEST_SCRATCH="+s.r.scratch,
		// Without a statement cache every statement sends its text, which is how a proxy can end
		// the connection as one particular statement runs.
		"JELLYSCALE_TEST_DATABASE_URL="+w.db.DSN()+"&default_query_exec_mode=exec", "JELLYSCALE_TEST_LIVENESS="+w.live.Addr(),
		"JELLYSCALE_TEST_SLOTS=1", "JELLYSCALE_TEST_NO_HELPERS=1", "JELLYSCALE_TEST_PUTLOG="+s.putlog)
	cmd.Env = append(cmd.Env, env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// waitRegistered waits for a worker to hold its lock and have its row, as a hang bound.
func (c *chaos) waitRegistered(w *chaosWorker) error {
	deadline := time.Now().Add(faultHang)
	for !c.r.locked(w.id) {
		if time.Now().After(deadline) || c.ctx.Err() != nil {
			return fmt.Errorf("worker %s never registered", w.id)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func (c *chaos) alive() []*chaosWorker {
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []*chaosWorker
	for _, w := range c.workers {
		if !w.killed.Load() {
			found = append(found, w)
		}
	}
	return found
}

func (c *chaos) workerByID(id string) *chaosWorker {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.workers {
		if w.id == id {
			return w
		}
	}
	return nil
}

// waitedSpecs are the specs a viewer is waiting on a segment of right now.
func (c *chaos) waitedSpecs() []string {
	var hashes []string
	for _, v := range c.viewers {
		if v.inRequest.Load() && !v.stopped.Load() && !slices.Contains(hashes, v.hash) {
			hashes = append(hashes, v.hash)
		}
	}
	return hashes
}

// holders are the workers that hold the lease of a running job, of a spec a viewer waits on when waited.
func (c *chaos) holders(waited bool) []*chaosWorker {
	hashes := []string(nil)
	if waited {
		hashes = c.waitedSpecs()
		if len(hashes) == 0 {
			return nil
		}
	}
	rows, err := c.r.pool.Query(context.Background(), `SELECT DISTINCT worker FROM transcode_jobs WHERE state = 'running' AND lease_expires_at > now() AND NOT helper
		AND ($1::text[] IS NULL OR spec_hash = ANY($1))`, hashes)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var found []*chaosWorker
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			if w := c.workerByID(id); w != nil && !w.killed.Load() {
				found = append(found, w)
			}
		}
	}
	slices.SortFunc(found, func(a, b *chaosWorker) int { return a.spawned - b.spawned })
	return found
}

// pick waits for the candidates of a step, as a hang bound, and takes the one the target names.
func (c *chaos) pick(step faultStep, candidates func() []*chaosWorker) *chaosWorker {
	deadline := time.Now().Add(faultHang)
	for c.ctx.Err() == nil && time.Now().Before(deadline) {
		if list := candidates(); len(list) > 0 {
			return list[step.Target%len(list)]
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// hit says whether another step of the plan falls in the window of step i, from a second
// before it begins to extra after it ends, and cuts what step i is judged on. The plan is
// what is asked, so the windows are the planned ones. Which steps cut it depends on what is
// judged: for a stop or a delay it is a restart of Postgres (no other step takes the same
// worker away, which the plan keeps). A restart is not judged here: its jobs are judged one
// by one, those of a worker that no step hit (judgedAfterRestart).
func (c *chaos) hit(i int, extra time.Duration, cuts func(other faultStep) bool) bool {
	return c.plan.hit(i, extra, cuts)
}

func (c *chaos) hitByPostgres(i int, extra time.Duration) bool {
	return c.hit(i, extra, func(o faultStep) bool { return o.isPostgres() })
}

func (c *chaos) hitByAnyStep(i int, extra time.Duration) bool {
	return c.hit(i, extra, func(o faultStep) bool { _, out := o.workerOut(); return out || o.isPostgres() })
}

// ---- the observer ----

func (c *chaos) snapshot(ctx context.Context) (jobs []jobSnap, nodes map[string]bool, viewers []viewerSnap, now time.Time, err error) {
	tx, err := c.r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, nil, now, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT id, spec_hash, owner, state, COALESCE(worker, ''), helper, COALESCE(lease_expires_at, 'epoch'), created_at, start_segment, next_segment, COALESCE(error, ''), clock_timestamp()
		FROM transcode_jobs`)
	if err != nil {
		return nil, nil, nil, now, err
	}
	for rows.Next() {
		var j jobSnap
		if err := rows.Scan(&j.ID, &j.Hash, &j.Owner, &j.State, &j.Worker, &j.Helper, &j.Lease, &j.Created, &j.Start, &j.Next, &j.Error, &now); err != nil {
			rows.Close()
			return nil, nil, nil, now, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	nodes = map[string]bool{}
	rows, err = tx.Query(ctx, "SELECT id FROM transcode_nodes")
	if err != nil {
		return nil, nil, nil, now, err
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			nodes[id] = true
		}
	}
	rows.Close()
	rows, err = tx.Query(ctx, "SELECT play_session_id, spec_hash, job_id, stopped, last_seen_at FROM transcode_viewers")
	if err != nil {
		return nil, nil, nil, now, err
	}
	for rows.Next() {
		var v viewerSnap
		if err := rows.Scan(&v.Session, &v.Hash, &v.Job, &v.Stopped, &v.Seen); err != nil {
			rows.Close()
			return nil, nil, nil, now, err
		}
		viewers = append(viewers, v)
	}
	rows.Close()
	if now.IsZero() {
		now = time.Now()
	}
	return jobs, nodes, viewers, now, rows.Err()
}

func (c *chaos) observe() {
	for c.ctx.Err() == nil {
		jobs, nodes, viewers, now, err := c.snapshot(c.ctx)
		if err == nil {
			c.judge(jobs, nodes, viewers, now)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// judge holds one snapshot to the invariants that are about a moment.
func (c *chaos) judge(jobs []jobSnap, nodes map[string]bool, viewers []viewerSnap, now time.Time) {
	byID := map[int64]jobSnap{}
	live := map[string]int{}
	for _, j := range jobs {
		byID[j.ID] = j
		if !j.Helper && (j.State == "queued" || (j.State == "running" && j.Lease.After(now))) {
			live[j.Hash]++
		}
	}
	c.mu.Lock()
	if c.claimant == nil {
		c.claimant, c.lastJobs, c.valid, c.created, c.rowGone, c.rowSeen = map[int64]string{}, map[int64]jobSnap{}, map[int64][]validity{}, map[int64]jobSnap{}, map[string][]time.Time{}, map[string]bool{}
	}
	for id := range nodes {
		c.rowSeen[id] = true
	}
	for id, was := range c.rowSeen {
		if was && !nodes[id] {
			c.rowSeen[id] = false
			c.rowGone[id] = append(c.rowGone[id], now)
		} else if !was && nodes[id] {
			c.rowSeen[id] = true
		}
	}
	type finding struct{ invariant, detail string }
	var found []finding
	for _, j := range jobs {
		if _, ok := c.created[j.ID]; !ok {
			c.created[j.ID] = j
		}
		if j.State == "failed" {
			found = append(found, finding{"no step fails a job", fmt.Sprintf("job %d of spec %s ended failed: %s", j.ID, j.Hash, j.Error)})
		}
		if j.Worker != "" {
			if was, ok := c.claimant[j.ID]; !ok {
				c.claimant[j.ID] = j.Worker
			} else if was != j.Worker {
				found = append(found, finding{"one worker per job", fmt.Sprintf("job %d's worker went from %s to %s", j.ID, was, j.Worker)})
			}
		}
		// Whether a put may begin for the job: the row is the worker's, running, with a lease that
		// has not run out, and the worker has its row.
		if owner := c.claimant[j.ID]; owner != "" {
			v := validity{at: now, valid: true}
			switch {
			case j.State != "running" || j.Worker != owner || !nodes[owner]:
				v.valid, v.invalid = false, now
			case !j.Lease.After(now):
				v.valid, v.invalid = false, j.Lease
			}
			c.valid[j.ID] = append(c.valid[j.ID], v)
		}
		// A job cut short: running a moment ago, cancelled now.
		if was, ok := c.lastJobs[j.ID]; ok && was.State == "running" && j.State == "cancelled" && !j.Helper {
			w := c.workerByIDLocked(j.Worker)
			replaced := false
			for _, o := range jobs {
				if o.ID != j.ID && o.Hash == j.Hash && !o.Helper && (o.State == "queued" || (o.State == "running" && o.Lease.After(now))) {
					replaced = true
				}
			}
			watched := false
			for _, v := range viewers {
				if v.Hash == j.Hash && !v.Stopped && earlier(now.Sub(v.Seen), 20*time.Second) {
					watched = true
				}
			}
			if j.Lease.After(now) && w != nil && !w.killed.Load() && !replaced && watched &&
				!c.delayedLongLocked(j.Worker, was.Lease.Add(-faultClock.Lease), now) {
				found = append(found, finding{"no job of a running worker with a lease is cancelled while a viewer watches it",
					fmt.Sprintf("job %d of spec %s on worker %s was cancelled with %v of its lease left, and no other job of the spec is live", j.ID, j.Hash, j.Worker, j.Lease.Sub(now).Round(time.Millisecond))})
			}
		}
		c.lastJobs[j.ID] = j
	}
	if n := live[c.hashes[0]]; n > 1 {
		var ids []string
		for _, j := range jobs {
			if j.Hash == c.hashes[0] && !j.Helper && (j.State == "queued" || (j.State == "running" && j.Lease.After(now))) {
				ids = append(ids, fmt.Sprintf("%d(%s %s)", j.ID, j.State, j.Worker))
			}
		}
		found = append(found, finding{"one live job per spec", fmt.Sprintf("%d jobs of the spec the viewers play in step were queued or running with a lease at once: %v", n, ids)})
	}
	// No job lost: a viewer that waits on a spec is not left with no job for it. A right build
	// starts the replacement of a dead job at the next look of the request, a moment after the lease
	// ran out; the bound only stops a hang, and the time Postgres is away and the moments after
	// it are not counted, since nothing can be started then.
	if c.noLive == nil {
		c.noLive = map[string]time.Time{}
	}
	pgNear := c.pgAway || earlier(time.Since(c.pgEnded), liveTiming("hold")+5*time.Second)
	for _, hash := range c.hashes {
		waiting := false
		for _, v := range c.viewers {
			if v.hash == hash && v.inRequest.Load() && !v.stopped.Load() {
				waiting = true
			}
		}
		if !waiting || live[hash] > 0 || pgNear {
			delete(c.noLive, hash)
			continue
		}
		since, ok := c.noLive[hash]
		if !ok {
			c.noLive[hash] = now
		} else if earlier(faultHang/6, now.Sub(since)) {
			found = append(found, finding{"no job lost", fmt.Sprintf("a viewer has waited %v on spec %s, which has had no job queued or running with a lease for that long", now.Sub(since).Round(time.Second), hash)})
			delete(c.noLive, hash)
		}
	}
	c.mu.Unlock()
	for _, f := range found {
		c.violate(f.invariant, "%s", f.detail)
	}
}

func (c *chaos) workerByIDLocked(id string) *chaosWorker {
	for _, w := range c.workers {
		if w.id == id {
			return w
		}
	}
	return nil
}

func (c *chaos) delayedLongLocked(worker string, from, to time.Time) bool {
	for _, d := range c.delays {
		if d.worker == worker && d.longerThan && d.from.Before(to) && from.Before(d.to.Add(faultClock.Lease)) {
			return true
		}
	}
	return false
}

// ---- the viewers ----

func (c *chaos) play(v *chaosViewer) {
	defer close(v.done)
	jobs := c.replicas[v.replica].jobs
	for n := 0; n < faultSegments && c.ctx.Err() == nil && !v.stopped.Load(); n++ {
		if !v.group.arrive(v, n) {
			return
		}
		for attempt := 0; c.ctx.Err() == nil && !v.stopped.Load(); attempt++ {
			ctx, cancel := context.WithTimeout(c.ctx, faultHang)
			v.inRequest.Store(true)
			began := time.Now()
			body, err := c.s.fetch(ctx, jobs, v.spec, v.session, n)
			v.inRequest.Store(false)
			cancel()
			if err == nil && len(body) > 0 {
				v.served.Store(int64(n + 1))
				break
			}
			if v.stopped.Load() || c.ctx.Err() != nil {
				return
			}
			if err == nil {
				err = fmt.Errorf("segment %d is empty", n)
			}
			var failed FailedError
			switch {
			case errors.Is(err, ErrStopped):
				c.violate("no job lost", "viewer %d (session %s) was told its session ended and did not stop", v.index, v.session)
			case errors.As(err, &failed):
				c.violate("no step fails a job", "viewer %d was answered with a failed job asking for segment %d: %v", v.index, n, err)
			case db.IsAway(err) || c.pgAwayDuring(began, time.Now()):
				// What a replica answers while Postgres is away is for the API tier to check; the viewer asks again.
			default:
				c.violate("no job lost", "viewer %d asking for segment %d through replica %d was answered with an error although a worker with a free slot ran: %v", v.index, n, v.replica, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// pgAwayDuring says whether Postgres was away from the replicas at some moment in [from, to].
func (c *chaos) pgAwayDuring(from, to time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pgAway || c.pgEnded.After(from)
}

// ---- the steps ----

type stepRecord struct {
	index      int
	kind       faultKind
	begin, end time.Time
	worker     string
}

func (c *chaos) begin(i int, step faultStep) {
	c.mu.Lock()
	c.current = fmt.Sprintf("step %d: %v", i, step)
	c.mu.Unlock()
	c.t.Logf("%v: step %d begins: %v", time.Since(c.started).Round(100*time.Millisecond), i, step)
}

func (c *chaos) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-c.ctx.Done():
	}
}

func (c *chaos) runStep(i int, step faultStep) {
	c.begin(i, step)
	switch step.Kind {
	case faultKill:
		c.kill(i, step)
	case faultPGShort, faultPGLong:
		c.restartPostgres(i, step)
	case faultStopShort, faultStopLease, faultStopStall:
		c.stopWorker(i, step)
	case faultDelaySmall, faultDelayLong:
		c.delayWorker(i, step)
	case faultViewerStop:
		c.stopViewer(step)
	}
	c.t.Logf("%v: step %d ends", time.Since(c.started).Round(100*time.Millisecond), i)
}

// jobsOf are the running jobs of a worker whose lease has not run out.
func (c *chaos) jobsOf(worker string) []jobRow {
	var found []jobRow
	for _, j := range c.r.jobRows(c.t) {
		if j.State == "running" && j.Worker == worker {
			found = append(found, j)
		}
	}
	return found
}

func (c *chaos) kill(i int, step faultStep) {
	w := c.pick(step, func() []*chaosWorker { return c.holders(step.Waited) })
	if w == nil {
		c.t.Logf("step %d found no worker that holds a lease to kill; it is skipped", i)
		return
	}
	// What the replacement is held to: the job the worker holds, the lease as it stood,
	// and whether another worker was running with a slot free to take it.
	var job int64
	var hash string
	var lease time.Time
	err := c.r.pool.QueryRow(context.Background(), `SELECT id, spec_hash, lease_expires_at FROM transcode_jobs
		WHERE state = 'running' AND worker = $1 AND NOT helper ORDER BY id DESC LIMIT 1`, w.id).Scan(&job, &hash, &lease)
	free := false
	for _, o := range c.alive() {
		if o != w && !o.stopped.Load() && c.r.locked(o.id) && len(c.jobsOf(o.id)) == 0 {
			free = true
		}
	}
	c.mu.Lock()
	recent := c.pgAway || earlier(time.Since(c.pgEnded), liveTiming("hold")+time.Second)
	c.mu.Unlock()
	pgSoon := false
	for _, other := range c.plan.Steps {
		if (other.Kind == faultPGShort || other.Kind == faultPGLong) && earlier(other.At, c.plan.Steps[i].At+faultClock.Lease) && earlier(c.plan.Steps[i].At, other.At+other.Length) {
			pgSoon = true
		}
	}
	waiting := slices.Contains(c.waitedSpecs(), hash)
	w.killed.Store(true)
	c.hitWorker(w.id, time.Now(), time.Now().Add(24*time.Hour)) // A dead worker stays hit.
	w.kill()
	c.t.Logf("killed worker %s", w.id)
	go func() {
		c.waitPostgres()
		if replacement, err := c.spawn(); err != nil {
			c.violate("test harness", "starting a replacement worker: %v", err)
		} else if err := c.waitRegistered(replacement); err != nil {
			c.violate("test harness", "%v", err)
		}
	}()
	if err != nil || !free || recent || pgSoon || !waiting {
		c.t.Logf("the kill of %s is not held to the resume check: lease read %v, another worker free %v, Postgres away in the last %v: %v, a restart planned within a lease %v, a viewer waiting on the job %v", w.id, err == nil, free, liveTiming("hold"), recent, pgSoon, waiting)
		return
	}
	c.t.Logf("the kill of %s is held to the resume check: job %d with its lease to %v", w.id, job, lease.Format(time.StampMilli))
	// Resume after a kill: the replacement is claimed before the lease the dead worker held at
	// the kill would have run out. The replacement is a job of the spec made after the dead one,
	// on another worker, and its claim is the first moment the database shows it running.
	for c.ctx.Err() == nil {
		var claimed *string
		var at time.Time
		c.r.pool.QueryRow(context.Background(), `SELECT worker, clock_timestamp() FROM transcode_jobs WHERE spec_hash = $1 AND id > $2 AND worker <> $3 AND NOT helper
			AND state <> 'queued' ORDER BY id LIMIT 1`, hash, job, w.id).Scan(&claimed, &at) //nolint:errcheck
		if claimed != nil {
			c.t.Logf("the replacement of job %d was first seen claimed by %s %v before the lease ran out", job, *claimed, lease.Sub(at).Round(time.Millisecond))
			if !at.Before(lease) {
				c.violate("resume after a kill", "worker %s was killed holding job %d with its lease running to %v and the replacement was first seen claimed at %v, after it", w.id, job, lease.Format(time.StampMilli), at.Format(time.StampMilli))
			}
			return
		}
		if !time.Now().Before(lease.Add(faultHang)) {
			c.violate("no job lost", "worker %s was killed holding job %d and no replacement was claimed", w.id, job)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitPostgres returns when no restart is in progress.
func (c *chaos) waitPostgres() {
	for c.ctx.Err() == nil {
		c.mu.Lock()
		away := c.pgAway
		c.mu.Unlock()
		if !away {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type jobsAndFfmpeg struct {
	jobs    []jobRow
	next    map[int64]int       // The first segment each job had not stored.
	ffmpegs map[string][]string // Per worker.
}

// snapshotRunning is every running job with a lease, and the ffmpeg of each worker.
func (c *chaos) snapshotRunning(only string) jobsAndFfmpeg {
	snap := jobsAndFfmpeg{ffmpegs: map[string][]string{}, next: map[int64]int{}}
	for _, j := range c.r.jobRows(c.t) {
		if j.State == "running" && (only == "" || j.Worker == only) {
			snap.jobs = append(snap.jobs, j)
			snap.next[j.ID] = c.r.count(c.t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", j.ID)
		}
	}
	for _, w := range c.alive() {
		if only != "" && w.id != only {
			continue
		}
		if p, ok := c.r.ffmpegOf(w.outageWorker); ok {
			snap.ffmpegs[w.id] = identities([]ffmpegProcess{p})
		}
	}
	return snap
}

// sameAs says what changed since the snapshot: a job that is not the same row on the same
// worker and still running, an ffmpeg that is not the same process, a worker's row that was gone.
func (c *chaos) sameAs(before jobsAndFfmpeg, from time.Time) []string {
	var changed []string
	now := c.r.jobRows(c.t)
	for _, j := range before.jobs {
		i := slices.IndexFunc(now, func(o jobRow) bool { return o.ID == j.ID })
		if i < 0 || now[i].State != "running" || now[i].Worker != j.Worker {
			state := "gone"
			if i >= 0 {
				state = now[i].State + " on " + now[i].Worker
			}
			changed = append(changed, fmt.Sprintf("job %d of %s was running and is %s", j.ID, j.Worker, state))
		}
	}
	for id, was := range before.ffmpegs {
		w := c.workerByID(id)
		if w == nil {
			continue
		}
		p, ok := c.r.ffmpegOf(w.outageWorker)
		if !ok || !slices.Equal(identities([]ffmpegProcess{p}), was) {
			changed = append(changed, fmt.Sprintf("worker %s's ffmpeg %v is not the process it was", id, was))
		}
	}
	c.mu.Lock()
	for id, times := range c.rowGone {
		for _, at := range times {
			if !at.Before(from) {
				changed = append(changed, fmt.Sprintf("worker %s's row was forgotten at %v", id, at.Format(time.StampMilli)))
			}
		}
	}
	c.mu.Unlock()
	return changed
}

func (c *chaos) restartPostgres(i int, step faultStep) {
	snap := c.snapshotRunning("")
	began := time.Now()
	workers := c.alive()
	c.mu.Lock()
	c.pgAway = true
	c.mu.Unlock()
	for _, w := range workers {
		w.away(true)
	}
	for _, r := range c.replicas {
		r.proxy.Away()
	}
	c.s.replica.Away()
	// Every connection of every process is ended: the workers' backends and the replicas' are gone.
	waitFor(c.t, "the connections of every process to end", faultHang, func() bool {
		var n int
		c.r.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'dbproxy-%'`).Scan(&n) //nolint:errcheck
		return n == 0
	})
	c.sleep(step.Length)
	for _, w := range workers {
		w.back()
	}
	for _, r := range c.replicas {
		r.proxy.Back()
	}
	c.s.replica.Back()
	c.mu.Lock()
	c.pgAway, c.pgEnded = false, time.Now()
	c.mu.Unlock()
	hit := false
	if step.Kind == faultPGShort {
		hit = c.hitByAnyStep(i, 6*time.Second)
	}
	if step.Kind == faultPGShort {
		c.t.Logf("a restart of %v held to 'nothing restarts' for %d jobs: %v", step.Length, len(snap.jobs), !hit)
		// Nothing restarts: the same rows on the same workers with the same ffmpeg, no row forgotten,
		// and what ffmpeg finished meanwhile stored once Postgres is back.
		c.sleep(4 * time.Second)
		if hit {
			return
		}
		for _, change := range c.sameAs(snap, began) {
			c.violate("resume after a Postgres restart", "a restart of %v, shorter than a lease less a heartbeat, restarted something: %s", step.Length, change)
		}
		for _, j := range snap.jobs {
			deadline := time.Now().Add(faultHang)
			for c.ctx.Err() == nil && c.r.count(c.t, "SELECT next_segment FROM transcode_jobs WHERE id = $1 AND state = 'running'", j.ID) <= snap.next[j.ID] {
				if time.Now().After(deadline) {
					c.violate("resume after a Postgres restart", "job %d stored no segment after a short restart", j.ID)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		return
	}
	// One longer than a lease ends those jobs cancelled, never failed; the next request starts
	// each again at the first segment not stored (the starts are judged when the schedule ends).
	// Judged job by job, not step by step: a job of a worker that a kill, a stop or a delay hit
	// in the meantime is left out (that worker's own check is the other step's), the rest are
	// judged whatever else falls beside the restart.
	judged := 0
	for _, j := range snap.jobs {
		waitFor(c.t, fmt.Sprintf("job %d to end after a restart longer than a lease", j.ID), faultHang, func() bool {
			return c.r.count(c.t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND state = 'running'", j.ID) == 0 || c.ctx.Err() != nil
		})
		if c.ctx.Err() != nil || !c.judgedAfterRestart(j.Worker, began) {
			continue
		}
		judged++
		var state string
		c.r.pool.QueryRow(context.Background(), "SELECT state FROM transcode_jobs WHERE id = $1", j.ID).Scan(&state) //nolint:errcheck
		if state != "cancelled" {
			c.violate("resume after a Postgres restart", "job %d was %s after a restart longer than a lease, want cancelled", j.ID, state)
		}
	}
	c.t.Logf("a restart of %v judged %d of %d jobs for 'the jobs end cancelled'", step.Length, judged, len(snap.jobs))
	c.mu.Lock()
	c.judged["restart-long"] += judged
	c.mu.Unlock()
}

// hitWorker records that a step took a worker away or hurt it, from one moment to another.
func (c *chaos) hitWorker(id string, from, to time.Time) {
	c.mu.Lock()
	c.hits = append(c.hits, workerHit{id, from, to})
	c.mu.Unlock()
}

func (c *chaos) judgedAfterRestart(worker string, began time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return judgedAfterRestart(c.hits, worker, began, time.Now())
}

func (c *chaos) stopWorker(i int, step faultStep) {
	w := c.pick(step, func() []*chaosWorker {
		if step.Waited {
			return c.holders(true)
		}
		return c.alive()
	})
	if w == nil {
		c.t.Logf("step %d found no worker to stop; it is skipped", i)
		return
	}
	snap := c.snapshotRunning(w.id)
	var leaseAtStop time.Time
	var jobAtStop int64
	var hash string
	c.r.pool.QueryRow(context.Background(), "SELECT id, spec_hash, lease_expires_at FROM transcode_jobs WHERE state = 'running' AND worker = $1 AND NOT helper ORDER BY id DESC LIMIT 1", w.id).Scan(&jobAtStop, &hash, &leaseAtStop) //nolint:errcheck
	began := time.Now()
	w.stopped.Store(true)
	w.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
	c.sleep(step.Length)
	w.cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
	w.stopped.Store(false)
	c.hitWorker(w.id, began, time.Now())
	c.sleep(4 * time.Second)
	hit := c.hitByPostgres(i, 5*time.Second)
	if hit || c.ctx.Err() != nil {
		return
	}
	if step.Kind == faultStopShort {
		c.t.Logf("a stop of %v held to 'nothing changes' for %d jobs of worker %s", step.Length, len(snap.jobs), w.id)
		// A stop shorter than a lease less a heartbeat leaves the jobs' ids, worker and ffmpeg as they were.
		for _, change := range c.sameAs(snap, began) {
			c.violate("resume after a stop", "a stop of %v, shorter than a lease less a heartbeat, changed something: %s", step.Length, change)
		}
		return
	}
	// After a longer one a job taken over is the job of a worker that stops its own ffmpeg when it continues.
	c.t.Logf("a stop of %v held to 'the worker stops its own ffmpeg when it continues' for %d jobs of worker %s", step.Length, len(snap.jobs), w.id)
	if step.Kind == faultStopLease {
		c.mu.Lock()
		c.judged["stop-lease"] += len(snap.ffmpegs)
		c.mu.Unlock()
	}
	for _, j := range snap.jobs {
		if c.r.count(c.t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND state = 'running'", j.ID) > 0 {
			continue // Nobody took it over, and the worker went on with it.
		}
		for id, was := range snap.ffmpegs {
			if id != w.id {
				continue
			}
			waitFor(c.t, fmt.Sprintf("worker %s to stop its own ffmpeg for job %d after it was taken over", w.id, j.ID), faultHang, func() bool {
				p, ok := c.r.ffmpegOf(w.outageWorker)
				return c.ctx.Err() != nil || !ok || !slices.Equal(identities([]ffmpegProcess{p}), was)
			})
		}
	}
	_, _ = jobAtStop, leaseAtStop
}

func (c *chaos) delayWorker(i int, step faultStep) {
	w := c.pick(step, c.alive)
	if w == nil {
		return
	}
	snap := c.snapshotRunning(w.id)
	began := time.Now()
	w.db.Delay(step.RoundTrip)
	w.live.Delay(step.RoundTrip)
	c.mu.Lock()
	c.delays = append(c.delays, delayWindow{worker: w.id, from: began, to: began.Add(step.Length), roundTrip: step.RoundTrip, longerThan: step.Kind == faultDelayLong})
	c.mu.Unlock()
	c.sleep(step.Length)
	w.db.Delay(0)
	w.live.Delay(0)
	c.hitWorker(w.id, began, time.Now())
	c.sleep(3 * time.Second)
	if step.Kind != faultDelaySmall || c.hitByPostgres(i, 3*time.Second) || c.ctx.Err() != nil {
		return
	}
	c.t.Logf("a round trip of %v held to 'nothing changes' for %d jobs of worker %s", step.RoundTrip, len(snap.jobs), w.id)
	for _, change := range c.sameAs(snap, began) {
		c.violate("a slow database is not a loss", "a round trip of %v changed something: %s", step.RoundTrip, change)
	}
}

func (c *chaos) stopViewer(step faultStep) {
	var stoppable []*chaosViewer
	for _, v := range c.viewers {
		if v.stoppable {
			stoppable = append(stoppable, v)
		}
	}
	v := stoppable[step.Target%len(stoppable)]
	v.stopped.Store(true)
	v.group.leave()
	for try := 0; try < 100 && c.ctx.Err() == nil; try++ {
		if err := c.replicas[v.replica].jobs.Stop(context.Background(), c.r.user, v.session, "", ""); err == nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.violate("test harness", "Stop of viewer %d did not go through", v.index)
}

// ---- the end of a schedule ----

var startedRe = regexp.MustCompile(`msg="transcode started".* job=(\d+)`)

// finish holds the whole schedule to the invariants that are about its history.
func (c *chaos) finish() {
	type finding struct{ invariant, detail string }
	var found []finding
	violate := func(invariant, format string, args ...any) {
		found = append(found, finding{invariant, fmt.Sprintf(format, args...)})
	}
	defer func() {
		for _, f := range found {
			c.violate(f.invariant, "%s", f.detail)
		}
	}()
	// Every check the plan holds was judged on something: a restart longer than a lease on a job,
	// a stop past the lease on a worker's ffmpeg. A plan that cuts them all leaves them unjudged.
	if c.ctx.Err() == nil {
		c.mu.Lock()
		for _, kind := range []faultKind{faultPGLong, faultStopLease} {
			name := map[faultKind]string{faultPGLong: "restart-long", faultStopLease: "stop-lease"}[kind]
			if slices.ContainsFunc(c.plan.Steps, func(s faultStep) bool { return s.Kind == kind }) && c.judged[name] == 0 {
				violate("test harness", "the check of every %s step was cut: nothing was judged by it in this schedule", kind)
			}
		}
		c.mu.Unlock()
	}
	// One worker per job: no two workers ever ran ffmpeg for one job.
	ran := map[int64][]string{}
	c.mu.Lock()
	workers := slices.Clone(c.workers)
	c.mu.Unlock()
	for _, w := range workers {
		for _, line := range strings.Split(w.log.String(), "\n") {
			if m := startedRe.FindStringSubmatch(line); m != nil {
				id, _ := strconv.ParseInt(m[1], 10, 64)
				if !slices.Contains(ran[id], w.id) {
					ran[id] = append(ran[id], w.id)
				}
			}
		}
	}
	for id, who := range ran {
		if len(who) > 1 {
			violate("one worker per job", "job %d was run by %d workers: %v", id, len(who), who)
		}
	}
	puts := c.puts()
	c.mu.Lock()
	created := make(map[int64]jobSnap, len(c.created))
	for id, j := range c.created {
		created[id] = j
	}
	c.mu.Unlock()
	// A job cut short is replaced by one that starts at the first segment not stored, never at the film's start.
	for _, j := range created {
		if j.Helper {
			continue
		}
		stored := map[int]bool{}
		for _, p := range puts {
			if hash, n, ok := parseSegmentKey(p.key); ok && hash == j.Hash && p.at.Before(j.Created) {
				stored[n] = true
			}
		}
		first := 0
		for stored[first] {
			first++
		}
		if j.Start > first || j.Start < first-2 {
			violate("a job cut short is replaced at the first segment not stored", "job %d of spec %s started at segment %d when the first segment not stored was %d", j.ID, j.Hash, j.Start, first)
		}
	}
	// No segment stored by a dead owner.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range puts {
		hash, _, ok := parseSegmentKey(p.key)
		if !ok {
			continue
		}
		var job int64
		for id, j := range created {
			if j.Hash == hash && c.claimant[id] == p.worker && !j.Created.After(p.at) && id > job {
				job = id
			}
		}
		if job == 0 {
			continue
		}
		samples := c.valid[job]
		last := -1
		for k, s := range samples {
			if !s.at.After(p.at) && s.valid {
				last = k
			}
		}
		if last < 0 || last+1 >= len(samples) {
			continue
		}
		first := samples[last+1]
		if first.valid || first.at.After(p.at) {
			continue
		}
		stillInvalid := true
		for _, s := range samples[last+1:] {
			if !s.at.After(p.at) && s.valid {
				stillInvalid = false
			}
		}
		if !stillInvalid {
			continue
		}
		allowed := 250*time.Millisecond + 100*time.Millisecond
		for _, d := range c.delays {
			if d.worker == p.worker && d.from.Before(p.at) && first.invalid.Before(d.to.Add(faultClock.Lease)) {
				allowed = max(allowed, 250*time.Millisecond+100*time.Millisecond+d.roundTrip)
			}
		}
		if p.at.Sub(first.invalid) > allowed {
			violate("no segment stored by a dead owner", "worker %s began a put of %s at %v, %v after job %d stopped being its own (the check before a put allows %v)",
				p.worker, p.key, p.at.Format(time.StampMilli), p.at.Sub(first.invalid).Round(time.Millisecond), job, allowed)
		}
	}
	// With the check unanswered (a round trip of half a lease), no put begins once the delay has had its effect.
	for _, d := range c.delays {
		if !d.longerThan {
			continue
		}
		for _, p := range puts {
			if p.worker == d.worker && p.at.After(d.from.Add(d.roundTrip+time.Second)) && p.at.Before(d.to) {
				violate("no segment stored by a dead owner", "worker %s began a put of %s at %v while its database answered after %v and the check before a put could not be answered", p.worker, p.key, p.at.Format(time.StampMilli), d.roundTrip)
			}
		}
	}
}

var segmentKeyRe = regexp.MustCompile(`^segments/([0-9a-f]+)/(\d+)\.mp4$`)

func parseSegmentKey(key string) (hash string, n int, ok bool) {
	m := segmentKeyRe.FindStringSubmatch(key)
	if m == nil {
		return "", 0, false
	}
	n, _ = strconv.Atoi(m[2])
	return m[1], n, true
}

func (c *chaos) puts() []putEvent {
	var all []putEvent
	for _, p := range putsOf(c.t, c.s.putlog) {
		all = append(all, putEvent{worker: p.worker, key: p.key, at: p.at})
	}
	return all
}

// ---- running a schedule ----

func runFaultSchedule(t *testing.T, seed uint64, index int, plan faultSchedule) {
	t.Helper()
	dir := chaosFilm(t) // Before the shim below, which would make it at the speed of the film.
	slow(t)             // ffmpeg reads at the speed of the film: viewers play at 1x.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStack(t, 0)
	r := s.r
	// The viewers play a longer film than the one the rig made.
	r.exec(t, "UPDATE libraries SET media_store_uri = $1, asset_store_uri = $1", "file://"+dir)
	r.exec(t, "UPDATE items SET run_time_ticks = 1500000000")
	c := &chaos{t: t, seed: seed, index: index, plan: plan, r: r, s: s, ctx: ctx, cancel: cancel, started: time.Now(), judged: map[string]int{}}
	other, otherHash := r.otherSpec()
	c.specs, c.hashes = [2]playback.Spec{r.spec, other}, [2]string{r.hash, otherHash}
	for i := range c.replicas {
		proxy := testenv.NewDBProxy(t, r.dsn)
		pool, err := db.Open(ctx, proxy.DSN())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		hub := &Hub{Pool: pool, Log: r.log}
		go hub.Run(ctx)
		c.replicas[i] = &chaosReplica{proxy: proxy, jobs: &Jobs{Pool: pool, Segments: r.segments, Hub: hub, QueueTimeout: 20 * time.Second, WaitTimeout: faultHang - 10*time.Second}}
	}
	t.Logf("%s", describeSchedule(seed, index, plan))
	for range faultWorkers {
		w, err := c.spawn()
		if err != nil {
			t.Fatal(err)
		}
		if err := c.waitRegistered(w); err != nil {
			t.Fatal(err)
		}
	}
	// Five viewers: three of one spec through both replicas (two of them in step through different
	// replicas, the third one that may be stopped), and two of the other.
	for i, spec := range []int{0, 0, 0, 1, 1} {
		c.viewers = append(c.viewers, &chaosViewer{index: i, spec: c.specs[spec], hash: c.hashes[spec], session: psid(100 + i), replica: i % 2,
			stoppable: i == 2 || i == 4, done: make(chan struct{})})
	}
	newChaosGroup(ctx, c.viewers[0], c.viewers[1], c.viewers[2])
	newChaosGroup(ctx, c.viewers[3], c.viewers[4])
	go c.observe()
	c.started = time.Now()
	for _, v := range c.viewers {
		go c.play(v)
	}
	// The steps, each at its moment, together.
	var steps sync.WaitGroup
	for i, step := range plan.Steps {
		steps.Add(1)
		go func() {
			defer steps.Done()
			c.sleep(time.Until(c.started.Add(step.At)))
			if c.ctx.Err() == nil {
				c.runStep(i, step)
			}
		}()
	}
	steps.Wait()
	// The viewers that have not stopped go on being served after the last step: no job is lost.
	c.mu.Lock()
	c.current = "after the last step"
	c.mu.Unlock()
	marks := map[*chaosViewer]int64{}
	for _, v := range c.viewers {
		marks[v] = v.served.Load()
	}
	deadline := time.Now().Add(faultHang)
	for c.ctx.Err() == nil {
		behind := false
		for _, v := range c.viewers {
			if !v.stopped.Load() && v.served.Load() < min(marks[v]+3, faultSegments) {
				behind = true
			}
		}
		if !behind {
			break
		}
		if time.Now().After(deadline) {
			for _, v := range c.viewers {
				if !v.stopped.Load() && v.served.Load() < min(marks[v]+3, faultSegments) {
					c.violate("no job lost", "viewer %d was not served another segment for %v after the last step (served %d, was at %d)", v.index, faultHang, v.served.Load(), marks[v])
				}
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	failedAlready := c.ctx.Err() != nil
	cancel()
	for _, v := range c.viewers {
		<-v.done
	}
	_ = failedAlready
	c.finish()
	t.Logf("schedule %d ran for %v", index, time.Since(c.started).Round(time.Second))
	c.mu.Lock()
	violations := slices.Clone(c.violations)
	c.mu.Unlock()
	for _, v := range violations {
		t.Errorf("invariant %q broken; seed %d, schedule %d, after step %q: %s", v.invariant, seed, index, v.after, v.detail)
	}
}

// runFaultRun is one run: the seed named by the environment or a new one, and the schedules
// of its plan one after the other. The error is nil when no invariant was broken.
func runFaultRun(t *testing.T, seed uint64) {
	t.Helper()
	began := time.Now()
	plan := planFaults(seed, faultClock)
	for i, schedule := range plan {
		// A schedule that broke an invariant has said what there is to say.
		if !t.Run(fmt.Sprintf("seed %d schedule %d", seed, i), func(t *testing.T) { runFaultSchedule(t, seed, i, schedule) }) {
			return
		}
	}
	t.Logf("seed %d: %d schedules in %v", seed, len(plan), time.Since(began).Round(time.Second))
}

// The invariants of the leases and of failover hold under randomized schedules of faults, run against
// real worker processes: at most one owner per job, one live job per spec, no job lost, no step that
// fails a job, a resume after a kill, a stop and a restart of Postgres, a slow database that is no
// loss, and no segment stored by a dead owner. JELLYSCALE_TEST_FAULT_SEED replays a seed.
// JELLYSCALE_TEST_FAULT_SOAK=n repeats the run n times with a new seed each time, for hunting
// flakes, and asserts nothing the normal run does not.
func TestFaultSchedulesHoldTheLeaseAndFailoverInvariants(t *testing.T) {
	soak := 0
	if raw := os.Getenv("JELLYSCALE_TEST_FAULT_SOAK"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			t.Fatalf("JELLYSCALE_TEST_FAULT_SOAK is %q, want a number of repetitions", raw)
		}
		soak = n
	}
	if soak == 0 {
		seed, err := faultSeed(os.Getenv("JELLYSCALE_TEST_FAULT_SEED"))
		if err != nil {
			t.Fatal(err)
		}
		runFaultRun(t, seed)
		return
	}
	var failing []string
	passed := 0
	for i := range soak {
		seed, err := faultSeed("")
		if err != nil {
			t.Fatal(err)
		}
		ok := t.Run(fmt.Sprintf("repetition %d", i), func(t *testing.T) { runFaultRun(t, seed) })
		if ok {
			passed++
		} else {
			failing = append(failing, strconv.FormatUint(seed, 10))
		}
	}
	t.Logf("passed %d of %d", passed, soak)
	if passed < soak {
		t.Errorf("passed %d of %d; failing seeds: %s", passed, soak, strings.Join(failing, " "))
	}
}
