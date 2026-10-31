package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// Worker claims jobs and runs ffmpeg for them.
type Worker struct {
	Pool     *pgxpool.Pool
	Items    *items.Repo
	Segments store.Store
	Hub      *Hub
	Log      *slog.Logger
	Media    func(uri string) (store.Store, error)

	// Scratch holds ffmpeg's output until it is uploaded.
	Scratch string
	Slots   int
	// Idle is how long a spec may go without a request before its jobs stop.
	Idle time.Duration
	// Retention is how long a spec's segments stay after its last request.
	Retention time.Duration
	// ID names this worker in transcode_nodes and on the jobs it claims. It
	// defaults to one made from the process ID.
	ID string
	// Lease and Heartbeat default to 10 and 3 seconds. A job waits QueueTimeout
	// (default 6) for a worker; later, nobody is waiting for it.
	Lease, Heartbeat, QueueTimeout time.Duration
	// Stall is how long ffmpeg may go without finishing a segment before the
	// job fails (default 30 seconds). The lease alone can't tell a hung ffmpeg
	// from a working one, and viewers would wait on it for good.
	Stall time.Duration
}

// Run claims and runs jobs until ctx ends, then waits for ffmpeg to be gone.
func (w *Worker) Run(ctx context.Context) {
	if w.ID == "" {
		w.ID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	slots := make(chan struct{}, max(w.Slots, 1))
	var running sync.WaitGroup
	defer running.Wait()
	go w.sweepLoop(ctx)

	wake, unsubscribe := w.Hub.Subscribe(jobHint)
	defer unsubscribe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case slots <- struct{}{}:
			job, ok, err := w.claim(ctx)
			if err != nil && ctx.Err() == nil {
				w.Log.Error("claiming a transcode job failed", "error", err)
			}
			if ok {
				running.Add(1)
				go func() {
					defer running.Done()
					defer func() { <-slots }()
					w.run(ctx, job)
				}()
				continue // There may be more queued.
			}
			<-slots
		case <-ctx.Done():
			return
		}
		select {
		case <-wake:
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

type claimed struct {
	ID    int64
	Hash  string
	Spec  playback.Spec
	Start int
}

func (w *Worker) lease() time.Duration     { return orDefault(w.Lease, 10*time.Second) }
func (w *Worker) heartbeat() time.Duration { return orDefault(w.Heartbeat, 3*time.Second) }
func (w *Worker) idle() time.Duration      { return orDefault(w.Idle, defaultIdle) }

// startup is extra lease for a claimed job, which is not heartbeating until
// ffmpeg is running.
const startup = 20 * time.Second

func (w *Worker) claim(ctx context.Context) (claimed, bool, error) {
	var job claimed
	var raw []byte
	err := w.Pool.QueryRow(ctx, `UPDATE transcode_jobs SET state = 'running', worker = $1,
			lease_expires_at = now() + make_interval(secs => $2)
		WHERE id = (SELECT id FROM transcode_jobs WHERE state = 'queued'
			AND created_at > now() - make_interval(secs => $3) ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id, spec_hash, spec, start_segment`, w.ID, (w.lease()+startup).Seconds(), orDefault(w.QueueTimeout, queueTimeout).Seconds()).Scan(&job.ID, &job.Hash, &raw, &job.Start)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, false, nil
	}
	if err != nil {
		return job, false, err
	}
	return job, true, json.Unmarshal(raw, &job.Spec)
}

// run runs one job and records how it ended. A job someone else already
// cancelled or replaced keeps that state.
func (w *Worker) run(ctx context.Context, job claimed) {
	// Pdeathsig fires when the thread that started ffmpeg exits, not the
	// process. Pinning this goroutine to its thread keeps that thread alive
	// until the job ends, and ends it then, since a goroutine that exits
	// while locked takes its thread with it.
	runtime.LockOSThread()
	log := w.Log.With("job", job.ID, "spec", job.Hash, "item", job.Spec.Item, "start", job.Start)
	log.Info("transcode started")
	began := time.Now()
	err := w.encode(ctx, job, log)
	state, reason := "done", ""
	switch {
	case err == nil:
		log.Info("transcode ended", "elapsed", time.Since(began).Round(time.Millisecond))
	case ctx.Err() != nil || errors.Is(err, errInterrupted):
		// Cut short, not failed: viewers restart it, and the spec isn't refused.
		state, reason = "cancelled", err.Error()
		log.Info("transcode interrupted", "reason", reason)
	default:
		state, reason = "failed", err.Error()
		log.Error("transcode failed", "error", err)
	}
	// A fresh context: ctx may be why we stopped.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := w.Pool.Exec(finish, `UPDATE transcode_jobs SET state = $2, error = NULLIF($3, ''), ended_at = now()
		WHERE id = $1 AND state = 'running' AND worker = $4`, job.ID, state, reason, w.ID); err != nil {
		log.Error("recording the end of a transcode job failed", "error", err)
	}
	Notify(finish, w.Pool, segmentHint(job.Hash)) // Waiters should see the new state now.
}

// errInterrupted marks a job that stopped before its work was done, because
// the worker was going away or no longer owned the job.
var errInterrupted = errors.New("interrupted")

// encode runs ffmpeg and uploads segments as they finish. It returns nil when
// the job is over for a good reason: the file ended, it caught up with stored
// segments, or it was stopped because nobody needs it. No ffmpeg outlives it.
func (w *Worker) encode(ctx context.Context, job claimed, log *slog.Logger) error {
	source, found, err := w.Items.Source(ctx, job.Spec.Item, job.Spec.Source)
	if err != nil {
		return err
	}
	if !found || source.ETag != job.Spec.ETag {
		return errors.New("the source file has changed or is gone")
	}
	streams, err := playback.ParseStreams(source.Streams)
	if err != nil {
		return err
	}
	ticks := playback.Source{RunTimeTicks: source.RunTimeTicks, Streams: streams}.Ticks()
	media, err := w.Media(source.StoreURI)
	if err != nil {
		return err
	}
	input, closeInput, err := store.Input(ctx, media, source.Key, source.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	// Absolute, because ffmpeg runs in the directory when it burns subtitles.
	scratch, err := filepath.Abs(w.Scratch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	// The first burn of a track reads the whole file, here, before ffmpeg
	// starts; it is cached for every job after.
	var ass string
	if job.Spec.Subtitle != nil {
		assets, err := w.Media(source.AssetStoreURI)
		if err != nil {
			return err
		}
		if ass, err = w.extractSubtitle(ctx, job, log, source, media, assets); err != nil {
			return fmt.Errorf("subtitle track %d: %w", *job.Spec.Subtitle, err)
		}
	}
	// Made after the read, which can outlast scratchIdle, so the sweep doesn't
	// remove it as abandoned.
	dir, err := os.MkdirTemp(scratch, "job-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cmd, err := Command(ctx, input, job.Spec, job.Start, dir, ass)
	if err != nil {
		return err
	}
	// Stopped by hand below so the log says why. CommandContext's kill is the
	// backstop, so no path leaves ffmpeg running.
	var stderr tail
	cmd.Stderr = &stderr
	subtitles.DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	next := job.Start
	stop := func(reason string) {
		log.Info("stopping ffmpeg", "reason", reason)
		cmd.Process.Kill() //nolint:errcheck
		<-exited
	}
	// final is set once ffmpeg has exited. The segment it flushes on the way
	// out is cut wherever its input ended, so it is only stored if it is the
	// film's real last one.
	upload := func(final bool) (caughtUp bool, err error) {
		for {
			name := filepath.Join(dir, strconv.Itoa(next)+".m4s")
			if _, err := os.Stat(name); err != nil {
				return false, nil
			}
			if _, more := os.Stat(filepath.Join(dir, strconv.Itoa(next+1)+".m4s")); final && more != nil {
				if err := checkLast(dir, next, ticks); err != nil {
					return false, err
				}
			}
			if err := w.upload(ctx, job, next, name); err != nil {
				return false, err
			}
			next++
			if w.stored(ctx, job.Hash, next) {
				return true, nil
			}
		}
	}

	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	beat := time.NewTicker(w.heartbeat())
	defer beat.Stop()
	lastBeat := time.Now()
	progressed, progressAt := next, time.Now()
	for {
		select {
		case err := <-exited:
			_, uploadErr := upload(true)
			if err != nil {
				return fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
			}
			if uploadErr != nil {
				return fmt.Errorf("%w: %s", uploadErr, stderr.String())
			}
			if next == job.Start {
				return fmt.Errorf("ffmpeg ended without a segment: %s", stderr.String())
			}
			return nil
		case <-poll.C:
			caughtUp, err := upload(false)
			if err != nil {
				stop("upload failed")
				return err
			}
			if caughtUp {
				stop("caught up with stored segments")
				return nil
			}
		case <-beat.C:
			alive, wanted, err := w.beat(ctx, job)
			switch {
			case err == nil:
				lastBeat = time.Now()
			case time.Since(lastBeat) > w.lease():
				// Nobody else knows we're alive, so another worker may own this now.
				stop("lease expired while the database was unreachable")
				return fmt.Errorf("%w: lease expired while the database was unreachable", errInterrupted)
			case ctx.Err() == nil:
				log.Warn("transcode heartbeat failed", "error", err)
			}
			if err == nil && !alive {
				reason := w.lostReason(ctx, job)
				stop(reason)
				return fmt.Errorf("%w: %s", errInterrupted, reason)
			}
			if next != progressed {
				progressed, progressAt = next, time.Now()
			} else if since := time.Since(progressAt); since > orDefault(w.Stall, 30*time.Second) {
				stop("ffmpeg has stalled")
				return fmt.Errorf("ffmpeg made no segment in %s: %s", since.Round(time.Second), stderr.String())
			}
			if err == nil && !wanted {
				w.Pool.Exec(context.WithoutCancel(ctx), "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1 AND state = 'running'", job.ID) //nolint:errcheck
				stop("no viewer is watching")
				return nil
			}
		case <-ctx.Done():
			stop("worker is shutting down")
			return fmt.Errorf("%w: worker shut down", errInterrupted)
		}
	}
}

// extractSubtitle returns the job's subtitle track as ASS. An extraction can
// take minutes, so it keeps the lease alive meanwhile, and stops when the job
// is lost or nobody wants it, so it doesn't hold a slot for a cancelled job.
func (w *Worker) extractSubtitle(ctx context.Context, job claimed, log *slog.Logger, source items.Source, media, assets store.Store) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		track subtitles.Track
		err   error
	}
	done := make(chan result, 1)
	go func() {
		track, err := subtitles.Cache{Pool: w.Pool, Log: log}.Track(ctx, source, media, assets, *job.Spec.Subtitle)
		done <- result{track, err}
	}()
	beat := time.NewTicker(w.heartbeat())
	defer beat.Stop()
	for {
		select {
		case r := <-done:
			return r.track.ASS, r.err
		case <-beat.C:
			alive, wanted, err := w.beat(ctx, job)
			switch {
			case err != nil:
				log.Warn("transcode heartbeat failed", "error", err)
			case !alive:
				cancel()
				<-done
				return "", fmt.Errorf("%w: %s", errInterrupted, w.lostReason(context.WithoutCancel(ctx), job))
			case !wanted:
				cancel()
				<-done
				w.Pool.Exec(context.WithoutCancel(ctx), "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1 AND state = 'running'", job.ID) //nolint:errcheck
				return "", fmt.Errorf("%w: no viewer is watching", errInterrupted)
			}
		}
	}
}

// lostReason says why the job is no longer ours to run: a seek replaced it, or
// something took the lease from us.
func (w *Worker) lostReason(ctx context.Context, job claimed) string {
	var state string
	w.Pool.QueryRow(ctx, "SELECT state FROM transcode_jobs WHERE id = $1", job.ID).Scan(&state) //nolint:errcheck
	if state == "cancelled" {
		return "a newer job replaced this one, or it was cancelled"
	}
	return "lease lost: the job is " + state
}

// checkLast rejects a final segment that ffmpeg cut short because its input
// ended early, which it does without an error when the read fails midway.
func checkLast(dir string, n int, runTimeTicks int64) error {
	if n < playback.Segments(runTimeTicks)-1 {
		return fmt.Errorf("the input ended early: ffmpeg stopped after segment %d of %d", n, playback.Segments(runTimeTicks))
	}
	if n > playback.Segments(runTimeTicks)-1 {
		return nil
	}
	text, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		return err
	}
	var length float64
	for _, line := range strings.Split(string(text), "\n") {
		if value, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			length, _ = strconv.ParseFloat(strings.TrimSuffix(value, ","), 64)
		}
	}
	if want := playback.SegmentLength(n, runTimeTicks); math.Abs(length-want) > 1.5 {
		return fmt.Errorf("the input ended early: the last segment is %.2fs, not %.2fs", length, want)
	}
	return nil
}

// beat extends the lease. alive is false when the job is no longer ours to
// run; wanted is false when no viewer has asked for the spec lately.
func (w *Worker) beat(ctx context.Context, job claimed) (alive, wanted bool, err error) {
	err = w.Pool.QueryRow(ctx, `UPDATE transcode_jobs j SET lease_expires_at = now() + make_interval(secs => $3)
		WHERE j.id = $1 AND j.worker = $2 AND j.state = 'running'
		RETURNING EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = j.spec_hash AND NOT v.stopped
			AND v.last_seen_at > now() - make_interval(secs => $4))`,
		job.ID, w.ID, w.lease().Seconds(), w.idle().Seconds()).Scan(&wanted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, wanted, err
}

// upload stores one finished segment, with the init segment alongside the
// job's first. Contents depend on the spec and number only, so a repeat is
// harmless.
func (w *Worker) upload(ctx context.Context, job claimed, n int, name string) error {
	if n == job.Start {
		if err := w.put(ctx, SegmentKey(job.Hash, -1), filepath.Join(filepath.Dir(name), "init.mp4")); err != nil {
			return err
		}
	}
	if err := w.put(ctx, SegmentKey(job.Hash, n), name); err != nil {
		return err
	}
	os.Remove(name)
	if _, err := w.Pool.Exec(ctx, `UPDATE transcode_jobs SET next_segment = $2, lease_expires_at = now() + make_interval(secs => $4)
		WHERE id = $1 AND worker = $3 AND state = 'running'`, job.ID, n+1, w.ID, w.lease().Seconds()); err != nil {
		return err
	}
	Notify(ctx, w.Pool, segmentHint(job.Hash))
	return nil
}

func (w *Worker) put(ctx context.Context, key, file string) error {
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	defer handle.Close()
	return w.Segments.Put(ctx, key, handle)
}

func (w *Worker) stored(ctx context.Context, hash string, n int) bool {
	body, err := w.Segments.Open(ctx, SegmentKey(hash, n), 0)
	if err != nil {
		return false
	}
	body.Close()
	return true
}

// scratchIdle is how long a job directory may go untouched before nobody is
// using it: a live job stores a segment, and so changes its directory, far
// more often, and a hung one is failed after Stall.
var scratchIdle = 2 * time.Minute

// clearScratch removes the directories a crashed worker left. Workers on one
// host may share Scratch, so it leaves any that a job is still writing to.
func (w *Worker) clearScratch() {
	dirs, _ := filepath.Glob(filepath.Join(w.Scratch, "job-*"))
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil && time.Since(info.ModTime()) > scratchIdle {
			os.RemoveAll(dir)
		}
	}
}

// tail keeps the end of what ffmpeg says, which is where the reason is.
type tail struct{ data []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.data = append(t.data, p...)
	if len(t.data) > 4096 {
		t.data = t.data[len(t.data)-4096:]
	}
	return len(p), nil
}

func (t *tail) String() string { return string(bytes.TrimSpace(t.data)) }

// sweepLoop removes work nobody needs, once a minute.
func (w *Worker) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := w.Sweep(ctx); err != nil && ctx.Err() == nil {
			w.Log.Error("transcode sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep cancels jobs whose worker vanished, which viewers restart, forgets old job rows, and deletes
// the segments of specs nobody has asked for in Retention. Every step is
// idempotent, so workers sweeping at once is harmless.
func (w *Worker) Sweep(ctx context.Context) error {
	w.clearScratch()
	if err := expireQueued(ctx, w.Pool, orDefault(w.QueueTimeout, queueTimeout)); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM transcode_nodes WHERE seen_at < now() - interval '1 hour'`,
		`UPDATE transcode_jobs SET state = 'cancelled', error = 'the worker stopped renewing its lease', ended_at = now()
			WHERE state = 'running' AND lease_expires_at < now()`,
		`DELETE FROM transcode_jobs WHERE state IN ('done', 'failed', 'cancelled') AND created_at < now() - interval '1 day'`,
	} {
		if _, err := w.Pool.Exec(ctx, statement); err != nil {
			return err
		}
	}
	retention := orDefault(w.Retention, 24*time.Hour).Seconds()
	rows, err := w.Pool.Query(ctx, `SELECT spec_hash FROM transcode_viewers GROUP BY spec_hash
		HAVING max(last_seen_at) < now() - make_interval(secs => $1)`, retention)
	if err != nil {
		return err
	}
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, hash := range hashes {
		if err := w.deleteUnused(ctx, hash, retention); err != nil {
			return err
		}
	}
	return nil
}

// deleteUnused deletes a spec's segments unless it has been wanted since the
// sweep looked. It holds the lock that starting a job for the spec needs, so a
// job can't begin storing segments between the check and the delete.
func (w *Worker) deleteUnused(ctx context.Context, hash string, retention float64) error {
	return pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", hash); err != nil {
			return err
		}
		var unused bool
		err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM transcode_viewers WHERE spec_hash = $1
				AND last_seen_at >= now() - make_interval(secs => $2))
			AND NOT EXISTS (SELECT 1 FROM transcode_jobs WHERE spec_hash = $1 AND state IN ('queued', 'running'))`,
			hash, retention).Scan(&unused)
		if err != nil || !unused {
			return err
		}
		objects, err := w.Segments.List(ctx, "segments/"+hash)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("list segments of %s: %w", hash, err)
		}
		for _, object := range objects {
			if err := w.Segments.Delete(ctx, object.Key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM transcode_viewers WHERE spec_hash = $1", hash); err != nil {
			return err
		}
		w.Log.Info("deleted the segments of an unused spec", "spec", hash, "segments", len(objects))
		return nil
	})
}
