package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// A subtitle job is a track somebody wants and the asset store doesn't have
// yet. Any role asks for one with FetchSubtitle; a worker takes it and puts the
// track in the asset store. Extractions have capacity of their own, apart from
// the transcode slots, because jellyfin-web asks for a subtitle once and does
// not retry.

// subtitleHint wakes workers when a job is queued and waiters when one ends.
const subtitleHint = "subtitle"

const (
	// subtitleAttempts is how many times a lease may be lost, with the worker
	// gone, before the track is given up on.
	subtitleAttempts = 3
	// subtitleWaitLimit stops a request that outlives the longest extraction
	// (an hour) and a wait in the queue.
	subtitleWaitLimit = 2 * time.Hour
	// subtitleSlots is how many extractions a worker runs at once, so a burst
	// of tracks can't use up CPU, connections or the object store.
	subtitleSlots = 4
)

var (
	// ErrNoSubtitleWorker means no worker is running to extract the track.
	ErrNoSubtitleWorker = errors.New("no worker is running to extract the subtitle track")
	// ErrUnreadable means the file can't be read for good (or the job was given
	// up on): asking again gets the same answer.
	ErrUnreadable = errors.New("the subtitle track could not be read")
	// ErrJobFailed means the job failed for a reason that may pass (the store, a
	// signal, a full disk). A job that failed within failureMemory is not run
	// again, so a failing one isn't run for every request, but a later ask is.
	ErrJobFailed = errors.New("the subtitle job failed")
)

// WorkerAlive says whether a process with the worker role has reported lately.
// Unlike the transcode check it doesn't need slots or a transcode store.
func WorkerAlive(ctx context.Context, pool *pgxpool.Pool) (alive bool, err error) {
	err = pool.QueryRow(ctx, "SELECT "+workerAliveSQL, workerAlive.Seconds()).Scan(&alive)
	return alive, err
}

// workerAliveSQL takes the seconds a report counts for as $1.
const workerAliveSQL = `EXISTS (SELECT 1 FROM transcode_nodes
	WHERE roles ~ '(^|,)worker(,|$)' AND seen_at > now() - make_interval(secs => $1))`

// FetchSubtitle returns stream index of src. A cached track is read at once.
// Otherwise it queues a job (one per track, whoever asks) and waits for the
// track to appear in the asset store, for as long as a live worker is on it.
// A job queued behind busy workers waits its turn, because jellyfin-web doesn't
// ask twice. With no worker, or a job unclaimed while no extraction is running
// anywhere (a worker from before subtitle jobs), it gives up after queueTimeout
// with ErrNoSubtitleWorker rather than leave the viewer waiting on nothing.
func FetchSubtitle(ctx context.Context, pool *pgxpool.Pool, hub *Hub, assets store.Store, itemID string, src items.Source, index int) (subtitles.Track, error) {
	key := subtitles.Key(src, index)
	if track, found, err := subtitles.Read(ctx, assets, key); found || err != nil {
		return track, err
	}
	err := awaitJob(ctx, pool, hub, subtitleJob{Key: key, Kind: "track", Item: itemID, Source: src.ID, Index: index})
	if err != nil {
		return subtitles.Track{}, err
	}
	// Only now is the store worth asking: a job that says done has put it.
	track, found, err := subtitles.Read(ctx, assets, key)
	if err == nil && !found {
		err = fmt.Errorf("%w: its result is missing", ErrUnreadable)
	}
	return track, err
}

// FetchFonts returns the hash of each font attached to src by attachment index
// (see subtitles.FontKey), asking a worker to take them out of the film if they
// are not in the asset store. One job serves the whole film, however many fonts
// it has and whoever asks.
func FetchFonts(ctx context.Context, pool *pgxpool.Pool, hub *Hub, assets store.Store, itemID string, src items.Source) (map[int]string, error) {
	if fonts, found, err := subtitles.ReadFonts(ctx, assets, src); found || err != nil {
		return fonts, err
	}
	err := awaitJob(ctx, pool, hub, subtitleJob{Key: subtitles.FontsKey(src.ID, src.ETag), Kind: "fonts", Item: itemID, Source: src.ID})
	if err != nil {
		return nil, err
	}
	fonts, found, err := subtitles.ReadFonts(ctx, assets, src)
	if err == nil && !found {
		err = fmt.Errorf("%w: its result is missing", ErrUnreadable)
	}
	return fonts, err
}

// StartFonts asks for src's fonts to be taken out of the film, without waiting
// for them, if they have not been (see FetchFonts). With lost, the fonts job
// runs again even if it ended a moment ago, because a font it stored is gone.
func StartFonts(ctx context.Context, pool *pgxpool.Pool, assets store.Store, itemID string, src items.Source, lost bool) error {
	if !lost {
		if _, found, err := subtitles.ReadFonts(ctx, assets, src); found || err != nil {
			return err
		}
	}
	memory := failureMemory
	if lost {
		memory = 0
	}
	if err := enqueueJob(ctx, pool, subtitleJob{Key: subtitles.FontsKey(src.ID, src.ETag), Kind: "fonts", Item: itemID, Source: src.ID}, memory); err != nil {
		return err
	}
	Notify(ctx, pool, subtitleHint)
	return nil
}

