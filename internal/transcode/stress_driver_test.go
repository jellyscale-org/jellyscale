//go:build integration || stress

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/stress"
)

// The control-plane stress driver: V viewers and W workers that make the calls
// the real code makes, at the rates it makes them, against one Postgres, with no
// video. Everything on the database goes through the real Jobs, Worker and Reap
// methods, so a change to them changes what is measured. What is faked is the
// ffmpeg: a claimed "job" stores a small segment every 4 seconds, as a
// re-encode at real time does, and heartbeats. It is driven by
// TestStressDriverCallsEveryOperationAtItsRate (integration tier, a few seconds)
// and TestControlPlaneLadder (stress tag, test/stress.sh).
//
// Calls are open loop: each tick starts its call in a goroutine, so a slow
// database shows as latency and not as a quietly lower rate. A call that
// finishes or fails counts; the step fails with stress.ErrRate if too few did.
// Left out: the worker's row refresh (a one-row update every 3 s per worker)
// and the hint-driven wake-ups of Segment waiters beyond their 250 ms poll.

type stressConfig struct {
	Viewers, Workers, Specs int
	Duration                time.Duration
	// Container is the Postgres container to read CPU from with docker stats;
	// empty reads none.
	Container string
	// Pool is the pgx pool size shared by everything but the liveness
	// connections; zero is JELLYSCALE_STRESS_POOL, else db.DefaultMaxConns.
	Pool int32
	// AfterSetup runs once the rows exist, before the load starts.
	AfterSetup func(ctx context.Context)
}

const (
	viewerPeriod = 4 * time.Second // A segment request per viewer, while re-encoding.
	segmentEvery = 4 * time.Second // A re-encode at real time stores a segment per 4 s.
	callTimeout  = 5 * time.Second
	stressUser   = "00000000-0000-4000-8000-000000057355"
	slotsPerNode = 4
)

// recorder holds what each operation did. Only calls scheduled before the
// deadline count; the ones after it keep the load realistic while the last
// Segment waits finish.
type recorder struct {
	deadline time.Time
	mu       sync.Mutex
	ops      map[string]*opLog
}

type opLog struct {
	intended, errors int
	samples          []time.Duration
}

func (r *recorder) want(name string) {
	if time.Now().After(r.deadline) {
		return
	}
	r.mu.Lock()
	r.log(name).intended++
	r.mu.Unlock()
}

// call times f. It returns f's error.
func (r *recorder) call(ctx context.Context, name string, timeout time.Duration, f func(context.Context) error) error {
	counted := !time.Now().After(r.deadline)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	began := time.Now()
	err := f(ctx)
	if counted {
		r.mu.Lock()
		op := r.log(name)
		op.samples = append(op.samples, time.Since(began))
		if err != nil {
			op.errors++
		}
		r.mu.Unlock()
	}
	return err
}

func (r *recorder) log(name string) *opLog {
	if r.ops[name] == nil {
		r.ops[name] = &opLog{}
	}
	return r.ops[name]
}

func (r *recorder) result() map[string]stress.Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]stress.Op{}
	for name, op := range r.ops {
		out[name] = stress.Op{Intended: op.intended, Calls: len(op.samples), Errors: op.errors,
			P50: stress.Percentile(op.samples, 50), P99: stress.Percentile(op.samples, 99)}
	}
	return out
}

type driver struct {
	cfg   stressConfig
	pool  *pgxpool.Pool
	jobs  *Jobs
	hub   *Hub
	segs  store.Store
	rec   *recorder
	calls sync.WaitGroup // In-flight calls of the viewers.
	specs []playback.Spec
}

