package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Every leased job of a worker, a transcode job or a subtitle job, keeps its
// lease through the worker's one leaseKeeper, whatever the job is doing: its work
// may be blocked in a call that never returns, and the lease is kept, and given
// up, from a goroutine of its own, on a connection of its own. Not the pool's,
// where a query that gave up closes its connection and a new one costs several
// round trips; and not the liveness lock's, which a renewal that gave up would
// close, and which is parked waiting for the lock's loss.
//
// Every heartbeat it renews all the worker's jobs, one statement per job table,
// each its own transaction, each giving up after half a heartbeat. It never waits
// for a row while it holds another, which is how two statements come to wait for
// each other and Postgres aborts one of them, whichever it likes: the statement
// skips the rows another session holds, and each row it skipped is asked again
// alone. A renewal counts when Postgres has answered it and not before. It
// cuts a job's work the cutMargin before the lease can lapse, counted from when the
// last answered renewal of that job was sent (the lease can't have started
// earlier), so a worker that cannot renew has stopped before another may own the
// job. A failed renewal is tried again at once, and then at a short interval,
// until it is answered or the job is cut.
//
// The fence before a put is a renewal too: the job asks the keeper for one sent
// after the ask, so a lease that was taken away (a cancel, a reap's cut, the
// deletion of the worker's row) is seen at put time, not a heartbeat later. It
// renews the asking jobs only; the heartbeat renews them all. While the keeper's
// last renewal of a job was not answered, the fence is not either, at once: the
// job's loop, which also pauses its ffmpeg at the lead, does not wait for it.

// errUnrenewed is why a job was given up when no renewal was answered in time.
var errUnrenewed = errors.New("the lease was not renewed in time, so the job is given up before it can lapse")

// errStalled is why a job was given up when its work did not come round.
var errStalled = errors.New("the job's work has not come round for a lease, so it is given up and its lease left to lapse")

// errNoViewer is why a job was ended when nobody watches its spec.
var errNoViewer = fmt.Errorf("%w: no viewer is watching", errInterrupted)

// queryer is what a pool and a connection both are.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const (
	transcodeTable = iota
	subtitleTable
)

type leaseKeeper struct {
	w *Worker

	mu      sync.Mutex
	jobs    map[leaseID]*leasedJob
	asks    []*fenceAsk
	due     [2]bool       // A table to renew at once: a job came, or a helper was cut.
	running bool          // run is going; it returns, under mu, when no job is left.
	wake    chan struct{} // Something run waits on has changed.
}

// leaseID names a leased job: a transcode job by its id, a subtitle job by its key.
type leaseID struct {
	id  int64
	key string
}

func (id leaseID) table() int {
	if id.key != "" {
		return subtitleTable
	}
	return transcodeTable
}

// leasedJob is what the keeper knows of a job whose lease it keeps. Its fields
// below the line are the keeper's, under its mu.
type leasedJob struct {
	id     leaseID
	keeper *leaseKeeper
	cancel context.CancelCauseFunc // Ends the job's work.
	speed  func() (float64, bool)  // The job's measured speed, for GET /Sessions; nil for a subtitle job.
	// lost says why the job is no longer this worker's, asking on q.
	lost func(ctx context.Context, q queryer) string
	// Beats gets a value after each renewal the job's heartbeat sent: true when
	// it was answered. It is for the job's own periodic work.
	Beats chan bool

	stale    *time.Timer // Fires the cut; only an answered renewal puts it off.
	cutAt    time.Time   // When it fires.
	over     bool        // Cut, lost or stopped: no renewal is sent for it.
	turnedAt time.Time   // When the job's work last came round, from when it was kept; zero for a subtitle job.
	// unanswered says the job's last renewal was not answered, so a fence is not either.
	unanswered bool
}

// fenceAsk is a job's wait for a renewal sent after asked.
type fenceAsk struct {
	id     leaseID
	asked  time.Time
	answer chan fenceAnswer
}

