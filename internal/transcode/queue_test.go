package transcode

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// logBuffer is a log destination that goroutines can share.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// A job nobody took in the queue timeout is dead: PlaybackInfo must stop
// offering the transcode it stands for, and a worker that turns up later must
// not start ffmpeg for a viewer who left long ago.
func TestStaleQueuedJobsAreDead(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	if _, err := j.ensure(ctx, r.spec, r.hash, psid(0), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('gone', 2)"); err != nil {
		t.Fatal(err)
	}
	if reason, err := j.Unavailable(ctx, r.hash, "", ""); err != nil || reason != "" {
		t.Fatalf("a fresh queued job is joined: %q %v", reason, err)
	}
	if _, err := r.pool.Exec(ctx, "DELETE FROM transcode_nodes; UPDATE transcode_jobs SET created_at = now() - interval '5 minutes'"); err != nil {
		t.Fatal(err)
	}
	if reason, err := j.Unavailable(ctx, r.hash, "", ""); err != nil || !strings.Contains(reason, "no transcode worker") {
		t.Fatalf("a queued job nobody took for minutes still counts as live: %q %v", reason, err)
	}

	// A worker returning to a queue with a stale job in it doesn't start it.
	if _, err := r.pool.Exec(ctx, "UPDATE transcode_jobs SET state = 'queued', created_at = now() - interval '5 minutes'"); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: r.pool, ID: "w"}
	if _, ok, err := w.claim(ctx); err != nil || ok {
		t.Fatalf("a worker claimed a job that waited minutes: %v %v", ok, err)
	}
}

// Workers at capacity are busy, not missing, and the error says so.
func TestFullWorkersAreBusyNotMissing(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, slots) VALUES ('w', 1);
		INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
		VALUES ('other', '{}', 'o', 0, 0, 'running', now() + interval '1 minute', 'w')`); err != nil {
		t.Fatal(err)
	}
	j := r.jobs(t)
	j.QueueTimeout = time.Second
	if reason, _ := j.Unavailable(ctx, r.hash, "", ""); !strings.Contains(reason, "slots are busy") {
		t.Fatalf("refusal: %q", reason)
	}
	if _, err := r.fetch(t, j, psid(0), 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs WHERE spec_hash = '"+r.hash+"'"); state != "cancelled" {
		t.Fatalf("the job the viewer gave up on is %s, and a worker would still start it", state)
	}
}

// Stores that differ between registered nodes are named, and a spec that
// already has a live job is offered anyway, since a viewer joins it.
func TestStoreMismatchIsFoundInTheNodes(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	j := r.jobs(t)
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots) VALUES
		('w', 'worker', 'file:///var/segments', 2), ('a', 'api', 's3://bucket/segments', 0)`); err != nil {
		t.Fatal(err)
	}
	reason, err := j.Unavailable(ctx, r.hash, "", "")
	if err != nil || !strings.Contains(reason, "file:///var/segments") || !strings.Contains(reason, "s3://bucket/segments") ||
		!strings.Contains(reason, "api") || !strings.Contains(reason, "worker") {
		t.Fatalf("got %q %v", reason, err)
	}
	if _, err := j.ensure(ctx, r.spec, r.hash, psid(0), 0, 0); err != nil {
		t.Fatal(err)
	}
	if reason, err := j.Unavailable(ctx, r.hash, "", ""); err != nil || reason != "" {
		t.Fatalf("a live job is joined whatever the stores say: %q %v", reason, err)
	}
	// One store everywhere is fine.
	if _, err := r.pool.Exec(ctx, "UPDATE transcode_nodes SET store = 'file:///var/segments'; TRUNCATE transcode_jobs"); err != nil {
		t.Fatal(err)
	}
	if reason, err := j.Unavailable(ctx, r.hash, "", ""); err != nil || reason != "" {
		t.Fatalf("matching stores: %q %v", reason, err)
	}
}

