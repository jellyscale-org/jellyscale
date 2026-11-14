package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

const (
	// A job whose next segment is within this many of the one asked for is
	// worth waiting on; further ahead, a restart gets there sooner.
	catchUpSegments = 3
	pollInterval    = 250 * time.Millisecond
	maxRestarts     = 3
)

// Errors a waiting request can end with.
var (
	// ErrStopped: the viewer was told to stop, or isn't the one who started it.
	ErrStopped = errors.New("playback session ended")
	// ErrNoWorker: a job sat in the queue and no worker is running.
	ErrNoWorker = errors.New("no transcode worker is running")
	// ErrBusy: a job sat in the queue and every worker slot was taken.
	ErrBusy = errors.New("all transcode slots are busy")
	// ErrOtherVersion: a job sat in the queue and only workers of another output
	// version run, which may not take it.
	ErrOtherVersion = errors.New("no transcode worker of the job's output version is running")
	// ErrTimeout: the segment didn't appear in time.
	ErrTimeout = errors.New("timed out waiting for the segment")
)

// FailedError carries why a transcode job failed.
type FailedError struct{ Reason string }

func (e FailedError) Error() string { return "transcode failed: " + e.Reason }

// Viewer is one playback session watching a spec.
type Viewer struct {
	PlaySessionID, DeviceID, UserID string
	// Item is the compact item ID, for a Stopped report that has no session.
	Item string
}

// Jobs is the stream replica's side of transcoding: it records who watches,
// finds or starts the job that will make a segment, and waits for it.
type Jobs struct {
	Pool     *pgxpool.Pool
	Segments store.Store
	Hub      *Hub
	// QueueTimeout is how long a job may wait for a worker. WaitTimeout bounds
	// a request. Zero means the defaults, 6 and 90 seconds.
	QueueTimeout, WaitTimeout time.Duration

	Log *slog.Logger // Optional.

}

// queueTimeout is the default for a job's wait in the queue, which workers
// and replicas must agree on: older jobs are not started and do not count as
// live.
const queueTimeout = 6 * time.Second

// expireQueued cancels jobs that waited longer than timeout for a worker. A
// viewer who gave up on one must not keep PlaybackInfo offering a transcode
// nobody runs, nor get a stale job started when a worker returns.
func expireQueued(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	_, err := pool.Exec(ctx, `UPDATE transcode_jobs SET state = 'cancelled', error = 'no worker took the job'
		WHERE state = 'queued' AND created_at < now() - make_interval(secs => $1)`, timeout.Seconds())
	return err
}

// StoreMismatch says why the running processes can't share segments, or
// returns "" if they can. Every process records its roles and transcodeStore in
// transcode_nodes, and this compares them, so a missing store and a different
// one are found the same way. A URI never holds credentials (invariant 12), so
// it can be named. A process that is only a worker and has no store just extracts
// subtitles: it shares no segments, so it doesn't count.
func StoreMismatch(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT ON (store) roles, store FROM transcode_nodes
		WHERE seen_at > now() - make_interval(secs => $1) AND NOT (store = '' AND roles = 'worker')
		ORDER BY store, id LIMIT 2`, workerAlive.Seconds())
	if err != nil {
		return "", err
	}
	type node struct{ Roles, Store string }
	nodes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[node])
	if err != nil || len(nodes) < 2 {
		return "", err
	}
	show := func(n node) string {
		if n.Store == "" {
			return n.Roles + " has no transcodeStore"
		}
		return fmt.Sprintf("%s has transcodeStore %q", n.Roles, n.Store)
	}
	return fmt.Sprintf("transcodeStore differs between roles: %s, but %s; it must be the same on every role", show(nodes[0]), show(nodes[1])), nil
}

// LiveJob reports whether a job of the spec is running, or queued and not yet
// past queueTimeout.
func LiveJob(ctx context.Context, pool *pgxpool.Pool, hash string) (live bool, err error) {
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcode_jobs WHERE spec_hash = $1
		AND ((state = 'queued' AND created_at > now() - make_interval(secs => $2))
			OR (state = 'running' AND lease_expires_at > now())))`, hash, queueTimeout.Seconds()).Scan(&live)
	return live, err
}

