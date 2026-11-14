package transcode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
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
	// Cores is what the worker has to give its jobs, in the unit of
	// playback.Spec.Cost. Slots, if set instead, stands for as many cores; with
	// neither the worker has the processors it may use (cores).
	Cores float64
	Slots int
	// Idle is how long a spec may go without a request before its jobs stop.
	Idle time.Duration
	// Lead is how far, in film time, a job may get ahead of the furthest
	// segment a viewer has asked for (default 60 seconds). It then pauses its
	// ffmpeg until requests come within the lead again.
	Lead time.Duration
	// ChunkSegments is how many segments one ffmpeg makes before the next
	// begins (default 4); every worker must use the same, since it decides which
	// encoder run makes a segment. HelpBelow is the encode speed, in film
	// seconds per second, under which a job gets helpers on other workers
	// (default 1.2). See helpers.go.
	ChunkSegments int
	HelpBelow     float64
	// NoHelpers stops this worker's jobs from asking for helpers. It does not
	// stop it from running helpers other workers asked for.
	NoHelpers bool
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
	// SubtitleTimeout bounds one subtitle extraction; zero is an hour.
	SubtitleTimeout time.Duration
	// LivenessConfig connects the liveness lock (Present). It defaults to the
	// pool's own, which is right except in a test that wants the lock's
	// connection to go through something else.
	LivenessConfig *pgx.ConnConfig

	// live is what the worker runs now, for /metrics.
	liveMu sync.Mutex
	live   map[int64]liveJob
	// freed wakes Run when a job ends, which frees cores for the next claim.
	freed chan struct{}

	idOnce sync.Once
	// held ends when the worker's row is found gone, and is nil before the first
	// lock; lock is the liveness lock while it is held, and nil while it is
	// being taken again, when the jobs go on but none is claimed.
	heldMu sync.Mutex
	held   context.Context
	lock   *liveness

	// lockGone is when this worker's looks first found each node's lock gone,
	// and lockLost when this worker's own lock last ended; only the liveness
	// loop reads and writes them (keepRow, lookForTheDead).
	lockGone map[string]time.Time
	lockLost time.Time

	// state is the liveness state (heldMu). Its zero value is locked, so a
	// worker the loop has not touched is judged by its lock alone.
	state liveState
}

