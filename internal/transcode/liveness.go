package transcode

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
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

// livenessTable is every timing of worker liveness, each an expression of the
// default lease. It is the cluster's table and not a worker's: a worker run with
// another Lease changes its jobs' lease only, so a replica and a worker, whatever
// their leases, agree on when a lock is gone and a node forgotten. DOMAINS 25
// holds the same table, row for row.
type livenessTiming struct {
	Name       string
	Derivation string
	Of         func(lease time.Duration) time.Duration
}

var livenessTable = []livenessTiming{
	{"lease", "lease", func(l time.Duration) time.Duration { return l }},
	{"heartbeat", "lease*3/10", func(l time.Duration) time.Duration { return l * 3 / 10 }},
	{"counted", "lease", func(l time.Duration) time.Duration { return l }},
	{"grace", "lease/10", func(l time.Duration) time.Duration { return l / 10 }},
	{"forgottenLease", "lease/20", func(l time.Duration) time.Duration { return l / 20 }},
	{"confirmGap", "lease/20", func(l time.Duration) time.Duration { return l / 20 }},
	{"bound", "lease/20", func(l time.Duration) time.Duration { return l / 20 }},
	{"lookPeriod", "lease/40", func(l time.Duration) time.Duration { return l / 40 }},
	{"relockRetry", "lease/200", func(l time.Duration) time.Duration { return l / 200 }},
	{"hold", "lease", func(l time.Duration) time.Duration { return l }},
	{"oldJobsWait", "lease", func(l time.Duration) time.Duration { return l }},
	{"oldJobsPoll", "lease/500", func(l time.Duration) time.Duration { return l / 500 }},
	{"lockRetry", "lease/10", func(l time.Duration) time.Duration { return l / 10 }},
	{"deleteBound", "lease/2", func(l time.Duration) time.Duration { return l / 2 }},
	{"refusalPoll", "lease/100", func(l time.Duration) time.Duration { return l / 100 }},
	{"warnEvery", "lease", func(l time.Duration) time.Duration { return l }},
}

// liveTiming is the table's row of that name at the default lease. A name the
// table lacks is a bug in the code that asks, so it panics at once.
func liveTiming(name string) time.Duration {
	for _, row := range livenessTable {
		if row.Name == name {
			return row.Of(defaultLease)
		}
	}
	panic("no worker-liveness timing named " + name)
}

// LivenessHeartbeat is the table's heartbeat, for the caller of Present.
func LivenessHeartbeat() time.Duration { return liveTiming("heartbeat") }

// A worker's liveness is a state machine. Every change of state is
// liveNext's answer to an event, decided from livenessTransitions alone, so the
// unit tier can ask it with no Postgres.
type liveState int

const (
	stLocked      liveState = iota // The lock is held and the row is there: the worker claims, forgets and refreshes. Zero, see Worker.state.
	stRegistering                  // The lock is held and the row is not yet inserted.
	stRelocking                    // The lock's connection ended; the jobs go on while it is taken again.
	stRowGone                      // Another replica forgot the worker: every ffmpeg is stopped.
	stStopped                      // Present is returning.
)

func (s liveState) String() string {
	switch s {
	case stLocked:
		return "locked"
	case stRegistering:
		return "registering"
	case stRelocking:
		return "relocking"
	case stRowGone:
		return "row gone"
	case stStopped:
		return "stopped"
	}
	return "liveState(" + strconv.Itoa(int(s)) + ")"
}

type liveEvent int

const (
	evRegistered          liveEvent = iota // The row is inserted.
	evLockEnded                            // The lock's connection ended.
	evLockRetaken                          // The lock is held again; its row is not yet looked for.
	evLockRetakenRowFound                  // The lock is held again and the row is there.
	evRowGone                              // A look found the row gone.
	evOwnLockMissing                       // A look found the worker's own lock missing from pg_locks while its connection stands.
	evReadyToRegister                      // No job of the old session runs and a new lock is held.
	evShutdown                             // Present's context ended.
	evLookFailed                           // Postgres did not answer within the bound: no event.
	evLooked                               // A look found nothing amiss.
)

func (e liveEvent) String() string {
	names := [...]string{"registered", "lock ended", "lock retaken", "lock retaken and row found", "row gone",
		"own lock missing", "ready to register", "shutdown", "look failed", "looked"}
	if int(e) < 0 || int(e) >= len(names) {
		return "liveEvent(" + strconv.Itoa(int(e)) + ")"
	}
	return names[e]
}

type liveTransition struct {
	From  liveState
	Event liveEvent
	To    liveState
}