// Register records this process in transcode_nodes until ctx ends, and then
// removes it, so a graceful stop is seen at once. slots is 0 unless it runs a
// worker.
func Register(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, id, roles, store string, slots int, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if _, err := pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, output_version) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET seen_at = now()`, id, roles, store, slots, playback.OutputVersion); err != nil && ctx.Err() == nil {
			log.Warn("registering with the transcode nodes failed", "error", err)
		}
		select {
		case <-ctx.Done():
			gone, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			pool.Exec(gone, "DELETE FROM transcode_nodes WHERE id = $1", id) //nolint:errcheck
			return
		case <-ticker.C:
		}
	}
}

// SegmentKey names a segment in the transcode store. n is -1 for the init
// segment.
func SegmentKey(hash string, n int) string {
	if n < 0 {
		return "segments/" + hash + "/init.mp4"
	}
	return fmt.Sprintf("segments/%s/%d.mp4", hash, n)
}

// Watch records that the viewer asked for something on the spec. It returns
// ErrStopped if the viewer was stopped, so a request still in flight when
// playback ended can't start work again. segment is the one asked for, or
// negative for a request that names none (a playlist, the init segment, or a
// wait that goes on); the worker bounds a job by the segment each viewer asked
// for last, so it rides on the write that was made anyway.
func (j *Jobs) Watch(ctx context.Context, v Viewer, hash string, segment int) error {
	var tag pgconn.CommandTag
	err := db.Retry(ctx, j.Pool, func() (err error) { tag, err = j.watch(ctx, v, hash, segment); return })
	if err == nil && tag.RowsAffected() == 0 {
		return refusedAsStopped()
	}
	return err
}

func (j *Jobs) watch(ctx context.Context, v Viewer, hash string, segment int) (pgconn.CommandTag, error) {
	return j.Pool.Exec(ctx, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, item_id, segment)
		VALUES ($1, $2, $3::uuid, $4, $5, $6)
		ON CONFLICT (play_session_id, spec_hash) DO UPDATE SET last_seen_at = now(),
			segment = CASE WHEN EXCLUDED.segment >= 0 THEN EXCLUDED.segment ELSE transcode_viewers.segment END
			WHERE NOT transcode_viewers.stopped AND transcode_viewers.user_id = EXCLUDED.user_id`,
		v.PlaySessionID, hash, v.UserID, v.DeviceID, v.Item, segment)
}

// defaultIdle is how long a spec may go without a request before its jobs stop.
const defaultIdle = 60 * time.Second

// workerAlive is how recently a worker must have reported to count.
const workerAlive = 10 * time.Second

// failureMemory is how long a failed transcode keeps PlaybackInfo from
// offering the same one again.
const failureMemory = 30 * time.Second