func runStress(ctx context.Context, t testing.TB, dsn string, cfg stressConfig) (stress.Step, error) {
	t.Helper()
	size := cfg.Pool
	if n, err := strconv.Atoi(os.Getenv("JELLYSCALE_STRESS_POOL")); size == 0 && err == nil {
		size = int32(n) // test/stress.sh asks for a pool size to compare.
	}
	if size == 0 {
		size = db.DefaultMaxConns
	}
	pool, err := db.Open(ctx, dsn, db.WithMaxConns(size))
	if err != nil {
		return stress.Step{}, err
	}
	defer pool.Close()
	d := &driver{cfg: cfg, pool: pool, segs: store.NewPosix(t.TempDir()), rec: &recorder{ops: map[string]*opLog{}}}
	d.hub = &Hub{Pool: pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.jobs = &Jobs{Pool: pool, Segments: d.segs, Hub: d.hub, WaitTimeout: 10 * time.Second}
	for i := range cfg.Specs {
		d.specs = append(d.specs, playback.Spec{Item: fmt.Sprintf("stress-%d", i)})
	}

	d.clean()
	defer d.clean()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash) VALUES ($1, 'stress', '') ON CONFLICT DO NOTHING`, stressUser); err != nil {
		return stress.Step{}, err
	}

	hubCtx, stopHub := context.WithCancel(ctx)
	hubDone := make(chan struct{})
	go func() { defer close(hubDone); d.hub.Run(hubCtx) }()
	defer func() { stopHub(); <-hubDone }()

	workers, err := d.startWorkers(ctx, t, dsn)
	if err != nil {
		return stress.Step{}, err
	}
	defer workers.close()
	time.Sleep(300 * time.Millisecond) // The hub's LISTEN.

	if cfg.AfterSetup != nil {
		cfg.AfterSetup(ctx)
	}
	d.rec.deadline = time.Now().Add(cfg.Duration)
	watching := sampleDatabase(ctx, dsn, cfg.Container)

	var viewers sync.WaitGroup
	loadCtx, stopLoad := context.WithDeadline(ctx, d.rec.deadline)
	defer stopLoad()
	for i := range cfg.Viewers {
		viewers.Add(1)
		go func() { defer viewers.Done(); d.viewer(loadCtx, ctx, i) }()
	}
	viewers.Wait()
	d.calls.Wait() // The last requests finish while the workers still serve them.
	workers.stopLoad()
	cpu, conns := watching()

	step := stress.Step{Viewers: cfg.Viewers, Workers: cfg.Workers, Pool: int(size), Ops: d.rec.result(), PostgresCPU: cpu, Connections: conns}
	t.Logf("pool %d\n%s", size, stress.Format([]stress.Step{step}))
	return step, stress.RateShortfall(step)
}

// clean removes the rows a run leaves. It gives up quickly: a test that holds a
// lock must not hang the cleanup, and the next run cleans again.
func (d *driver) clean() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, statement := range []string{
		"DELETE FROM transcode_viewers WHERE spec_hash LIKE 'stress-%'",
		"DELETE FROM transcode_jobs WHERE spec_hash LIKE 'stress-%'",
		"DELETE FROM transcode_nodes WHERE id LIKE 'stress-%'",
	} {
		d.pool.Exec(ctx, statement) //nolint:errcheck
	}
}

func hashOf(spec int) string { return fmt.Sprintf("stress-%d", spec) }

// viewer asks for a segment every viewerPeriod, as a player does: a Watch and a
// Segment, and a PlaybackInfo (Unavailable) when it starts and every 30 s. Its
// first request is anywhere in the first period.
func (d *driver) viewer(load, ctx context.Context, i int) {
	spec := i % d.cfg.Specs
	viewer := Viewer{PlaySessionID: fmt.Sprintf("stress-v%d", i), DeviceID: fmt.Sprintf("stress-d%d", i), UserID: stressUser, Item: d.specs[spec].Item}
	if !sleep(load, rand.N(viewerPeriod)) {
		return
	}
	ticker := time.NewTicker(viewerPeriod)
	defer ticker.Stop()
	for n := 0; ; n++ {
		d.rec.want("Watch")
		d.rec.want("Segment")
		d.calls.Add(1)
		go func() {
			defer d.calls.Done()
			err := d.rec.call(ctx, "Watch", callTimeout, func(ctx context.Context) error {
				return d.jobs.Watch(ctx, viewer, hashOf(spec), n)
			})
			if err != nil {
				return
			}
			d.rec.call(ctx, "Segment", 12*time.Second, func(ctx context.Context) error { //nolint:errcheck
				_, err := d.jobs.Segment(ctx, d.specs[spec], hashOf(spec), viewer, n, 0)
				return err
			})
		}()
		if n%8 == 0 {
			d.rec.want("Unavailable")
			d.calls.Add(1)
			go func() {
				defer d.calls.Done()
				d.rec.call(ctx, "Unavailable", callTimeout, func(ctx context.Context) error { //nolint:errcheck
					_, err := d.jobs.Unavailable(ctx, hashOf(spec), stressUser, viewer.DeviceID)
					return err
				})
			}()
		}
		select {
		case <-ticker.C:
		case <-load.Done():
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// fleet is the fake workers.
type fleet struct {
	live    []*liveness
	cancel  context.CancelFunc
	running sync.WaitGroup
}

func (f *fleet) stopLoad() { f.cancel(); f.running.Wait() }

func (f *fleet) close() {
	f.stopLoad()
	for _, l := range f.live {
		l.Close()
	}
}

// startWorkers registers W workers, each holding its liveness lock on a
// connection of its own as Present does, and starts their loops.
func (d *driver) startWorkers(ctx context.Context, t testing.TB, dsn string) (*fleet, error) {
	connConfig := d.pool.Config().ConnConfig
	f := &fleet{live: make([]*liveness, d.cfg.Workers)}
	loops, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	nodes := make([]*Worker, d.cfg.Workers)
	var group sync.WaitGroup
	var failed atomic.Value
	gate := make(chan struct{}, 32)
	for i := range nodes {
		nodes[i] = &Worker{Pool: d.pool, Segments: d.segs, Hub: d.hub, Slots: slotsPerNode, Scratch: t.TempDir(),
			ID: fmt.Sprintf("stress-w%d", i), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		group.Add(1)
		go func() {
			defer group.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			live, err := holdLiveness(ctx, connConfig, nodes[i].ID)
			if err == nil {
				f.live[i] = live
				_, err = d.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ($1, 'worker', '', $2, true)
					ON CONFLICT (id) DO UPDATE SET seen_at = now(), locked = true`, nodes[i].ID, slotsPerNode)
			}
			if err != nil {
				failed.Store(err)
			}
		}()
	}
	group.Wait()
	if err, _ := failed.Load().(error); err != nil {
		cancel()
		for _, l := range f.live {
			if l != nil {
				l.Close()
			}
		}
		return nil, fmt.Errorf("starting the workers: %w", err)
	}
	// The loops start after every worker is registered, as a deployment's are
	// long running; each begins anywhere in its period.
	for _, w := range nodes {
		f.running.Add(3)
		go func() { defer f.running.Done(); d.reapLoop(loops, w) }()
		go func() { defer f.running.Done(); d.claimLoop(loops, w, &f.running) }()
		go func() { defer f.running.Done(); d.sweepLoop(loops, w) }()
	}
	return f, nil
}

