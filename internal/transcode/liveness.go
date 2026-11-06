package transcode

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A worker proves it is alive by holding a session advisory lock on a
// connection of its own: Postgres drops the lock the moment the connection
// closes, which the kernel does when the process dies, however it dies. Any
// replica can then see that the worker is gone (Reap) in a second, where its
// lease takes ten. The key is the two-int form (livenessClass, hash of the node
// id), which cannot collide with the single-bigint locks of the rest of the code.
//
// Two node ids with one hash share a lock: a dead one is taken for live until
// its lease runs out, and the second of two live ones waits, claiming nothing
// and saying so in its log, until the first lets go. Never two owners.
const livenessClass = "27219"

// reapGrace is how long a dead worker's jobs stay leased after its lock is
// seen gone. The worker itself learns of the loss within milliseconds when the
// connection was closed or terminated, and stops its ffmpeg then; the grace
// keeps a replacement from starting before that has happened.
const reapGrace = time.Second

// lockCheck is how often a worker reaps: it forgets dead workers and cuts
// their jobs' leases short, so a viewer whose player has read ahead finds the
// job ready to be taken over when it next asks, and it looks for its own lock
// in the same scan of pg_locks. A lock can go while its connection stands (a
// pooler recycled the server's end), and then only looking tells the worker;
// looking well within reapGrace stops its ffmpeg before anyone may take its
// jobs.
const lockCheck = reapGrace / 4

// reapEvery is how often one replica looks for dead workers while it waits.
const reapEvery = time.Second

// errLockLost is why a worker's jobs were stopped.
var errLockLost = errors.New("the worker's liveness lock was lost")

// liveness is a held liveness lock.
type liveness struct {
	conn *pgx.Conn
	lost chan struct{}
	err  error // Why the connection ended, once lost is closed.
	stop context.CancelFunc
}

// holdLiveness connects with cfg and takes the lock for id. Lost is closed
// when the connection ends. Only a closed or failed connection counts: the
// goroutine blocks reading it, so a slow database is not a loss.
func holdLiveness(ctx context.Context, cfg *pgx.ConnConfig, id string) (*liveness, error) {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var got bool
	err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock("+livenessClass+", hashtext($1))", id).Scan(&got)
	if err == nil && !got {
		// Only another node whose id hashes alike holds it. Present tries again.
		err = errors.New("the liveness lock is held by another node whose id has the same hash; this worker claims nothing until it is free")
	}
	if err != nil {
		conn.Close(context.WithoutCancel(ctx)) //nolint:errcheck
		return nil, err
	}
	l := &liveness{conn: conn, lost: make(chan struct{})}
	wait, stop := context.WithCancel(context.Background())
	l.stop = stop
	go func() {
		defer close(l.lost)
		// Nothing is ever sent to this connection, so this returns only when it
		// ends, or when Close stops it.
		_, l.err = l.conn.WaitForNotification(wait)
	}()
	return l, nil
}

// Lost is closed once the lock is gone.
func (l *liveness) Lost() <-chan struct{} { return l.lost }

// Err says why the lock is gone, once Lost is closed.
func (l *liveness) Err() error { return l.err }

// Close releases the lock. The reader is stopped first, so only one goroutine
// ever closes the connection.
func (l *liveness) Close() {
	l.stop()
	<-l.lost
	l.conn.Close(context.Background()) //nolint:errcheck
}

// lockHeld is SQL true when the node whose id is idExpr holds its liveness lock.
// pg_locks covers the whole cluster, hence the database. objid is an unsigned
// oid and hashtext is signed, hence the mask.
func lockHeld(idExpr string) string {
	return `EXISTS (SELECT 1 FROM pg_locks l
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2 AND l.classid = ` + livenessClass + `
			AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND l.objid::bigint = hashtext(` + idExpr + `)::bigint & 4294967295)`
}