type fenceAnswer struct {
	until   time.Time // When the lease ends at the soonest, if it was renewed.
	lost    string    // Why the job is not the worker's, if it was not.
	waiting bool      // The renewal was not answered.
}

func (w *Worker) keeper() *leaseKeeper {
	w.keeperOnce.Do(func() {
		w.leases = &leaseKeeper{w: w, jobs: map[leaseID]*leasedJob{}, wake: make(chan struct{}, 1)}
	})
	return w.leases
}

// keeperConfig connects the keeper: KeeperConfig, which defaults to the pool's own.
func (w *Worker) keeperConfig() *pgx.ConnConfig {
	if w.KeeperConfig != nil {
		return w.KeeperConfig
	}
	cfg := w.Pool.Config().ConnConfig // A copy.
	cfg.Tracer = nil                  // A renewal that cannot be sent is not Postgres being away for the pool's readers.
	return cfg
}

// keepJobLease keeps the lease of a transcode job and returns the context of its
// work, which the keeper cancels when the job is no longer to be run. A caller
// with a wake channel also renews whenever it fires. The caller stops the job
// when its work ends.
func (w *Worker) keepJobLease(ctx context.Context, job claimed, wake <-chan struct{}) (context.Context, *leasedJob) {
	return w.keeper().keep(ctx, &leasedJob{
		id:    leaseID{id: job.ID},
		speed: func() (float64, bool) { return w.measuredSpeed(job) },
		lost:  func(ctx context.Context, q queryer) string { return lostReason(ctx, q, job) },
	}, wake)
}

// keepSubtitleLease is keepJobLease for a subtitle or chapter job.
func (w *Worker) keepSubtitleLease(ctx context.Context, job subtitleJob) (context.Context, *leasedJob) {
	return w.keeper().keep(ctx, &leasedJob{
		id:   leaseID{key: job.Key},
		lost: func(context.Context, queryer) string { return "the subtitle job's lease was lost" },
	}, nil)
}

func (k *leaseKeeper) keep(ctx context.Context, job *leasedJob, wake <-chan struct{}) (context.Context, *leasedJob) {
	work, cancel := context.WithCancelCause(ctx)
	job.keeper, job.cancel, job.Beats = k, cancel, make(chan bool, 1)
	k.mu.Lock()
	if job.id.key == "" {
		// A transcode job's setup (the source, the keyframes, the image cues) is
		// calls too, and one that hangs is given up like a loop that does not turn.
		// A subtitle job is one long extraction, which has no loop to turn.
		job.turnedAt = time.Now()
	}
	job.cutAt = time.Now().Add(k.cutAfter())
	job.stale = time.AfterFunc(time.Until(job.cutAt), func() { k.giveUp(job, errUnrenewed) })
	k.jobs[job.id] = job
	k.due[job.id.table()] = true
	if !k.running {
		k.running = true
		go k.run()
	}
	k.mu.Unlock()
	k.poke()
	if wake != nil {
		go func() {
			for {
				select {
				case <-wake:
					k.mu.Lock()
					k.due[transcodeTable] = true
					k.mu.Unlock()
					k.poke()
				case <-work.Done():
					return
				}
			}
		}()
	}
	return work, job
}

// stop ends the job's keeping and its work.
func (j *leasedJob) stop() {
	k := j.keeper
	k.mu.Lock()
	j.over = true
	j.stale.Stop()
	delete(k.jobs, j.id)
	k.mu.Unlock()
	j.cancel(nil)
	k.poke()
}

// turn says the job's work has come round. A transcode job that does not turn for
// a lease, from when it was kept, is given up: it is stuck in a call that does
// not return.
func (j *leasedJob) turn() {
	j.keeper.mu.Lock()
	j.turnedAt = time.Now()
	j.keeper.mu.Unlock()
}

// giveUp ends the job's work and stops renewing its lease.
func (k *leaseKeeper) giveUp(j *leasedJob, why error) {
	k.mu.Lock()
	j.over = true
	k.mu.Unlock()
	j.cancel(fmt.Errorf("%w: %w", errInterrupted, why))
}