// Unavailable says why a transcode of the spec can't be started right now, or
// returns "" if it can. PlaybackInfo asks before offering a transcode: a client
// given a stream that then fails asks again forever, while one refused shows an
// error.
//
// A viewer's own jobs don't count as busy: jellyfin-web asks for the new stream
// before it deletes the old one, which is about to free its slot.
func (j *Jobs) Unavailable(ctx context.Context, hash, userID, deviceID string) (reason string, err error) {
	// Only workers forget dead workers (lookForTheDead): a replica holds no
	// liveness lock, so it may have missed an outage altogether, and would
	// forget live workers still taking their lock again. It counts a worker
	// only while its lock is held, and writes nothing.
	if err := expireQueued(ctx, j.Pool, orDefault(j.QueueTimeout, queueTimeout)); err != nil {
		return "", err
	}
	if live, err := LiveJob(ctx, j.Pool, hash); err != nil || live {
		return "", err // A live job is joined, whatever failed before.
	}
	// A refusal that rests on a worker whose lock is gone is looked at again for
	// a grace: it is either taking its lock again, or never will.
	deadline := time.Now().Add(reapGrace)
	for {
		var workers, versionWorkers, slots, taken, lockless int
		var failure *string
		// Only the slots of this replica's output version count: a worker of
		// another version may not take the job. With no worker of the version
		// at all the job is offered, and waits queueTimeout for one to start.
		err = j.Pool.QueryRow(ctx, `WITH counted AS (
				SELECT id, slots, output_version FROM transcode_nodes WHERE seen_at > now() - make_interval(secs => $2)
					AND (NOT locked OR `+lockHeld("id")+`))
			SELECT
				(SELECT count(*) FROM counted WHERE slots > 0),
				(SELECT count(*) FROM counted WHERE slots > 0 AND output_version = $6),
				(SELECT COALESCE(sum(slots), 0) FROM counted WHERE output_version = $6),
				(SELECT count(*) FROM transcode_jobs WHERE output_version = $6 AND (state = 'queued' OR (state = 'running' AND lease_expires_at > now()
						AND worker IN (SELECT id FROM counted WHERE output_version = $6)))
					AND NOT helper AND owner NOT IN (SELECT play_session_id FROM transcode_viewers WHERE user_id::text = $4 AND device_id = $5)),
				(SELECT error FROM transcode_jobs WHERE spec_hash = $1 AND state = 'failed' AND NOT helper
					AND ended_at > now() - make_interval(secs => $3) ORDER BY id DESC LIMIT 1),
				(SELECT count(*) FROM transcode_nodes WHERE slots > 0 AND seen_at > now() - make_interval(secs => $2)
					AND locked AND NOT `+lockHeld("transcode_nodes.id")+`)`,
			hash, workerAlive.Seconds(), failureMemory.Seconds(), userID, deviceID, playback.OutputVersion).Scan(&workers, &versionWorkers, &slots, &taken, &failure, &lockless)
		if err != nil {
			return "", err
		}
		if failure == nil && (workers == 0 || (versionWorkers > 0 && taken >= slots)) && lockless > 0 && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if reason, err := StoreMismatch(ctx, j.Pool); err != nil || reason != "" {
			return reason, err
		}
		switch {
		case failure != nil:
			return "the last attempt failed: " + *failure, nil
		case workers == 0:
			return "no transcode worker is running", nil
		case versionWorkers > 0 && taken >= slots:
			return fmt.Sprintf("all %d transcode slots are busy (output version %d)", slots, playback.OutputVersion), nil
		}
		return "", nil
	}
}

// Stop ends a user's viewing: every spec of the play session, or, when the
// client sent no session, of the device (and the item, if given). It is
// idempotent, as clients send it twice. A job nobody is left watching is
// cancelled now, so its slot is free and it stops at the next heartbeat. A
// viewer watches the job it last waited on, or any of the spec's if it has
// only read segments (job_id 0); a helper is watched by every viewer of the spec.
func (j *Jobs) Stop(ctx context.Context, userID, playSessionID, deviceID, item string) error {
	if playSessionID == "" && deviceID == "" {
		return nil
	}
	rows, err := j.Pool.Query(ctx, `UPDATE transcode_viewers SET stopped = true WHERE user_id = $1::uuid
		AND (($2 <> '' AND play_session_id = $2) OR ($2 = '' AND device_id = $3 AND ($4 = '' OR lower(item_id) = lower($4))))
		RETURNING spec_hash`, userID, playSessionID, deviceID, item)
	if err != nil {
		return err
	}
	specs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// Only the stopped viewers' specs: another spec's viewer may be between two jobs.
	_, err = j.Pool.Exec(ctx, `UPDATE transcode_jobs j SET state = 'cancelled' WHERE state IN ('queued', 'running')
		AND spec_hash = ANY($1)
		AND NOT EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = j.spec_hash AND NOT v.stopped
			AND (j.helper OR v.job_id IN (j.id, 0)))`, specs)
	return err
}