// enqueueJob queues job unless one is live. A finished or failed row is
// replaced only once it is older than memory, so the object that has just been
// put, and a failure that has just been reported, are found and not run again.
func enqueueJob(ctx context.Context, pool *pgxpool.Pool, job subtitleJob, memory time.Duration) error {
	_, err := pool.Exec(ctx, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key) DO UPDATE SET state = 'queued', worker = NULL, attempts = 0, error = NULL, permanent = false, ended_at = NULL,
			lease_expires_at = NULL, created_at = now()
		WHERE subtitle_jobs.state IN ('done', 'failed') AND subtitle_jobs.ended_at < now() - make_interval(secs => $6)`,
		job.Key, job.Kind, job.Item, job.Source, job.Index, memory.Seconds())
	return err
}

// awaitJob queues job unless one is live and waits for it to be done.
func awaitJob(ctx context.Context, pool *pgxpool.Pool, hub *Hub, job subtitleJob) error {
	ctx, cancel := context.WithTimeout(ctx, subtitleWaitLimit)
	defer cancel()
	wake, unsubscribe := hub.Subscribe(subtitleHint)
	defer unsubscribe()
	enqueue := func() error { return enqueueJob(ctx, pool, job, failureMemory) }
	if err := enqueue(); err != nil {
		return err
	}
	Notify(ctx, pool, subtitleHint)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var stuckSince time.Time
	for {
		var state, reason string
		var permanent, alive, unclaimed, extracting bool
		// A running job whose lease has lapsed is as good as queued.
		err := pool.QueryRow(ctx, `SELECT COALESCE(j.state, ''), COALESCE(j.error, ''), COALESCE(j.permanent, false), `+workerAliveSQL+`,
				COALESCE(j.state = 'queued' OR (j.state = 'running' AND j.lease_expires_at < now()), false),
				EXISTS (SELECT 1 FROM subtitle_jobs WHERE state = 'running' AND lease_expires_at > now())
			FROM (SELECT 1) one LEFT JOIN subtitle_jobs j ON j.key = $2`, workerAlive.Seconds(), job.Key).Scan(&state, &reason, &permanent, &alive, &unclaimed, &extracting)
		if err != nil {
			return err
		}
		switch state {
		case "":
			if err := enqueue(); err != nil { // Swept while we waited.
				return err
			}
		case "done":
			return nil
		case "failed":
			if !permanent {
				return fmt.Errorf("%w: %s", ErrJobFailed, reason)
			}
			return fmt.Errorf("%w: %s", ErrUnreadable, reason)
		}
		switch {
		case alive && (!unclaimed || extracting):
			stuckSince = time.Time{}
		case stuckSince.IsZero():
			stuckSince = time.Now()
		case time.Since(stuckSince) > queueTimeout:
			return ErrNoSubtitleWorker
		}
		select {
		case <-wake:
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type subtitleJob struct {
	Key, Kind, Item, Source string
	Index                   int
}

// RunSubtitles claims subtitle jobs until ctx ends, up to four at a time,
// separately from the transcode slots, and waits for its extractions to end.
func (w *Worker) RunSubtitles(ctx context.Context) {
	w.setID()
	slots := make(chan struct{}, subtitleSlots)
	var running sync.WaitGroup
	defer running.Wait()
	wake, unsubscribe := w.Hub.Subscribe(subtitleHint)
	defer unsubscribe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case slots <- struct{}{}:
			job, ok, err := w.claimSubtitle(ctx)
			if err != nil && ctx.Err() == nil {
				w.Log.Error("claiming a subtitle job failed", "error", err)
			}
			if ok {
				running.Add(1)
				go func() {
					defer running.Done()
					defer func() { <-slots }()
					w.runSubtitle(ctx, job)
				}()
				continue
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

// setID gives the worker the ID Run would, whichever loop starts first.
func (w *Worker) setID() {
	w.idOnce.Do(func() {
		if w.ID == "" {
			w.ID = fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
		}
	})
}

// claimSubtitle takes a queued job, or one whose worker stopped renewing its
// lease. A job whose lease has been lost subtitleAttempts times is failed
// instead, since whatever it does is killing its workers.
func (w *Worker) claimSubtitle(ctx context.Context) (subtitleJob, bool, error) {
	if _, err := w.Pool.Exec(ctx, `UPDATE subtitle_jobs SET state = 'failed', permanent = true, error = 'the worker stopped renewing its lease', ended_at = now()
		WHERE state = 'running' AND lease_expires_at < now() AND attempts >= $1`, subtitleAttempts); err != nil {
		return subtitleJob{}, false, err
	}
	var job subtitleJob
	err := w.Pool.QueryRow(ctx, `UPDATE subtitle_jobs SET state = 'running', worker = $1, attempts = attempts + 1,
			lease_expires_at = now() + make_interval(secs => $2)
		WHERE key = (SELECT key FROM subtitle_jobs WHERE state = 'queued' OR (state = 'running' AND lease_expires_at < now())
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING key, kind, item_id, source_id, idx`, w.ID, w.lease().Seconds()).Scan(&job.Key, &job.Kind, &job.Item, &job.Source, &job.Index)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, false, nil
	}
	return job, err == nil, err
}

// runSubtitle extracts one track, renewing the lease meanwhile, and records how
// it ended. A job whose lease was taken is left to its new owner. Cut off from
// the database it stops a beat before the lease lapses, so that a file is never
// read by two workers at once.
func (w *Worker) runSubtitle(ctx context.Context, job subtitleJob) {
	log := w.Log.With("subtitle", job.Key)
	extractCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.extractTrack(extractCtx, job, log) }()
	beat := time.NewTicker(w.heartbeat())
	defer beat.Stop()
	// Only a renewal puts this off, and it fires even during a beat that hangs.
	stale := time.AfterFunc(w.lease()-w.heartbeat(), cancel)
	defer stale.Stop()
	var err error
wait:
	for {
		select {
		case err = <-done:
			break wait
		case <-beat.C:
			began := time.Now()
			beatCtx, stopBeat := context.WithTimeout(ctx, w.heartbeat()/2)
			tag, beatErr := w.Pool.Exec(beatCtx, `UPDATE subtitle_jobs SET lease_expires_at = now() + make_interval(secs => $3)
				WHERE key = $1 AND worker = $2 AND state = 'running'`, job.Key, w.ID, w.lease().Seconds())
			stopBeat()
			switch {
			case beatErr != nil:
			case tag.RowsAffected() == 0:
				cancel()
				<-done
				log.Warn("the subtitle job's lease was lost")
				return
			default:
				// Counted from before the beat: the lease can't have started earlier.
				stale.Reset(w.lease() - w.heartbeat() - time.Since(began))
			}
		}
	}
	if err != nil && extractCtx.Err() != nil && ctx.Err() == nil {
		log.Warn("stopped a subtitle extraction: the database is out of reach and the lease is running out")
		return
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	state, reason := "done", ""
	// Only what is the file's doing is for good; see FillFonts for the fonts,
	// whose such failures are recorded as a result, not as a failure.
	permanent := errors.Is(err, subtitles.ErrBadFile)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The worker is going away, which isn't the track's fault.
		state = "queued"
	default:
		log.Error("subtitle extraction failed", "error", err)
		state, reason = "failed", err.Error()
	}
	if _, err := w.Pool.Exec(finish, `UPDATE subtitle_jobs SET state = $2, error = NULLIF(left($3, 500), ''), permanent = $5,
			ended_at = CASE WHEN $2 = 'queued' THEN NULL ELSE now() END,
			worker = CASE WHEN $2 = 'queued' THEN NULL ELSE worker END,
			attempts = CASE WHEN $2 = 'queued' THEN attempts - 1 ELSE attempts END
		WHERE key = $1 AND worker = $4 AND state = 'running'`, job.Key, state, reason, w.ID, state == "failed" && permanent); err != nil {
		log.Error("recording the end of a subtitle job failed", "error", err)
	}
	Notify(finish, w.Pool, subtitleHint)
}

// extractTrack makes what the job asks for. A subtitle file that can't be read
// for good is hidden from the film until the file changes (library.MarkUnreadable),
// so that it costs one job and not one for every request and every scan; any
// other failure, a store that didn't answer or a timeout, is only the job's.
func (w *Worker) extractTrack(ctx context.Context, job subtitleJob, log *slog.Logger) error {
	source, found, err := w.Items.Source(ctx, job.Item, job.Source)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("the source file is gone")
	}
	media, err := w.Media(source.StoreURI)
	if err != nil {
		return err
	}
	assets, err := w.Media(source.AssetStoreURI)
	if err != nil {
		return err
	}
	if job.Kind == "fonts" {
		if subtitles.FontsKey(source.ID, source.ETag) != job.Key {
			return errors.New("the source file has changed or is gone")
		}
		return subtitles.FillFonts(ctx, source, media, assets, w.SubtitleTimeout, log)
	}
	if subtitles.Key(source, job.Index) != job.Key {
		return errors.New("the source file has changed or is gone")
	}
	err = subtitles.Fill(ctx, source, media, assets, job.Index, w.SubtitleTimeout, log)
	file, beside := subtitles.External(source, job.Index)
	if beside && errors.Is(err, subtitles.ErrBadFile) {
		log.Warn("subtitle file not listed: it can't be read", "file", file.Path, "error", err)
		// Not the job's context, which ends with the job.
		marked, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if markErr := library.MarkUnreadable(marked, w.Pool, job.Item, job.Index, file.Fingerprint); markErr != nil {
			err = errors.Join(err, markErr)
		}
	}
	return err
}