func (k *leaseKeeper) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// cutAfter is how long after a renewal was sent the job is given up: the table's
// cutMargin before the lease can lapse, which is what ending an ffmpeg takes with
// room to spare. Until then the job is the worker's and nobody may take it, so it
// is kept for as long as a renewal can still be answered. A lease no longer than
// the margin is cut at its end.
func (k *leaseKeeper) cutAfter() time.Duration {
	if d := k.w.lease() - liveTiming("cutMargin"); d > 0 {
		return d
	}
	return k.w.lease()
}

// bound is how long one renewal may take; a connection made for it may take longer.
func (k *leaseKeeper) bound() time.Duration { return k.w.heartbeat() / 2 }

// retryEvery is how long after a failed renewal, the first retry being at once,
// the next is tried.
func (k *leaseKeeper) retryEvery() time.Duration { return k.w.heartbeat() / 10 }

// leaseTable is what run knows of one job table's renewals.
type leaseTable struct {
	next     time.Time // The next heartbeat's renewal.
	backoff  time.Time // No attempt before this, after a failure.
	failures int       // In a row.
}

// run is the keeper's one goroutine, and the only user of its connection.
func (k *leaseKeeper) run() {
	var conn *pgx.Conn
	defer func() { k.closeConn(&conn) }()
	var tables [2]leaseTable
	for {
		if conn != nil && conn.PgConn().CheckConn() != nil {
			// Severed while idle. A connection takes several round trips, so it is made
			// again now, not at the next heartbeat, which may then be past the cut.
			k.closeConn(&conn)
			k.mu.Lock()
			k.due = [2]bool{true, true}
			k.mu.Unlock()
		}
		now := time.Now()
		var kept [2][]*leasedJob
		var cut, stalled []*leasedJob
		var deadline time.Time // The soonest a kept job's work is given up for not coming round.
		k.mu.Lock()
		if len(k.jobs) == 0 {
			k.running = false
			k.mu.Unlock()
			return
		}
		for _, j := range k.jobs {
			switch {
			case j.over:
			case !now.Before(j.cutAt):
				// The worker was stopped (a frozen host) past the point where the lease may
				// be taken over: no renewal now can be trusted to find it still ours.
				cut = append(cut, j)
			case !j.turnedAt.IsZero() && !now.Before(j.turnedAt.Add(k.w.lease())):
				stalled = append(stalled, j)
			default:
				kept[j.id.table()] = append(kept[j.id.table()], j)
				if at := j.turnedAt.Add(k.w.lease()); !j.turnedAt.IsZero() && (deadline.IsZero() || at.Before(deadline)) {
					deadline = at
				}
			}
		}
		due := k.due
		k.mu.Unlock()
		for _, j := range cut {
			k.giveUp(j, errUnrenewed)
		}
		for _, j := range stalled {
			// The keeper's connection answers though the job's work does not: the row
			// says so before the work is cancelled, not when the lease lapses.
			k.writeCancelled(conn, j, errStalled.Error())
			k.giveUp(j, errStalled)
		}

		var wakeAt time.Time
		soonest := func(at time.Time) {
			if wakeAt.IsZero() || at.Before(wakeAt) {
				wakeAt = at
			}
		}
		if !deadline.IsZero() {
			soonest(deadline)
		}
		for t := range tables {
			tb := &tables[t]
			if len(kept[t]) == 0 {
				*tb = leaseTable{}
				continue
			}
			// The heartbeat, or a job that came, renews every job of the table; a fence
			// alone renews the jobs that asked.
			heartbeat := !now.Before(tb.next) || due[t]
			renewing := kept[t]
			if !heartbeat {
				renewing = k.asking(kept[t])
			}
			if (heartbeat || len(renewing) > 0) && !now.Before(tb.backoff) {
				k.attempt(&conn, t, renewing, tb, heartbeat)
				now = time.Now()
			}
			// When the table next wants an attempt: its heartbeat, or now when
			// something asked for one while this one ran, and not before its backoff.
			at := tb.next
			k.mu.Lock()
			if k.due[t] {
				at = now
			}
			k.mu.Unlock()
			if len(k.asking(kept[t])) > 0 {
				at = now
			}
			if at.Before(tb.backoff) {
				at = tb.backoff
			}
			soonest(at)
		}
		if len(kept[0])+len(kept[1]) == 0 {
			k.closeConn(&conn)
		}
		if wakeAt.IsZero() {
			<-k.wake
			continue
		}
		if conn != nil && time.Until(wakeAt) > k.retryEvery() {
			wakeAt = time.Now().Add(k.retryEvery()) // To see a severed connection.
		}
		timer := time.NewTimer(time.Until(wakeAt))
		select {
		case <-timer.C:
		case <-k.wake:
			timer.Stop()
		}
	}
}

