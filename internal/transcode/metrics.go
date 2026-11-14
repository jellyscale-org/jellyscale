package transcode

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Counters and histograms of events are package-level but not registered
// anywhere: Collectors hands them to whoever serves /metrics. What belongs to
// one Worker (its running jobs and slots) is read from it at scrape time, so
// no exit path of a job has to remember to clear anything.
var (
	jobsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "jellyscale_transcode_jobs_started_total", Help: "Transcode jobs a worker began."})
	jobsEnded = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_transcode_jobs_ended_total",
		Help: "Transcode jobs that ended: done, failed, or interrupted (cut short, not failed)."}, []string{"result"})
	reaped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_transcode_reaped_total",
		Help: "Workers forgotten and jobs cut short because a worker's liveness lock was gone."}, []string{"what"})
	extractions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jellyscale_extraction_jobs_running", Help: "Subtitle extractions and chapter-picture jobs running."}, []string{"kind"})

	segmentWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "jellyscale_segment_wait_seconds",
		Help:    "How long a segment request took. source: store (already there), waited (on a job that was queued or running), started (a job it started), none (ended before it looked). outcome: ok, timeout, failed, refused, canceled (the client left), error.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 90}}, []string{"source", "kind", "outcome"})
	segmentStalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_segment_stalls_total", Help: "Segment requests that took longer than 5 seconds, whatever their outcome."}, []string{"kind"})
	segmentRefused = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jellyscale_segment_requests_refused_total", Help: "Requests refused: stopped (a playlist or segment of an ended session), no_worker, other_version (workers run, none of the job's output version) or busy (a segment that needed a job)."}, []string{"reason"})
)

// stallAfter is how long a segment request may take before it counts as a
// stall; jellyfin-web gives up on a request at 10 seconds.
var stallAfter = 5 * time.Second

// Collectors are the event metrics of this package.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{jobsStarted, jobsEnded, reaped, extractions, segmentWait, segmentStalls, segmentRefused}
}

func countReaped(workers []string, jobs []int64) {
	reaped.WithLabelValues("workers").Add(float64(len(workers)))
	reaped.WithLabelValues("jobs").Add(float64(len(jobs)))
}

// refusedAsStopped is ErrStopped, counted. Watch and ensure return it, one or
// the other once per request, and the api calls Watch before it reads a segment,
// so counting here and not in observeSegment keeps each refusal to one.
func refusedAsStopped() error {
	segmentRefused.WithLabelValues("stopped").Inc()
	return ErrStopped
}

func kindOf(copied bool) string {
	if copied {
		return "copy"
	}
	return "reencode"
}

// observeSegment records one segment request. source is "" if the request
// ended before it looked at the store.
func observeSegment(began time.Time, copied bool, source string, err error) {
	elapsed := time.Since(began)
	kind := kindOf(copied)
	outcome := "ok"
	var failed FailedError
	switch {
	case err == nil:
	case errors.Is(err, ErrTimeout):
		outcome = "timeout"
	case errors.As(err, &failed):
		outcome = "failed"
	case errors.Is(err, ErrStopped):
		outcome = "refused" // Counted where it was refused: see refusedAsStopped.
	case errors.Is(err, ErrNoWorker):
		outcome = "refused"
		segmentRefused.WithLabelValues("no_worker").Inc()
	case errors.Is(err, ErrOtherVersion):
		outcome = "refused"
		segmentRefused.WithLabelValues("other_version").Inc()
	case errors.Is(err, ErrBusy):
		outcome = "refused"
		segmentRefused.WithLabelValues("busy").Inc()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		outcome = "canceled"
	default:
		outcome = "error"
	}
	if source == "" {
		source = "none"
	}
	segmentWait.WithLabelValues(source, kind, outcome).Observe(elapsed.Seconds())
	if elapsed > stallAfter {
		segmentStalls.WithLabelValues(kind).Inc()
	}
}

// liveJob is what a worker knows of a job it is running, for /metrics.
type liveJob struct {
	copied   bool
	cost     float64
	progress *progress
}