// Seeking back before the point a resume started at needs a job there.
func TestSeekBackBeforeTheResumePoint(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	if _, err := r.fetchAt(t, j, psid(0), 12, 12); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if body, err := r.fetchAt(t, j, psid(0), 3, 12); err != nil || !isMP4Segment(body) {
		t.Fatalf("segment 3 after a resume at 12: %d bytes, %v", len(body), err)
	}
	if time.Since(began) > 20*time.Second {
		t.Fatalf("took %v", time.Since(began))
	}
}

// jellyfin-web asks for the new stream before it deletes the old one, and a
// viewer must not be refused for the slot their own stream is about to free.
func TestChangingStreamIsNotRefusedByTheViewersOwnJob(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, slots) VALUES ('w', 1);
		INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, lease_expires_at, worker)
		VALUES ('other', '{}', 'old', 0, 0, 'running', now() + interval '1 minute', 'w')`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id)
		VALUES ('old', 'other', $1, 'device')`, r.user); err != nil {
		t.Fatal(err)
	}
	j := r.jobs(t)
	if reason, err := j.Unavailable(ctx, r.hash, r.user, "device"); err != nil || reason != "" {
		t.Fatalf("the viewer's own job fills the only slot: %q %v", reason, err)
	}
	if reason, _ := j.Unavailable(ctx, r.hash, r.user, "another device"); !strings.Contains(reason, "slots are busy") {
		t.Fatalf("someone else's job isn't busy: %q", reason)
	}
	// Stopping frees the slot at once, not at the job's next heartbeat.
	if err := j.Stop(ctx, r.user, "old", "", ""); err != nil {
		t.Fatal(err)
	}
	if reason, _ := j.Unavailable(ctx, r.hash, r.user, "another device"); reason != "" {
		t.Fatalf("a stopped viewer's job still holds the slot: %q", reason)
	}
}

// ffmpeg that hangs holds its lease for as long as the worker lives, so the
// worker has to fail the job itself, and the viewer gets an answer.
func TestHungFFmpegFailsTheJob(t *testing.T) {
	needFFmpeg(t)
	film(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) { w.Stall = time.Second })
	j := r.jobs(t)
	began := time.Now()
	_, err := r.fetch(t, j, psid(0), 0)
	var failed FailedError
	if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "no segment") {
		t.Fatalf("got %v", err)
	}
	if time.Since(began) > 15*time.Second {
		t.Fatalf("took %v", time.Since(began))
	}
	if reason, _ := j.Unavailable(context.Background(), r.hash, "", ""); !strings.Contains(reason, "failed") {
		t.Fatalf("PlaybackInfo keeps offering the hung stream: %q", reason)
	}
}

// The init segment is asked for before the first media segment, and comes
// with any job, so a resume's init request starts the job at the resume point
// and not one at 0 that the media request would replace.
func TestInitSegmentOfAResumeStartsNoJobAtZero(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	init, err := r.fetchAt(t, j, psid(0), -1, 12)
	if err != nil || !bytes.Contains(init[:32], []byte("ftyp")) {
		t.Fatalf("init: %d bytes, %v", len(init), err)
	}
	if _, err := r.fetchAt(t, j, psid(0), 12, 12); err != nil {
		t.Fatal(err)
	}
	if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs"); jobs != 1 {
		t.Fatalf("%d jobs, want the one at the resume point", jobs)
	}
	if start := r.count(t, "SELECT start_segment FROM transcode_jobs"); start != 12 {
		t.Fatalf("the job started at %d", start)
	}
}

// A seek that lands in another viewer's job drops the viewer's own earlier job.
func TestSeekJoiningAnothersJobEndsTheOwn(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	var logs logBuffer
	r.worker(t, func(w *Worker) { w.Log = logs.logger() })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil { // Viewer A, from the start.
		t.Fatal(err)
	}
	if _, err := r.fetch(t, j, psid(1), 10); err != nil { // Viewer B, somewhere else.
		t.Fatal(err)
	}
	if _, err := r.fetch(t, j, psid(0), 11); err != nil { // A seeks into B's job.
		t.Fatal(err)
	}
	waitFor(t, "A's earlier ffmpeg to exit", 10*time.Second, func() bool {
		procs := r.ffmpegProcesses()
		return len(procs) == 1 && segmentStart(procs[0]) == "10"
	})
	if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs"); jobs != 2 {
		t.Fatalf("%d jobs", jobs)
	}
	waitFor(t, "the log to say why", 5*time.Second, func() bool { return strings.Contains(logs.String(), "a newer job replaced") })
	if strings.Contains(logs.String(), "lease lost") {
		t.Fatalf("a replaced job is logged as a lost lease:\n%s", logs.String())
	}
}