// asking is the jobs among these that wait for a fence's renewal.
func (k *leaseKeeper) asking(jobs []*leasedJob) []*leasedJob {
	k.mu.Lock()
	defer k.mu.Unlock()
	var asking []*leasedJob
	for _, j := range jobs {
		for _, a := range k.asks {
			if a.id == j.id {
				asking = append(asking, j)
				break
			}
		}
	}
	return asking
}

// attempt renews these jobs of one table and applies the answer. beat says the
// attempt is the heartbeat's, which renews every job of the table and which the
// jobs are told of; a renewal made only to answer a put's fence renews the jobs
// that asked, and is not told. A job Postgres answered is dealt with whether or
// not a later statement of the attempt failed; one it did not answer is left as
// it was, and its fence is not answered either, until a renewal of it is.
func (k *leaseKeeper) attempt(conn **pgx.Conn, table int, jobs []*leasedJob, tb *leaseTable, beat bool) {
	began := time.Now()
	if beat {
		k.mu.Lock()
		k.due[table] = false
		k.mu.Unlock()
	}
	sent, answers, err := k.renew(conn, table, jobs)
	if err != nil {
		tb.failures++
		tb.backoff = time.Now()
		if tb.failures > 1 {
			tb.backoff = tb.backoff.Add(k.retryEvery())
		}
		if tb.failures == 1 && k.w.Log != nil {
			k.w.Log.Warn("transcode heartbeat failed", "error", err)
		}
		if sent.IsZero() {
			sent = began
		}
		// Again at the interval, whoever asks.
		k.mu.Lock()
		k.due[table] = true
		k.mu.Unlock()
	} else {
		tb.failures = 0
		if beat {
			tb.next = sent.Add(k.w.heartbeat())
		}
	}
	lost := map[leaseID]string{}
	for _, j := range jobs {
		answer, answered := answers[j.id]
		k.mu.Lock()
		over := j.over
		j.unanswered = !answered
		if answered && answer.ours && !over {
			j.cutAt = sent.Add(k.cutAfter())
			j.stale.Reset(time.Until(j.cutAt))
		}
		k.mu.Unlock()
		switch {
		case over:
		case !answered:
			if beat {
				j.beat(false)
			}
		case !answer.ours:
			reason := "the job is no longer this worker's"
			if j.lost != nil && *conn != nil {
				bound, cancel := context.WithTimeout(context.Background(), k.bound())
				reason = j.lost(bound, *conn)
				cancel()
			}
			lost[j.id] = reason
			k.mu.Lock()
			j.over = true
			k.mu.Unlock()
			j.cancel(fmt.Errorf("%w: %s", errInterrupted, reason))
		case !answer.wanted:
			k.writeCancelled(*conn, j, "")
			k.mu.Lock()
			j.over = true
			k.mu.Unlock()
			lost[j.id] = "no viewer is watching"
			j.cancel(errNoViewer)
		case beat:
			j.beat(true)
		}
	}
	if table != transcodeTable {
		return
	}
	// The asks made before this attempt, by the jobs in it, are answered by it: a
	// renewal sent after the ask answers it; a job that is not the worker's, or was
	// just cut, is lost; a job whose renewal was not answered is waiting. The asks
	// made while it ran wait for the next.
	inAttempt := map[leaseID]bool{}
	for _, j := range jobs {
		inAttempt[j.id] = true
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	rest := k.asks[:0]
	for _, a := range k.asks {
		if !a.asked.Before(sent) || !inAttempt[a.id] {
			rest = append(rest, a)
			continue
		}
		answer, answered := answers[a.id]
		j := k.jobs[a.id]
		switch {
		case answered && answer.ours && j != nil && !j.over:
			a.answer <- fenceAnswer{until: sent.Add(k.w.lease())}
		case !answered && j != nil && !j.over:
			a.answer <- fenceAnswer{waiting: true}
		case lost[a.id] != "":
			a.answer <- fenceAnswer{lost: lost[a.id]}
		default:
			a.answer <- fenceAnswer{lost: "the job is no longer kept"}
		}
	}
	k.asks = rest
}

func (j *leasedJob) beat(answered bool) {
	select {
	case j.Beats <- answered:
	default:
	}
}

// renewal is what Postgres answered for one job: that it is still the worker's,
// and whether a viewer wants it.
type renewal struct{ ours, wanted bool }

// renew sends the statements that renew the jobs of a table on the keeper's
// connection, made when there is none, and gives when the first was sent and
// what Postgres answered for each job it did. Each statement is its own
// transaction, so what it found is what Postgres has kept; the answers stand
// though a later statement failed. The first statement waits for no row: a job
// whose row another session holds at that moment, or which is no longer the
// worker's, is left out of it, and is asked again alone, waiting for the row,
// which holds none while it waits.
func (k *leaseKeeper) renew(conn **pgx.Conn, table int, jobs []*leasedJob) (sent time.Time, answers map[leaseID]renewal, err error) {
	if *conn == nil || (*conn).IsClosed() {
		// A connection takes several round trips: its bound is longer than a
		// renewal's, and the cut does not wait for it.
		ctx, cancel := context.WithTimeout(context.Background(), k.w.lease()/2)
		c, err := pgx.ConnectConfig(ctx, k.w.keeperConfig())
		cancel()
		if err != nil {
			return time.Time{}, nil, err
		}
		*conn = c
	}
	answers = map[leaseID]renewal{}
	ctx, cancel := context.WithTimeout(context.Background(), k.bound())
	sent = time.Now()
	found, err := k.renewTable(ctx, *conn, table, jobs, true)
	cancel()
	for id, wanted := range found {
		answers[id] = renewal{ours: true, wanted: wanted}
	}
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), k.bound())
		for _, j := range jobs {
			if _, ok := answers[j.id]; ok {
				continue
			}
			var one map[leaseID]bool
			if one, err = k.renewTable(ctx, *conn, table, []*leasedJob{j}, false); err != nil {
				break
			}
			wanted, ours := one[j.id]
			answers[j.id] = renewal{ours: ours, wanted: wanted}
		}
		cancel()
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (*conn).IsClosed() {
			k.closeConn(conn) // One that gave up is closed, or in an unknown state.
		}
	}
	return sent, answers, err
}