// track notes that the worker runs the job, from the moment it claims it
// until untrack, and returns where ffmpeg's progress goes.
func (w *Worker) track(job claimed) *progress {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	if w.live == nil {
		w.live = map[int64]liveJob{}
	}
	p := &progress{now: time.Now, began: time.Now()}
	w.live[job.ID] = liveJob{copied: job.Spec.Copy, cost: job.Spec.Cost(), progress: p}
	return p
}

// holding is the cores and the copies of the jobs the worker runs now.
func (w *Worker) holding() (cores float64, copies int) {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	for _, job := range w.live {
		cores += job.cost
		if job.copied {
			copies++
		}
	}
	return cores, copies
}

func (w *Worker) untrack(id int64) {
	w.liveMu.Lock()
	delete(w.live, id)
	w.liveMu.Unlock()
}

var (
	jobsRunningDesc = prometheus.NewDesc("jellyscale_transcode_jobs_running", "Transcode jobs this worker runs, paused ones included, by kind (reencode, copy).", []string{"kind"}, nil)
	speedDesc       = prometheus.NewDesc("jellyscale_transcode_job_speed_ratio", "Seconds of film a running job encodes per second of its own running time, over its last 10 seconds. Time paused at the lead limit does not count. Absent until the job has run for a second.", []string{"job", "kind"}, nil)
	pausedDesc      = prometheus.NewDesc("jellyscale_transcode_job_paused", "1 while a job's ffmpeg is paused at its lead limit.", []string{"job"}, nil)
	coresDesc       = prometheus.NewDesc("jellyscale_transcode_cores", "Cores this worker declared (transcodeCores).", nil, nil)
	busyDesc        = prometheus.NewDesc("jellyscale_transcode_cores_busy", "Cores the jobs this worker runs cost, paused jobs included (docs/transcoding.md has the costs).", nil, nil)
	queueCoresDesc  = prometheus.NewDesc("jellyscale_transcode_queue_cores", "Cores the queued jobs cost, in the whole cluster: every worker reports the same number, so take one, not a sum. The signal to scale workers on.", nil, nil)
	queueDesc       = prometheus.NewDesc("jellyscale_transcode_queue_depth", "Transcode jobs waiting for a worker, in the whole cluster: every worker reports the same number, so take one, not a sum. Copies held back by their bound show here and hardly in queue_cores.", nil, nil)
)

func (w *Worker) Describe(chan<- *prometheus.Desc) {}

