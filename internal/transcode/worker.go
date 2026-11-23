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
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/mediatool"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// Worker claims jobs and runs ffmpeg for them.
type Worker struct {
	Pool     *pgxpool.Pool
	Items    *items.Repo
	Segments store.Store
	// SegmentsURI is where Segments is, for an error to name.
	SegmentsURI string
	Hub         *Hub
	Log         *slog.Logger
	Media       func(uri string) (store.Store, error)

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
	// Budget is the most bytes the transcode store may hold, and 0 is no budget
	// (sweep.go). SweepEvery is the sweep period; zero is a minute.
	Budget     int64
	SweepEvery time.Duration
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
	// SubtitleStall is how long a text-track extraction may make no progress
	// before it is ended; zero is a minute.
	SubtitleStall time.Duration
	// LivenessConfig connects the liveness lock (Present). It defaults to the
	// pool's own, which is right except in a test that wants the lock's
	// connection to go through something else.
	LivenessConfig *pgx.ConnConfig
	// KeeperConfig connects the lease keeper (lease_keeper.go), apart from the
	// liveness lock's connection. It defaults to the pool's own.
	KeeperConfig *pgx.ConnConfig

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

	// leases keeps the lease of every job the worker runs (lease_keeper.go).
	keeperOnce sync.Once
	leases     *leaseKeeper
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
			job, ok, err := w.claimIn(ctx, sessionOf(jobCtx))
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

	progress *progress  // How fast ffmpeg goes; set by Run.
	lease    *leasedJob // Keeps the job's lease; set by run.
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
		// viewers at or before its chunk are its, and so are those a request
		// would join it for (past its chunk, within catchUpSegments of its next
		// segment, DOMAINS 37). The furthest of them bounds it, not its start. A
		// viewer far past that has a job of its own, and the helpers of that
		// one: its lead is not this helper's to fill.
		from, to = 0, max(job.Start+w.chunkSize(), next+catchUpSegments)
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

// registeredIn is registeredAs for a claim: the row must also be of the session
// the claim was sent in, unless it names none (a worker with no Present).
func registeredIn(idExpr, sessionExpr string) string {
	return `EXISTS (SELECT 1 FROM transcode_nodes WHERE id = ` + idExpr + ` AND (` + sessionExpr + ` = '' OR session = ` + sessionExpr + `))`
}

// startup is extra lease for a claimed job, which is not heartbeating until
// ffmpeg is running.
const startup = 20 * time.Second

func (w *Worker) claim(ctx context.Context) (claimed, bool, error) {
	return w.claimIn(ctx, "")
}

// claimIn claims a job for the session the worker's context is of. The statement
// is sent while the worker holds its row and commits later, and in between the
// worker may have been forgotten, stopped its jobs and put its row back: the job
// would then be taken under a context that is cancelled. It commits only while
// the row is still of the session it was sent in, so it is left for the new one.
func (w *Worker) claimIn(ctx context.Context, session string) (claimed, bool, error) {
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
	if _, registered := mine.free[w.ID]; !registered {
		return job, false, nil
	}
	local, copies := w.holding()
	mine.free[w.ID] = min(mine.free[w.ID], mine.cores[w.ID]-local)
	mine.copies[w.ID] = max(mine.copies[w.ID], copies)
	// A helper goes to a worker that has no job of its spec, or after a second
	// to one that has only helpers of it, if none took it (a dead helper's
	// replacement, with the other workers busy); never to one running a job of
	// its spec that is not a helper (D-052). No room is kept for a viewer who has not asked (D-046);
	// one who has goes before any helper. A job of another output version is not
	// here, as if it were not there; one that does not fit is left for a worker
	// that has the cores, and a later one that fits goes first.
	sort.SliceStable(jobs, func(i, j int) bool { return !jobs[i].Helper && jobs[j].Helper })
	for _, queued := range jobs {
		if queued.Worker != "" || !mine.fits(w.ID, queued) {
			continue
		}
		var raw []byte
		err := w.Pool.QueryRow(ctx, `UPDATE transcode_jobs SET state = 'running', worker = $1,
				lease_expires_at = now() + make_interval(secs => $2)
			WHERE `+registeredIn("$1", "$5::text")+` AND id = $3 AND state = 'queued' AND created_at > now() - make_interval(secs => $4)
				AND (NOT helper OR (NOT EXISTS (SELECT 1 FROM transcode_jobs v WHERE v.spec_hash = transcode_jobs.spec_hash AND v.worker = $1 AND v.state = 'running' AND NOT v.helper)
					AND (created_at < now() - make_interval(secs => 1)
						OR NOT EXISTS (SELECT 1 FROM transcode_jobs o WHERE o.spec_hash = transcode_jobs.spec_hash AND o.worker = $1 AND o.state = 'running'))))
				-- A helper waits while a viewer's job is queued: the viewer who has asked goes first, whatever this worker read earlier.
				AND (NOT helper OR NOT EXISTS (SELECT 1 FROM transcode_jobs q WHERE NOT q.helper AND q.state = 'queued' AND q.output_version = transcode_jobs.output_version
					AND q.created_at > now() - make_interval(secs => $4)))
			RETURNING id, spec_hash, spec, start_segment, helper`, w.ID, (w.lease()+startup).Seconds(), queued.ID, queue.Seconds(), session).
			Scan(&job.ID, &job.Hash, &raw, &job.Start, &job.Helper)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // Another worker took it first.
		}
		if err != nil {
			return job, false, err
		}
		if err := json.Unmarshal(raw, &job.Spec); err != nil {
			// No worker could run it, and its lease would only lapse: end it here, and go on to the next.
			w.failUndecodable(ctx, job.ID, err)
			job = claimed{}
			continue
		}
		return job, true, nil
	}
	return job, false, nil
}