// Reap makes the jobs of dead workers dead at once and forgets the workers: a
// node that took a liveness lock and no longer holds it is gone, so its row is
// deleted (its slots stop counting) and its running jobs are left a grace of
// reapGrace. Everything that decides a job is dead already reads
// lease_expires_at, so nothing else changes. Idempotent, so any replica may
// call it at any time. It returns the nodes it forgot and the jobs it cut short,
// and whether the row of the node self is there, from the same snapshot.
//
// A worker whose lock is gone but whose connection stands (a pooler recycled
// the server's end, or Postgres dropped a vanished host's backend on its
// keepalives) cannot be told from a dead one here. It finds the lock gone
// itself within lockCheck, well inside the grace, and stops its ffmpeg; and
// with its row gone it can neither renew a lease nor store a segment
// (Worker.beat and Worker.upload check the row). Two renewals can still land
// in the moments before the worker stops: one already waiting on the job's row
// when Reap commits, which still sees the old row, and the update after a store
// that was in flight. That job is then taken over at its full lease.
func Reap(ctx context.Context, pool *pgxpool.Pool, self string) (workers []string, jobs []int64, here bool, err error) {
	err = pool.QueryRow(ctx, `WITH dead AS (
			SELECT id FROM transcode_nodes n WHERE locked AND NOT `+lockHeld("n.id")+`),
		expired AS (
			UPDATE transcode_jobs SET lease_expires_at = LEAST(lease_expires_at, now() + make_interval(secs => $1))
			WHERE state = 'running' AND worker IN (SELECT id FROM dead) RETURNING id),
		forgot AS (DELETE FROM transcode_nodes WHERE id IN (SELECT id FROM dead) RETURNING id)
		SELECT COALESCE((SELECT array_agg(id) FROM forgot), '{}'), COALESCE((SELECT array_agg(id) FROM expired), '{}'),
			EXISTS (SELECT 1 FROM transcode_nodes WHERE id = $2)`,
		reapGrace.Seconds(), self).Scan(&workers, &jobs, &here)
	return workers, jobs, here, err
}

// logReaped says what Reap did, when it did anything.
func logReaped(log *slog.Logger, workers []string, jobs []int64) {
	if len(workers) > 0 && log != nil {
		log.Warn("workers whose liveness lock is gone were forgotten, and their jobs given a second to end before a live worker takes them",
			"workers", workers, "jobs", jobs)
	}
}

// reap is Reap at most every reapEvery per Jobs, for a loop that asks often.
func (j *Jobs) reap(ctx context.Context) error {
	now := time.Now().UnixNano()
	last := j.reaped.Load()
	if now-last < int64(reapEvery) || !j.reaped.CompareAndSwap(last, now) {
		return nil
	}
	workers, jobs, _, err := Reap(ctx, j.Pool, "")
	logReaped(j.Log, workers, jobs)
	return err
}

