//go:build integration

package transcode

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/metrics"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"

	"github.com/prometheus/client_golang/prometheus"
)

// The kit for JS-167's checks: a helper whose viewers have all moved away from
// it ends, and its worker is free for the film's new place (D-046).
//
// The workers are in this process (each with its own pool, as r.worker makes
// them), one slot each, so a test reads each one's log and /metrics. Their
// heartbeat and lease are the production ones (3 s, 10 s) unless a check is
// about what the helper does before its next heartbeat, which then runs at 10 s
// and 30 s as TestAPausedHelperLetsGoOfItsSlotAtOnce does. A shorter heartbeat
// would let a build that ends a helper only at its heartbeat pass the 5 second
// bounds below, and one that long makes the bound a statement about the poll.
//
// "Within 5 seconds" is the bound the criteria give, ten times the half second a
// job at its limit waits between looks (leadCheck): it is what a hang is told
// from, not a speed this machine must reach.

// leftBehindBound is how soon after a seek's first segment request a helper with no viewer left is over.
const leftBehindBound = 5 * time.Second

// lbStream is a stream of the rig's film: a spec and its hash.
type lbStream struct {
	spec playback.Spec
	hash string
}

func (r *rig) mainStream() lbStream { return lbStream{r.spec, r.hash} }

func (r *rig) streamOf(bitrate int64) lbStream {
	spec, hash := r.otherStream(bitrate)
	return lbStream{spec, hash}
}

// cluster is n workers of one slot, in this process.
type cluster struct {
	r     *rig
	j     *Jobs
	ids   []string
	ws    map[string]*Worker
	logs  map[string]*logBuffer
	stops map[string]func()
	proxy *testenv.DBProxy // Set when the workers' statements go through one.
	late  func()           // Starts the workers that are made after the first request, if any.
}

type clusterOpts struct {
	n         int
	lead      time.Duration
	heartbeat time.Duration // 3 s when zero.
	idle      time.Duration
	noHelpers bool
	cores     float64 // The cores each worker declares; one slot's worth (1) when zero. JS-168's checks give the cost of one job, 0.8.
	// proxied sends the statements of every worker but the first through a proxy
	// (c.proxy), which the test delays, holds and reads; the first runs the viewer's
	// own job, direct, and the others start after the viewer's first segment.
	// The proxy's connections send each statement's text (exec mode), which a
	// helper's insert of its spec as JSON cannot be sent in, so the worker that
	// inserts helpers (the one running the viewer's job) is direct, and a helper
	// is run, and reads the viewers, on the others.
	proxied bool
	// proxiedAll sends every worker's through the proxy, for a test with no helper.
	proxiedAll bool
}

func (r *rig) cluster(t *testing.T, o clusterOpts) *cluster {
	t.Helper()
	if o.heartbeat == 0 {
		o.heartbeat = 3 * time.Second
	}
	c := &cluster{r: r, ws: map[string]*Worker{}, logs: map[string]*logBuffer{}, stops: map[string]func(){}}
	var apply func(*Worker)
	if o.proxied || o.proxiedAll {
		c.proxy, apply = r.proxied(t)
	}
	add := func(through bool) {
		var w *Worker
		log := &logBuffer{}
		stop := r.worker(t, func(x *Worker) {
			w = x
			w.Slots, w.ChunkSegments, w.Lead = 1, 4, o.lead
			w.Heartbeat, w.Lease = o.heartbeat, o.heartbeat*10/3
			if o.idle > 0 {
				w.Idle = o.idle
			}
			w.NoHelpers = o.noHelpers
			if o.cores > 0 {
				w.Cores = o.cores
			}
			w.Log = log.logger()
			if through {
				apply(w)
			}
		})
		c.ids = append(c.ids, w.ID)
		c.ws[w.ID], c.logs[w.ID], c.stops[w.ID] = w, log, stop
	}
	if o.proxied {
		add(false)
		c.late = func() {
			for i := 1; i < o.n; i++ {
				add(true)
			}
		}
	} else {
		for i := 0; i < o.n; i++ {
			add(o.proxiedAll)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			for id, log := range c.logs {
				t.Logf("worker %s log:\n%s", id, log.String())
			}
		}
	})
	c.j = r.jobs(t) // After the workers: the replica reads the store they write.
	return c
}

// hrow is a row of a spec's jobs.
type hrow struct {
	ID          int64
	Worker      string
	Start, Next int
	State, Err  string
	Helper      bool
}