// failUndecodable ends the job this worker has just claimed, whose spec it cannot read.
func (w *Worker) failUndecodable(ctx context.Context, id int64, cause error) {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	reason := "the job's spec could not be decoded: " + cause.Error()
	w.Log.Error("a claimed transcode job's spec could not be decoded", "job", id, "error", cause)
	if _, err := w.Pool.Exec(finish, `UPDATE transcode_jobs SET state = 'failed', error = $2, ended_at = now()
		WHERE id = $1 AND state = 'running' AND worker = $3`, id, reason, w.ID); err != nil {
		w.Log.Error("recording the end of a transcode job failed", "job", id, "error", err)
	}
}

// run runs one job and records how it ended. A job someone else already
// cancelled or replaced keeps that state.
func (w *Worker) run(ctx context.Context, job claimed) {
	log := w.Log.With("job", job.ID, "spec", job.Hash, "item", job.Spec.Item, "source", job.Spec.Source, "start", job.Start)
	log.Info("transcode started", "copy", job.Spec.Copy)
	// A helper cancelled for a new viewer (preemptHelper) renews at once, so it
	// gives its cores back now rather than at its next heartbeat. The first
	// renewal is at once too, for a cancel that came before the job listened (a
	// helper is often the newest job, cancelled as it starts).
	var preempted <-chan struct{}
	if job.Helper {
		wake, unsubscribe := w.Hub.Subscribe(preemptHint)
		defer unsubscribe()
		preempted = wake
	}
	// From here the keeper cancels ctx when the job is no longer to be run, and
	// the job's every stage, a fetch or a call that hangs included, ends with it.
	ctx, job.lease = w.keepJobLease(ctx, job, preempted)
	ctx = withTurn(ctx, job.lease)
	defer job.lease.stop()
	began := time.Now()
	jobsStarted.Inc()
	err := w.encode(ctx, job, log)
	// Read before cause: the keeper may have cancelled ctx on seeing the row cancelled.
	left := errors.Is(err, errHelperLeft)
	err = cause(ctx, err)
	if errors.Is(err, errNoViewer) {
		// Nobody watches: the slot is given back and the job is no failure.
		w.cancelUnwanted(ctx, job)
		err = nil
	}
	// Only this error says the index is wrong; a cancelled or failed run says
	// nothing of it. The next PlaybackInfo re-encodes the film.
	if errors.Is(err, errWrongIndex) && ctx.Err() == nil && job.Spec.Recode == "" {
		if dropped, dropErr := w.Items.DropKeyframes(ctx, job.Spec.Source, job.Spec.ETag); dropErr != nil {
			log.Error("dropping the wrong keyframe index failed", "source", job.Spec.Source, "error", dropErr)
		} else if !dropped {
			log.Warn("the keyframe index was wrong, but the file changed during the job; its new index stays", "source", job.Spec.Source, "error", err)
		} else {
			log.Error("the film's keyframe index is wrong; it is re-encoded from now on", "source", job.Spec.Source, "error", err)
		}
	}
	state, reason, again := "done", "", false
	switch {
	case left:
		// Its row was written cancelled before its ffmpeg was ended, and what it stored stays.
		state = "cancelled"
		jobsEnded.WithLabelValues(resultHelperLeft).Inc()
		log.Info("transcode ended", "reason", resultHelperLeft, "elapsed", time.Since(began).Round(time.Millisecond))
	case err == nil:
		jobsEnded.WithLabelValues("done").Inc()
		log.Info("transcode ended", "elapsed", time.Since(began).Round(time.Millisecond))
	default:
		// A job cut short is cancelled, not failed: viewers restart it, and the spec isn't refused.
		if state, reason = endState(ctx, err); state == "cancelled" {
			jobsEnded.WithLabelValues("interrupted").Inc()
			log.Info("transcode interrupted", "reason", reason)
		} else {
			jobsEnded.WithLabelValues("failed").Inc()
			again = notTheFiles(err)
			log.Error("transcode failed", "error", err, "again", again)
		}
	}
	// A fresh context: ctx may be why we stopped.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// The end of a job is a sign of life: a worker that was stopped past the lease and ends its job is seen in the
	// same statement, so no viewer's question finds the job's row ended and its worker not counted.
	if _, err := w.Pool.Exec(finish, `WITH seen AS (UPDATE transcode_nodes SET seen_at = now() WHERE $4 = id)
		UPDATE transcode_jobs SET state = $2, error = NULLIF($3, ''), ended_at = now(), again = $5
		WHERE id = $1 AND state = 'running' AND worker = $4`, job.ID, state, reason, w.ID, again); err != nil {
		log.Error("recording the end of a transcode job failed", "error", err)
	}
	Notify(finish, w.Pool, segmentHint(job.Hash)) // Waiters should see the new state now.
}