// reapLoop is keepRow's look every lookPeriod.
func (d *driver) reapLoop(ctx context.Context, w *Worker) {
	if !sleep(ctx, rand.N(liveTiming("lookPeriod"))) {
		return
	}
	ticker := time.NewTicker(liveTiming("lookPeriod"))
	defer ticker.Stop()
	for {
		d.rec.want("Reap")
		go d.rec.call(ctx, "Reap", callTimeout, func(ctx context.Context) error { //nolint:errcheck
			_, _, _, err := Reap(ctx, d.pool, w.ID)
			return err
		})
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// sweepLoop is sweepLoop, once a minute.
func (d *driver) sweepLoop(ctx context.Context, w *Worker) {
	if !sleep(ctx, rand.N(time.Minute)) {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		d.rec.want("Sweep")
		d.rec.call(ctx, "Sweep", 30*time.Second, w.Sweep) //nolint:errcheck
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// claimLoop is Run: a claim every second and on each new-job hint, while there
// is a free slot, and again at once after a claim that found a job.
func (d *driver) claimLoop(ctx context.Context, w *Worker, running *sync.WaitGroup) {
	var busy atomic.Int64
	wake, unsubscribe := d.hub.Subscribe(jobHint)
	defer unsubscribe()
	if !sleep(ctx, rand.N(time.Second)) {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	scheduled := true
	for {
		first := scheduled
		for busy.Load() < slotsPerNode {
			if first {
				d.rec.want("claim")
			}
			first = false
			var job claimed
			var ok bool
			err := d.rec.call(ctx, "claim", callTimeout, func(ctx context.Context) (err error) {
				job, ok, err = w.claim(ctx)
				return err
			})
			if err != nil || !ok {
				break
			}
			busy.Add(1)
			running.Add(1)
			go func() { defer running.Done(); defer busy.Add(-1); d.fakeJob(ctx, w, job) }()
		}
		select {
		case <-wake:
			scheduled = false
		case <-ticker.C:
			scheduled = true
		case <-ctx.Done():
			return
		}
	}
}

// fakeJob is a re-encode that needs no ffmpeg: it stores a segment at once and
// then one every segmentEvery, beats every heartbeat, and, as a job at its
// lead limit does, asks every leadCheck how far the viewers are.
func (d *driver) fakeJob(ctx context.Context, w *Worker, job claimed) {
	next := job.Start
	storeSegment := func() {
		d.rec.want("store")
		err := d.rec.call(ctx, "store", callTimeout, func(ctx context.Context) error {
			if next == job.Start {
				if err := d.segs.Put(ctx, SegmentKey(job.Hash, -1), bytes.NewReader(make([]byte, 1024))); err != nil {
					return err
				}
			}
			if err := d.segs.Put(ctx, SegmentKey(job.Hash, next), bytes.NewReader(make([]byte, 1024))); err != nil {
				return err
			}
			_, err := d.pool.Exec(ctx, "UPDATE transcode_jobs SET next_segment = $2 WHERE id = $1 AND worker = $3", job.ID, next+1, w.ID)
			return err
		})
		if err == nil {
			next++
		}
		d.rec.want("Notify")
		d.rec.call(ctx, "Notify", callTimeout, func(ctx context.Context) error { //nolint:errcheck
			Notify(ctx, d.pool, segmentHint(job.Hash))
			return nil
		})
	}
	beat := func() {
		d.rec.want("beat")
		go d.rec.call(ctx, "beat", callTimeout, func(ctx context.Context) error { //nolint:errcheck
			_, _, err := w.beat(ctx, job)
			return err
		})
	}
	look := func() {
		d.rec.want("furthest")
		go d.rec.call(ctx, "furthest", callTimeout, func(ctx context.Context) error { //nolint:errcheck
			_, _, _, err := w.furthest(ctx, job, next)
			return err
		})
	}
	storeSegment()
	beat()
	segments, beats, looks := time.NewTicker(segmentEvery), time.NewTicker(w.heartbeat()), time.NewTicker(leadCheck)
	defer segments.Stop()
	defer beats.Stop()
	defer looks.Stop()
	for {
		select {
		case <-segments.C:
			storeSegment()
		case <-beats.C:
			beat()
		case <-looks.C:
			look()
		case <-ctx.Done():
			return
		}
	}
}

// sampleDatabase watches the database until the deadline: the most connections
// it has, and the CPU of the container (docker stats, percent of one core)
// averaged over the samples. The returned function stops it and gives both.
func sampleDatabase(ctx context.Context, dsn, container string) func() (cpu float64, conns int) {
	sampling, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	var total float64
	var samples, peak int
	go func() {
		defer close(done)
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		defer conn.Close(context.Background()) //nolint:errcheck
		for sampling.Err() == nil {
			var n int
			if conn.QueryRow(sampling, "SELECT count(*) FROM pg_stat_activity").Scan(&n) == nil {
				peak = max(peak, n)
			}
			if container != "" {
				out, err := exec.CommandContext(sampling, "docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}", container).Output()
				if value, perr := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(string(out)), "%"), 64); err == nil && perr == nil {
					total += value
					samples++
				}
			} else {
				sleep(sampling, time.Second)
			}
		}
	}()
	return func() (float64, int) {
		stop()
		<-done
		if samples == 0 {
			return -1, peak // never measured; the report says so rather than 0%
		}
		return total / float64(samples), peak
	}
}
