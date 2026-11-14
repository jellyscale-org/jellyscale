package transcode

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/playback"
)

// Capacity is counted in cores. A worker declares how many it has
// (transcode_nodes.cores), a job costs what playback.Spec.Cost says, and a job
// fits a worker when the jobs it already holds leave that many free. The count
// is made in Go over the few rows involved: the live workers, and the jobs
// queued or running on them. A queued job, which no worker has claimed yet,
// is counted on the worker with the most cores free, oldest first; one that
// fits nowhere is counted nowhere, as it will be left and expire.
const (
	// copyBound is how many copies one worker holds, whatever they cost: a copy
	// takes almost no cores but still reads, remuxes and writes a stream.
	copyBound = 32
	// coresSlack makes 5 x 0.8 fit in 4.0, which floats do not add up to.
	coresSlack = 1e-9
	// largestCost is the dearest job, a 4K tone mapped re-encode. A worker that
	// declares nothing has at least that many cores, so that it takes any job
	// alone.
	largestCost = 3.2
)

// nodeCores is SQL for the cores of a worker row: those it declares, or its
// slots if it is a row of the release before.
const nodeCores = `(CASE WHEN cores > 0 THEN cores ELSE slots END)::float8`

// cores is what the worker declares: its cores, else its slots (set by tests,
// and by no config), else the processors the process may use, but never less
// than the dearest job needs.
func (w *Worker) cores() float64 {
	if w.Cores > 0 {
		return w.Cores
	}
	if w.Slots > 0 {
		return float64(w.Slots)
	}
	return max(float64(runtime.GOMAXPROCS(0)), largestCost)
}

// registered is the cores and slots a worker row carries. A worker without a
// transcode store runs no transcode, and declares none. Slots are the cores
// rounded up, so that a replica of the release before, which counts a worker by
// slots > 0, counts this one.
func (w *Worker) registered() (cores float64, slots int) {
	if w.Segments == nil {
		return 0, 0
	}
	cores = w.cores()
	return cores, int(math.Ceil(cores))
}

type capNode struct {
	ID    string
	Cores float64
}

// capJob is a job that holds or waits for cores.
type capJob struct {
	ID     int64
	Worker string // "" while it is queued.
	Cost   float64
	Copy   bool
	Helper bool
	Own    bool // The asking viewer's.
	Start  int
}

// capQuery says what readCapacity counts.
type capQuery struct {
	// Filter is SQL over transcode_nodes that a worker must pass to count.
	Filter string
	// Queue is how long a queued job counts for.
	Queue time.Duration
	// Self is a worker that counts whenever it is registered, however long ago
	// it was seen: the one that asks.
	Self string
	// UserID and DeviceID name the viewer whose own jobs are marked.
	UserID, DeviceID string
}

