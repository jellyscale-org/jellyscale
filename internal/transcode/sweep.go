package transcode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/store"
)

// watchedWindow is how long after its last request an unstopped viewer still
// counts as watching: a film paused for up to this long is still served
// (invariant 37), so the budget leaves its segments alone.
const watchedWindow = 10 * time.Minute

// assetSweepEvery is how many sweep periods lie between two sweeps of the asset
// stores: they list a whole store and read every fonts manifest, and the
// objects they remove are those of files that changed or went, which is not
// urgent.
const assetSweepEvery = 10

// sweepLoop removes work nobody needs, once a sweep period (a minute). Every
// worker runs it, but the stores are listed by one worker a period: the others
// find the turn taken in Postgres (claimSweep) and only do the cheap part.
func (w *Worker) sweepLoop(ctx context.Context) {
	every := orDefault(w.SweepEvery, time.Minute)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		segments := w.claimSweep(ctx, "segments", every)
		assets := w.claimSweep(ctx, "assets", assetSweepEvery*every)
		if err := w.sweep(ctx, segments, assets); err != nil && ctx.Err() == nil {
			w.Log.Error("transcode sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// claimSweep takes the turn for a kind of sweep unless another worker took it
// within the last 90% of its period (a little under, so that a worker whose
// ticker runs a touch early is not turned away every time). One statement, so
// of workers that ask at once exactly one wins.
func (w *Worker) claimSweep(ctx context.Context, name string, every time.Duration) bool {
	var claimed bool
	err := w.Pool.QueryRow(ctx, `INSERT INTO sweeps (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET at = now() WHERE sweeps.at < now() - make_interval(secs => $2)
		RETURNING true`, name, every.Seconds()*0.9).Scan(&claimed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
		w.Log.Warn("taking the turn to sweep failed", "sweep", name, "error", err)
	}
	return claimed
}

// Sweep cancels jobs whose worker vanished, which viewers restart, forgets old job rows, deletes
// the segments of specs nobody has asked for in Retention and, over Budget,
// those least recently asked for, and removes the assets of file versions that
// no longer exist. Every step is idempotent and the segment sweep takes a lock,
// so workers sweeping at once is harmless.
func (w *Worker) Sweep(ctx context.Context) error { return w.sweep(ctx, true, true) }

// sweep is Sweep, with the sweeps of the stores (which list them) only when it
// is this worker's turn.
func (w *Worker) sweep(ctx context.Context, segments, assets bool) error {
	w.clearScratch()
	if err := expireQueued(ctx, w.Pool, orDefault(w.QueueTimeout, queueTimeout)); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM transcode_nodes WHERE seen_at < now() - interval '1 hour'`,
		`UPDATE transcode_jobs SET state = 'cancelled', error = 'the worker stopped renewing its lease', ended_at = now()
			WHERE state = 'running' AND lease_expires_at < now()`,
		`DELETE FROM transcode_jobs WHERE state IN ('done', 'failed', 'cancelled') AND created_at < now() - interval '1 day'`,
		`DELETE FROM subtitle_jobs WHERE state IN ('done', 'failed') AND ended_at < now() - interval '1 day'`,
	} {
		if _, err := w.Pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if segments && w.Segments != nil {
		w.sweepSegments(ctx)
	}
	if assets && w.Media != nil {
		w.sweepAssets(ctx)
	}
	return nil
}

// specUse is what the sweep knows of one spec in the store.
type specUse struct {
	hash    string
	objects []store.Object
	size    int64
	idle    float64 // Seconds since a viewer last asked for it; infinite if none ever did.
	watched bool
}

// sweepSegments removes from the transcode store the specs nobody asked for in
// Retention, and then, while the store is over its Budget, the least recently
// asked for of those nobody is watching, whole. One worker at a time: a second
// finds the first's lock and leaves it the work, so that it never plans from a
// listing the first has made out of date. A listing that fails removes
// nothing, is logged, and leaves the size metric as it was.
func (w *Worker) sweepSegments(ctx context.Context) {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		w.logSweepFailure(ctx, "segments", err)
		return
	}
	defer conn.Release()
	var held bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended('sweep:segments', 0))").Scan(&held); err != nil {
		w.logSweepFailure(ctx, "segments", err)
		return
	}
	if !held {
		return
	}
	defer func() {
		// A lock left on a pooled connection would be held for good.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtextextended('sweep:segments', 0))"); err != nil {
			conn.Hijack().Close(context.WithoutCancel(ctx)) //nolint:errcheck
		}
	}()

	objects, err := w.Segments.List(ctx, "segments")
	if errors.Is(err, fs.ErrNotExist) {
		return // Nothing has been stored yet; the size is not known to be 0.
	}
	if err != nil {
		w.logSweepFailure(ctx, "segments", fmt.Errorf("listing the transcode store: %w", err))
		return
	}
	specs := map[string]*specUse{}
	var total int64
	for _, object := range objects {
		total += object.Size
		parts := strings.Split(object.Key, "/")
		if len(parts) != 3 {
			continue
		}
		spec := specs[parts[1]]
		if spec == nil {
			spec = &specUse{hash: parts[1], idle: math.Inf(1)}
			specs[parts[1]] = spec
		}
		spec.objects = append(spec.objects, object)
		spec.size += object.Size
	}
	if err := w.sweepSpecs(ctx, specs, &total); err != nil {
		w.logSweepFailure(ctx, "segments", err)
	}
	// What is held now, whatever stopped the sweep: the listing less what it removed.
	if _, err := w.Pool.Exec(ctx, `INSERT INTO sweeps (name, store_bytes) VALUES ('segments', $1)
		ON CONFLICT (name) DO UPDATE SET store_bytes = $1`, total); err != nil {
		w.logSweepFailure(ctx, "segments", err)
	}
}

func (w *Worker) logSweepFailure(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		w.Log.Error("a sweep of the "+what+" removed nothing more", "error", err)
	}
}

// sweepSpecs removes the specs unused for Retention, then enough of the least
// recently used ones to bring total to Budget, and takes what it removed off
// total.
func (w *Worker) sweepSpecs(ctx context.Context, specs map[string]*specUse, total *int64) error {
	rows, err := w.Pool.Query(ctx, `SELECT spec_hash, bool_or(watched), min(idle) FROM (
			SELECT spec_hash, NOT stopped AND last_seen_at >= now() - make_interval(secs => $1) AS watched,
				extract(epoch FROM now() - last_seen_at)::float8 AS idle FROM transcode_viewers
			UNION ALL
			SELECT spec_hash, true, 0::float8 FROM transcode_jobs WHERE state IN ('queued', 'running')) u
		GROUP BY spec_hash`, watchedWindow.Seconds())
	if err != nil {
		return err
	}
	for rows.Next() {
		var hash string
		var watched bool
		var idle *float64
		if err := rows.Scan(&hash, &watched, &idle); err != nil {
			rows.Close()
			return err
		}
		if spec := specs[hash]; spec != nil {
			spec.watched = watched
			if idle != nil {
				spec.idle = *idle
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// By age: the specs of viewers none of whom has asked for Retention, which
	// may have no objects any more (their rows go too).
	retention := orDefault(w.Retention, 24*time.Hour).Seconds()
	rows, err = w.Pool.Query(ctx, `SELECT spec_hash FROM transcode_viewers GROUP BY spec_hash
		HAVING max(last_seen_at) < now() - make_interval(secs => $1)`, retention)
	if err != nil {
		return err
	}
	expired, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, hash := range expired {
		spec := specs[hash]
		if spec == nil {
			spec = &specUse{hash: hash}
		}
		freed, done, err := w.evict(ctx, spec, retention, true)
		*total -= freed
		if err != nil {
			return err
		}
		if done {
			w.Log.Info("deleted the segments of an unused spec", "spec", hash, "segments", len(spec.objects))
			delete(specs, hash)
		}
	}

	// What a sweep that died partway began to remove, whatever the budget.
	rows, err = w.Pool.Query(ctx, "SELECT spec_hash FROM sweep_evictions")
	if err != nil {
		return err
	}
	started, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, hash := range started {
		spec := specs[hash]
		done := spec == nil || len(spec.objects) == 0 || spec.watched
		if !done {
			freed, ok, err := w.evict(ctx, spec, watchedWindow.Seconds(), false)
			*total -= freed
			if err != nil {
				return err
			}
			done = ok
			if ok {
				w.Log.Info("finished removing a spec that an earlier sweep began to remove", "spec", hash, "objects", len(spec.objects))
				delete(specs, hash)
			}
		}
		if done {
			if _, err := w.Pool.Exec(ctx, "DELETE FROM sweep_evictions WHERE spec_hash = $1", hash); err != nil {
				return err
			}
		}
	}

	// By budget.
	if w.Budget <= 0 || *total <= w.Budget {
		return nil
	}
	order := make([]*specUse, 0, len(specs))
	for _, spec := range specs {
		order = append(order, spec)
	}
	slices.SortFunc(order, func(a, b *specUse) int {
		switch {
		case a.idle > b.idle:
			return -1
		case a.idle < b.idle:
			return 1
		}
		return strings.Compare(a.hash, b.hash)
	})
	for _, spec := range order {
		if *total <= w.Budget {
			return nil
		}
		if spec.watched {
			continue
		}
		freed, done, err := w.evict(ctx, spec, watchedWindow.Seconds(), false)
		*total -= freed
		if err != nil {
			return err
		}
		if done {
			w.Log.Info("deleted the segments of the least recently used spec, the transcode store being over its budget",
				"spec", spec.hash, "objects", len(spec.objects), "bytes", spec.size, "budget", w.Budget)
		}
	}
	if *total > w.Budget {
		w.Log.Warn("the transcode store is over its budget and every spec that is left is being watched, so none is removed",
			"bytes", *total, "budget", w.Budget)
	}
	return nil
}

// evict removes a spec's objects, one at a time, each under the spec's lock
// and only if nobody has asked for the spec since: a viewer who comes back
// waits for one delete, not for the whole spec, and the removal stops the
// moment the spec is wanted again. The spec's finished jobs are made to cover
// nothing first, so that no viewer is told a segment is stored that is not
// (the viewer's request then starts a job). A sweeper killed partway leaves
// objects that the next sweep removes. window is how recently a request makes
// the spec wanted; for the age sweep (ageOnly) a stopped viewer's request
// counts too, and the viewers' rows go with the spec. It returns the bytes
// freed and whether the whole spec went.
func (w *Worker) evict(ctx context.Context, spec *specUse, window float64, ageOnly bool) (freed int64, done bool, err error) {
	locked := func(do func(pgx.Tx) error) (bool, error) {
		ran := false
		err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", spec.hash); err != nil {
				return err
			}
			var unused bool
			err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM transcode_viewers WHERE spec_hash = $1
					AND last_seen_at >= now() - make_interval(secs => $2) AND (NOT stopped OR $3))
				AND NOT EXISTS (SELECT 1 FROM transcode_jobs WHERE spec_hash = $1 AND state IN ('queued', 'running'))`,
				spec.hash, window, ageOnly).Scan(&unused)
			if err != nil || !unused {
				return err
			}
			ran = true
			return do(tx)
		})
		return ran, err
	}
	ok, err := locked(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO sweep_evictions VALUES ($1) ON CONFLICT DO NOTHING", spec.hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE transcode_jobs SET next_segment = start_segment
			WHERE spec_hash = $1 AND state NOT IN ('queued', 'running') AND next_segment > start_segment`, spec.hash)
		return err
	})
	if err != nil || !ok {
		return 0, false, err
	}
	for _, object := range spec.objects {
		ok, err := locked(func(pgx.Tx) error { return w.Segments.Delete(ctx, object.Key) })
		if err != nil || !ok {
			return freed, false, err
		}
		freed += object.Size
	}
	if _, err := w.Pool.Exec(ctx, "DELETE FROM sweep_evictions WHERE spec_hash = $1", spec.hash); err != nil {
		return freed, false, err
	}
	if ageOnly {
		ok, err = locked(func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "DELETE FROM transcode_viewers WHERE spec_hash = $1", spec.hash)
			return err
		})
		return freed, ok, err
	}
	return freed, true, nil
}