// A worker cut short by shutdown records its job as interrupted, not done or
// failed, and a viewer's request starts it again at once.
func TestShutdownInterruptsRatherThanFinishesOrFails(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	stop := r.worker(t, nil)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	stop()
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs"); state != "cancelled" {
		t.Fatalf("a job cut short by shutdown is %s", state)
	}
	if reason, err := j.Unavailable(context.Background(), r.hash, "", ""); err != nil || strings.Contains(reason, "failed") {
		t.Fatalf("shutdown counts as a failure: %q %v", reason, err)
	}
	r.worker(t, nil)
	if body, err := r.fetch(t, j, psid(0), 1); err != nil || !isMP4Segment(body) {
		t.Fatalf("restart after shutdown: %d bytes, %v", len(body), err)
	}
}

// Shutdown while a job is still starting must not read as a failure either.
func TestShutdownDuringStartupIsNotAFailure(t *testing.T) {
	r := newRig(t, nil)
	started, release := make(chan struct{}), make(chan struct{})
	stop := r.worker(t, func(w *Worker) {
		w.Media = func(string) (store.Store, error) {
			close(started)
			<-release
			return nil, errors.New("late failure")
		}
	})
	go r.fetch(t, r.jobs(t), psid(0), 0) //nolint:errcheck
	<-started
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	close(release)
	<-done
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs"); state != "cancelled" {
		t.Fatalf("a job cut short while starting is %s", state)
	}
}