// readCapacity reads the workers of the output version that were seen lately
// and pass the filter, and the jobs of the version that are queued and not
// expired, or running on one of them.
func readCapacity(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, c capQuery) ([]capNode, []capJob, error) {
	rows, err := q.Query(ctx, `SELECT id, `+nodeCores+` FROM transcode_nodes WHERE output_version = $1
		AND (seen_at > now() - make_interval(secs => $2) OR id = $3) AND (slots > 0 OR cores > 0) AND (`+c.Filter+`) ORDER BY id`,
		playback.OutputVersion, liveTiming("counted").Seconds(), c.Self)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[capNode])
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT id, COALESCE(worker, ''), helper, start_segment,
			owner IN (SELECT play_session_id FROM transcode_viewers WHERE user_id::text = $3 AND device_id = $4),
			COALESCE((spec->>'Copy')::boolean, false), cost::float8, CASE WHEN cost IS NULL THEN spec END
		FROM transcode_jobs WHERE output_version = $1
			AND ((state = 'queued' AND created_at > now() - make_interval(secs => $2))
				OR (state = 'running' AND lease_expires_at > now()))
		ORDER BY id`, playback.OutputVersion, c.Queue.Seconds(), c.UserID, c.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var jobs []capJob
	for rows.Next() {
		var job capJob
		var cost *float64
		var spec []byte
		if err := rows.Scan(&job.ID, &job.Worker, &job.Helper, &job.Start, &job.Own, &job.Copy, &cost, &spec); err != nil {
			return nil, nil, err
		}
		if job.Cost, err = costOf(cost, spec); err != nil {
			return nil, nil, err
		}
		// A running job of a worker that is not counted holds nothing here.
		if job.Worker != "" && !slices.ContainsFunc(nodes, func(n capNode) bool { return n.ID == job.Worker }) {
			continue
		}
		jobs = append(jobs, job)
	}
	return nodes, jobs, rows.Err()
}

// costOf is a job's cost: the one written beside the row, or, for a row queued
// by the release before, the spec's.
func costOf(cost *float64, raw []byte) (float64, error) {
	if cost != nil {
		return *cost, nil
	}
	var spec playback.Spec
	err := json.Unmarshal(raw, &spec)
	return spec.Cost(), err
}

// ledger is what each worker has free.
type ledger struct {
	nodes  []capNode
	cores  map[string]float64
	free   map[string]float64
	copies map[string]int
}

// newLedger takes the running jobs among jobs as held. keep says which count.
func newLedger(nodes []capNode, jobs []capJob, keep func(capJob) bool) *ledger {
	l := &ledger{nodes: nodes, cores: map[string]float64{}, free: map[string]float64{}, copies: map[string]int{}}
	for _, n := range nodes {
		l.cores[n.ID], l.free[n.ID] = n.Cores, n.Cores
	}
	for _, job := range jobs {
		if job.Worker != "" && keep(job) {
			l.take(job.Worker, job)
		}
	}
	return l
}

func (l *ledger) clone() *ledger {
	return &ledger{nodes: l.nodes, cores: l.cores, free: maps.Clone(l.free), copies: maps.Clone(l.copies)}
}

func (l *ledger) take(worker string, job capJob) {
	l.free[worker] -= job.Cost
	if job.Copy {
		l.copies[worker]++
	}
}

func (l *ledger) fits(worker string, job capJob) bool {
	return l.free[worker] >= job.Cost-coresSlack && (!job.Copy || l.copies[worker] < copyBound)
}

// place takes the job on the worker with the most cores free that holds it.
func (l *ledger) place(job capJob) (string, bool) {
	best := ""
	for _, n := range l.nodes {
		if l.fits(n.ID, job) && (best == "" || l.free[n.ID] > l.free[best]) {
			best = n.ID
		}
	}
	if best == "" {
		return "", false
	}
	l.take(best, job)
	return best, true
}

// placeQueued counts the queued jobs that keep says count, oldest first.
func (l *ledger) placeQueued(jobs []capJob, keep func(capJob) bool) {
	for _, job := range jobs {
		if job.Worker == "" && keep(job) {
			l.place(job)
		}
	}
}

// viewerJobs are the jobs a viewer's new one waits behind: other viewers' jobs.
// Helpers give way to it (preemptHelpers), and the asker's own are replaced by it.
func viewerJobs(job capJob) bool { return !job.Helper && !job.Own }

// room says whether a job of the cost fits on some worker, and if not, why, in
// the words of a refusal.
func (l *ledger) room(job capJob) (string, bool) {
	for _, n := range l.nodes {
		if l.fits(n.ID, job) {
			return "", true
		}
	}
	mostCores, mostFree, atBound := 0.0, 0.0, true
	for _, n := range l.nodes {
		mostCores = max(mostCores, n.Cores)
		mostFree = max(mostFree, l.free[n.ID])
		atBound = atBound && l.copies[n.ID] >= copyBound
	}
	switch {
	case job.Copy && atBound:
		return fmt.Sprintf("every worker holds %d copies, the most it takes (output version %d)", copyBound, playback.OutputVersion), false
	case job.Cost > mostCores+coresSlack:
		return fmt.Sprintf("cores are short: the job costs %.1f cores and the most any worker has is %g (output version %d)", job.Cost, mostCores, playback.OutputVersion), false
	}
	return fmt.Sprintf("cores are short: the job costs %.1f cores and the most any worker has free is %.1f (output version %d)", job.Cost, max(mostFree, 0), playback.OutputVersion), false
}

// preemptHelpers cancels the helpers that a viewer's job of the cost needs
// gone, and none unless that makes room: those of the one worker where fewest
// do, the ones furthest ahead first, since a helper that is far ahead of its
// viewer is the least missed. A viewer's job is never cancelled. It reports
// whether any helper was, so the caller can send preemptHint once the
// transaction commits and the helpers stop at once rather than at their next
// heartbeat. What a helper stored stays, and the rest of its chunk is made when
// a viewer reaches it, as after a seek. Called under the helpers lock.
func preemptHelpers(ctx context.Context, tx pgx.Tx, queue time.Duration, job capJob) (bool, error) {
	nodes, jobs, err := readCapacity(ctx, tx, capQuery{Filter: "true", Queue: queue})
	if err != nil {
		return false, err
	}
	l := newLedger(nodes, jobs, func(capJob) bool { return true })
	l.placeQueued(jobs, func(j capJob) bool { return !j.Helper })
	if _, ok := l.room(job); ok {
		return false, nil
	}
	var cancel []int64
	for _, n := range nodes {
		var helpers []capJob
		for _, held := range jobs {
			if held.Worker == n.ID && held.Helper {
				helpers = append(helpers, held)
			}
		}
		slices.SortFunc(helpers, func(a, b capJob) int { return cmp.Or(cmp.Compare(b.Start, a.Start), cmp.Compare(b.ID, a.ID)) })
		free, copies, stop := l.free[n.ID], l.copies[n.ID], []int64(nil)
		for _, helper := range helpers {
			if free >= job.Cost-coresSlack && (!job.Copy || copies < copyBound) {
				break
			}
			free, stop = free+helper.Cost, append(stop, helper.ID)
		}
		if free >= job.Cost-coresSlack && (!job.Copy || copies < copyBound) && (cancel == nil || len(stop) < len(cancel)) {
			cancel = stop
		}
	}
	if len(cancel) == 0 {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'cancelled', error = 'a new viewer needed its slot', ended_at = now()
		WHERE id = ANY($1) AND state IN ('queued', 'running')`, cancel)
	return tag.RowsAffected() > 0, err
}