// Present keeps the worker's place in transcode_nodes, and the lock that
// proves it. The worker claims nothing, and its row is absent, while the lock
// is not held; when it is lost, every ffmpeg is stopped at once, because
// Reap lets another worker take the jobs, and only after the lock is held
// again does the row come back. It returns when ctx ends.
func (w *Worker) Present(ctx context.Context, roles, storeURI string, every time.Duration) {
	w.setID()
	cfg := w.LivenessConfig
	if cfg == nil {
		cfg = w.Pool.Config().ConnConfig
	}
	var lostOnce bool
	for ctx.Err() == nil {
		live, err := holdLiveness(ctx, cfg, w.ID)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.Warn("taking the worker's liveness lock failed", "error", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		if lostOnce {
			w.Log.Info("the worker holds its liveness lock again and takes jobs")
		}
		held, lose := context.WithCancelCause(ctx)
		// On its own, so that a slow query in keepRow never holds it up.
		go func() {
			select {
			case <-live.Lost():
				if held.Err() == nil { // Not Close, which comes after held ends.
					w.Log.Error("the worker's liveness connection ended", "error", live.Err())
					lose(errLockLost)
				}
			case <-held.Done():
			}
		}()
		w.setHeld(held)
		w.keepRow(held, lose, roles, storeURI, every)
		lost := errors.Is(context.Cause(held), errLockLost)
		if lost {
			lostOnce = true
			w.Log.Error("the worker lost its liveness lock; its jobs are stopped and it claims none until the lock is held again")
		}
		w.setHeld(nil)
		lose(nil)
		live.Close()

	}
}

// keepRow puts the worker's row in transcode_nodes and keeps it there until
// held ends, then deletes it. Every lockCheck it reaps, and the lock is lost if
// that finds this worker dead, or its row gone because another reaper got there
// first. The row is inserted once: putting it back would let the worker renew
// the leases Reap cut short. seen_at is refreshed every every.
func (w *Worker) keepRow(held context.Context, lose context.CancelCauseFunc, roles, storeURI string, every time.Duration) {
	defer func() {
		gone, cancel := context.WithTimeout(context.WithoutCancel(held), 5*time.Second)
		defer cancel()
		w.Pool.Exec(gone, "DELETE FROM transcode_nodes WHERE id = $1", w.ID) //nolint:errcheck
	}()
	check := time.NewTicker(lockCheck)
	defer check.Stop()
	var inserted, seen, warned time.Time
	for {
		dead := false
		switch {
		case inserted.IsZero():
			// A slow commit is no reason not to register, so this has longer than a look.
			ctx, cancel := context.WithTimeout(held, every)
			_, err := w.Pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ($1, $2, $3, $4, true)
				ON CONFLICT (id) DO UPDATE SET seen_at = now(), locked = true`, w.ID, roles, storeURI, w.Slots)
			cancel()
			if err == nil {
				inserted, seen = time.Now(), time.Now()
			} else if held.Err() == nil && time.Since(warned) > every {
				warned = time.Now()
				w.Log.Warn("registering with the transcode nodes failed", "error", err)
			}
		default:
			dead = w.lookForTheDead(held)
			if !dead && time.Since(seen) >= every {
				w.refreshRow(held, &seen)
			}
		}
		if dead {
			lose(errLockLost)
			return
		}
		select {
		case <-held.Done():
			return
		case <-check.C:
		}
	}
}

// lookForTheDead reaps, and says whether this worker is among the dead. The
// look is bounded well inside reapGrace, and a database that does not answer
// is not a loss.
func (w *Worker) lookForTheDead(held context.Context) bool {
	ctx, cancel := context.WithTimeout(held, reapGrace/2)
	defer cancel()
	workers, jobs, here, err := Reap(ctx, w.Pool, w.ID)
	if err != nil {
		return false
	}
	switch {
	case slices.Contains(workers, w.ID):
		// Open, yet not in pg_locks: the server's end of the connection is not
		// the one that took the lock. That is what a pooler in transaction mode does.
		w.Log.Error("the liveness connection is open but Postgres does not show its lock; is the database URL behind a transaction-mode pooler? It must be a direct, session-mode connection", "jobs", jobs)
		return true
	case !here:
		w.Log.Error("another replica took this worker for dead and forgot it; its lock is not where Postgres shows it")
		return true
	}
	logReaped(w.Log, workers, jobs)
	return false
}

// refreshRow marks the worker seen. A row that is gone was already found by
// the look before it, in the same tick.
func (w *Worker) refreshRow(held context.Context, seen *time.Time) {
	// Bounded like a look, so a slow commit never holds the next look up; a
	// refresh that times out is tried again at the next tick.
	ctx, cancel := context.WithTimeout(held, reapGrace/2)
	defer cancel()
	if _, err := w.Pool.Exec(ctx, "UPDATE transcode_nodes SET seen_at = now() WHERE id = $1", w.ID); err == nil {
		*seen = time.Now()
	}
}

// setHeld says which context the worker's jobs run in: one that ends when the
// lock is lost, or nil while it is not held.
func (w *Worker) setHeld(ctx context.Context) {
	w.heldMu.Lock()
	w.held = ctx
	w.heldMu.Unlock()
}

// jobContext is the context a claimed job runs in, or false if the worker may
// not claim.
func (w *Worker) jobContext() (context.Context, bool) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	// A lock that is lost and not yet let go of is not held.
	return w.held, w.held != nil && w.held.Err() == nil
}