// Collect reports the worker's jobs and cores, and the cluster's queue.
func (w *Worker) Collect(ch chan<- prometheus.Metric) {
	w.liveMu.Lock()
	byKind := map[string]int{"reencode": 0, "copy": 0}
	for id, job := range w.live {
		kind := kindOf(job.copied)
		byKind[kind]++
		name := strconv.FormatInt(id, 10)
		if speed, ok := job.progress.speed(); ok {
			ch <- prometheus.MustNewConstMetric(speedDesc, prometheus.GaugeValue, speed, name, kind)
		}
		ch <- prometheus.MustNewConstMetric(pausedDesc, prometheus.GaugeValue, job.progress.pausedValue(), name)
	}
	w.liveMu.Unlock()
	busy, _ := w.holding()
	for kind, n := range byKind {
		ch <- prometheus.MustNewConstMetric(jobsRunningDesc, prometheus.GaugeValue, float64(n), kind)
	}
	cores, _ := w.registered()
	ch <- prometheus.MustNewConstMetric(coresDesc, prometheus.GaugeValue, cores)
	ch <- prometheus.MustNewConstMetric(busyDesc, prometheus.GaugeValue, float64(busy))

	// Queue depth is cluster state in Postgres, so it is asked for here and not
	// kept: a count kept by each process would be wrong. Jobs older than the
	// queue timeout are not waited for any more and do not count.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := w.Pool.Query(ctx, `SELECT cost::float8, CASE WHEN cost IS NULL THEN spec END FROM transcode_jobs WHERE state = 'queued'
		AND created_at > now() - make_interval(secs => $1)`, orDefault(w.QueueTimeout, queueTimeout).Seconds())
	if err != nil {
		return
	}
	defer rows.Close()
	var queued int
	var queuedCores float64
	for rows.Next() {
		var cost *float64
		var spec []byte
		if rows.Scan(&cost, &spec) != nil {
			return
		}
		queuedCost, err := costOf(cost, spec)
		if err != nil {
			return
		}
		queued, queuedCores = queued+1, queuedCores+queuedCost
	}
	if rows.Err() == nil {
		ch <- prometheus.MustNewConstMetric(queueDesc, prometheus.GaugeValue, float64(queued))
		ch <- prometheus.MustNewConstMetric(queueCoresDesc, prometheus.GaugeValue, queuedCores)
	}
}

// progress reads ffmpeg's -progress output and measures how fast the job
// encodes. ffmpeg's own speed counts its start-up and every pause at the lead
// limit, so a healthy job that paused would look slow; this one counts only
// the time the job could run.
type progress struct {
	mu        sync.Mutex
	now       func() time.Time // time.Now, unless a test says otherwise.
	began     time.Time
	pausedAt  time.Time // Zero unless paused.
	pausedFor time.Duration
	pending   []byte
	samples   []sample
}

// sample is how much film ffmpeg had written after active seconds of running.
type sample struct{ film, active float64 }

// speedWindow is how many running seconds a speed is measured over.
const speedWindow = 10

// Write takes ffmpeg's stdout, a block of key=value lines every half second.
func (p *progress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = append(p.pending, b...)
	for {
		line, rest, found := bytes.Cut(p.pending, []byte("\n"))
		if !found {
			return len(b), nil
		}
		p.pending = append([]byte(nil), rest...)
		if micros, ok := bytes.CutPrefix(line, []byte("out_time_us=")); ok {
			if n, err := strconv.ParseInt(string(micros), 10, 64); err == nil {
				p.add(float64(n) / 1e6)
			}
		}
	}
}

// activeLocked is the time the job has run, without the time it was paused.
func (p *progress) activeLocked(now time.Time) float64 {
	paused := p.pausedFor
	if !p.pausedAt.IsZero() {
		paused += now.Sub(p.pausedAt)
	}
	return (now.Sub(p.began) - paused).Seconds()
}

func (p *progress) add(film float64) {
	p.samples = append(p.samples, sample{film, p.activeLocked(p.now())})
	last := p.samples[len(p.samples)-1]
	// Keep the newest sample that is at least a window old, and all after it.
	for len(p.samples) > 1 && p.samples[1].active <= last.active-speedWindow {
		p.samples = p.samples[1:]
	}
}

// pause says that ffmpeg was stopped or let go on.
func (p *progress) pause(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	switch {
	case on && p.pausedAt.IsZero():
		p.pausedAt = now
	case !on && !p.pausedAt.IsZero():
		p.pausedFor += now.Sub(p.pausedAt)
		p.pausedAt = time.Time{}
	}
}

func (p *progress) pausedValue() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pausedAt.IsZero() {
		return 0
	}
	return 1
}

// speed is film seconds per running second over the window, if the job has run
// for a second or more since its first sample. It is measured up to now, so a
// job whose ffmpeg stalls (and is not paused) sees its speed fall to nothing
// and not stay at the last good value.
func (p *progress) speed() (float64, bool) { return p.speedOver(1) }

// speedOver is speed, unless the job has run less than span seconds since its
// first sample. ffmpeg's output comes in bursts, and a speed over a second or
// two of a job's start swings by a third either way.
func (p *progress) speedOver(span float64) (float64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.samples) < 2 {
		return 0, false
	}
	first, last := p.samples[0], p.samples[len(p.samples)-1]
	active := p.activeLocked(p.now())
	if active-first.active < span {
		return 0, false
	}
	return (last.film - first.film) / (active - first.active), true
}