// livenessTransitions is the one place the next state is decided. A state and
// an event with no row here stay as they are, which is how a look Postgres does
// not answer, and a lock taken again whose row is not yet looked for, change nothing.
// A lock that ends during registration is delivered again once the row is in.
var livenessTransitions = []liveTransition{
	{stRegistering, evRegistered, stLocked},
	{stLocked, evLockEnded, stRelocking},
	{stRelocking, evLockRetakenRowFound, stLocked},
	{stLocked, evRowGone, stRowGone},
	{stRelocking, evRowGone, stRowGone},
	{stLocked, evOwnLockMissing, stRowGone},
	{stRowGone, evReadyToRegister, stRegistering},
	{stRegistering, evShutdown, stStopped},
	{stLocked, evShutdown, stStopped},
	{stRelocking, evShutdown, stStopped},
	{stRowGone, evShutdown, stStopped},
	{stStopped, evShutdown, stStopped},
}

func liveNext(s liveState, e liveEvent) liveState {
	for _, t := range livenessTransitions {
		if t.From == s && t.Event == e {
			return t.To
		}
	}
	return s
}

type liveAction int

const (
	actClaim         liveAction = iota // Claim a job.
	actForget                          // Forget other workers.
	actRefreshSeenAt                   // Refresh seen_at.
	actReadOwnRow                      // Read the worker's own row.
	actRetryLock                       // Take the lock again.
)

// allows says what a state may do: only a locked worker claims, forgets and
// refreshes seen_at; a relocking one only reads its own row and retries the lock.
func (s liveState) allows(a liveAction) bool {
	switch s {
	case stLocked:
		return a == actClaim || a == actForget || a == actRefreshSeenAt
	case stRelocking:
		return a == actReadOwnRow || a == actRetryLock
	}
	return false
}

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

// forgotHint tells a worker it was forgotten, so that it looks for its row at
// once and not at its next look: a viewer's request can start a replacement
// for its job within milliseconds.
func forgotHint(id string) string { return "forgot:" + id }

// reap makes the jobs of dead workers dead at once and forgets the workers: a
// node that took a liveness lock and no longer holds it is gone, so its row is
// deleted (its slots stop counting) and its running jobs are left the table's
// forgottenLease. Everything that decides a job is dead already reads
// lease_expires_at, so nothing else changes. Only a worker's own look
// (lookForTheDead) calls it, for a process that holds no lock cannot tell an
// outage from a death (JS-104), and it forgets nobody for the table's hold after
// Postgres was away. It returns the nodes it forgot and the jobs it cut short,
// and whether the row of the node self is there, from the same snapshot.
//
// A worker whose lock is gone but whose connection stands (a pooler recycled
// the server's end, or Postgres dropped a vanished host's backend on its
// keepalives) cannot be told from a dead one here. It finds its row gone
// itself within a lookPeriod, well inside the grace, and stops its ffmpeg; and
// with its row gone it can neither renew a lease nor store a segment
// (Worker.beat and Worker.upload check the row). Two renewals can still land
// in the moments before the worker stops: one already waiting on the job's row
// when reap commits, which still sees the old row, and the update after a store
// that was in flight. That job is then taken over at its full lease.
// reap forgets the dead nodes among only, or all of them when only is nil. When
// self is set, only a snapshot in which self holds its own lock forgets anyone
// but self: a worker that cannot see its own lock cannot see anyone's.
func reap(ctx context.Context, pool *pgxpool.Pool, self string, forget bool, only []string) (workers []string, jobs []int64, here bool, err error) {
	err = pool.QueryRow(ctx, `WITH dead AS (
			SELECT id FROM transcode_nodes n WHERE $3 AND locked AND NOT `+lockHeld("n.id")+`
				AND ($2 = '' OR n.id = $2 OR `+lockHeld("$2")+`)
				AND ($5::text[] IS NULL OR n.id = ANY($5))),
		expired AS (
			UPDATE transcode_jobs SET lease_expires_at = LEAST(lease_expires_at, now() + make_interval(secs => $1))
			WHERE state = 'running' AND worker IN (SELECT id FROM dead) RETURNING id),
		forgot AS (DELETE FROM transcode_nodes WHERE id IN (SELECT id FROM dead) RETURNING id)
		SELECT COALESCE((SELECT array_agg(id) FROM forgot), '{}'), COALESCE((SELECT array_agg(id) FROM expired), '{}'),
			EXISTS (SELECT 1 FROM transcode_nodes WHERE id = $2),
			(SELECT count(pg_notify($4, 'forgot:' || id)) FROM forgot)`,
		liveTiming("forgottenLease").Seconds(), self, forget, channel, only).Scan(&workers, &jobs, &here, new(int))
	return workers, jobs, here, err
}