// Run claims and runs jobs until ctx ends, then waits for ffmpeg to be gone.
// It claims only while Present holds the liveness lock, and a job runs in a
// context that ends when the lock is lost.
func (w *Worker) Run(ctx context.Context) {
	w.setID()
	w.freed = make(chan struct{}, 1)
	var running sync.WaitGroup
	defer running.Wait()
	go w.sweepLoop(ctx)

	wake, unsubscribe := w.Hub.Subscribe(jobHint)
	defer unsubscribe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		jobCtx, held := w.jobContext()
		if held {
			job, ok, err := w.claim(ctx)
			if err != nil && ctx.Err() == nil {
				w.Log.Error("claiming a transcode job failed", "error", err)
			}
			if ok {
				job.progress = w.track(job)
				running.Add(1)
				go func() {
					defer running.Done()
					defer w.freeCores()
					defer w.untrack(job.ID)
					w.run(jobCtx, job)
				}()
				continue // There may be more queued.
			}
		}
		select {
		case <-wake:
		case <-w.freed:
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// freeCores tells Run that a job has ended.
func (w *Worker) freeCores() {
	select {
	case w.freed <- struct{}{}:
	default:
	}
}

type claimed struct {
	ID    int64
	Hash  string
	Spec  playback.Spec
	Start int
	// Helper says the job is a helper (helpers.go): it belongs to all the
	// spec's viewers, and its limit follows the furthest of them.
	Helper bool

	progress *progress // How fast ffmpeg goes; set by Run.
}

// defaultLease is the lease a job gets unless the worker is told another, and
// the one every liveness timing is written for (livenessTable).
const defaultLease = 10 * time.Second

func (w *Worker) lease() time.Duration     { return orDefault(w.Lease, defaultLease) }
func (w *Worker) heartbeat() time.Duration { return orDefault(w.Heartbeat, liveTiming("heartbeat")) }
func (w *Worker) idle() time.Duration      { return orDefault(w.Idle, defaultIdle) }
func (w *Worker) lead() time.Duration      { return orDefault(w.Lead, defaultLead) }

// defaultLead is 15 segments of 4 seconds. The bound is the furthest segment
// asked for plus the lead, not the playback position, so whatever the player
// asks for is always inside it, and the player's own buffer does not enter. The
// lead only has to cover what a resume takes: the worker's look every
// leadCheck, ffmpeg making the next segment (a re-encode slower than real time
// makes a 4 second one in over 4), and the beat of a viewer that asks one
// segment at a time.
const defaultLead = 60 * time.Second

// leadCheck is how often a job at its limit asks the database whether the
// viewers have moved on. A job well short of its limit does not ask.
const leadCheck = 500 * time.Millisecond

// limitFor is the first segment a job may not store yet: the first that starts
// lead or more of film after segment from. It is the layout's count if the
// film ends sooner.
func limitFor(layout playback.Layout, from int, lead time.Duration) int {
	if from < 0 || from >= layout.Count() {
		return layout.Count() // encode ends a job that starts past the end before it gets here.
	}
	start, _ := layout.Segment(from)
	for n := from + 1; n < layout.Count(); n++ {
		if at, _ := layout.Segment(n); at-start >= int64(lead.Seconds()*ticksPerSecond) {
			return n
		}
	}
	return layout.Count()
}

// furthest is the segment the viewers a job serves asked for last, the furthest
// of them, or the job's start if none did; served says whether any did. A viewer's own segment counts if it
// is one the job has made or is about to (the window ensure joins a job by); a
// viewer far past it has its own job, and one before its start is not served by
// it, so neither holds this job open. free is true if a viewer of the spec has
// asked for no segment at all: an api or stream replica from before the column
// writes none, and the job then runs unbounded, as it did before the bound
// existed, rather than stop for good where no request can move it. A viewer
// that has only fetched a playlist does the same until its first segment.
func (w *Worker) furthest(ctx context.Context, job claimed, next int) (n int, served, free bool, err error) {
	from, to := job.Start, next+catchUpSegments
	if job.Helper {
		// A helper is ahead of the viewers, so none is within its window; the
		// viewers at or before its chunk are its, and the furthest of them bounds
		// it, not its start. A viewer far past the chunk has a job of its own,
		// and the helpers of that one: its lead is not this helper's to fill.
		from, to = 0, job.Start+w.chunkSize()
	}
	err = w.Pool.QueryRow(ctx, `SELECT
			COALESCE(max(segment) FILTER (WHERE segment >= $3 AND segment < $4), -1),
			COALESCE(bool_or(segment < 0), false)
		FROM transcode_viewers
		WHERE spec_hash = $1 AND NOT stopped AND last_seen_at > now() - make_interval(secs => $2)`,
		job.Hash, w.idle().Seconds(), from, to).Scan(&n, &free)
	if job.Helper {
		return n, n >= 0, free, err
	}
	return max(n, job.Start), n >= 0, free, err
}

// registeredAs is SQL true while the worker's row is in transcode_nodes. Reap
// deletes the row of a worker whose liveness lock is gone, and the worker puts
// it back only once it holds the lock again, so a worker without its row may
// not claim a job, renew a lease or store a segment: the cheap check that
// keeps a reaped worker from extending the grace Reap gave its jobs.
func registeredAs(idExpr string) string {
	return `EXISTS (SELECT 1 FROM transcode_nodes WHERE id = ` + idExpr + `)`
}

// startup is extra lease for a claimed job, which is not heartbeating until
// ffmpeg is running.
const startup = 20 * time.Second

func (w *Worker) claim(ctx context.Context) (claimed, bool, error) {
	var job claimed
	queue := orDefault(w.QueueTimeout, queueTimeout)
	// Most looks find nothing queued, and read one indexed row to say so.
	var waiting bool
	if err := w.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcode_jobs WHERE state = 'queued' AND output_version = $1
		AND created_at > now() - make_interval(secs => $2))`, playback.OutputVersion, queue.Seconds()).Scan(&waiting); err != nil || !waiting {
		return job, false, err
	}
	nodes, jobs, err := readCapacity(ctx, w.Pool, capQuery{Filter: "true", Queue: queue, Self: w.ID})
	if err != nil {
		return job, false, err
	}
	// What the worker holds is what its rows say, and what it still runs: a job
	// someone cancelled keeps its cores until ffmpeg is gone.
	mine := newLedger(nodes, jobs, func(j capJob) bool { return j.Worker == w.ID })
	running := newLedger(nodes, jobs, func(capJob) bool { return true })
	running.placeQueued(jobs, func(j capJob) bool { return !j.Helper })
	if _, registered := mine.free[w.ID]; !registered {
		return job, false, nil
	}
	local, copies := w.holding()
	mine.free[w.ID] = min(mine.free[w.ID], mine.cores[w.ID]-local)
	mine.copies[w.ID] = max(mine.copies[w.ID], copies)
	// A helper goes to a worker that has no job of its spec, or after a second
	// to one that has, if none took it (a dead helper's replacement, with the
	// other workers busy). It goes only while room stays for a viewer who has not
	// asked yet; that was so when it was queued, not necessarily now. A job of
	// another output version is not here, as if it were not there; one that does
	// not fit is left for a worker that has the cores, and a later one that fits
	// goes first.
	for _, queued := range jobs {
		if queued.Worker != "" || !mine.fits(w.ID, queued) {
			continue
		}
		if queued.Helper {
			after := running.clone()
			after.take(w.ID, queued)
			if _, spare := after.room(capJob{Cost: queued.Cost}); !spare {
				continue
			}
		}
		var raw []byte
		err := w.Pool.QueryRow(ctx, `UPDATE transcode_jobs SET state = 'running', worker = $1,
				lease_expires_at = now() + make_interval(secs => $2)
			WHERE `+registeredAs("$1")+` AND id = $3 AND state = 'queued' AND created_at > now() - make_interval(secs => $4)
				AND (NOT helper OR created_at < now() - make_interval(secs => 1)
					OR NOT EXISTS (SELECT 1 FROM transcode_jobs o WHERE o.spec_hash = transcode_jobs.spec_hash AND o.worker = $1 AND o.state = 'running'))
			RETURNING id, spec_hash, spec, start_segment, helper`, w.ID, (w.lease()+startup).Seconds(), queued.ID, queue.Seconds()).
			Scan(&job.ID, &job.Hash, &raw, &job.Start, &job.Helper)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // Another worker took it first.
		}
		if err != nil {
			return job, false, err
		}
		return job, true, json.Unmarshal(raw, &job.Spec)
	}
	return job, false, nil
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
	log.Info("transcode started", "copy", job.Spec.Copy)
	began := time.Now()
	jobsStarted.Inc()
	err := w.encode(ctx, job, log)
	// Only this error says the index is wrong; a cancelled or failed run says
	// nothing of it. The next PlaybackInfo re-encodes the film.
	if errors.Is(err, errWrongIndex) && ctx.Err() == nil {
		if dropped, dropErr := w.Items.DropKeyframes(ctx, job.Spec.Source, job.Spec.ETag); dropErr != nil {
			log.Error("dropping the wrong keyframe index failed", "source", job.Spec.Source, "error", dropErr)
		} else if !dropped {
			log.Warn("the keyframe index was wrong, but the file changed during the job; its new index stays", "source", job.Spec.Source, "error", err)
		} else {
			log.Error("the film's keyframe index is wrong; it is re-encoded from now on", "source", job.Spec.Source, "error", err)
		}
	}
	state, reason := "done", ""
	switch {
	case err == nil:
		jobsEnded.WithLabelValues("done").Inc()
		log.Info("transcode ended", "elapsed", time.Since(began).Round(time.Millisecond))
	case ctx.Err() != nil || errors.Is(err, errInterrupted) || db.IsAway(err):
		// Cut short, not failed: viewers restart it, and the spec isn't refused.
		// Postgres being away is not the job's fault.
		state, reason = "cancelled", err.Error()
		jobsEnded.WithLabelValues("interrupted").Inc()
		log.Info("transcode interrupted", "reason", reason)
	default:
		state, reason = "failed", err.Error()
		jobsEnded.WithLabelValues("failed").Inc()
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

// fetchImageFile copies the PGS file, or the .idx and .sub of a VobSub pair,
// out of the media store into dir and gives the path ffmpeg opens. The pair
// keeps one name, which is how ffmpeg finds the .sub beside the .idx.
func fetchImageFile(ctx context.Context, media store.Store, dir string, track playback.Stream) (string, error) {
	files := map[string]string{filepath.Join(dir, "track.sup"): track.Path}
	input := filepath.Join(dir, "track.sup")
	if track.SubPath != "" {
		input = filepath.Join(dir, "track.idx")
		files = map[string]string{input: track.Path, filepath.Join(dir, "track.sub"): track.SubPath}
	}
	for local, key := range files {
		body, err := media.Open(ctx, key, 0)
		if err != nil {
			return "", err
		}
		err = writeFile(local, body)
		body.Close()
		if err != nil {
			return "", err
		}
	}
	return input, nil
}

// checkImageFile reads the copied image track on its own before the burn, so a
// file ffmpeg refuses, or holds no picture at all (a .sub of noise reads as
// nothing and would stall the burn), is found out at once. Such a file is
// hidden from the film until it changes, as an unreadable text file is
// (library.MarkUnreadable). Only ffprobe's own answer counts: it exited with
// an error status, or read the file and found no packets. A probe that was
// killed by a signal, timed out or could not start says nothing about the file,
// so the burn goes on and ffmpeg judges it.
func (w *Worker) checkImageFile(ctx context.Context, job claimed, track playback.Stream, log *slog.Logger) error {
	probed, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	out, err := ffprobe(probed, "-count_packets", "-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", track.File)
	var exited *exec.ExitError
	switch {
	case err == nil:
		if packets, _ := strconv.Atoi(strings.TrimSpace(string(out))); packets != 0 {
			return nil
		}
		err = errors.New("it holds no subtitle packets")
	case errors.As(err, &exited) && exited.ExitCode() > 0 && probed.Err() == nil:
	default:
		return nil
	}
	log.Warn("subtitle file not listed: it can't be read", "file", track.Path, "error", err)
	// Not the job's context, which ends with the job.
	marked, stopMark := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopMark()
	if markErr := library.MarkUnreadable(marked, w.Pool, job.Spec.Item, *job.Spec.Subtitle, track.Fingerprint, ""); markErr != nil {
		err = errors.Join(err, markErr)
	}
	return err
}

func writeFile(path string, body io.Reader) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, body); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// errInterrupted marks a job that stopped before its work was done, because
// the worker was going away or no longer owned the job.
var errInterrupted = errors.New("interrupted")

// errWaiting says a finished segment could not be stored because Postgres is
// away. The job is not over: the segment is kept and tried again.
var errWaiting = errors.New("waiting for Postgres")

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
	film := playback.Source{RunTimeTicks: source.RunTimeTicks, Streams: source.Streams}
	if job.Spec.Subtitle != nil && film.SubtitleFingerprint(*job.Spec.Subtitle) != job.Spec.SubtitleFingerprint {
		return errors.New("the subtitle file has changed or is gone")
	}
	if job.Spec.Copy {
		if film.Keyframes, film.VideoEnd, err = w.Items.Keyframes(ctx, source.ID); err != nil {
			return err
		}
	}
	layout := film.Layout(job.Spec.Copy)
	if layout.Count() == 0 {
		return errors.New("the file has no keyframe index to copy from")
	}
	if job.Start >= layout.Count() {
		// Nothing is left to make: a job taken over or queued at the film's end. It
		// ends done, not failed (a failed job refuses the film to every viewer for
		// failureMemory), and a copy never reaches copyStart, which indexes the layout.
		log.Info("transcode has nothing left to make", "start", job.Start, "segments", layout.Count())
		return nil
	}
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
	// An image track is read by the transcode itself, in the same pass as the
	// video, so there is nothing to extract first.
	var image *playback.Stream
	if job.Spec.Subtitle != nil && film.ImageSubtitle(*job.Spec.Subtitle) {
		image = film.Stream(*job.Spec.Subtitle)
	}
	// The first burn of a text track reads the whole file, here, before ffmpeg
	// starts; it is cached for every job after.
	var ass string
	var assets store.Store
	if job.Spec.Subtitle != nil && image == nil {
		if assets, err = w.Media(source.AssetStoreURI); err != nil {
			return err
		}
		if ass, err = w.extractSubtitle(ctx, job, log, source, assets); err != nil {
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
	// An image track beside the film is copied to the scratch directory, where
	// ffmpeg reads it as a second input.
	if image != nil && image.Fingerprint != "" {
		local := *image
		if local.File, err = fetchImageFile(ctx, media, dir, local); err != nil {
			return fmt.Errorf("subtitle track %d: %w", *job.Spec.Subtitle, err)
		}
		image = &local
		if err := w.checkImageFile(ctx, job, local, log); err != nil {
			return fmt.Errorf("subtitle track %d: %w", *job.Spec.Subtitle, err)
		}
	}
	// Only ASS and SSA name fonts; the others are drawn in the default one.
	if job.Spec.Subtitle != nil && (film.SubtitleCodec(*job.Spec.Subtitle) == "ass" || film.SubtitleCodec(*job.Spec.Subtitle) == "ssa") {
		// The segments are kept under a hash that doesn't name the fonts, so a
		// burn drawn without them is what every viewer gets for 24 hours. Any
		// failure here (a store error, a signal, a full disk, a lost lease, no
		// worker) may pass, so it fails this job, to be asked for again. A film
		// whose fonts can never be taken out is not a failure: the fonts job
		// records it as having none (subtitles.FillFonts), and the burn goes on.
		if err := w.putFonts(ctx, job, log, source, assets, filepath.Join(dir, "fonts")); err != nil {
			return fmt.Errorf("fonts of the film: %w", err)
		}
	}
	// limit is the first segment this job may not store yet. It, and the lead
	// state beside it, outlive one ffmpeg: a job goes on into the next chunk
	// with a new one.
	limit := limitFor(layout, job.Start, w.lead())
	if job.Helper {
		// Ahead of its viewers, a helper is bounded by the furthest of them.
		switch at, served, free, err := w.furthest(ctx, job, job.Start); {
		case err == nil && free:
			limit = layout.Count()
		case err == nil && served:
			limit = limitFor(layout, at, w.lead())
		default:
			limit = job.Start // Nothing may be stored until the gate has looked.
		}
	}
	// freed is set while a viewer that has asked for no segment lifts the
	// limit, which is looked at again until it is gone.
	freed := false
	// unbounded is set when the film ends within the lead, or ffmpeg can't be
	// paused here.
	unbounded := limit >= layout.Count()
	limitAt := time.Now()
	chunked, size := w.chunked(job), w.chunkSize()
	// A track inside the film whose cue points the scan read: a run that begins
	// mid-film reads the packet of the cue showing at its cut (a few small reads
	// of the film, under the job's lease), and opens early enough to see it.
	if image != nil && image.Fingerprint == "" {
		var cues library.ImageCues
		if err := w.Pool.QueryRow(ctx, "SELECT image_cues FROM media_sources WHERE id = $1::uuid", source.ID).Scan(&cues); err != nil {
			return err
		}
		if track, ok := cues[image.Index]; ok {
			local := *image
			local.OpenAt = func(t float64) (open float64, err error) {
				err = w.awaitJob(ctx, job, log, func(ctx context.Context) (err error) {
					open, err = track.OpenAt(ctx, media, source.Key, source.Size, t)
					return err
				})
				return open, err
			}
			image = &local
		}
	}
	next := job.Start // The next segment to store.

	// run is the job's ffmpeg, from segment begin.
	run := func(begin int) (err error) {
		var cmd *exec.Cmd
		var gather *gatherer
		if job.Spec.Copy {
			next = begin
			video, audio := copyStart(film.Keyframes, layout, begin)
			cmd, err = CopyCommand(ctx, input, job.Spec, film.VideoCodec(), video, audio, dir)
			gather = newGatherer(dir, layout, film.Keyframes[0])
		} else {
			next = begin
			for _, stale := range globOr(filepath.Join(dir, "*.m4s")) {
				os.Remove(stale)
			}
			cmd, err = Command(ctx, input, job.Spec, next, dir, ass, image)
		}
		if err != nil {
			return err
		}
		// Stopped by hand below so the log says why. CommandContext's kill is the
		// backstop, so no path leaves ffmpeg running.
		var stderr tail
		cmd.Stderr = &stderr
		cmd.Stdout = job.progress
		subtitles.DieWithParent(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()

		// paused says whether ffmpeg is stopped at the limit.
		paused := false
		// chunkDone is set when the segment stored last ended a chunk another
		// job has.
		chunkDone := false
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
				if next >= layout.Count() {
					return false, nil // A copied run has fragments past the end.
				}
				if next < begin {
					name := filepath.Join(dir, strconv.Itoa(next)+".m4s")
					if _, err := os.Stat(name); err != nil {
						return false, nil
					}
					os.Remove(name)
					next++
					continue
				}
				if !final && next >= limit {
					return false, nil // Made, for when the viewers come to it.
				}
				name := filepath.Join(dir, strconv.Itoa(next)+".m4s")
				if gather != nil {
					// A segment gathered before, whose store waited for Postgres, is
					// already built and its fragments are gone: store it as it is.
					if _, err := os.Stat(name); err != nil {
						if built, err := gather.segment(ctx, next, final); err != nil || !built {
							return false, err
						}
					}
				} else if _, err := os.Stat(name); err != nil {
					return false, nil
				} else if _, more := os.Stat(filepath.Join(dir, strconv.Itoa(next+1)+".m4s")); final && more != nil {
					if err := checkLast(dir, next, layout); err != nil {
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
				// At a chunk start the same ffmpeg goes on unless another job has
				// the chunk (a helper, or a seek's job), so a job nobody helps is
				// one ffmpeg, as it was before chunks.
				if chunked && next%size == 0 {
					free, err := w.chunkFree(ctx, job, next)
					if db.IsAway(err) {
						free, err = true, nil // The same ffmpeg goes on; it only does what a helper may also have done.
					}
					if err != nil {
						return false, err
					}
					chunkDone = !free
					return false, nil
				}
			}
		}

		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		beat := time.NewTicker(w.heartbeat())
		loggedSlow := false
		waiting := false // A finished segment is kept for Postgres.
		defer beat.Stop()
		// A helper cancelled for a new viewer (preemptHelper) beats at once, so
		// it gives its cores back now rather than at its next heartbeat. It also
		// beats as soon as it listens, for a cancel that came before it did (a
		// helper is often the newest job, cancelled as it starts).
		var preempted <-chan struct{}
		if job.Helper {
			wake, unsubscribe := w.Hub.Subscribe(preemptHint)
			defer unsubscribe()
			preempted = wake
			beat.Reset(time.Millisecond)
		}
		lastBeat := time.Now()
		progressed, progressAt := next, time.Now()
		// gate pauses ffmpeg once everything below the limit is made, so what it
		// reads past the limit is what it reads before the next look, and lets it
		// go on when the limit has moved.
		// It reports false when the job is paused and none of its viewers is
		// within its window any more: they have seeked past it or gone, and its
		// slot is better given back than held for them.
		gate := func() bool {
			if unbounded {
				return true
			}
			// Segment limit-1 is made when it is in scratch (a copy: when its
			// fragments are), or already stored.
			made := func() bool {
				if next >= limit {
					return true
				}
				if gather != nil {
					ok, _ := gather.reached(ctx, limit-1)
					return ok
				}
				_, err := os.Stat(filepath.Join(dir, strconv.Itoa(limit-1)+".m4s"))
				return err == nil
			}
			wait := limit < layout.Count() && made()
			if (wait || paused || freed) && time.Since(limitAt) >= leadCheck {
				limitAt = time.Now()
				at, served, free, err := w.furthest(ctx, job, next)
				switch {
				case err != nil:
					log.Warn("reading the viewers' segments failed", "error", err)
				case free:
					limit, freed = layout.Count(), true
				case paused && !served:
					return false
				default:
					limit, freed = limitFor(layout, at, w.lead()), false
				}
				wait = limit < layout.Count() && made()
			}
			switch {
			case wait && !paused:
				if err := pauseProcess(cmd.Process, true); err != nil {
					log.Warn("ffmpeg cannot be paused here, so this job runs without a lead limit", "error", err)
					limit, unbounded = layout.Count(), true
					return true
				}
				paused = true
				job.progress.pause(true)
				log.Debug("pausing ffmpeg at its lead", "limit", limit)
			case !wait && paused:
				if err := pauseProcess(cmd.Process, false); err != nil {
					log.Error("resuming ffmpeg failed", "error", err)
					return true
				}
				paused = false
				job.progress.pause(false)
				log.Debug("resuming ffmpeg", "limit", limit)
			}
			return true
		}
		for {
			select {
			case err := <-exited:
				caughtUp, uploadErr := upload(true)
				// What ffmpeg made waits for Postgres, for as long as the lease it last renewed lasts.
				for errors.Is(uploadErr, errWaiting) && time.Since(lastBeat) <= w.lease() && ctx.Err() == nil {
					time.Sleep(100 * time.Millisecond)
					caughtUp, uploadErr = upload(true)
				}
				if errors.Is(uploadErr, errWaiting) {
					uploadErr = fmt.Errorf("%w: %v", errInterrupted, uploadErr)
				}
				if err != nil {
					return fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
				}
				if uploadErr != nil {
					return fmt.Errorf("%w: %s", uploadErr, stderr.String())
				}
				if next <= begin {
					return fmt.Errorf("ffmpeg ended without a segment: %s", stderr.String())
				}
				// A cut input can end exactly between two segments, leaving no
				// partial one to notice.
				if next < layout.Count() && !caughtUp {
					return fmt.Errorf("the input ended early: ffmpeg stopped after segment %d of %d: %s", next-1, layout.Count(), stderr.String())
				}
				return nil
			case <-poll.C:
				caughtUp, err := upload(false)
				if errors.Is(err, errWaiting) {
					// The segment stays where it is, and ffmpeg and the job go on; the heartbeat
					// ends the job if the lease runs out first.
					if !waiting {
						log.Warn("a finished segment is kept until Postgres answers", "error", err)
					}
					waiting, caughtUp, err = true, false, nil
				} else {
					waiting = false
				}
				if err != nil {
					stop("upload failed")
					return err
				}
				if caughtUp {
					stop("caught up with stored segments")
					return nil
				}
				if chunkDone {
					stop("the next chunk is another job's")
					return nil
				}
				if !gate() {
					// What it made stays stored, and a viewer who comes back to
					// the rest starts a job there.
					w.Pool.Exec(context.WithoutCancel(ctx), "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1 AND state = 'running'", job.ID) //nolint:errcheck
					stop("its viewers have left it behind")
					return nil
				}
			case <-preempted:
				beat.Reset(time.Millisecond)
			case <-beat.C:
				beat.Reset(w.heartbeat())
				if time.Since(lastBeat) > w.lease() {
					// The worker itself was stopped (a frozen host, a paused
					// container) for longer than a lease: another worker may own
					// the job now, as when the database was unreachable. It gives
					// the job up as interrupted, never as failed: the time without
					// a segment was the worker's, not a hung ffmpeg's, and a failed
					// job refuses a good film to every viewer for failureMemory
					// (JS-066's queue run).
					stop("lease expired while the worker was stopped")
					return fmt.Errorf("%w: lease expired while the worker was stopped", errInterrupted)
				}
				alive, wanted, err := w.beat(ctx, job)
				switch {
				case err == nil:
					lastBeat = time.Now()
					// The operator's only signal beside GET /Sessions: jellyfin-web
					// shows neither the speed nor the worker.
					if speed, ok := w.measuredSpeed(job); ok {
						if speed < 1 && !loggedSlow {
							log.Warn("transcode is slower than real time, so playback will stall", "speed", speed, "worker", w.ID)
						}
						loggedSlow = speed < 1
						if alive && chunked && !w.NoHelpers && w.needsHelp(job) {
							if err := w.startHelper(ctx, job, next, layout); err != nil && ctx.Err() == nil {
								log.Warn("starting a helper job failed", "error", err)
							}
						}
					}
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
				if paused {
					// A stopped ffmpeg makes no segment and writes nothing, which
					// the stall check and the scratch sweep would both read as
					// a hang.
					now := time.Now()
					progressAt = now
					os.Chtimes(dir, now, now) //nolint:errcheck
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
				reason := "worker is shutting down"
				if errors.Is(context.Cause(ctx), errLockLost) {
					reason = errLockLost.Error()
				}
				stop(reason)
				return fmt.Errorf("%w: %s", errInterrupted, reason)
			}
		}
	}

	return run(job.Start)
}

// globOr is filepath.Glob without the error, which only a bad pattern has.
func globOr(pattern string) []string {
	names, _ := filepath.Glob(pattern)
	return names
}

// extractSubtitle returns the job's subtitle track as ASS. Burning shares the
// track's extraction with everyone who asks for it (see FetchSubtitle).
func (w *Worker) extractSubtitle(ctx context.Context, job claimed, log *slog.Logger, source items.Source, assets store.Store) (string, error) {
	var track subtitles.Track
	err := w.awaitJob(ctx, job, log, func(ctx context.Context) (err error) {
		track, err = FetchSubtitle(ctx, w.Pool, w.Hub, assets, job.Spec.Item, source, *job.Spec.Subtitle)
		return err
	})
	return track.ASS, err
}

// awaitJob runs fetch, which waits for a subtitle job (the track, or the film's
// fonts), which can take minutes. It keeps the lease alive meanwhile, and stops
// waiting when the job is lost or nobody wants it, so it doesn't hold any cores
// for a cancelled job.
func (w *Worker) awaitJob(ctx context.Context, job claimed, log *slog.Logger, fetch func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fetch(ctx) }()
	beat := time.NewTicker(w.heartbeat())
	defer beat.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-beat.C:
			alive, wanted, err := w.beat(ctx, job)
			switch {
			case err != nil:
				log.Warn("transcode heartbeat failed", "error", err)
			case !alive:
				cancel()
				<-done
				return fmt.Errorf("%w: %s", errInterrupted, w.lostReason(context.WithoutCancel(ctx), job))
			case !wanted:
				cancel()
				<-done
				w.Pool.Exec(context.WithoutCancel(ctx), "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1 AND state = 'running'", job.ID) //nolint:errcheck
				return fmt.Errorf("%w: no viewer is watching", errInterrupted)
			}
		}
	}
}

// lostReason says why the job is no longer ours to run: a seek replaced it, or
// something took the lease from us.
func (w *Worker) lostReason(ctx context.Context, job claimed) string {
	var state, reason string
	w.Pool.QueryRow(ctx, "SELECT state, COALESCE(error, '') FROM transcode_jobs WHERE id = $1", job.ID).Scan(&state, &reason) //nolint:errcheck
	if state == "cancelled" && reason != "" {
		return reason // A helper stopped for a new viewer (preemptHelper).
	}
	if state == "cancelled" {
		return "a newer job replaced this one, or it was cancelled"
	}
	if state == "running" {
		return "the worker was taken for dead: its liveness lock and row are gone"
	}
	return "lease lost: the job is " + state
}

// checkLast rejects a final segment that ffmpeg cut short because its input
// ended early, which it does without an error when the read fails midway.
func checkLast(dir string, n int, layout playback.Layout) error {
	if n < layout.Count()-1 {
		return fmt.Errorf("the input ended early: ffmpeg stopped after segment %d of %d", n, layout.Count())
	}
	if n > layout.Count()-1 {
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
	if _, ticks := layout.Segment(n); math.Abs(length-float64(ticks)/ticksPerSecond) > 1.5 {
		return fmt.Errorf("the input ended early: the last segment is %.2fs, not %.2fs", length, float64(ticks)/ticksPerSecond)
	}
	return nil
}

// measuredSpeed is the job's speed, unless it is paused at its lead limit,
// where it is idle by design and has no speed worth reporting.
func (w *Worker) measuredSpeed(job claimed) (float64, bool) {
	if job.progress == nil || job.progress.pausedValue() != 0 {
		return 0, false
	}
	return job.progress.speed()
}

// helpSpan is how many running seconds a job's speed is measured over before it
// asks for helpers: ffmpeg's output comes in bursts, and a job that makes 1.4x
// can show 0.9x over its first two seconds.
const helpSpan = 2

// needsHelp says whether the job runs below HelpBelow, over at least helpSpan.
// A job that has made nothing yet is starting, not slow: ffmpeg's first output
// comes a second or two after it begins, and a restart at a late segment has
// seconds of warm-up to read before its first frame.
func (w *Worker) needsHelp(job claimed) bool {
	speed, ok := job.progress.speedOver(helpSpan)
	return ok && speed > 0 && speed < w.helpBelow()
}

// beat extends the lease, and stores the job's measured speed for GET /Sessions.
// A paused job keeps the speed it had, since it is idle by design. alive is
// false when the job is no longer ours to run; wanted is false when no viewer
// has asked for the spec lately.
func (w *Worker) beat(ctx context.Context, job claimed) (alive, wanted bool, err error) {
	var speed *float64
	if value, ok := w.measuredSpeed(job); ok {
		speed = &value
	}
	err = w.Pool.QueryRow(ctx, `UPDATE transcode_jobs j SET lease_expires_at = now() + make_interval(secs => $3),
			encode_speed = COALESCE($5, j.encode_speed)
		WHERE j.id = $1 AND j.worker = $2 AND j.state = 'running' AND `+registeredAs("$2")+`
		RETURNING EXISTS (SELECT 1 FROM transcode_viewers v WHERE v.spec_hash = j.spec_hash AND NOT v.stopped
			AND v.last_seen_at > now() - make_interval(secs => $4))`,
		job.ID, w.ID, w.lease().Seconds(), w.idle().Seconds(), speed).Scan(&wanted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, wanted, err
}

// upload stores one finished segment, with the init segment alongside the
// job's first. Contents depend on the spec and number only, so a repeat is
// harmless.
func (w *Worker) upload(ctx context.Context, job claimed, n int, name string) error {
	// A put overwrites, and the update below comes after it, so this is what
	// keeps a worker whose lease is gone (stopped for longer than the lease, cut
	// off from Postgres, reaped while its connection stood) from overwriting
	// what the job's new owner stored. The replacement's first segment is
	// seconds away when this passes; only a put slower than that can overlap.
	// A database that does not answer is the same as a lease that is gone.
	// The answer is how much lease Postgres says is left, measured when it ran
	// the query, which was no earlier than now. So the lease ends no sooner than
	// asked+left, whatever the round trip, and whatever stops this worker before
	// a put (a database that answers late, a SIGSTOP): every put checks the
	// clock against that.
	fenced, cancel := context.WithTimeout(ctx, w.lease()/2)
	asked := time.Now()
	var left float64
	err := w.Pool.QueryRow(fenced, `SELECT EXTRACT(EPOCH FROM lease_expires_at - now())::float8 FROM transcode_jobs
		WHERE id = $1 AND worker = $2 AND state = 'running' AND `+registeredAs("$2"), job.ID, w.ID).Scan(&left)
	cancel()
	if db.IsAway(err) {
		// Nothing is stored until the check passes (invariant 37); the segment
		// waits where it is, and the lease says how long.
		return fmt.Errorf("%w: checking the job is still ours: %v", errWaiting, err)
	}
	if errors.Is(err, pgx.ErrNoRows) || err == nil && left <= 0 {
		return fmt.Errorf("%w: %s", errInterrupted, w.lostReason(ctx, job))
	}
	if err != nil {
		// Unknown is not failed: the job is cut short and restarted, and the
		// spec is not refused for a database that was slow.
		return fmt.Errorf("%w: checking the job is still ours: %v", errInterrupted, err)
	}
	ownedUntil := asked.Add(time.Duration(left * float64(time.Second)))
	put := func(key, file string) error {
		if !time.Now().Before(ownedUntil) {
			return fmt.Errorf("%w: the lease ran out before the segment could be stored", errInterrupted)
		}
		return w.put(ctx, key, file)
	}
	if n == job.Start {
		if err := put(SegmentKey(job.Hash, -1), filepath.Join(filepath.Dir(name), "init.mp4")); err != nil {
			return err
		}
	}
	if err := put(SegmentKey(job.Hash, n), name); err != nil {
		return err
	}
	tag, err := w.Pool.Exec(ctx, `UPDATE transcode_jobs SET next_segment = $2, lease_expires_at = now() + make_interval(secs => $4)
		WHERE id = $1 AND worker = $3 AND state = 'running'`, job.ID, n+1, w.ID, w.lease().Seconds())
	if db.IsAway(err) {
		// Stored, and not yet recorded: the file stays, and the next try stores it again.
		return fmt.Errorf("%w: recording the segment: %v", errWaiting, err)
	}
	os.Remove(name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// A copied run can have many segments waiting; don't keep storing them.
		return fmt.Errorf("%w: %s", errInterrupted, w.lostReason(ctx, job))
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
		`DELETE FROM subtitle_jobs WHERE state IN ('done', 'failed') AND ended_at < now() - interval '1 day'`,
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

// putFonts puts the fonts attached to the film in dir, for libass to draw a
// burnt-in track with, as the player draws a track it renders itself. It waits
// for the film's fonts job, which takes them out of the film once for everyone
// (see FetchFonts); a film with none has none.
func (w *Worker) putFonts(ctx context.Context, job claimed, log *slog.Logger, source items.Source, assets store.Store, dir string) error {
	if len(subtitles.Fonts(source)) == 0 {
		return nil
	}
	var hashes map[int]string
	err := w.awaitJob(ctx, job, log, func(ctx context.Context) (err error) {
		hashes, err = FetchFonts(ctx, w.Pool, w.Hub, assets, job.Spec.Item, source)
		return err
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	restored := false
	for index, sum := range hashes {
		path := filepath.Join(dir, strconv.Itoa(index))
		err := copyFont(ctx, assets, sum, path)
		if errors.Is(err, errBadFont) {
			// Cut short or changed in the store. Taken out so the fonts job
			// puts a whole one in its place instead of finding this one there.
			if err := assets.Delete(ctx, subtitles.FontKey(sum)); err != nil {
				return err
			}
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) && !restored {
			// A burn-in viewer never asks for the font itself, so nobody else
			// would put it back. Run the fonts job again and burn with what it
			// restores; segments are kept for every viewer, so a burn without
			// the font must not be served.
			restored = true
			if err := w.restoreFonts(ctx, job, log, source, assets); err != nil {
				return err
			}
			fonts, _, readErr := subtitles.ReadFonts(ctx, assets, source)
			if readErr != nil {
				return readErr
			}
			sum, ok := fonts[index]
			if !ok {
				continue
			}
			err = copyFont(ctx, assets, sum, path)
		}
		if errors.Is(err, errBadFont) {
			// Left for the next burn to restore; this one must not draw with it.
			err = errors.Join(err, assets.Delete(ctx, subtitles.FontKey(sum)))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var errBadFont = errors.New("a font in the store does not match its hash")

// copyFont copies the font stored under its SHA-256 sum to path, and fails with
// errBadFont if the bytes read are not the ones the sum names.
func copyFont(ctx context.Context, assets store.Store, sum, path string) error {
	body, err := assets.Open(ctx, subtitles.FontKey(sum), 0)
	if err != nil {
		return err
	}
	defer body.Close()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(file, hash), body)
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != sum {
		return errBadFont
	}
	return nil
}

// restoreFonts runs the film's fonts job again, because a font it stored is
// gone, and waits for it like putFonts waits for the first run.
func (w *Worker) restoreFonts(ctx context.Context, job claimed, log *slog.Logger, source items.Source, assets store.Store) error {
	if err := StartFonts(ctx, w.Pool, assets, job.Spec.Item, source, true); err != nil {
		return err
	}
	return w.awaitJob(ctx, job, log, func(ctx context.Context) error {
		return awaitJob(ctx, w.Pool, w.Hub, subtitleJob{Key: subtitles.FontsKey(source.ID, source.ETag), Kind: "fonts", Item: job.Spec.Item, Source: source.ID})
	})
}