// fetchImageFile copies the PGS file, or the .idx and .sub of a VobSub pair,
// out of the media store into dir and gives the path ffmpeg opens. The pair
// keeps one name, which is how ffmpeg finds the .sub beside the .idx.
func fetchImageFile(ctx context.Context, media store.Store, uri, dir string, track playback.Stream) (string, error) {
	files := map[string]string{filepath.Join(dir, "track.sup"): track.Path}
	input := filepath.Join(dir, "track.sup")
	if track.SubPath != "" {
		input = filepath.Join(dir, "track.idx")
		files = map[string]string{input: track.Path, filepath.Join(dir, "track.sub"): track.SubPath}
	}
	for local, key := range files {
		body, err := media.Open(ctx, key, 0)
		if err != nil {
			return "", fromStore(uri, err)
		}
		err = writeFile(local, turning(ctx, storeBody{uri, body}))
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
	probed, stopProbe := context.WithTimeout(ctx, 10*time.Second)
	defer stopProbe()
	defer w.turnWhile(ctx)() // The probe is bounded by its own timeout; a long one is no stall.
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

// errHelperLeft is how a helper's run ends when no viewer is left for it. It is no failure and no
// interruption, so it is counted under a reason of its own.
var errHelperLeft = errors.New("the helper's viewers have left it behind")

// errWaiting says a finished segment could not be stored because Postgres is
// away. The job is not over: the segment is kept and tried again.
var errWaiting = errors.New("waiting for Postgres")

// encode runs ffmpeg and uploads segments as they finish. It returns nil when
// the job is over for a good reason: the file ended, it caught up with stored
// segments, or it was stopped because nobody needs it. No ffmpeg outlives it.
func (w *Worker) encode(ctx context.Context, job claimed, log *slog.Logger) error {
	source, found, err := w.Items.Source(ctx, job.Spec.Item, job.Spec.Source)
	if err != nil {
		return fromPool(err)
	}
	if !found || source.ETag != job.Spec.ETag {
		return errors.New("the source file has changed or is gone")
	}
	film := playback.Source{RunTimeTicks: source.RunTimeTicks, Streams: source.Streams}
	if job.Spec.Subtitle != nil && film.SubtitleFingerprint(*job.Spec.Subtitle) != job.Spec.SubtitleFingerprint {
		return errors.New("the subtitle file has changed or is gone")
	}
	// A copy, and the encode that stands in for a copy whose index was found wrong, are cut on the index.
	fragments := job.Spec.Copy || job.Spec.Recode != ""
	switch {
	case job.Spec.Copy:
		film.Keyframes, film.VideoEnd, err = w.Items.Keyframes(ctx, source.ID)
	case job.Spec.Recode != "":
		film.Keyframes, film.VideoEnd, err = w.Items.DroppedKeyframes(ctx, source.ID)
	}
	if err != nil {
		return fromPool(err)
	}
	layout := film.Layout(fragments)
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
		return fromStore(source.StoreURI, err)
	}
	read := &readWatch{}
	input, closeInput, err := store.InputWithFailure(ctx, media, source.Key, source.Size, read.fail)
	if err != nil {
		return fromStore(source.StoreURI, err)
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
		if local.File, err = fetchImageFile(ctx, media, source.StoreURI, dir, local); err != nil {
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
	// An AV1 fallback is made by one ffmpeg for each segment (AV1SegmentCommand).
	segmentRuns := job.Spec.Recode == "av1"
	// A track inside the film that is stored (JS-089): a run that begins mid-film
	// draws the cue showing at its cut from the stored file, read as a second
	// input, and opens the film at its cut and no earlier. The read of the file
	// ends with the job.
	var feed *trackRead
	stored := false
	if image != nil && image.Fingerprint == "" && job.Start > 0 {
		track, found, err := w.findStoredTrack(ctx, source, image.Index)
		if err != nil {
			return err
		}
		if found {
			stored = true
			feed = newTrackRead(ctx)
			defer feed.end()
			local := *image
			local.Stored = func(t float64) (io.ReadCloser, error) { return feed.open(track, t) }
			image = &local
		}
	}
	// A track inside the film whose cue points the scan read: a run that begins
	// mid-film reads the packet of the cue showing at its cut (a few small reads
	// of the film, under the job's lease), and opens early enough to see it.
	if image != nil && image.Fingerprint == "" && !stored {
		var cues library.ImageCues
		if err := w.Pool.QueryRow(ctx, "SELECT image_cues FROM media_sources WHERE id = $1::uuid", source.ID).Scan(&cues); err != nil {
			return fromPool(err)
		}
		if track, ok := cues[image.Index]; ok {
			local := *image
			local.OpenAt = func(t float64) (open float64, err error) {
				err = w.awaitJob(ctx, job, log, func(ctx context.Context) (err error) {
					open, err = track.OpenAt(ctx, media, source.Key, source.Size, t)
					return fromStore(source.StoreURI, err)
				})
				return open, err
			}
			image = &local
		}
	}
	next := job.Start // The next segment to store.

	// room holds the job, before it starts the ffmpeg for segment n, until n is within the lead of its viewers, as gate
	// holds one that runs. It reports false when the job is to end: its viewers have left it behind.
	room := func(n int) (bool, error) {
		for !unbounded && (n >= limit || freed) {
			job.lease.turn()
			if time.Since(limitAt) >= leadCheck {
				limitAt = time.Now()
				at, served, free, err := w.furthest(ctx, job, n)
				switch {
				case err != nil:
					log.Warn("reading the viewers' segments failed", "error", err)
				case free:
					limit, freed = layout.Count(), true
				case !served && n >= limit:
					w.cancelUnwanted(ctx, job)
					return false, nil
				default:
					limit, freed = limitFor(layout, at, w.lead()), false
				}
			}
			if n < limit {
				break
			}
			select {
			case <-ctx.Done():
				return false, endedBy(ctx)
			case <-time.After(100 * time.Millisecond):
			}
		}
		return true, nil
	}

	// run is the job's ffmpeg, from segment begin.
	run := func(begin int) (err error) {
		var cmd *exec.Cmd
		var gather *gatherer
		if fragments {
			next = begin
			gather = newGatherer(dir, layout, film.Keyframes[0])
			if job.Spec.Recode == "av1" {
				// One ffmpeg for this segment alone; the job goes on with the next below.
				for _, stale := range globOr(filepath.Join(dir, "*.m4s")) {
					os.Remove(stale)
				}
				os.Remove(filepath.Join(dir, "index.m3u8"))
				video, _ := recodeStart(film.Keyframes, layout, begin)
				cmd, err = AV1SegmentCommand(ctx, input, job.Spec, video, recodeEnd(film.Keyframes, layout, begin), dir)
				gather.slack = recodeSlack
			} else if job.Spec.Recode != "" {
				video, audio := recodeStart(film.Keyframes, layout, begin)
				cmd, err = RecodeCommand(ctx, input, job.Spec, film.VideoCodec(), video, audio, recodeCuts(film.Keyframes, layout, begin), dir)
				gather.slack = recodeSlack
			} else {
				video, audio := copyStart(film.Keyframes, layout, begin)
				cmd, err = CopyCommand(ctx, input, job.Spec, film.VideoCodec(), video, audio, dir)
			}
		} else {
			next = begin
			for _, stale := range globOr(filepath.Join(dir, "*.m4s")) {
				os.Remove(stale)
			}
			cmd, err = Command(ctx, input, job.Spec, next, dir, ass, image, x264)
		}
		if err != nil {
			return err
		}
		// Stopped by hand below so the log says why. CommandContext's kill is the
		// backstop, so no path leaves ffmpeg running.
		var stderr tail
		cmd.Stderr = &stderr
		cmd.Stdout = job.progress
		if segmentRuns && job.progress != nil {
			job.progress.restart()
		}
		if err := mediatool.Start(cmd); err != nil {
			return err
		}
		// A stored track that can't be read ends the run, whatever ffmpeg made meanwhile.
		var trackFailed <-chan struct{}
		if feed != nil {
			trackFailed = feed.failed
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
			if feed != nil {
				feed.end() // Its read of the track may wait for the run, and ffmpeg's exit waits for it.
			}
			<-exited
		}
		// final is set once ffmpeg has exited. The segment it flushes on the way
		// out is cut wherever its input ended, so it is only stored if it is the
		// film's real last one.
		upload := func(final bool) (caughtUp bool, err error) {
			for {
				job.lease.turn() // A backlog is many calls to Postgres, each of which can be slow.
				if next >= layout.Count() {
					return false, nil // A copied run has fragments past the end.
				}
				if segmentRuns && next > begin {
					return false, nil // The run was for one segment.
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
				} else if _, more := os.Stat(filepath.Join(dir, strconv.Itoa(next+1)+".m4s")); more != nil {
					// The newest segment in place: ffmpeg may be writing it out as its input ends.
					if final {
						if err := checkLast(dir, next, layout); err != nil {
							return false, err
						}
					} else if !wholeSegment(dir, name, next, layout) {
						return false, nil // The exit path judges it, if it is the one cut short.
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
					if err != nil {
						return false, err
					}
					chunkDone = !free
					if chunkDone || !final {
						return false, nil
					}
					// ffmpeg has exited with segments still waiting for Postgres: store the rest.
				}
			}
		}

		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		loggedSlow := false
		waiting := false // A finished segment is kept for Postgres.
		progressed, progressAt := next, time.Now()
		// gate pauses ffmpeg once everything below the limit is made, so what it
		// reads past the limit is what it reads before the next look, and lets it
		// go on when the limit has moved.
		// It reports false when the job is paused and none of its viewers is
		// within its window any more: they have seeked past it or gone, and its
		// slot is better given back than held for them.
		gate := func() bool {
			// A one-segment run ends by itself once its segment is made, so it is never paused (the pause
			// would find the process just gone, and its failure lifts the lead for good); room holds the
			// job between its runs.
			if unbounded || segmentRuns {
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
			// A helper looks all the while, not only at its limit: its viewers may
			// have left it behind while it makes segments no one will ask for.
			if (wait || paused || freed || job.Helper) && time.Since(limitAt) >= leadCheck {
				limitAt = time.Now()
				at, served, free, err := w.furthest(ctx, job, next)
				switch {
				case err != nil:
					log.Warn("reading the viewers' segments failed", "error", err)
				case free:
					limit, freed = layout.Count(), true
				case job.Helper && (!served || limitFor(layout, at, w.lead()) <= job.Start):
					// No viewer is at or before its chunk, or the furthest is so far
					// behind that the helper's first segment is past its lead (a
					// viewer who sought forward or far back), and lifting the limit
					// would make the helper run to the film's end for no one.
					return false
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
			job.lease.turn()
			select {
			case <-trackFailed:
				stop("the stored track could not be read")
				return fromStore(source.AssetStoreURI, feed.cause())
			case err := <-exited:
				if feed != nil && feed.cause() != nil {
					return fromStore(source.AssetStoreURI, feed.cause())
				}
				caughtUp, uploadErr := upload(true)
				// What ffmpeg made waits for Postgres, until the keeper gives the job up.
				for errors.Is(uploadErr, errWaiting) && ctx.Err() == nil {
					job.lease.turn()
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
				if chunkDone {
					return nil // The segments that follow are another job's.
				}
				if next <= begin {
					return fmt.Errorf("ffmpeg ended without a segment: %s", stderr.String())
				}
				if segmentRuns {
					return nil // The caller starts the run for the next segment.
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
					// The segment stays where it is, and ffmpeg and the job go on; the lease
					// keeper ends the job if the lease runs out first.
					if !waiting {
						log.Warn("a finished segment is kept until it can be recorded", "error", err)
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
					// The row first (DOMAINS 25); the ffmpeg is paused here, so it waits.
					w.cancelUnwanted(ctx, job)
					stop("its viewers have left it behind")
					if job.Helper {
						return errHelperLeft
					}
					return nil
				}
			case answered := <-job.lease.Beats:
				// The lease keeper renewed the job, or tried to.
				if answered {
					// The operator's only signal beside GET /Sessions: jellyfin-web
					// shows neither the speed nor the worker.
					if speed, ok := w.measuredSpeed(job); ok {
						if speed < 1 && !loggedSlow {
							log.Warn("transcode is slower than real time, so playback will stall", "speed", speed, "worker", w.ID)
						}
						loggedSlow = speed < 1
						if chunked && !w.NoHelpers && w.needsHelp(job) {
							if err := w.startHelper(ctx, job, next, layout); err != nil && ctx.Err() == nil {
								log.Warn("starting a helper job failed", "error", err)
							}
						}
					}
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
			case <-ctx.Done():
				if cause := context.Cause(ctx); errors.Is(cause, errInterrupted) {
					// The lease keeper ended it: the lease was not renewed, the job is
					// no longer this worker's, its work did not come round, or no
					// viewer is watching.
					stop(cause.Error())
					return cause
				}
				err := endedBy(ctx)
				stop(err.Error())
				return err
			}
		}
	}

	err = run(job.Start)
	for err == nil && segmentRuns && next < layout.Count() && !w.stored(ctx, job.Hash, next) {
		if ok, waitErr := room(next); !ok || waitErr != nil {
			err = waitErr
			break
		}
		err = run(next)
	}
	if cut := read.failure(); err != nil && cut != nil && ctx.Err() == nil {
		// ffmpeg says only that its input was invalid or ended; a read the store failed for good, which
		// store.Reader did not heal, is the cause. It also outranks a wrong index, which would be
		// found in output cut short by that read, and dropping an index is not undone.
		return fromStore(source.StoreURI, cut)
	}
	return err
}

// endedBy is the error of a job whose context is done. The lease keeper ends it when the lease was not renewed,
// the job is no longer this worker's, its work did not come round, or no viewer is watching; otherwise the worker
// is shutting down.
func endedBy(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, errInterrupted) {
		return cause
	}
	reason := "worker is shutting down"
	if errors.Is(context.Cause(ctx), errLockLost) {
		reason = errLockLost.Error()
	}
	return fmt.Errorf("%w: %s", errInterrupted, reason)
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
// fonts), which can take minutes. The job's lease is kept meanwhile by the lease
// keeper, which ends fetch, through ctx, when the job is lost or nobody wants it,
// so it doesn't hold any cores for a cancelled job; fetch then returns the
// keeper's cause.
func (w *Worker) awaitJob(ctx context.Context, job claimed, log *slog.Logger, fetch func(ctx context.Context) error) error {
	return cause(ctx, fetch(ctx))
}

// lostReason says why the job is no longer ours to run: a seek replaced it, or
// something took the lease from us.
func lostReason(ctx context.Context, q queryer, job claimed) string {
	var state, reason string
	q.QueryRow(ctx, "SELECT state, COALESCE(error, '') FROM transcode_jobs WHERE id = $1", pgx.QueryExecModeExec, job.ID).Scan(&state, &reason) //nolint:errcheck
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

// wholeSegmentSlack is how much shorter than its place a segment may measure and still be whole: a frame
// or two, since a re-encode cuts on a frame.
const wholeSegmentSlack = ticksPerSecond / 4

// wholeSegment says whether segment n, which ffmpeg has put in place while it still runs, holds its whole place.
// Only the segment ffmpeg flushes as its input ends can fall short (invariant 40), and while ffmpeg runs
// nothing but its length tells that one from a segment that is whole and has no successor yet. One that
// cannot be measured is kept back: the next segment in place, or the exit, settles it.
func wholeSegment(dir, name string, n int, layout playback.Layout) bool {
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		return false
	}
	body, err := os.ReadFile(name)
	if err != nil {
		return false
	}
	ticks, err := segmentTicks(init, body)
	if err != nil {
		return false
	}
	_, place := layout.Segment(n)
	return ticks >= place-wholeSegmentSlack
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

// upload stores one finished segment, with the init segment alongside the
// job's first. Contents depend on the spec and number only, so a repeat is
// harmless.
func (w *Worker) upload(ctx context.Context, job claimed, n int, name string) error {
	// A put overwrites, and the update below comes after it, so this is what
	// keeps a worker whose lease is gone (stopped for longer than the lease, cut
	// off from Postgres, reaped while its connection stood, cancelled by a seek)
	// from overwriting what the job's new owner stored. The replacement's first
	// segment is seconds away when this passes; only a put slower than that can
	// overlap. The check is a renewal of the job's lease, sent after this ask and
	// answered by Postgres (lease_keeper.go), which finds a lease taken away as
	// well as one run out. A database that does not answer is the same as a lease
	// that is gone. The renewal was sent no later than the lease began, so the
	// lease ends no sooner than sent+lease, whatever the round trip, and whatever
	// stops this worker before a put (a database that answers late, a SIGSTOP):
	// every put checks the clock against that.
	// A put can take longer than a lease while its bytes move, and the lease is
	// kept meanwhile, so each put is fenced on its own: one check at the start of
	// the upload would be a lease old by the time of the next put.
	put := func(key, file string) error {
		turnFrom(ctx)
		ownedUntil, err := w.keeper().fence(ctx, leaseID{id: job.ID})
		if err != nil {
			return err
		}
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
	recorded, err := w.recordSegment(ctx, job, n)
	if errors.Is(err, errWaiting) {
		return err // Stored, and not yet recorded: the file stays, and the next try stores it again.
	}
	os.Remove(name)
	turnFrom(ctx)
	if err != nil {
		return err
	}
	if !recorded {
		// A copied run can have many segments waiting; don't keep storing them.
		return fmt.Errorf("%w: %s", errInterrupted, lostReason(ctx, w.Pool, job))
	}
	Notify(ctx, w.Pool, segmentHint(job.Hash))
	return nil
}

// recordSegment records that the job stored segment n, and says whether the job still
// held its row. Postgres being away, or aborting the write for another transaction's
// sake, leaves the segment to be stored and recorded again.
func (w *Worker) recordSegment(ctx context.Context, job claimed, n int) (bool, error) {
	tag, err := w.Pool.Exec(ctx, `UPDATE transcode_jobs SET next_segment = $2
		WHERE id = $1 AND worker = $3 AND state = 'running'`, job.ID, n+1, w.ID)
	if db.IsAway(err) || abortedForAnother(err) {
		return false, fmt.Errorf("%w: recording the segment: %v", errWaiting, err)
	}
	return tag.RowsAffected() > 0, fromPool(err)
}

// put stores a file. The store's reads of the body are the job's work coming
// round, so a put that is slow but moving keeps the job, and one that stops is
// given up by the keeper. A store that does not return when the job is given up
// is left behind: the job ends without it, and it records nothing, for the
// caller never hears of its end.
func (w *Worker) put(ctx context.Context, key, file string) error {
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	body := &countedBody{f: handle, ctx: ctx}
	done := make(chan error, 1)
	go func() {
		defer handle.Close()
		err := w.Segments.Put(ctx, key, body)
		if local := body.failed.Load(); err != nil && local != nil {
			done <- *local // The scratch file could not be read: the store only passed that on.
			return
		}
		done <- fromStore(w.SegmentsURI, err)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// countedBody is what a store reads: a file that seeks and whose reads turn the
// job. It has no WriteTo of its own, which a copy would use instead of Read. It
// keeps the file's own read error, which a store returns as if it were its own.
type countedBody struct {
	f      *os.File
	ctx    context.Context
	failed atomic.Pointer[error]
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.f.Read(p)
	if n > 0 {
		turnFrom(b.ctx)
	}
	if err != nil && err != io.EOF {
		b.failed.CompareAndSwap(nil, &err)
	}
	return n, err
}

func (b *countedBody) Seek(offset int64, whence int) (int64, error) { return b.f.Seek(offset, whence) }

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
		err := copyFont(ctx, assets, sum, path, source.AssetStoreURI)
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
				return fromStore(source.AssetStoreURI, readErr)
			}
			sum, ok := fonts[index]
			if !ok {
				continue
			}
			err = copyFont(ctx, assets, sum, path, source.AssetStoreURI)
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
func copyFont(ctx context.Context, assets store.Store, sum, path, uri string) error {
	body, err := assets.Open(ctx, subtitles.FontKey(sum), 0)
	if err != nil {
		return fromStore(uri, err)
	}
	defer body.Close()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(file, hash), turning(ctx, storeBody{uri, body}))
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