// renewTable renews the jobs, leaving out the rows another session holds when
// skipLocked is true, and gives, for each it renewed, whether a viewer wants it.
func (k *leaseKeeper) renewTable(ctx context.Context, q queryer, table int, jobs []*leasedJob, skipLocked bool) (map[leaseID]bool, error) {
	if table == transcodeTable {
		return k.renewTranscodes(ctx, q, jobs, skipLocked)
	}
	return k.renewSubtitles(ctx, q, jobs, skipLocked)
}

// lockClause is how the renewal's subquery locks the rows it updates.
func lockClause(skipLocked bool) string {
	if skipLocked {
		return "FOR UPDATE SKIP LOCKED"
	}
	return "FOR UPDATE"
}

// writeCancelled records the job as cancelled on the keeper's own connection,
// before its work is cancelled: the row is written before the ffmpeg is ended
// (DOMAINS 25), and the connection is not the pool, which is what may not answer.
// It gives up after a renewal's bound, and what it did not write is the job's own
// end's to try, then the sweep's.
func (k *leaseKeeper) writeCancelled(conn *pgx.Conn, j *leasedJob, reason string) {
	if conn == nil || conn.IsClosed() || j.id.key != "" {
		return
	}
	cancelRunning(context.Background(), conn, k.bound(), j.id.id, reason)
}