// logReaped says what Reap did, when it did anything.
func logReaped(log *slog.Logger, workers []string, jobs []int64) {
	countReaped(workers, jobs)
	if len(workers) > 0 && log != nil {
		log.Warn("workers whose liveness lock is gone were forgotten, and their jobs given half a second to end before a live worker takes them",
			"workers", workers, "jobs", jobs)
	}
}

// Present keeps the worker's place in transcode_nodes, and the lock that
// proves it; keepRow is its loop. When the lock's connection ends (a switchover
// or a restart ends every connection, and the jobs have a lease for that) the
// jobs go on and the worker claims none; it takes the lock again, and only then
// looks for its row. Present while the row is there, nothing touched the jobs.
// Gone, another replica took the worker for dead and may give its jobs away, so
// every ffmpeg is stopped at once, and the row comes back only afterwards, with
// a new lock. It returns when ctx ends. every is the seen_at refresh, and the
// bound of registering; callers pass LivenessHeartbeat().
func (w *Worker) Present(ctx context.Context, roles, storeURI string, every time.Duration) {
	w.setID()
	db.Of(w.Pool).SetLog(w.Log)
	cfg := w.LivenessConfig
	if cfg == nil {
		cfg = w.Pool.Config().ConnConfig // A copy.
		cfg.Tracer = nil                 // A lock that cannot be taken is not Postgres being away.
	}
	var lostOnce bool
	state := stRegistering
	for ctx.Err() == nil {
		live, err := holdLiveness(ctx, cfg, w.ID)
		if err != nil {
			if ctx.Err() == nil {
				w.Log.Warn("taking the worker's liveness lock failed", "error", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(liveTiming("lockRetry")):
			}
			continue
		}
		if lostOnce {
			w.Log.Info("the worker holds its liveness lock again and takes jobs")
		}
		state = liveNext(state, evReadyToRegister)
		held, lose := context.WithCancelCause(ctx)
		w.setHeld(held, live)
		live, state = w.keepRow(held, lose, live, cfg, state, roles, storeURI, every)
		if errors.Is(context.Cause(held), errLockLost) {
			lostOnce = true
			w.Log.Error("the worker's row is gone, so another replica may have its jobs; they are stopped and it claims none until the lock is held again")
		}
		w.setHeld(nil, nil)
		lose(nil)
		if live != nil {
			live.Close()
		}
		// The row comes back only once no ffmpeg of the old session runs.
		w.waitForJobs()
	}
}

// waitForJobs returns when the worker runs no job, or after a bound: a job
// that cannot record its end is not worth keeping the worker out for.
func (w *Worker) waitForJobs() {
	for deadline := time.Now().Add(liveTiming("oldJobsWait")); time.Now().Before(deadline); time.Sleep(liveTiming("oldJobsPoll")) {
		w.liveMu.Lock()
		n := len(w.live)
		w.liveMu.Unlock()
		if n == 0 {
			return
		}
	}
}

// keepRow runs the worker's liveness state machine from state, which is
// registering or, after a row gone, what liveNext made of it. It puts the row in
// transcode_nodes and keeps it there until held ends, then deletes it. Every
// lookPeriod a locked worker reaps, and the row is gone if that finds this
// worker dead, or its row gone because another reaper got there first. When the
// lock's connection ends it takes the lock again on a connection of its own and
// looks for its row only then. The row is inserted once: putting it back would
// let the worker renew the leases Reap cut short. seen_at is refreshed every
// every. It returns the lock it holds at the end, if any, and the state.
func (w *Worker) keepRow(held context.Context, lose context.CancelCauseFunc, live *liveness, cfg *pgx.ConnConfig, state liveState, roles, storeURI string, every time.Duration) (*liveness, liveState) {
	defer func() {
		gone, cancel := context.WithTimeout(context.WithoutCancel(held), liveTiming("deleteBound"))
		defer cancel()
		w.Pool.Exec(gone, "DELETE FROM transcode_nodes WHERE id = $1", w.ID) //nolint:errcheck
	}()
	// next takes an event and moves to the state liveNext says.
	next := func(e liveEvent) {
		state = liveNext(state, e)
		w.setState(state)
	}
	w.setState(state)
	check := time.NewTicker(liveTiming("lookPeriod"))
	defer check.Stop()
	forgotten, unsubscribe := w.Hub.Subscribe(forgotHint(w.ID))
	defer unsubscribe()
	// The lock taken again, whose row has yet to be looked for.
	relocked := make(chan *liveness, 1)
	var relocking context.CancelFunc
	var relockDone chan struct{}
	var fresh *liveness
	defer func() {
		if relocking != nil {
			relocking()
			<-relockDone
		}
		select {
		case l := <-relocked:
			l.Close()
		default:
		}
		if fresh != nil {
			fresh.Close()
		}
	}()
	var seen, warned time.Time
	var confirm <-chan time.Time // When a node first found lockless is to be looked at again.
	for {
		if live != nil {
			select {
			case <-live.Lost():
				w.Log.Error("the worker's liveness connection ended; its jobs go on, and it claims none until it holds the lock again and its row is there", "error", live.Err())
				w.setLock(nil)
				live.Close()
				live = nil
				w.lockLost = time.Now()
			default:
			}
		}
		if live == nil && state == stLocked {
			next(evLockEnded)
		}
		if state.allows(actRetryLock) && fresh == nil && relocking == nil {
			ctx, cancel := context.WithCancel(held)
			relocking = cancel
			relockDone = make(chan struct{})
			go func() {
				defer close(relockDone)
				w.relock(ctx, cfg, relocked)
			}()
		}
		if state.allows(actRetryLock) && fresh == nil {
			select {
			case fresh = <-relocked:
				relocking = nil
				next(evLockRetaken)
			default:
			}
		}
		switch state {
		case stRegistering:
			// A slow commit is no reason not to register, so this has longer than a look.
			ctx, cancel := context.WithTimeout(held, every)
			_, err := w.Pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked, output_version) VALUES ($1, $2, $3, $4, true, $5)
				ON CONFLICT (id) DO UPDATE SET seen_at = now(), locked = true`, w.ID, roles, storeURI, w.Slots, playback.OutputVersion)
			cancel()
			if err == nil {
				seen = time.Now()
				next(evRegistered)
				if live == nil { // The lock ended before the row was in.
					next(evLockEnded)
				}
			} else if held.Err() == nil && time.Since(warned) > every {
				warned = time.Now()
				w.Log.Warn("registering with the transcode nodes failed", "error", err)
			}
		case stRelocking:
			// Without the lock a reap would take this worker for dead, so it only looks
			// for its row, which another replica's reap may have taken.
			next(w.lookForOwnRow(held, fresh != nil))
			switch state {
			case stRowGone:
				w.Log.Error("another replica took this worker for dead and forgot it while its lock was gone")
			case stLocked:
				live, fresh = fresh, nil
				w.setLock(live)
				w.Log.Info("the worker holds its liveness lock again and its row is there; its jobs went on")
			}
		case stLocked:
			next(w.look(held))
			confirm = nil
			if wait := w.nextConfirmation(); wait > 0 {
				confirm = time.After(wait)
			}
			if state.allows(actRefreshSeenAt) && time.Since(seen) >= every {
				w.refreshRow(held, &seen)
			}
		}
		if state == stRowGone {
			lose(errLockLost)
			return live, state
		}
		// A look follows at once when the lock ends and when a reap forgets this worker.
		var lockEnded <-chan struct{}
		if live != nil {
			lockEnded = live.Lost()
		}
		select {
		case <-held.Done():
			next(evShutdown)
			return live, state
		case <-check.C:
		case <-confirm:
		case <-lockEnded:
		case <-forgotten:
		}
	}
}

// relock takes the lock again, as often as it takes, and hands it over.
func (w *Worker) relock(ctx context.Context, cfg *pgx.ConnConfig, taken chan<- *liveness) {
	var warned time.Time
	for ctx.Err() == nil {
		l, err := holdLiveness(ctx, cfg, w.ID)
		if err == nil {
			taken <- l
			return
		}
		if ctx.Err() == nil && time.Since(warned) > liveTiming("warnEvery") {
			warned = time.Now()
			w.Log.Warn("taking the worker's liveness lock again failed; it tries on", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(liveTiming("relockRetry")):
		}
	}
}

// lookForOwnRow reads the worker's own row, which a relocking worker may and
// a locked one need not, and says which event that is. A database that does
// not answer is no answer.
func (w *Worker) lookForOwnRow(held context.Context, lockBack bool) liveEvent {
	ctx, cancel := context.WithTimeout(held, liveTiming("bound"))
	defer cancel()
	var here bool
	if err := w.Pool.QueryRow(ctx, "SELECT "+registeredAs("$1"), w.ID).Scan(&here); err != nil {
		return evLookFailed
	}
	switch {
	case !here:
		return evRowGone
	case lockBack:
		return evLockRetakenRowFound
	}
	return evLooked
}

// look reaps, and says what it found: this worker among the dead (its own lock
// missing, or its row gone), nothing amiss, or no answer. The look is bounded
// well inside the grace, and a database that does not answer is not a loss.
//
// A node is forgotten only when this worker's earlier look, at least the
// confirmGap before, already found its lock gone; when this worker holds its
// own lock in the same snapshot (reap); and when this worker's own lock has not
// ended within the hold. A worker can never miss an outage, for its own lock's
// connection ends in it; a live worker takes its lock again within
// milliseconds, a dead one never does (JS-029: on two CPUs a worker forgot a
// live one and stopped its ffmpeg).
func (w *Worker) look(held context.Context) liveEvent {
	ctx, cancel := context.WithTimeout(held, liveTiming("bound"))
	defer cancel()
	var missing []string
	if err := w.Pool.QueryRow(ctx, `SELECT COALESCE(array_agg(id), '{}') FROM transcode_nodes n WHERE locked AND NOT `+lockHeld("n.id")).Scan(&missing); err != nil {
		return evLookFailed
	}
	now, confirmed := time.Now(), []string{}
	seen := map[string]time.Time{}
	for _, id := range missing {
		since, ok := w.lockGone[id]
		if !ok {
			since = now
		}
		seen[id] = since
		if now.Sub(since) >= liveTiming("confirmGap") {
			confirmed = append(confirmed, id)
		}
	}
	w.lockGone = seen
	workers, jobs, here, err := reap(ctx, w.Pool, w.ID, time.Since(w.lockLost) >= liveTiming("hold"), confirmed)
	if err != nil {
		return evLookFailed
	}
	switch {
	case slices.Contains(workers, w.ID):
		// Open, yet not in pg_locks: the server's end of the connection is not
		// the one that took the lock. That is what a pooler in transaction mode does.
		countReaped(workers, jobs)
		w.Log.Error("the liveness connection is open but Postgres does not show its lock; is the database URL behind a transaction-mode pooler? It must be a direct, session-mode connection", "jobs", jobs)
		return evOwnLockMissing
	case !here:
		w.Log.Error("another replica took this worker for dead and forgot it; its lock is not where Postgres shows it")
		return evRowGone
	}
	logReaped(w.Log, workers, jobs)
	return evLooked
}

// nextConfirmation is how long until the first node found lockless by the last
// look has been so for the confirmGap, or 0 if none waits on that. A look is made
// then, so a dead worker is forgotten at that mark and not at the next tick.
func (w *Worker) nextConfirmation() time.Duration {
	var next time.Duration
	for _, since := range w.lockGone {
		if wait := time.Until(since.Add(liveTiming("confirmGap"))); wait > 0 && (next == 0 || wait < next) {
			next = wait
		}
	}
	return next
}

// refreshRow marks the worker seen. A row that is gone was already found by
// the look before it, in the same tick.
func (w *Worker) refreshRow(held context.Context, seen *time.Time) {
	// Bounded like a look, so a slow commit never holds the next look up; a
	// refresh that times out is tried again at the next tick.
	ctx, cancel := context.WithTimeout(held, liveTiming("bound"))
	defer cancel()
	if _, err := w.Pool.Exec(ctx, "UPDATE transcode_nodes SET seen_at = now() WHERE id = $1", w.ID); err == nil {
		*seen = time.Now()
	}
}

// setHeld says which context the worker's jobs run in, and the lock it holds:
// the context ends when the worker's row is found gone, and is nil while there
// is no session.
func (w *Worker) setHeld(ctx context.Context, lock *liveness) {
	w.heldMu.Lock()
	w.held, w.lock = ctx, lock
	w.heldMu.Unlock()
}

func (w *Worker) setState(s liveState) {
	w.heldMu.Lock()
	w.state = s
	w.heldMu.Unlock()
}

func (w *Worker) setLock(lock *liveness) {
	w.heldMu.Lock()
	w.lock = lock
	w.heldMu.Unlock()
}

// jobContext is the context a claimed job runs in, or false if the worker may
// not claim: it claims only while it is locked, which is while it holds its lock, which can be gone for a
// moment without the jobs it has being touched.
func (w *Worker) jobContext() (context.Context, bool) {
	w.heldMu.Lock()
	defer w.heldMu.Unlock()
	if w.held == nil || w.held.Err() != nil || w.lock == nil || !w.state.allows(actClaim) {
		return nil, false
	}
	select {
	case <-w.lock.Lost():
		return nil, false
	default:
	}
	return w.held, true
}