// readSegment reads a segment whole. Its size isn't known, so the Reader is
// told it is as large as can be, and ends at the object's end. An empty segment
// or one still cut after the Reader's resumes counts as not there yet, so the
// caller's wait reads it again.
func readSegment(ctx context.Context, s store.Store, key string) ([]byte, error) {
	body, err := io.ReadAll(store.NewReader(ctx, s, key, math.MaxInt64))
	if errors.Is(err, io.ErrUnexpectedEOF) || (err == nil && len(body) == 0) {
		return nil, store.ErrNotFound
	}
	return body, err
}

// Stored returns segment n of a spec (-1 is the init segment) if the store
// holds it, and store.ErrNotFound if not. It starts and joins no job and asks
// Postgres nothing.
func (j *Jobs) Stored(ctx context.Context, hash string, n int) ([]byte, error) {
	return readSegment(ctx, j.Segments, SegmentKey(hash, n))
}

// Segment returns segment n of a spec (-1 is the init segment): from the
// store if it's there, else once the active job that will make it has, else
// after starting a job at n. It gives up with an error rather than hang.
// Any job's first segment brings the init segment with it, so a request for
// the init joins a live job, or starts one at startAt, where the viewer
// starts.
func (j *Jobs) Segment(ctx context.Context, spec playback.Spec, hash string, viewer Viewer, n, startAt int) ([]byte, error) {
	began := time.Now()
	var source string
	body, err := j.segment(ctx, spec, hash, viewer, n, startAt, &source)
	observeSegment(began, spec.Copy, source, err)
	return body, err
}

