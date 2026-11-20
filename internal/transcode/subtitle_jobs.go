package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/playback"
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
	// subtitleWaitLimit stops a request that outlives the longest extraction (a
	// pass of two runs, an hour each) and a wait in the queue.
	subtitleWaitLimit = 3 * time.Hour
	// subtitleSlots is how many extractions a worker runs at once, so a burst
	// of tracks can't use up CPU, connections or the object store.
	subtitleSlots = 4
	// chapterSlots is how many films' chapter pictures a worker makes at once.
	// They are background work, a seek into the file for every chapter, so they
	// take no subtitle slot: a library full of them must not hold up a viewer's
	// track.
	chapterSlots = 2
	// failedJobMemory is how long the daily sweep keeps a failed job's row, which is how long a
	// failure for good is remembered (the sweep's "1 day").
	failedJobMemory = 24 * time.Hour
	// imageSlots is how many films' image tracks a worker extracts at once.
	// Nobody waits on one and its read is the whole film, up to an hour, so it
	// takes no subtitle slot either: a few burnt films must not hold up a viewer's
	// text track.
	imageSlots = 2
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
	err = pool.QueryRow(ctx, "SELECT "+workerAliveSQL, liveTiming("counted").Seconds()).Scan(&alive)
	return alive, err
}

// workerAliveSQL takes the seconds a report counts for as $1.
const workerAliveSQL = `EXISTS (SELECT 1 FROM transcode_nodes
	WHERE roles ~ '(^|,)worker(,|$)' AND seen_at > now() - make_interval(secs => $1))`

// Workers is what one read of transcode_nodes says: whether a worker has reported lately (WorkerAlive), and whether a
// picked audio track can be delivered: a worker of this output version that runs transcodes has reported lately and
// the live roles agree on one transcodeStore (StoreMismatch). A worker with no free cores or a spec that just failed
// is transient and not known here.
type Workers struct{ Alive, Transcodes bool }

// ReadWorkers reads Workers in one statement.
func ReadWorkers(ctx context.Context, pool *pgxpool.Pool) (w Workers, err error) {
	err = pool.QueryRow(ctx, "SELECT "+workerAliveSQL+`, EXISTS (SELECT 1 FROM transcode_nodes
			WHERE roles ~ '(^|,)worker(,|$)' AND (slots > 0 OR cores > 0) AND output_version = $2 AND seen_at > now() - make_interval(secs => $1))
		AND (SELECT count(DISTINCT store) FROM transcode_nodes
			WHERE seen_at > now() - make_interval(secs => $1) AND NOT (store = '' AND roles = 'worker')) <= 1`,
		liveTiming("counted").Seconds(), playback.OutputVersion).Scan(&w.Alive, &w.Transcodes)
	return w, err
}

// FetchSubtitle returns stream index of src. A cached track is read at once.
// Otherwise it queues a job and waits for the
// track to appear in the asset store, for as long as a live worker is on it.
// A job queued behind busy workers waits its turn, because jellyfin-web doesn't
// ask twice. With no worker, or a job unclaimed while no extraction is running
// anywhere (a worker from before subtitle jobs), it gives up after queueTimeout
// with ErrNoSubtitleWorker rather than leave the viewer waiting on nothing.
//
// A text track inside the film is extracted by the film's pass job, which
// writes every such track from one read, so whoever asks for whichever track
// waits for the same job. A track the pass did not store, one ffmpeg could not
// extract, is ErrUnreadable. A file beside the film has a job of its own.
func FetchSubtitle(ctx context.Context, pool *pgxpool.Pool, hub *Hub, assets store.Store, itemID string, src items.Source, index int) (subtitles.Track, error) {
	key := subtitles.Key(src, index)
	if track, found, err := subtitles.Read(ctx, assets, key); found || err != nil {
		return track, fromStore(src.AssetStoreURI, err)
	}
	if subtitles.Unreadable(src, index) {
		return subtitles.Track{}, fmt.Errorf("%w: ffmpeg could not extract it", ErrUnreadable)
	}
	job := subtitleJob{Key: key, Kind: "track", Item: itemID, Source: src.ID, Index: index}
	if _, beside := subtitles.External(src, index); !beside && slices.Contains(subtitles.TextTracks(src), index) {
		job.Key = subtitles.PassKey(src)
	}
	if err := awaitJob(ctx, pool, hub, job); err != nil {
		return subtitles.Track{}, err
	}
	// Only now is the store worth asking: a job that says done has put it.
	track, found, err := subtitles.Read(ctx, assets, key)
	if err == nil && !found {
		err = fmt.Errorf("%w: its result is missing", ErrUnreadable)
	}
	return track, fromStore(src.AssetStoreURI, err)
}