func (k *leaseKeeper) closeConn(conn **pgx.Conn) {
	if *conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), k.bound())
		(*conn).Close(ctx) //nolint:errcheck
		cancel()
		*conn = nil
	}
}

// renewTranscodes extends transcode jobs' leases and stores their measured speed
// for GET /Sessions; a paused job keeps the speed it had, since it is idle by
// design. It gives, for each job still the worker's, whether a viewer has asked
// for its spec lately.
func (k *leaseKeeper) renewTranscodes(ctx context.Context, q queryer, jobs []*leasedJob, skipLocked bool) (map[leaseID]bool, error) {
	w := k.w
	ids, speeds := make([]int64, len(jobs)), make([]float64, len(jobs))
	for i, job := range jobs {
		ids[i], speeds[i] = job.id.id, -1 // Negative: no speed to store.
		if job.speed != nil {
			if value, ok := job.speed(); ok {
				speeds[i] = value
			}
		}
	}
	rows, err := q.Query(ctx, `UPDATE transcode_jobs j SET lease_expires_at = now() + make_interval(secs => $3),
			encode_speed = CASE WHEN s.speed >= 0 THEN s.speed ELSE j.encode_speed END
		FROM (SELECT id FROM transcode_jobs
			WHERE id = ANY($1::bigint[]) AND worker = $2 AND state = 'running' AND lease_expires_at > now() AND `+registeredAs("$2")+`
			`+lockClause(skipLocked)+`) mine, unnest($1::bigint[], $5::float8[]) AS s(id, speed)
		WHERE j.id = mine.id AND s.id = mine.id
		RETURNING j.id, EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = j.spec_hash AND NOT v.stopped
			AND v.last_seen_at > now() - make_interval(secs => $4))`,
		pgx.QueryExecModeExec, ids, w.ID, w.lease().Seconds(), w.idle().Seconds(), speeds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[leaseID]bool{}
	for rows.Next() {
		var id int64
		var wanted bool
		if err := rows.Scan(&id, &wanted); err != nil {
			return nil, err
		}
		found[leaseID{id: id}] = wanted
	}
	return found, rows.Err()
}

// renewSubtitles extends subtitle jobs' leases; nobody has to want them.
func (k *leaseKeeper) renewSubtitles(ctx context.Context, q queryer, jobs []*leasedJob, skipLocked bool) (map[leaseID]bool, error) {
	keys := make([]string, len(jobs))
	for i, job := range jobs {
		keys[i] = job.id.key
	}
	rows, err := q.Query(ctx, `UPDATE subtitle_jobs s SET lease_expires_at = now() + make_interval(secs => $3)
		FROM (SELECT key FROM subtitle_jobs WHERE key = ANY($1::text[]) AND worker = $2 AND state = 'running' AND `+registeredAs("$2")+`
			`+lockClause(skipLocked)+`) mine WHERE s.key = mine.key RETURNING s.key`,
		pgx.QueryExecModeExec, keys, k.w.ID, k.w.lease().Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[leaseID]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		found[leaseID{key: key}] = true
	}
	return found, rows.Err()
}

// fence is the check before an upload's puts: a renewal of the job's lease sent
// after this call, which Postgres has answered. It gives when the lease ends at
// the soonest, and each put compares the clock with that. A job that is no longer
// the worker's is interrupted; no answer is errWaiting, the segment staying where
// it is: at once when the keeper's last attempt for the job was not answered, so
// that the job's loop goes on, and else after half a lease at the latest. Jobs
// that ask while a renewal is in flight share the next one.
func (k *leaseKeeper) fence(ctx context.Context, id leaseID) (time.Time, error) {
	ask := &fenceAsk{id: id, asked: time.Now(), answer: make(chan fenceAnswer, 1)}
	k.mu.Lock()
	if j := k.jobs[id]; j == nil || j.over {
		k.mu.Unlock()
		return time.Time{}, fmt.Errorf("%w: the job's lease is no longer kept", errInterrupted)
	} else if j.unanswered {
		k.mu.Unlock()
		return time.Time{}, fmt.Errorf("%w: checking the job is still ours: the last renewal was not answered", errWaiting)
	}
	k.asks = append(k.asks, ask)
	k.mu.Unlock()
	k.poke()
	wait, cancel := context.WithTimeout(ctx, k.w.lease()/2)
	defer cancel()
	select {
	case a := <-ask.answer:
		if a.lost != "" {
			return time.Time{}, fmt.Errorf("%w: %s", errInterrupted, a.lost)
		}
		if a.waiting {
			return time.Time{}, fmt.Errorf("%w: checking the job is still ours: the renewal was not answered", errWaiting)
		}
		return a.until, nil
	case <-wait.Done():
	}
	k.mu.Lock()
	for i, a := range k.asks {
		if a == ask {
			k.asks = append(k.asks[:i], k.asks[i+1:]...)
			break
		}
	}
	k.mu.Unlock()
	if ctx.Err() != nil {
		return time.Time{}, fmt.Errorf("%w: checking the job is still ours: %v", errInterrupted, ctx.Err())
	}
	// Nothing is stored until the check is answered (invariant 37); the segment
	// waits where it is, and the keeper gives the job up if the lease runs out.
	return time.Time{}, fmt.Errorf("%w: checking the job is still ours: no renewal was answered in time", errWaiting)
}

type turnKey struct{}

// withTurn gives the waits under ctx the job's turn, which they call each time
// they come round, for a wait on a subtitle job is long and is no stall.
func withTurn(ctx context.Context, j *leasedJob) context.Context {
	return context.WithValue(ctx, turnKey{}, j.turn)
}

func turnFrom(ctx context.Context) {
	if turn, ok := ctx.Value(turnKey{}).(func()); ok {
		turn()
	}
}

// turning counts the bytes it reads as the job's work coming round, for a copy
// that is slow but moving is no stall, and one that has stopped is.
func turning(ctx context.Context, r io.Reader) io.Reader {
	return turningReader{ctx, r}
}

type turningReader struct {
	ctx context.Context
	r   io.Reader
}

func (t turningReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		turnFrom(t.ctx)
	}
	return n, err
}

