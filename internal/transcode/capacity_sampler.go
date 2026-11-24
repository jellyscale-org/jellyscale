package transcode

import (
	"context"
	"math"
	"sync"
	"time"

	"jellyscale/internal/mediatool"
)

// What a worker has learned of its own capacity (JS-177, D-064). The estimator (capacity_learn.go) is the arithmetic; this
// is the worker's side of it: a sample at each heartbeat while a job runs, the figure written to the worker's row when it
// moves, and what the operator reads of it.

// reasonOwnExtraction is the slow-job reason of a re-encode under real time beside its worker's own extraction (D-070).
const reasonOwnExtraction = "own_extraction"

// writeEvery is the least time between two writes of a moved figure to the worker's row.
const writeEvery = 15 * time.Second

// capacityLearning is the worker's capacity. estimator is the sampler's alone; the rest is read by the row, /metrics and the
// log, and is what the worker holds: the figure its row has, which is what admission counts.
type capacityLearning struct {
	estimator  *capacityEstimator
	unreadable bool

	mu      sync.Mutex
	held    float64
	source  string
	wroteAt time.Time
	logged  float64
}

// initCapacity sets the start figure, once, as the worker's ID is set.
func (w *Worker) initCapacity() {
	start, source := w.start()
	w.capacity.estimator = newCapacityEstimator(start, source, w.lease(), time.Now())
	w.capacity.held, w.capacity.source, w.capacity.logged = start, source, start
	if err := w.kernel().Readable(); err != nil {
		w.capacity.unreadable = true
		if w.Segments != nil && w.Log != nil {
			w.Log.Warn("transcode capacity cannot be learned", "error", err, "cores", start,
				"note", "the kernel's scheduler figures cannot be read, so a slow CPU cannot be told from a slow store: the capacity stays what the worker started with; set transcodeCores to what it holds")
		}
	}
}

// heldCapacity is the figure the worker's row holds and where it comes from.
func (w *Worker) heldCapacity() (float64, string) {
	w.setID()
	w.capacity.mu.Lock()
	defer w.capacity.mu.Unlock()
	return w.capacity.held, w.capacity.source
}

// capacitySource is the word for where the worker's capacity comes from, as the log and the labels of /metrics say it.
func (w *Worker) capacitySource() string {
	_, source := w.heldCapacity()
	return source
}

// cannotLearn says whether the kernel's figures could not be read at the worker's start.
func (w *Worker) cannotLearn() bool {
	w.setID()
	return w.capacity.unreadable
}

// learnCapacity takes a sample at each heartbeat, and makes a timed rise when one is due.
func (w *Worker) learnCapacity(ctx context.Context) {
	if w.Segments == nil {
		return
	}
	w.setID()
	ticker := time.NewTicker(w.heartbeat())
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			w.sampleCapacity(ctx, now)
		case <-ctx.Done():
			return
		}
	}
}

func (w *Worker) sampleCapacity(ctx context.Context, now time.Time) {
	estimator := w.capacity.estimator
	before := estimator.Figure()
	if sample, ok := w.sample(now); ok {
		estimator.Observe(sample)
	}
	estimator.Tick(now)
	if refuted, failed := estimator.TakeFailedRise(); failed {
		attrs := []any{"before", before, "after", estimator.Figure(), "refuted", refuted}
		if refuted >= 2 {
			attrs = append(attrs, "note", "the timed rise keeps failing; set transcodeCores to what the worker holds to end the retries")
		}
		w.Log.Warn("transcode capacity timed rise failed", attrs...)
	}
	w.publishCapacity(ctx, now)
}

// sample is what the worker's running jobs show now, if any run.
func (w *Worker) sample(now time.Time) (learnSample, bool) {
	var jobs []learnJob
	var pids []int
	w.liveMu.Lock()
	for _, job := range w.live {
		speed, age, paused, pid := job.progress.forLearning(now)
		jobs = append(jobs, learnJob{Price: job.cost, Speed: speed, Age: age, Paused: paused, Copy: job.copied, Fallback: job.fallback})
		if pid != 0 {
			pids = append(pids, pid)
		}
	}
	ended := !w.endedAt.IsZero() && now.Sub(w.endedAt) < speedWindow*time.Second
	w.liveMu.Unlock()
	if len(jobs) == 0 {
		return learnSample{}, false
	}
	kernel := w.kernel()
	sample := learnSample{At: now, Allotment: kernel.Allotment(), Jobs: jobs, JobEnded: ended,
		Extraction: mediatool.Extracting(speedWindow * time.Second), Unreadable: w.capacity.unreadable}
	if !sample.Unreadable {
		var err error
		sample.Demand, err = kernel.Demand(pids)
		sample.Unreadable = err != nil
	}
	return sample, true
}

// publishCapacity writes the figure the estimator holds to the worker's row, if it has moved and the last write was not
// within writeEvery. It is an UPDATE of the row of this worker's session and never an insert: a worker the reaper forgot
// stays forgotten, and registers again with the figure it holds (DOMAINS 25). The figure counts for the metrics, the log and
// the claim only once the row has it, so that all three say what the other replicas count.
func (w *Worker) publishCapacity(ctx context.Context, now time.Time) {
	figure := w.capacity.estimator.Figure()
	w.capacity.mu.Lock()
	held, wroteAt := w.capacity.held, w.capacity.wroteAt
	w.capacity.mu.Unlock()
	if figure == held || now.Sub(wroteAt) < writeEvery {
		return
	}
	jobCtx, ok := w.jobContext()
	if !ok {
		return
	}
	writeCtx, cancel := context.WithTimeout(jobCtx, w.lease())
	defer cancel()
	tag, err := w.Pool.Exec(writeCtx, `UPDATE transcode_nodes SET cores = $3, slots = $4 WHERE id = $1 AND session = $2`,
		w.ID, sessionOf(jobCtx), figure, int(math.Ceil(figure)))
	if err != nil || tag.RowsAffected() == 0 {
		return
	}
	source := w.capacity.estimator.Source()
	w.capacity.mu.Lock()
	before := w.capacity.logged
	w.capacity.held, w.capacity.source, w.capacity.wroteAt = figure, source, now
	moved := math.Abs(figure-before) > before/5
	if moved {
		w.capacity.logged = figure
	}
	w.capacity.mu.Unlock()
	if moved {
		w.Log.Info("transcode capacity moved", "before", before, "after", figure, "source", source)
	}
}