// FetchSup makes sure PGS track index of src is in the asset store as a .sup
// file (see subtitles.SupKey), asking a worker to copy it out of the film if
// not, and returns the object's key. As for a text track, one job serves
// everyone who asks.
func FetchSup(ctx context.Context, pool *pgxpool.Pool, hub *Hub, assets store.Store, itemID string, src items.Source, index int) (string, error) {
	key := subtitles.SupKey(src, index)
	if subtitles.Exists(ctx, assets, key) {
		return key, nil
	}
	if err := awaitJob(ctx, pool, hub, subtitleJob{Key: key, Kind: "sup", Item: itemID, Source: src.ID, Index: index}); err != nil {
		return "", err
	}
	if !subtitles.Exists(ctx, assets, key) {
		return "", fmt.Errorf("%w: its result is missing", ErrUnreadable)
	}
	return key, nil
}

// FetchFonts returns the hash of each font attached to src by attachment index
// (see subtitles.FontKey), asking a worker to take them out of the film if they
// are not in the asset store. One job serves the whole film, however many fonts
// it has and whoever asks.
func FetchFonts(ctx context.Context, pool *pgxpool.Pool, hub *Hub, assets store.Store, itemID string, src items.Source) (map[int]string, error) {
	if fonts, found, err := subtitles.ReadFonts(ctx, assets, src); found || err != nil {
		return fonts, fromStore(src.AssetStoreURI, err)
	}
	err := awaitJob(ctx, pool, hub, subtitleJob{Key: subtitles.FontsKey(src.ID, src.ETag), Kind: "fonts", Item: itemID, Source: src.ID})
	if err != nil {
		return nil, err
	}
	fonts, found, err := subtitles.ReadFonts(ctx, assets, src)
	if err == nil && !found {
		err = fmt.Errorf("%w: its result is missing", ErrUnreadable)
	}
	return fonts, fromStore(src.AssetStoreURI, err)
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

// StartImageTracks asks for the image tracks inside src's file (PGS, VobSub, DVB)
// to be stored in the asset store (JS-089), without waiting for them, as a play
// that burns one does: a run that begins mid-film then draws the cue showing at
// its cut from the stored track, not by reading the film from where the cue began.
// One job reads the film once and stores every such track, whoever asks and
// however many tracks the film has. It asks for none that is stored, none the
// film cannot give (left out by an earlier job that ended, or a job that failed
// for the film's own reason; both end with a new file), and none while a job for
// the film is live or has just failed, as for a font.
func StartImageTracks(ctx context.Context, pool *pgxpool.Pool, assets store.Store, itemID string, src items.Source) error {
	missing := slices.Clone(subtitles.ImageTracks(src))
	var err error
	missing = slices.DeleteFunc(missing, func(index int) bool {
		if err != nil {
			return false
		}
		_, statErr := assets.Stat(ctx, subtitles.ImageKey(src, index))
		if statErr != nil && !errors.Is(statErr, store.ErrNotFound) {
			err = statErr
		}
		return statErr == nil
	})
	if err != nil {
		return fromStore(src.AssetStoreURI, err)
	}
	if len(missing) == 0 {
		return nil
	}
	key := subtitles.ImagesKey(src)
	var state string
	var permanent bool
	var skipped []int32
	err = pool.QueryRow(ctx, "SELECT state, permanent, skipped FROM subtitle_jobs WHERE key = $1", key).Scan(&state, &permanent, &skipped)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fromPool(err)
	case state == "failed" && permanent:
		return nil
	case state == "done":
		missing = slices.DeleteFunc(missing, func(index int) bool { return slices.Contains(skipped, int32(index)) })
		if len(missing) == 0 {
			return nil
		}
	}
	if err := enqueueJob(ctx, pool, subtitleJob{Key: key, Kind: "images", Item: itemID, Source: src.ID}, failureMemory); err != nil {
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
			lease_expires_at = NULL, created_at = now(), skipped = '{}'
		WHERE subtitle_jobs.state IN ('done', 'failed') AND subtitle_jobs.ended_at < now() - make_interval(secs => $6)`,
		job.Key, job.Kind, job.Item, job.Source, job.Index, memory.Seconds())
	return fromPool(err)
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
		turnFrom(ctx) // Each round is a query that may hang; the wait itself is no stall.
		var state, reason string
		var permanent, alive, unclaimed, extracting bool
		// A running job whose lease has lapsed is as good as queued.
		err := pool.QueryRow(ctx, `SELECT COALESCE(j.state, ''), COALESCE(j.error, ''), COALESCE(j.permanent, false), `+workerAliveSQL+`,
				COALESCE(j.state = 'queued' OR (j.state = 'running' AND j.lease_expires_at < now()), false),
				EXISTS (SELECT 1 FROM subtitle_jobs WHERE state = 'running' AND lease_expires_at > now() AND kind NOT IN ('chapters', 'images'))
			FROM (SELECT 1) one LEFT JOIN subtitle_jobs j ON j.key = $2`, liveTiming("counted").Seconds(), job.Key).Scan(&state, &reason, &permanent, &alive, &unclaimed, &extracting)
		if err != nil {
			return fromPool(err)
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
// The film's image tracks (JS-089) are claimed here too, but on imageSlots of
// their own.
func (w *Worker) RunSubtitles(ctx context.Context) {
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		w.runJobs(ctx, "images", imageSlots)
	}()
	w.runJobs(ctx, "viewer", subtitleSlots)
	running.Wait()
}

// RunChapterImages claims chapter-image jobs until ctx ends, two at a time, on
// slots of their own, and waits for them to end.
func (w *Worker) RunChapterImages(ctx context.Context) {
	w.runJobs(ctx, "chapters", chapterSlots)
}

// runJobs claims the jobs of one queue: "chapters", "images" or "viewer", the
// kinds a viewer's request waits on.
func (w *Worker) runJobs(ctx context.Context, queue string, n int) {
	w.setID()
	slots := make(chan struct{}, n)
	var running sync.WaitGroup
	defer running.Wait()
	kind := map[string]string{"viewer": "subtitles", "chapters": "chapter_images", "images": "image_tracks"}[queue]
	wake, unsubscribe := w.Hub.Subscribe(subtitleHint)
	defer unsubscribe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case slots <- struct{}{}:
			jobCtx, session := w.subtitleContext(ctx)
			job, ok, err := w.claimSubtitle(ctx, queue, session)
			if err != nil && ctx.Err() == nil {
				w.Log.Error("claiming a subtitle job failed", "error", err)
			}
			if ok {
				running.Add(1)
				go func() {
					defer running.Done()
					defer func() { <-slots }()
					extractions.WithLabelValues(kind).Inc()
					defer extractions.WithLabelValues(kind).Dec()
					w.runSubtitle(ctx, jobCtx, job)
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
		w.Cores, w.Slots = w.registered()
	})
}

// subtitleContext is the context a job claimed now runs in, and the session it is
// claimed for: the worker's own while it has one, which ends when another replica
// forgets the worker, so that its extractions end with its jobs; the process's
// when no Present keeps the worker's row.
func (w *Worker) subtitleContext(ctx context.Context) (context.Context, string) {
	w.heldMu.Lock()
	held := w.held
	w.heldMu.Unlock()
	if held == nil {
		return ctx, ""
	}
	return held, sessionOf(held)
}

// claimSubtitle takes a queued job, or one whose worker stopped renewing its
// lease. A job whose lease has been lost subtitleAttempts times is failed
// instead, since whatever it does is killing its workers. Chapter-image jobs,
// image-track jobs and the others are claimed apart, so none waits behind another.
func (w *Worker) claimSubtitle(ctx context.Context, queue string, session string) (subtitleJob, bool, error) {
	if _, err := w.Pool.Exec(ctx, `UPDATE subtitle_jobs SET state = 'failed', permanent = true, error = 'the worker stopped renewing its lease', ended_at = now()
		WHERE state = 'running' AND lease_expires_at < now() AND attempts >= $1`, subtitleAttempts); err != nil {
		return subtitleJob{}, false, err
	}
	var job subtitleJob
	err := w.Pool.QueryRow(ctx, `UPDATE subtitle_jobs SET state = 'running', worker = $1, attempts = attempts + 1,
			lease_expires_at = now() + make_interval(secs => $2)
		WHERE `+registeredIn("$1", "$4::text")+` AND key = (SELECT key FROM subtitle_jobs WHERE (state = 'queued' OR (state = 'running' AND lease_expires_at < now()))
				AND (CASE WHEN kind IN ('chapters', 'images') THEN kind ELSE 'viewer' END) = $3
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING key, kind, item_id, source_id, idx`, w.ID, w.lease().Seconds(), queue, session).Scan(&job.Key, &job.Kind, &job.Item, &job.Source, &job.Index)
	if errors.Is(err, pgx.ErrNoRows) {
		return job, false, nil
	}
	return job, err == nil, err
}

// runSubtitle extracts one track, renewing the lease meanwhile, and records how
// it ended. A job whose lease was taken is left to its new owner. Cut off from
// the database it stops the cutMargin before the lease lapses, so that a file is never
// read by two workers at once. ctx is the process's and jobCtx the job's, which
// ends with the worker's session.
func (w *Worker) runSubtitle(ctx, jobCtx context.Context, job subtitleJob) {
	log := w.Log.With("subtitle", job.Key)
	// The lease keeper cancels extractCtx when the lease can no longer be trusted,
	// the cutMargin before it lapses, so that a file is never read by two workers at
	// once, and when it was taken from this worker.
	extractCtx, lease := w.keepSubtitleLease(jobCtx, job)
	defer lease.stop()
	err := w.extractTrack(extractCtx, job, log)
	if err != nil && extractCtx.Err() != nil && ctx.Err() == nil {
		if errors.Is(context.Cause(extractCtx), errUnrenewed) {
			log.Warn("stopped a subtitle extraction: the database is out of reach and the lease is running out")
		} else {
			log.Warn("the subtitle job's lease was lost")
		}
		return
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	state, reason := "done", ""
	// Only what is the file's doing is for good; see FillFonts for the fonts,
	// whose such failures are recorded as a result, not as a failure.
	// A film whose image tracks could not be read within the time limit is not read
	// again on every play, a whole-film read each time: it is left until the daily
	// sweep of finished jobs, or the file's change, as a bad file is. The limit is
	// not configurable, and the viewer meanwhile has the cue drawn as before JS-089.
	permanent := errors.Is(err, subtitles.ErrBadFile) || (job.Kind == "images" && errors.Is(err, subtitles.ErrTooSlow))
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The worker is going away, which isn't the track's fault.
		state = "queued"
	case job.Kind == "images" && errors.Is(err, subtitles.ErrTooSlow):
		// The operator's only sight of it: the film's viewers are told nothing (decision D-002).
		retry := time.Now().Add(failedJobMemory)
		log.Error("image track extraction ran out of time: the film is not read again for its tracks before the retry time, and then only by a play of it",
			"item", job.Item, "source", job.Source, "reason", err.Error(), "retry_not_before", retry.Format(time.RFC3339))
		state, reason = "failed", err.Error()
	case job.Kind == "chapters":
		log.Error("chapter image job failed", "item", job.Item, "error", err)
		state, reason = "failed", err.Error()
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
	if job.Kind == "chapters" {
		// Before the source is looked up: a removed source has no row, and its
		// pictures are still to be deleted.
		return library.SyncChapterImages(ctx, w.Pool, job.Source, w.jobAssets, log)
	}
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
	assets, err := w.leasedAssets(source.AssetStoreURI, job.Key)
	if err != nil {
		return err
	}
	if job.Kind == "fonts" {
		if subtitles.FontsKey(source.ID, source.ETag) != job.Key {
			return errors.New("the source file has changed or is gone")
		}
		return subtitles.FillFonts(ctx, source, media, assets, w.SubtitleTimeout, log)
	}
	if job.Kind == "images" {
		if subtitles.ImagesKey(source) != job.Key {
			return errors.New("the source file has changed or is gone")
		}
		left, err := subtitles.FillImages(ctx, source, media, assets, w.SubtitleTimeout, log)
		if err != nil {
			return err
		}
		// What the film can't give is not asked for again until the file changes.
		leftOut := make([]int32, len(left))
		for i, index := range left {
			leftOut[i] = int32(index)
		}
		_, err = w.Pool.Exec(ctx, "UPDATE subtitle_jobs SET skipped = $3 WHERE key = $1 AND worker = $2", job.Key, w.ID, leftOut)
		return fromPool(err)
	}
	if job.Kind == "sup" {
		if subtitles.SupKey(source, job.Index) != job.Key {
			return errors.New("the source file has changed or is gone")
		}
		return subtitles.FillSup(ctx, source, media, assets, job.Index, w.SubtitleTimeout, log)
	}
	if job.Key == subtitles.PassKey(source) {
		if err := subtitles.FillTextStalling(ctx, source, media, assets, w.SubtitleTimeout, w.SubtitleStall, log); err != nil {
			// The tracks ffmpeg's decoder refused are marked although the pass
			// failed, so no ask reads the film for them again.
			var refused *subtitles.RefusedError
			if errors.As(err, &refused) {
				marked, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer stop()
				for _, index := range refused.Tracks {
					log.Warn("track not listed: ffmpeg can't decode it", "track", index)
					if markErr := library.MarkUnreadable(marked, w.Pool, job.Item, index, "", source.ETag); markErr != nil {
						return markErr
					}
				}
			}
			return err
		}
		// A track the pass left out is hidden until a scan sees the file change,
		// as a subtitle file beside the film is. Done before the job ends, so no
		// one asks again in between.
		marked, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		for _, index := range subtitles.TextTracks(source) {
			missing, err := subtitles.Missing(ctx, assets, subtitles.Key(source, index))
			if err != nil {
				return err
			}
			if !missing {
				continue
			}
			log.Warn("track not listed: ffmpeg can't extract it", "track", index)
			if err := library.MarkUnreadable(marked, w.Pool, job.Item, index, "", source.ETag); err != nil {
				return err
			}
		}
		return nil
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
		if markErr := library.MarkUnreadable(marked, w.Pool, job.Item, job.Index, file.Fingerprint, ""); markErr != nil {
			err = errors.Join(err, markErr)
		}
	}
	return err
}