// A crashed worker leaves job directories behind; the next one clears them,
// but not those of a job another worker on the same host is writing, since
// workers share the default scratch directory.
func TestWorkerClearsScratchLeftByACrash(t *testing.T) {
	r := newRig(t, nil)
	made := func(name string, age time.Duration) string {
		dir := filepath.Join(r.scratch, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0.m4s"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	crashed, live := made("job-crashed", 10*time.Minute), made("job-live", time.Second)
	r.worker(t, nil)
	waitFor(t, "the old directory to go", 5*time.Second, func() bool {
		_, err := os.Stat(crashed)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("another worker's job directory was removed: %v", err)
	}
}

// A client that asks again after a failure, as a browser does when it gives up
// on a request, gets the failure again for a while rather than a new ffmpeg
// every time. PlaybackInfo's memory runs from when the job ended, not began.
func TestRecentFailureIsNotRestartedByAnotherRequest(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('w', 2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, error, created_at, ended_at)
		VALUES ($1, '{}', 'o', 0, 0, 'failed', 'ffmpeg made no segment', now() - interval '5 minutes', now())`, r.hash); err != nil {
		t.Fatal(err)
	}
	j := r.jobs(t)
	if reason, _ := j.Unavailable(ctx, r.hash, "", ""); !strings.Contains(reason, "no segment") {
		t.Fatalf("a job that ran for minutes and failed just now is forgotten: %q", reason)
	}
	var failed FailedError
	if _, err := r.fetch(t, j, psid(0), 0); !errors.As(err, &failed) || !strings.Contains(failed.Reason, "no segment") {
		t.Fatalf("got %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'queued'"); n != 0 {
		t.Fatalf("%d jobs started again", n)
	}
}

// A viewer who seeks replaces their own earlier job, but not one that another
// viewer joined: that viewer's next segment is still coming from it.
func TestSeekLeavesAJobAnotherViewerJoined(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	ensure := func(session string, n int) int64 {
		t.Helper()
		if err := j.Watch(ctx, r.viewer(session), r.hash); err != nil {
			t.Fatal(err)
		}
		id, err := j.ensure(ctx, r.spec, r.hash, session, n, n)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := ensure(psid(0), 0)
	if joined := ensure(psid(1), 1); joined != first {
		t.Fatalf("the second viewer started job %d instead of joining %d", joined, first)
	}
	far := ensure(psid(0), 12) // The first viewer seeks.
	if far == first {
		t.Fatal("a seek past the job joined it")
	}
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs WHERE id = "+strconv.FormatInt(first, 10)); state != "queued" {
		t.Fatalf("the job the second viewer joined is %s", state)
	}
	if again := ensure(psid(1), 2); again != first {
		t.Fatalf("the second viewer's next segment started job %d", again)
	}
	// With nobody else on it, a seek does end the viewer's own job.
	if err := j.Stop(ctx, r.user, psid(1), "", ""); err != nil {
		t.Fatal(err)
	}
	if joined := ensure(psid(0), 14); joined != far {
		t.Fatalf("a seek near job %d started job %d", far, joined)
	}
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs WHERE id = "+strconv.FormatInt(first, 10)); state != "cancelled" {
		t.Fatalf("a job joined by nobody else is %s: the seek should have replaced it", state)
	}
}

// A worker that died leaves a running job with an expired lease. Sweeping it
// is not a failure of the spec: viewers restart it and PlaybackInfo offers it.
func TestSweepingADeadWorkersJobDoesNotRefuseTheSpec(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_nodes (id, slots) VALUES ('w', 2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ($1, '{}', 'o', 0, 3, 'running', 'dead', now() - interval '30 seconds')`, r.hash); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: r.pool, Log: r.log}
	if err := w.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs"); state != "cancelled" {
		t.Fatalf("a job whose worker died is %s", state)
	}
	if reason, err := j.Unavailable(ctx, r.hash, "", ""); err != nil || reason != "" {
		t.Fatalf("PlaybackInfo refuses the spec: %q %v", reason, err)
	}
	if _, err := j.ensure(ctx, r.spec, r.hash, psid(0), 0, 0); err != nil {
		t.Fatalf("a viewer's request is refused: %v", err)
	}
}

// A worker that is restarting leaves the queue empty for a few seconds. A
// request for a segment waits the queue timeout for its replacement, instead
// of answering 503 at once.
func TestRequestWaitsForAReplacementWorker(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	type result struct {
		data []byte
		err  error
	}
	got := make(chan result, 1)
	began := time.Now()
	go func() {
		data, err := r.fetch(t, j, psid(0), 0)
		got <- result{data, err}
	}()
	time.Sleep(2 * time.Second)
	r.worker(t, nil)
	res := <-got
	if res.err != nil || len(res.data) == 0 {
		t.Fatalf("after %v: %d bytes, %v", time.Since(began), len(res.data), res.err)
	}
}

// A viewer who joined another's job and then seeked elsewhere no longer keeps
// that job alive, so it ends with the viewer who started it.
func TestAJobEndsWhenNobodyIsWatchingIt(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	for i := range 2 {
		if err := j.Watch(ctx, r.viewer(psid(i)), r.hash); err != nil {
			t.Fatal(err)
		}
	}
	first, err := j.ensure(ctx, r.spec, r.hash, psid(0), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := j.ensure(ctx, r.spec, r.hash, psid(1), 1, 0)
	if err != nil || joined != first {
		t.Fatalf("the second viewer started job %d, not %d: %v", joined, first, err)
	}
	far, err := j.ensure(ctx, r.spec, r.hash, psid(1), 300, 300)
	if err != nil || far == first {
		t.Fatalf("a far seek joined job %d: %v", far, err)
	}
	if err := j.Stop(ctx, r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	state := func(id int64) string {
		return mustScalar(t, r, "SELECT state FROM transcode_jobs WHERE id = "+strconv.FormatInt(id, 10))
	}
	if state(first) != "cancelled" || state(far) != "queued" {
		t.Fatalf("after the starter stopped, the first job is %s and the seek's is %s", state(first), state(far))
	}
}