// segment is Segment. source says where the segment came from: the store, a job
// that was running, or a job this request started; the first of them to apply.
func (j *Jobs) segment(ctx context.Context, spec playback.Spec, hash string, viewer Viewer, n, startAt int, source *string) ([]byte, error) {
	wake, unsubscribe := j.Hub.Subscribe(segmentHint(hash))
	defer unsubscribe()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(orDefault(j.WaitTimeout, 90*time.Second))
	defer deadline.Stop()
	// The wait is for a worker and the store, not for Postgres: time it was
	// away is added to the deadline, as it is to the queue's age.
	awayAtStart, extended := db.Of(j.Pool).Total(), time.Duration(0)

	var job int64
	starts, unreadable := 0, 0
	var awayAtJob time.Duration // How long Postgres had been away when the job was found.
	watched := time.Now()
	for {
		body, err := readSegment(ctx, j.Segments, SegmentKey(hash, n))
		if err == nil {
			if *source == "" {
				*source = "store"
			}
			return body, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if job == 0 {
			if err := j.Watch(ctx, viewer, hash, -1); err != nil {
				return nil, err
			}
			if starts++; starts > maxRestarts+1 {
				return nil, FailedError{"jobs kept ending without the segment"}
			}
			start := n
			if n < 0 {
				start = startAt
			}
			var created bool
			err = db.Retry(ctx, j.Pool, func() (err error) {
				job, created, err = j.ensure(ctx, spec, hash, viewer.PlaySessionID, n, start)
				return
			})
			if err != nil {
				return nil, err
			}
			awayAtJob = db.Of(j.Pool).Total()
			if *source == "" {
				*source = "waited"
				if created {
					*source = "started"
				}
			}
		} else {
			var state jobState
			err := db.Retry(ctx, j.Pool, func() (err error) { state, err = j.state(ctx, job); return })
			if err != nil {
				return nil, err
			}
			// A job waited for a worker only while Postgres was there to take it.
			state.Age -= db.Of(j.Pool).Total() - awayAtJob
			if err := db.Retry(ctx, j.Pool, func() error { return j.queuedTooLong(ctx, job, state) }); err != nil {
				return nil, err
			}
			switch {
			case state.State == "failed":
				return nil, FailedError{state.Error}
			case state.State == "done" && state.covers(n):
				// The worker says it stored this segment and we can't find it.
				// Look once more, in case it landed since we last looked.
				if body, err := readSegment(ctx, j.Segments, SegmentKey(hash, n)); err == nil {
					return body, nil
				}
				if unreadable++; unreadable >= 2 {
					reason := fmt.Sprintf("a worker stored segment %d but this replica cannot read it: transcodeStore must be the same on every role, and a directory shared by them", n)
					j.Pool.Exec(ctx, "UPDATE transcode_jobs SET state = 'failed', error = $2, ended_at = now() WHERE id = $1", job, reason) //nolint:errcheck
					return nil, FailedError{reason}
				}
				job = 0
				continue
			case state.State == "done" || state.State == "cancelled" || state.Dead:
				job = 0 // Look in the store once more, then start again.
				continue
			}
		}
		// A viewer waiting on a slow start is still watching, and the job
		// must not be stopped for want of requests.
		if time.Since(watched) > time.Second {
			if err := j.Watch(ctx, viewer, hash, -1); err != nil {
				return nil, err
			}
			watched = time.Now()
		}
		select {
		case <-wake:
		case <-ticker.C:
		case <-deadline.C:
			away := db.Of(j.Pool).Total() - awayAtStart
			if away <= extended {
				return nil, ErrTimeout
			}
			deadline.Reset(away - extended)
			extended = away
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// queuedTooLong cancels a queued job and says why once it has waited longer
// than the queue timeout; the player gives up on a request after 10 seconds, so
// it must hear before then. Until then the job waits, worker or not, so a
// replacement for a restarting worker can take it. It returns nil if it can wait.
func (j *Jobs) queuedTooLong(ctx context.Context, job int64, state jobState) error {
	if state.State != "queued" || state.Age <= orDefault(j.QueueTimeout, queueTimeout) {
		return nil
	}
	// Cancelled, so a worker that turns up later starts nothing for a viewer who has been refused.
	if _, err := j.Pool.Exec(ctx, `UPDATE transcode_jobs SET state = 'cancelled', error = 'no worker took the job'
		WHERE id = $1 AND state = 'queued'`, job); err != nil {
		return err
	}
	var workers, versionWorkers int
	err := j.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE output_version = $2) FROM transcode_nodes
		WHERE slots > 0 AND seen_at > now() - make_interval(secs => $1)`,
		workerAlive.Seconds(), playback.OutputVersion).Scan(&workers, &versionWorkers)
	if err != nil {
		return err
	}
	switch {
	case workers == 0:
		return ErrNoWorker
	case versionWorkers == 0:
		return ErrOtherVersion
	}
	return ErrBusy
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// ensure returns the active job that will produce segment n, starting one at
// start if there is none. The advisory lock makes check-then-insert atomic per
// spec, so concurrent requests for one missing segment start one job. A
// request replaces the same viewer's previous job, whether it joins another
// viewer's job or starts its own; that one stops at its next heartbeat. Other
// viewers' jobs, and any a viewer has lately used, are left running.
func (j *Jobs) ensure(ctx context.Context, spec playback.Spec, hash, owner string, n, start int) (int64, bool, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return 0, false, err
	}
	if err := expireQueued(ctx, j.Pool, orDefault(j.QueueTimeout, queueTimeout)); err != nil {
		return 0, false, err
	}
	var id int64
	var failure string
	created, preempted := false, false
	err = pgx.BeginFunc(ctx, j.Pool, func(tx pgx.Tx) error {
		// The helpers lock first, as insertHelper takes them, so that a free slot
		// is not counted twice.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('helpers', 0)), pg_advisory_xact_lock(hashtextextended($1, 0))", hash); err != nil {
			return err
		}
		// Locks the viewer's row, so a Stop that lands now waits and then cancels
		// what this inserts.
		var stopped bool
		err := tx.QueryRow(ctx, "SELECT stopped FROM transcode_viewers WHERE play_session_id = $1 AND spec_hash = $2 FOR SHARE", owner, hash).Scan(&stopped)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if stopped {
			return refusedAsStopped()
		}
		// The init segment (n < 0) comes with any job's first segment, so any
		// live job will do.
		err = tx.QueryRow(ctx, `SELECT id FROM transcode_jobs WHERE spec_hash = $1
			AND (state = 'queued' OR (state = 'running' AND lease_expires_at > now()))
			AND ($2 < 0 OR (start_segment <= $2 AND $2 < next_segment + $3))
			ORDER BY owner = $4 DESC, id LIMIT 1`, hash, n, catchUpSegments, owner).Scan(&id)
		found := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if found && n < 0 {
			return namesJob(ctx, tx, owner, hash, id)
		}
		if !found {
			// A client that asks again after a failure, as a browser does when it
			// abandons a request, must not start the same broken job every time.
			// A helper's failure is not remembered: the viewer's own job is tried,
			// and it fails too if the film is what is broken.
			err := tx.QueryRow(ctx, `SELECT error FROM transcode_jobs WHERE spec_hash = $1 AND state = 'failed' AND NOT helper
				AND ended_at > now() - make_interval(secs => $2) ORDER BY id DESC LIMIT 1`, hash, failureMemory.Seconds()).Scan(&failure)
			if err == nil {
				return FailedError{"the last attempt failed: " + failure}
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE transcode_jobs j SET state = 'cancelled' WHERE spec_hash = $1 AND id <> $3
			AND state IN ('queued', 'running') AND ((owner = $2 AND NOT EXISTS (SELECT 1 FROM transcode_viewers v
				WHERE v.job_id = j.id AND v.play_session_id <> $2 AND NOT v.stopped AND v.last_seen_at > now() - make_interval(secs => $4)))
				OR (state = 'running' AND lease_expires_at <= now()))`,
			hash, owner, id, defaultIdle.Seconds()); err != nil {
			return err
		}
		if found {
			return namesJob(ctx, tx, owner, hash, id)
		}
		created = true
		if preempted, err = preemptHelper(ctx, tx, orDefault(j.QueueTimeout, queueTimeout)); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version)
			VALUES ($1, $2, $3, $4, $4, $5) RETURNING id`, hash, raw, owner, start, playback.OutputVersion).Scan(&id)
		if err != nil {
			return err
		}
		return namesJob(ctx, tx, owner, hash, id)
	})
	if err != nil {
		return 0, false, err
	}
	if preempted {
		Notify(ctx, j.Pool, preemptHint)
	}
	if created {
		Notify(ctx, j.Pool, jobHint)
	}
	return id, created, nil
}

// namesJob makes the viewer's row name its job in the transaction that chose or
// inserted it, so a Stop never sees a new job that no viewer's row yet names.
func namesJob(ctx context.Context, tx pgx.Tx, owner, hash string, id int64) error {
	_, err := tx.Exec(ctx, "UPDATE transcode_viewers SET job_id = $3 WHERE play_session_id = $1 AND spec_hash = $2", owner, hash, id)
	return err
}

type jobState struct {
	State, Error string
	Start, Next  int
	Dead         bool // Running, but its lease ran out.
	Age          time.Duration
}

// covers reports whether the job went far enough to have stored segment n.
func (s jobState) covers(n int) bool {
	if n < 0 {
		return s.Next > s.Start
	}
	return s.Start <= n && n < s.Next
}

func (j *Jobs) state(ctx context.Context, id int64) (jobState, error) {
	var s jobState
	var age float64
	err := j.Pool.QueryRow(ctx, `SELECT state, COALESCE(error, ''), state = 'running' AND lease_expires_at <= now(),
			extract(epoch FROM now() - created_at), start_segment, next_segment FROM transcode_jobs WHERE id = $1`, id).
		Scan(&s.State, &s.Error, &s.Dead, &age, &s.Start, &s.Next)
	s.Age = time.Duration(age * float64(time.Second))
	return s, err
}
