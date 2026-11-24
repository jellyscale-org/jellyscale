package transcode

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/playback"
)

// Capacity is counted in cores. A worker's row says how many it has
// (transcode_nodes.cores: what it learned, JS-177), a job costs what playback.Spec.Cost says, and a job
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
	// shareFraction is how much of a job's cost a worker must have free to take part of it in a play that two workers
	// hold together (D-046, D-066). Two workers that each made the 3.2 core job at 0.79 to 0.83 times real time alone held it
	// at 1.0 together (docs/transcoding.md, JS-162 and JS-168), so the slowest measured, 0.78 of real time, less a little, is
	// the share. Capacity is the price a worker keeps at 1.2 times real time, so 0.78 of real time is 0.78 / 1.2 = 0.65 of
	// the price: a worker below it is too slow to help whatever the sum of the workers is.
	shareFraction = 0.65
)

// nodeCores is SQL for the cores of a worker row: those it declares, or its
// slots if it is a row of the release before.
const nodeCores = `(CASE WHEN cores > 0 THEN cores ELSE slots END)::float8`

type capNode struct {
	ID    string
	Cores float64
	// NoHelpers: the worker's own jobs ask for no helpers (transcodeHelpers false), so it cannot host a shared play.
	NoHelpers bool
}

// capJob is a job that holds or waits for cores.
type capJob struct {
	ID     int64
	Worker string // "" while it is queued.
	Cost   float64
	Copy   bool
	Helper bool
	Own    bool // The asking viewer's.
	Spec   string
	Start  int
	// Whole: the job is not cut into chunks (an HEVC or AV1 fallback), so no helper can take a part of it.
	Whole bool
	// Share: the job fits where shareFraction of its cost is free: a helper of a play that two workers hold together, or
	// the viewer's job of one.
	Share bool
	// Watched: a viewer other than the asker is watching the job's spec.
	Watched bool
}

// need is the cores a worker must have free to take the job.
func (j capJob) need() float64 {
	if j.Share {
		return j.Cost * shareFraction
	}
	return j.Cost
}

// shareable says a play of the job may be held by two workers together: it is a re-encode cut into chunks.
func (j capJob) shareable() bool { return !j.Copy && !j.Whole }

// capQuery says what readCapacity counts.
type capQuery struct {
	// Filter is SQL over transcode_nodes that a worker must pass to count.
	Filter string
	// Queue is how long a queued job counts for.
	Queue time.Duration
	// Self is a worker that counts whenever it is registered, however long ago
	// it was seen: the one that asks.
	Self string
	// UserID and DeviceID name the viewer whose own jobs are marked, and Session the play session whose viewing of a
	// spec is not another viewer's.
	UserID, DeviceID, Session string
}