// turnWhile counts the time until stop is called as the job's work coming round,
// for a step that has a bound of its own (a probe's timeout) and so is no hang.
func (w *Worker) turnWhile(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(w.heartbeat())
		defer ticker.Stop()
		for {
			turnFrom(ctx)
			select {
			case <-ticker.C:
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// cause is what a job's work returns once the keeper cut it: the keeper's cause
// stands in for whatever error the cancelled call gave.
func cause(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		if c := context.Cause(ctx); errors.Is(c, errInterrupted) {
			return c
		}
	}
	return err
}

// cancelRunning records that a running job is ended, on the connection or pool q
// (not the job's own context, which may be why it is ended). It gives up after
// bound: the job's slot is given back whatever Postgres does, and a cancel that
// was not written is the sweep's, once the lease runs out. It is written before
// the job's ffmpeg is ended (DOMAINS 25). A job nobody watches has no reason to
// record.
func cancelRunning(ctx context.Context, q queryer, bound time.Duration, id int64, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	q.Exec(ctx, "UPDATE transcode_jobs SET state = 'cancelled', error = NULLIF($2, '') WHERE id = $1 AND state = 'running'", id, reason) //nolint:errcheck
}

// cancelUnwanted records that nobody watches the job, giving up after half a heartbeat.
func (w *Worker) cancelUnwanted(ctx context.Context, job claimed) {
	cancelRunning(ctx, w.Pool, w.heartbeat()/2, job.ID, "")
}