func (r *rig) rowsOf(t *testing.T, hash string) []hrow {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT id, COALESCE(worker, ''), start_segment, next_segment, state, COALESCE(error, ''), helper
		FROM transcode_jobs WHERE spec_hash = $1 ORDER BY id`, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []hrow
	for rows.Next() {
		var h hrow
		if err := rows.Scan(&h.ID, &h.Worker, &h.Start, &h.Next, &h.State, &h.Err, &h.Helper); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

func (r *rig) rowOf(t *testing.T, id int64) hrow {
	t.Helper()
	var h hrow
	if err := r.pool.QueryRow(context.Background(), `SELECT id, COALESCE(worker, ''), start_segment, next_segment, state, COALESCE(error, ''), helper
		FROM transcode_jobs WHERE id = $1`, id).Scan(&h.ID, &h.Worker, &h.Start, &h.Next, &h.State, &h.Err, &h.Helper); err != nil {
		t.Fatal(err)
	}
	return h
}

// helpersOf are the spec's helper rows.
func (r *rig) helpersOf(t *testing.T, hash string) (out []hrow) {
	t.Helper()
	for _, h := range r.rowsOf(t, hash) {
		if h.Helper {
			out = append(out, h)
		}
	}
	return out
}

func (r *rig) storedOn(hash string, n int) bool {
	body, err := r.segments.Open(context.Background(), SegmentKey(hash, n), 0)
	if err == nil {
		body.Close()
	}
	return err == nil
}

// storedBetween are the stored segments of the spec from lo up to, not including, hi.
func (r *rig) storedBetween(hash string, lo, hi int) (out []int) {
	for n := lo; n < hi; n++ {
		if r.storedOn(hash, n) {
			out = append(out, n)
		}
	}
	return out
}

// ffmpegRun is an ffmpeg writing into the rig's scratch: the segment it starts at and the kernel's state letter (T: stopped).
type ffmpegRun struct{ start, state string }

func (r *rig) runs() (out []ffmpegRun) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 || !strings.Contains(string(raw), r.scratch) {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if !strings.HasPrefix(filepath.Base(args[0]), "ffmpeg") {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		after := string(stat[strings.LastIndexByte(string(stat), ')')+2:])
		out = append(out, ffmpegRun{segmentStart(strings.Join(args, " ")), after[:1]})
	}
	return out
}

// ffmpegAt is the state letter of an ffmpeg started at segment n, or "" if none runs.
func (r *rig) ffmpegAt(n int) string {
	for _, p := range r.runs() {
		if p.start == strconv.Itoa(n) {
			return p.state
		}
	}
	return ""
}

// seeking is a viewer's request for a segment, made in the background.
type seeking struct {
	at   time.Time // Just before the viewer's first request.
	done chan error
}

func (r *rig) seek(t *testing.T, j *Jobs, s lbStream, psid string, n int) *seeking {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	sk := &seeking{at: time.Now(), done: make(chan error, 1)}
	go func() {
		v := r.viewer(psid)
		if err := j.Watch(ctx, v, s.hash, n); err != nil {
			sk.done <- err
			return
		}
		_, err := bytesOf(j.Segment(ctx, s.spec, s.hash, v, n, 0))
		sk.done <- err
	}()
	t.Cleanup(func() { cancel(); <-sk.done })
	return sk
}

// answered fails the test unless the seek's segment came, within the hang bound.
func (sk *seeking) answered(t *testing.T) {
	t.Helper()
	select {
	case err := <-sk.done:
		if err != nil {
			t.Errorf("the seek's segment request failed: %v", err)
		}
		sk.done <- err // For the cleanup.
	case <-time.After(80 * time.Second):
		t.Errorf("the seek's segment was not answered in 80 seconds")
	}
}

// within waits until ok, and fails the test if it is not true by at, with what.
func within(t *testing.T, at time.Time, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		if time.Now().After(at) {
			t.Fatalf("not true by the bound: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func sleepUntil(at time.Time) { time.Sleep(time.Until(at)) }

// wantOver is the bound the criteria give for a helper whose viewers are all
// away: by leftBehindBound after t0 its row is cancelled (not failed), its
// ffmpeg is gone and its worker's cores are free. The row's state is read last,
// so a row still running when the rest is true fails on its own.
func (c *cluster) wantOver(t *testing.T, h hrow, t0 time.Time, why string) {
	t.Helper()
	by := t0.Add(leftBehindBound)
	within(t, by, why+": the helper "+strconv.FormatInt(h.ID, 10)+" still running: "+strconv.Quote(c.r.rowOf(t, h.ID).State), func() bool {
		return c.r.rowOf(t, h.ID).State != "running"
	})
	within(t, by, why+": the helper's ffmpeg is still running", func() bool { return c.r.ffmpegAt(h.Start) == "" })
	// The worker may take another job at once (the new viewer's, or a helper of it), so what it counts is
	// the cost of the jobs it runs, which the helper's is not among.
	within(t, by, why+": the helper's worker still counts the helper's cores busy", func() bool {
		var others int
		if err := c.r.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_jobs WHERE worker = $1 AND state = 'running' AND id <> $2", h.Worker, h.ID).Scan(&others); err != nil {
			t.Fatal(err)
		}
		return value(scrape(t, c.ws[h.Worker]), "jellyscale_transcode_cores_busy") == float64(others)*c.r.spec.Cost()
	})
	if row := c.r.rowOf(t, h.ID); row.State != "cancelled" {
		t.Fatalf("%s: the helper ended as %q (%q), want cancelled: a viewer who left is no failure", why, row.State, row.Err)
	}
}

// scene is a viewer of the stream, a running helper of it, and the cluster.
type scene struct {
	c      *cluster
	s      lbStream
	helper hrow
}

// The helper is chosen by the state its ffmpeg is in: "running" is making a
// segment short of its limit, "paused" is stopped at it.
func (c *cluster) helperIn(t *testing.T, s lbStream, state string, minNext int) hrow {
	t.Helper()
	var found hrow
	defer func() {
		if t.Failed() {
			t.Logf("jobs of the spec: %+v; ffmpeg: %+v", c.r.rowsOf(t, s.hash), c.r.runs())
		}
	}()
	waitFor(t, "a helper of the spec "+state+" with segment "+strconv.Itoa(minNext)+" next or later", 90*time.Second, func() bool {
		for _, h := range c.r.helpersOf(t, s.hash) {
			if h.State != "running" || h.Next < minNext {
				continue
			}
			at := c.r.ffmpegAt(h.Start)
			if at == "" || (state == "paused") != (at == "T") {
				continue
			}
			found = h
			return true
		}
		return false
	})
	return found
}

// viewerAt is one viewer who asked for segment n of the rig's stream, and goes on being seen.
func (c *cluster) viewerAt(t *testing.T, psid string, n int) {
	t.Helper()
	if _, err := c.r.fetch(t, c.j, psid, n); err != nil {
		t.Fatalf("the first segment request of viewer %s for segment %d: %v", psid, n, err)
	}
	c.r.keepWatching(t, c.j, psid)
	if c.late != nil {
		late := c.late
		c.late = nil
		late()
	}
}

// forwardScene: one viewer at segment 0 and a helper for the chunk ahead of it.
// lead 60 s makes the limit segment 15, so a helper at 1x is short of it for a
// long time (state "running"); lead 20 s makes it 5, which the helper reaches
// and waits at ("paused").
func (r *rig) forwardScene(t *testing.T, c *cluster, state string) scene {
	t.Helper()
	c.viewerAt(t, psid(0), 0)
	next := 6
	if state == "paused" {
		next = 5
	}
	return scene{c, r.mainStream(), c.helperIn(t, r.mainStream(), state, next)}
}

// backScene: viewers at segment 12, the middle of the film, and a helper for the
// chunk at 16. It needs the encode slower than real time so that the helper is
// short of its limit for long enough to be found there (slowAt makes it half).
func (r *rig) backScene(t *testing.T, c *cluster, state string, viewers ...string) scene {
	t.Helper()
	for _, v := range viewers {
		c.viewerAt(t, v, 12)
	}
	next := 17
	if state == "paused" {
		next = 18
	}
	return scene{c, r.mainStream(), c.helperIn(t, r.mainStream(), state, next)}
}

// filmEnd is the segment after the film's last.
const filmEnd = filmSegments

// scrapeAll is /metrics as cmd/jellyscale builds it: the worker's own collector and the package's event
// metrics (Collectors), which is where jellyscale_transcode_jobs_ended_total is served. scrape(t, w) has
// only the worker's, so a counter read through it is always empty.
func scrapeAll(t *testing.T, w *Worker) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler(append([]prometheus.Collector{w}, Collectors()...)...).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatalf("/metrics answered %d", recorder.Code)
	}
	return recorder.Body.String()
}

// endedCounts reads jellyscale_transcode_jobs_ended_total by its one label, and fails a label that names more
// than a reason. The counter is the package's, shared by the workers of this process, so a test reads
// the difference between two reads. A family with no series yet is not served, so a read before any job
// ended is empty; the tests that expect nothing new also expect something old to have grown.
func endedCounts(t *testing.T, w *Worker) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	text := scrapeAll(t, w)
	line := regexp.MustCompile(`(?m)^jellyscale_transcode_jobs_ended_total\{([^}]*)\} (\S+)$`)
	for _, m := range line.FindAllStringSubmatch(text, -1) {
		label := regexp.MustCompile(`^result="([a-z][a-z_]*)"$`).FindStringSubmatch(m[1])
		if label == nil {
			t.Fatalf("jellyscale_transcode_jobs_ended_total has the label %s, want result with a plain word for its value: no job, spec, film, account, device or session in a label", m[1])
		}
		n, _ := strconv.ParseFloat(m[2], 64)
		out[label[1]] = n
	}
	return out
}

// newReasons are the reasons the counter gained since before, beyond done, failed and interrupted, and by how much.
func newReasons(before, after map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for reason, n := range after {
		if reason != "done" && reason != "failed" && reason != "interrupted" && n > before[reason] {
			out[reason] = n - before[reason]
		}
	}
	return out
}