// readCapacity reads the workers of the output version that were seen lately
// and pass the filter, and the jobs of the version that are queued and not
// expired, or running on one of them.
func readCapacity(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, c capQuery) ([]capNode, []capJob, error) {
	rows, err := q.Query(ctx, `SELECT id, `+nodeCores+`, no_helpers FROM transcode_nodes WHERE output_version = $1
		AND (seen_at > now() - make_interval(secs => $2) OR id = $3) AND (slots > 0 OR cores > 0) AND (`+c.Filter+`) ORDER BY id`,
		playback.OutputVersion, liveTiming("counted").Seconds(), c.Self)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[capNode])
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT id, COALESCE(worker, ''), helper, start_segment, spec_hash,
			owner IN (SELECT play_session_id FROM transcode_viewers WHERE user_id::text = $3 AND device_id = $4),
			COALESCE((spec->>'Copy')::boolean, false), cost::float8, CASE WHEN cost IS NULL THEN spec END,
			COALESCE(spec->>'Recode', '') <> '',
			EXISTS (SELECT 1 FROM transcode_viewers w WHERE w.spec_hash = transcode_jobs.spec_hash AND NOT w.stopped
				AND w.last_seen_at > now() - make_interval(secs => $5) AND NOT (w.user_id::text = $3 AND w.device_id = $4) AND w.play_session_id <> $6)
		FROM transcode_jobs WHERE output_version = $1
			AND ((state = 'queued' AND created_at > now() - make_interval(secs => $2))
				OR (state = 'running' AND lease_expires_at > now()))
		ORDER BY id`, playback.OutputVersion, c.Queue.Seconds(), c.UserID, c.DeviceID, defaultIdle.Seconds(), c.Session)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var jobs []capJob
	for rows.Next() {
		var job capJob
		var cost *float64
		var spec []byte
		if err := rows.Scan(&job.ID, &job.Worker, &job.Helper, &job.Start, &job.Spec, &job.Own, &job.Copy, &cost, &spec, &job.Whole, &job.Watched); err != nil {
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
	// A helper of a play that runs on more than one worker needs only its share free.
	for i, j := range jobs {
		jobs[i].Share = j.Helper && sharedSpec(nodes, jobs, j.Spec, j.Cost)
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
	nodes     []capNode
	cores     map[string]float64
	free      map[string]float64
	copies    map[string]int
	noHelpers map[string]bool
}

// newLedger takes the running jobs among jobs as held. keep says which count.
func newLedger(nodes []capNode, jobs []capJob, keep func(capJob) bool) *ledger {
	l := &ledger{nodes: nodes, cores: map[string]float64{}, free: map[string]float64{}, copies: map[string]int{}, noHelpers: map[string]bool{}}
	for _, n := range nodes {
		l.cores[n.ID], l.free[n.ID], l.noHelpers[n.ID] = n.Cores, n.Cores, n.NoHelpers
	}
	for _, job := range jobs {
		if job.Worker != "" && keep(job) {
			l.take(job.Worker, job)
		}
	}
	return l
}

func (l *ledger) take(worker string, job capJob) {
	l.free[worker] -= job.Cost
	if job.Copy {
		l.copies[worker]++
	}
}

func (l *ledger) fits(worker string, job capJob) bool {
	// The viewer's job of a shared play asks for the helpers, so it runs where they are asked for.
	if job.Share && !job.Helper && l.noHelpers[worker] {
		return false
	}
	return l.free[worker] >= job.need()-coresSlack && (!job.Copy || l.copies[worker] < copyBound)
}

// place takes the job on the worker with the most cores free that holds it.
func (l *ledger) place(job capJob) (string, bool) { return l.placeOff(job, nil) }

// placeOff is place without the workers in barred.
func (l *ledger) placeOff(job capJob, barred map[string]bool) (string, bool) {
	best := ""
	for _, n := range l.nodes {
		if !barred[n.ID] && l.fits(n.ID, job) && (best == "" || l.free[n.ID] > l.free[best]) {
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
		if job.Worker != "" || !keep(job) {
			continue
		}
		if _, ok := l.place(job); !ok && job.shareable() && !job.Helper {
			job.Share = true
			l.place(job) // Waiting for a play of two workers: counted on the one that will host it.
		}
	}
}

// viewerJobs are the jobs a viewer's new one waits behind: other viewers' jobs.
// The asker's own are replaced by it, and helpers give way to it, except those of a
// film another viewer is watching that runs on helpers (heldFilms).
func viewerJobs(job capJob) bool { return !job.Helper && !job.Own }

// room says whether a job of the cost fits on some worker, and if not, why, in
// the words of a refusal.
func (l *ledger) room(job capJob) (string, bool) {
	_, text := l.why(job)
	return text, text == ""
}

// why is room's answer with the reason of the refusal beside its words: playback.NoWorkerWithFreeCores, or
// playback.WorkersAtCopyBound. Both are empty where the job fits.
func (l *ledger) why(job capJob) (reason, text string) {
	for _, n := range l.nodes {
		if l.fits(n.ID, job) {
			return "", ""
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
		return playback.WorkersAtCopyBound, fmt.Sprintf("every worker holds %d copies, the most it takes (output version %d)", copyBound, playback.OutputVersion)
	case job.Cost > mostCores+coresSlack:
		return playback.NoWorkerWithFreeCores, fmt.Sprintf("cores are short: the job costs %.1f cores and the most any worker has is %g (output version %d)", job.Cost, mostCores, playback.OutputVersion)
	}
	return playback.NoWorkerWithFreeCores, fmt.Sprintf("cores are short: the job costs %.1f cores and the most any worker has free is %.1f (output version %d)", job.Cost, max(mostFree, 0), playback.OutputVersion)
}

// Admission is how a viewer's new job was judged: admitted on one worker or shared between two, or refused with a reason
// and its words. Cost is the job's, and Capacities those of the workers it was admitted on (reference cores); Most is
// the most capacity any worker had, of a refusal. The decision line and the log say all of it.
type Admission struct {
	Reason, Text string
	How          string // "one_worker" or "shared", of an admitted job; "" of a refused one.
	Cost         float64
	Capacities   []float64
	Most         float64
}

// Where the job was admitted: on one worker that holds it alone, or shared.
const (
	AdmittedOnOneWorker = "one_worker"
	AdmittedShared      = "shared"
)

// admit is the rule, the same for every price: a job is admitted where a worker has its cost free, or, when it is
// cut into chunks, where two workers each have their share of it free and the one that runs it asks for helpers (D-046).
func (l *ledger) admit(job capJob) Admission {
	a := Admission{Cost: job.Cost}
	for _, n := range l.nodes {
		a.Most = max(a.Most, n.Cores)
	}
	best := ""
	for _, n := range l.nodes {
		if l.fits(n.ID, job) && (best == "" || l.free[n.ID] > l.free[best]) {
			best = n.ID
		}
	}
	if best != "" {
		a.How, a.Capacities = AdmittedOnOneWorker, []float64{l.cores[best]}
		return a
	}
	if host, partner, ok := l.pair(job); ok {
		a.How, a.Capacities = AdmittedShared, []float64{l.cores[host], l.cores[partner]}
		return a
	}
	a.Reason, a.Text = l.why(job)
	return a
}

// pair finds the two workers of a shared play: a host that runs the viewer's job, and a partner for its helpers, each
// with its share free. The ones with the most free are taken.
func (l *ledger) pair(job capJob) (host, partner string, ok bool) {
	if !job.shareable() {
		return "", "", false
	}
	job.Share = true
	helper := job
	helper.Helper = true
	best := 0.0
	for _, h := range l.nodes {
		for _, p := range l.nodes {
			if p.ID != h.ID && l.fits(h.ID, job) && l.fits(p.ID, helper) && (!ok || l.free[h.ID]+l.free[p.ID] > best) {
				host, partner, ok, best = h.ID, p.ID, true, l.free[h.ID]+l.free[p.ID]
			}
		}
	}
	return host, partner, ok
}

// sharedSpec says whether a spec's play runs on helpers of other workers: its viewer's job runs on a worker too small
// to hold it alone, or none runs and no worker could. This is the capacity of the workers and the price, not the speed
// the job happens to make now, so a play does not come and go as a held film with its heartbeat.
func sharedSpec(nodes []capNode, jobs []capJob, spec string, cost float64) bool {
	capacity := map[string]float64{}
	most := 0.0
	for _, n := range nodes {
		capacity[n.ID], most = n.Cores, max(most, n.Cores)
	}
	for _, j := range jobs {
		if j.Spec == spec && !j.Helper && j.Worker != "" {
			return capacity[j.Worker] < cost-coresSlack
		}
	}
	return most < cost-coresSlack
}

// heldFilms are the specs a viewer is watching that run on helpers that their own worker cannot do without, and that
// cut into chunks: a newcomer never stops their helpers (D-046, D-050). The spec the newcomer asks for is its own and is
// not one.
func heldFilms(nodes []capNode, jobs []capJob, own string) map[string]float64 {
	held := map[string]float64{}
	for _, j := range jobs {
		if j.Watched && j.Spec != own && j.shareable() && sharedSpec(nodes, jobs, j.Spec, j.Cost) {
			held[j.Spec] = j.Cost
		}
	}
	return held
}

// reserve holds the cores that a held film's next helper will take where it has none now (it has just made its chunk,
// or its last helper ended), on the worker where one would go: the film that was watched at real time is not given up
// to a newcomer in the moments between two helpers.
func (l *ledger) reserve(held map[string]float64, jobs []capJob) {
	specs := make([]string, 0, len(held))
	for spec := range held {
		specs = append(specs, spec)
	}
	slices.Sort(specs)
	for _, spec := range specs {
		host, hasHelper := "", false
		for _, j := range jobs {
			if j.Spec == spec && j.Helper {
				hasHelper = true
			}
			if j.Spec == spec && !j.Helper && j.Worker != "" {
				host = j.Worker
			}
		}
		if !hasHelper && host != "" {
			l.placeOff(capJob{Cost: held[spec], Helper: true, Share: true}, map[string]bool{host: true})
		}
	}
}

// judge says whether a viewer's new job is admitted, as PlaybackInfo and a stream replica's request both ask. The asker's
// own jobs and the helpers of films it does not watch give way to it; the helpers of a held film (heldFilms) do not, and a
// newcomer that would fit only by stopping them is refused with playback.CoresHeldByWatchedFilm. held says whether any film
// is held, which makes a refusal final: a job that does not fit is then not queued to wait for cores that stay held.
func judge(nodes []capNode, jobs []capJob, job capJob) (a Admission, held bool) {
	free := newLedger(nodes, jobs, viewerJobs)
	free.placeQueued(jobs, viewerJobs)
	films := heldFilms(nodes, jobs, job.Spec)
	if len(films) == 0 {
		return free.admit(job), false
	}
	keep := func(j capJob) bool { return viewerJobs(j) || (j.Helper && films[j.Spec] != 0) }
	l := newLedger(nodes, jobs, keep)
	l.placeQueued(jobs, keep)
	l.reserve(films, jobs)
	a = l.admit(job)
	if a.Reason != "" && free.admit(job).Reason == "" {
		a.Reason = playback.CoresHeldByWatchedFilm
		a.Text = fmt.Sprintf("the cores are held by a film another viewer is watching, which runs on helpers of more than one worker and would stall if they stopped: the job costs %.1f cores and the most any worker has is %g (output version %d)",
			job.Cost, a.Most, playback.OutputVersion)
	}
	return a, true
}

// preemptHelpers cancels the helpers that a viewer's job of the cost needs
// gone, and none unless that makes room: those of the one worker where fewest
// do, the ones furthest ahead first, since a helper that is far ahead of its
// viewer is the least missed; for a job two workers hold together, those of the
// two where fewest do. A viewer's job is never cancelled, nor is a helper of a
// held film (heldFilms) or of the job's own spec. It reports
// whether any helper was, so the caller can send preemptHint once the
// transaction commits and the helpers stop at once rather than at their next
// heartbeat. What a helper stored stays, and the rest of its chunk is made when
// a viewer reaches it, as after a seek. Called under the helpers lock. With
// stuck set the job has waited queued with room counted for it, which no worker
// took (a killed worker is counted until it is forgotten), so room is not asked.
func preemptHelpers(ctx context.Context, tx pgx.Tx, nodes []capNode, jobs []capJob, job capJob, stuck bool) (bool, error) {
	l := newLedger(nodes, jobs, func(capJob) bool { return true })
	l.placeQueued(jobs, func(j capJob) bool { return !j.Helper })
	if !stuck && l.admit(job).Reason == "" {
		return false, nil
	}
	films := heldFilms(nodes, jobs, job.Spec)
	// What stopping helpers on a worker would take to make the job fit it. A job's own helpers stay while it is placed
	// (a seek of the viewer they work for), unless it has waited queued for a worker that never took it (stuck).
	plan := func(n capNode, job capJob) (stop []int64, ok bool) {
		var helpers []capJob
		for _, held := range jobs {
			if held.Worker == n.ID && held.Helper && films[held.Spec] == 0 && (held.Spec != job.Spec || stuck) {
				helpers = append(helpers, held)
			}
		}
		slices.SortFunc(helpers, func(a, b capJob) int { return cmp.Or(cmp.Compare(b.Start, a.Start), cmp.Compare(b.ID, a.ID)) })
		free, copies := l.free[n.ID], l.copies[n.ID]
		fits := func() bool { return free >= job.need()-coresSlack && (!job.Copy || copies < copyBound) }
		for _, helper := range helpers {
			if fits() {
				break
			}
			free, stop = free+helper.Cost, append(stop, helper.ID)
		}
		return stop, fits()
	}
	var cancel []int64
	for _, n := range nodes {
		if stop, ok := plan(n, job); ok && len(stop) > 0 && (cancel == nil || len(stop) < len(cancel)) {
			cancel = stop
		}
	}
	if cancel == nil && job.shareable() {
		share := job
		share.Share = true
		for _, h := range nodes {
			hostStop, hostOK := plan(h, share)
			if h.NoHelpers || !hostOK {
				continue
			}
			for _, p := range nodes {
				helper := share
				helper.Helper = true
				partnerStop, partnerOK := plan(p, helper)
				if p.ID == h.ID || !partnerOK {
					continue
				}
				if stop := append(slices.Clone(hostStop), partnerStop...); len(stop) > 0 && (cancel == nil || len(stop) < len(cancel)) {
					cancel = stop
				}
			}
		}
	}
	if len(cancel) == 0 {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE transcode_jobs SET state = 'cancelled', error = 'a new viewer needed its slot', ended_at = now()
		WHERE id = ANY($1) AND state IN ('queued', 'running')`, cancel)
	return tag.RowsAffected() > 0, err
}
