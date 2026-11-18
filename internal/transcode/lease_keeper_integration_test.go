//go:build integration

package transcode

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-105: one lease keeper for every leased job, so a hung call cannot keep a job
// past its lease. The checks hold a worker's every database connection
// (testenv.DBProxy.Hold: open, passing nothing, Postgres running on) and look,
// with the test reading Postgres directly, at what the worker does with its
// jobs before their leases run out.
//
// The workers run at their production lease (10 s) and heartbeat (3 s): the
// criteria are that a worker stops its job the cutMargin before its lease can
// lapse (9.5 s after its last renewal) and that a renewal gives up after half a
// heartbeat (1.5 s), and a shorter lease would move those lines. No check
// judges how long anything took; what is compared is the moment a thing was seen
// to end against the lease Postgres holds, which the worker aims to beat by the
// cutMargin (0.5 s), and a hang is bounded by many leases. These compile only
// once testenv.DBProxy has Hold and Release (JS-105 builds them with this task).

const keeperHang = 90 * time.Second // Only bounds a hang: the checks judge nothing by it.

// hold keeps all three of the worker's connections open and unanswered: the
// pool's, the liveness lock's and the lease keeper's.
func (w *outageWorker) hold() {
	w.db.Hold()
	w.live.Hold()
	w.keep.Hold()
}

func (w *outageWorker) release() {
	w.db.Release()
	w.live.Release()
	w.keep.Release()
}

// delay makes every round trip of all three connections longer.
func (w *outageWorker) delay(d time.Duration) {
	w.db.Delay(d)
	w.live.Delay(d)
	w.keep.Delay(d)
}

// twoLeases is what a job under the slow database must outlast for its test to
// prove anything: the lease is 10 s, and a job cut at the first lease (a renewal
// that gives up every time) ends well before a second one.
const twoLeases = 20 * time.Second

// slowFilmSeconds is the length of the film the slow-database tests play. At 1 s
// a round trip every segment costs the job several of them, so a few segments
// last longer than two leases, which is all the claim needs; the whole 90 s film
// took 551 s.
const slowFilmSeconds = 14

// useSlowFilm points the rig's item at a cut of the film of slowFilmSeconds and
// returns its number of segments.
func (r *rig) useSlowFilm(t *testing.T) int {
	t.Helper()
	name := fmt.Sprintf("slow-database-%d.mkv", slowFilmSeconds)
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", filepath.Join(film(t), "film.mkv"), "-t", strconv.Itoa(slowFilmSeconds), "-c", "copy", filepath.Join(film(t), name))
	source := r.useFile(t, name)
	return playback.EncodedLayout(source.RunTimeTicks).Count()
}

// notDoneInsideTwoLeases fails the test as a harness error when its job ended
// done within two leases of the moment the 1 s round trips were in force on the
// job: such a run cannot tell a keeper that works from one that cuts the job at
// its first lease. It logs how long the job took.
func notDoneInsideTwoLeases(t *testing.T, state string, since time.Time) {
	t.Helper()
	lasted := time.Since(since)
	t.Logf("the job ended %s %v after the 1 s round trips were in force on it", state, lasted.Round(time.Second))
	if state == "done" && lasted < twoLeases {
		t.Fatalf("harness error: the job was done in %v, inside two leases (%v): the check proves nothing; lengthen slowFilmSeconds", lasted, twoLeases)
	}
}

// isTheRenewal matches the statement that renews a transcode job's lease: it
// sets lease_expires_at to now() plus the lease, and is neither a claim nor the
// segment commit.
func isTheRenewal(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "UPDATE transcode_jobs") && strings.Contains(s, "lease_expires_at = now()") &&
		!strings.Contains(s, "SET state = 'running'") && !strings.Contains(s, "next_segment")
}

// isTheSegmentCommit matches the write that records a stored segment.
func isTheSegmentCommit(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "UPDATE transcode_jobs") && strings.Contains(s, "next_segment") && !strings.Contains(s, "SELECT")
}

// leaseOf is the lease of the job as Postgres holds it.
func (r *rig) leaseOf(t *testing.T, id int64) time.Time {
	t.Helper()
	var at time.Time
	if err := r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// leaseRunOut says whether Postgres, by its own clock, has the job's lease as passed.
func (r *rig) leaseRunOut(t *testing.T, id int64) bool {
	t.Helper()
	var out bool
	if err := r.pool.QueryRow(context.Background(), "SELECT lease_expires_at < now() FROM transcode_jobs WHERE id = $1", id).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// jobOf is the running job of the spec.
func (r *rig) jobOf(t *testing.T, hash string) int64 {
	t.Helper()
	var id int64
	if err := r.pool.QueryRow(context.Background(), "SELECT id FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' ORDER BY id DESC LIMIT 1", hash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// goneBefore waits for the ffmpeg process (pid and start) to be gone, and says
// whether that was seen before the lease ended, by the clock of this machine
// and the lease as Postgres holds it. The worker's kill is the cutMargin (0.5 s)
// before the lease, so a build that kills in time has that in hand, and a
// build that kills at the lease or later, or never, does not.
func (r *rig) goneBefore(t *testing.T, p ffmpegProcess, lease time.Time) (gone, inTime bool) {
	t.Helper()
	for deadline := time.Now().Add(keeperHang); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		alive := false
		for _, q := range r.ffmpegs() {
			if q.Pid == p.Pid && q.Start == p.Start {
				alive = true
			}
		}
		if !alive {
			return true, time.Now().Before(lease)
		}
	}
	return false, false
}

// interruptedLines are the lines of the worker's log that say a job was given
// up, each with its reason.
func interruptedLines(w *outageWorker) []string {
	var found []string
	for _, line := range strings.Split(w.log.String(), "\n") {
		if strings.Contains(line, "transcode interrupted") {
			found = append(found, line)
		}
	}
	return found
}

// theReasonIsTheLease fails unless every given-up job's reason in the log names
// the lease that was not renewed, and none is a shutdown.
func theReasonIsTheLease(t *testing.T, w *outageWorker, want int) {
	t.Helper()
	lines := interruptedLines(w)
	if len(lines) < want {
		t.Errorf("the log of worker %s has %d 'transcode interrupted' lines, want %d:\n%s", w.id, len(lines), want, w.log.String())
	}
	for _, line := range lines {
		if strings.Contains(line, "shutting down") || !regexp.MustCompile(`reason=.*lease`).MatchString(line) {
			t.Errorf("a job was given up for a reason that is not the unrenewed lease: %s", line)
		}
	}
}

// Every database connection of a worker held while it runs two jobs, one paused
// at its lead and one storing segments: each job's ffmpeg is gone before that
// job's lease, as Postgres holds it, has passed; each job ends cancelled, never
// failed, once the connections answer again, with the lease as the reason in the
// worker's log.
func TestAHeldWorkerEndsEveryFfmpegBeforeItsJobsLeaseRunsOut(t *testing.T) {
	slowAt(t, "2")
	// Cores for two re-encodes (0.8 each) and a lead of 8 s, so that a job nobody
	// has asked far ahead of stops at its lead.
	s := newStack(t, 1, "JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_LEAD_SECONDS=8")
	r, w, ctx := s.r, s.workers[0], context.Background()
	specA, specB := r.spec, r.spec
	specB.VideoBitrate = 1_200_000
	count := playback.EncodedLayout(900000000).Count()

	if _, err := s.fetch(ctx, s.jobs, specA, psid(0), 0); err != nil { // One segment asked for: this job stops at its lead.
		t.Fatal(err)
	}
	s.play(s.jobs, specB, psid(1), 0, count) // And this one is played, so it keeps storing.
	jobA, jobB := r.jobOf(t, specA.Hash()), int64(0)
	var paused ffmpegProcess
	waitFor(t, "job A's ffmpeg to be paused at its lead and job B to be running", keeperHang, func() bool {
		for _, p := range r.ffmpegs() {
			if p.Parent == w.cmd.Process.Pid && p.State == "T" {
				paused = p
			}
		}
		if paused.Pid == 0 {
			return false
		}
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' AND next_segment >= 2", specB.Hash()) == 1
	})
	jobB = r.jobOf(t, specB.Hash())
	var running ffmpegProcess
	for _, p := range r.ffmpegs() {
		if p.Parent == w.cmd.Process.Pid && p.Pid != paused.Pid {
			running = p
		}
	}
	if running.Pid == 0 {
		t.Fatal("job B has no ffmpeg of its own beside job A's paused one")
	}

	// Hold at the moment job B records a stored segment: the put is done, the
	// write that says so does not arrive.
	held := make(chan struct{})
	w.db.OnStatement(isTheSegmentCommit, func() { w.hold(); close(held) }, nil)
	select {
	case <-held:
	case <-time.After(keeperHang):
		t.Fatal("job B never recorded a segment")
	}
	time.Sleep(200 * time.Millisecond) // What reached Postgres before the hold has been applied.
	leaseA, leaseB := r.leaseOf(t, jobA), r.leaseOf(t, jobB)

	for _, c := range []struct {
		what  string
		p     ffmpegProcess
		lease time.Time
	}{{"job A's, paused at its lead,", paused, leaseA}, {"job B's, storing segments,", running, leaseB}} {
		gone, inTime := r.goneBefore(t, c.p, c.lease)
		switch {
		case !gone:
			t.Errorf("%s ffmpeg is still a process %v after the worker's connections were held", c.what, keeperHang)
		case !inTime:
			t.Errorf("%s ffmpeg was a process after its job's lease ran out (%v) with the worker's connections held", c.what, c.lease.Format("15:04:05.000"))
		}
	}
	w.release()
	waitFor(t, "both jobs to end", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'") == 0
	})
	for _, id := range []int64{jobA, jobB} {
		if got := r.jobState(t, id); got != "cancelled" {
			t.Errorf("job %d, given up while its worker's connections were held, ended %s, want cancelled", id, got)
		}
	}
	r.noFailed(t)
	theReasonIsTheLease(t, w, 2)
}

// A burn-in job waiting in awaitJob for its subtitle track, its worker's every
// connection held: the wait ends and the job is given up, logged as interrupted,
// before its lease has passed. Nobody extracts the track, so the job is in
// awaitJob when the hold begins; the track is the film's text track.
func TestAHeldWorkerEndsAJobWaitingForItsSubtitleTrackBeforeItsLeaseRunsOut(t *testing.T) {
	testenv.FFmpeg(t)
	testenv.Font(t)
	s := newStack(t, 1) // No subtitle runner: the track is asked for and not made.
	r, w := s.r, s.workers[0]
	r.useBurnFilm(t)
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.fetch(ctx, s.jobs, r.spec, psid(0), 0) //nolint:errcheck
	waitFor(t, "the burn-in job to run and ask for its track", keeperHang, func() bool {
		return r.owner(t) == w.id && r.count(t, "SELECT count(*) FROM subtitle_jobs") >= 1
	})
	if n := len(r.ffmpegs()); n != 0 {
		t.Fatalf("%d ffmpeg processes run while the job should wait for its track", n)
	}
	job := r.jobOf(t, r.hash)
	w.hold()
	time.Sleep(200 * time.Millisecond)
	lease := r.leaseOf(t, job)
	var ended time.Time
	waitFor(t, "the worker to log that it gave the job up", keeperHang, func() bool {
		if len(interruptedLines(w)) == 0 {
			return false
		}
		ended = time.Now()
		return true
	})
	if !ended.Before(lease) {
		t.Errorf("the job that waited for its track was given up at %v, after its lease ran out (%v) with the worker's connections held", ended.Format("15:04:05.000"), lease.Format("15:04:05.000"))
	}
	w.release()
	waitFor(t, "the job to end", keeperHang, func() bool { return r.jobState(t, job) != "running" })
	if got := r.jobState(t, job); got != "cancelled" {
		t.Errorf("the job given up while waiting for its track ended %s, want cancelled", got)
	}
	r.noFailed(t)
	theReasonIsTheLease(t, w, 1)
	if reason, err := s.jobs.Unavailable(context.Background(), r.spec, "", ""); err != nil || reason != "" {
		t.Errorf("PlaybackInfo would refuse the spec after the job was given up: %q %v", reason, err)
	}
}

// The slot is given back as well as the ffmpeg: the statement left unanswered is
// the worker's own write cancelling a job no viewer watches, which is sent with
// no deadline of its own. Once the connections answer again the worker, with the
// cores for one job, claims and runs the next.
func TestTheSlotIsGivenBackWhenTheStatementLeftUnansweredIsTheWorkersOwnCancel(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1) // Cores for one re-encode.
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to run with its ffmpeg", keeperHang, func() bool { _, ok := r.ffmpegOf(w); return ok })
	old := r.jobOf(t, r.hash)
	p, _ := r.ffmpegOf(w)
	held := make(chan struct{})
	w.db.OnStatement(func(sent []byte) bool {
		return strings.Contains(string(sent), "UPDATE transcode_jobs") && strings.Contains(string(sent), "SET state = 'cancelled'")
	}, func() { w.hold(); close(held) }, nil)
	// Nobody watches the spec any more: the worker's next renewal finds that, and writes the cancel.
	r.exec(t, "UPDATE transcode_viewers SET last_seen_at = now() - interval '1 hour' WHERE spec_hash = $1", r.hash)
	select {
	case <-held:
	case <-time.After(keeperHang):
		t.Fatal("the worker never wrote the cancel of a job nobody watches")
	}
	time.Sleep(200 * time.Millisecond)
	lease := r.leaseOf(t, old)
	if gone, inTime := r.goneBefore(t, p, lease); !gone || !inTime {
		t.Errorf("the ffmpeg of a job whose cancel write hung was gone %v, in time %v (lease %v)", gone, inTime, lease.Format("15:04:05.000"))
	}
	waitFor(t, "the lease to run out", keeperHang, func() bool { return r.leaseRunOut(t, old) })
	w.release()
	// The next viewer's request starts a job, and this worker, whose one slot the
	// old job held, claims it.
	attempt, stop := context.WithTimeout(ctx, keeperHang)
	defer stop()
	// A segment the old job did not store, so the request starts a job: segment 0
	// is in the store already, and a request for it is answered from there.
	missing := r.firstMissing(r.hash)
	if _, err := s.fetch(attempt, s.jobs, r.spec, psid(1), missing); err != nil {
		t.Fatalf("after the connections answered again the viewer's request for segment %d: %v", missing, err)
	}
	var worker string
	var id int64
	if err := r.pool.QueryRow(ctx, "SELECT id, worker FROM transcode_jobs WHERE spec_hash = $1 AND id > $2 ORDER BY id LIMIT 1", r.hash, old).Scan(&id, &worker); err != nil {
		t.Fatalf("no job after job %d: %v", old, err)
	}
	if worker != w.id {
		t.Errorf("job %d ran on %q, want the worker whose slot was given back (%s)", id, worker, w.id)
	}
	if got := r.jobState(t, old); got != "cancelled" {
		t.Errorf("the job nobody watched ended %s, want cancelled", got)
	}
	r.noFailed(t)
}

// With a second worker free, the viewer's next request after the held worker's
// lease ran out is answered by a job on the other worker from the first segment
// not stored; the held worker begins no put for the spec after its lease ran
// out, during the hold or after the release.
func TestAHeldWorkersJobIsReplacedOnTheOtherWorkerAndItStoresNothingMore(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, held, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	old := r.jobOf(t, r.hash)
	waitFor(t, "the job to have stored two segments", keeperHang, func() bool {
		return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", old) >= 2
	})
	free := s.addWorker() // Its one slot is free.
	held.hold()
	time.Sleep(200 * time.Millisecond)
	lease := r.leaseOf(t, old)
	waitFor(t, "the held worker's lease to run out", keeperHang, func() bool { return r.leaseRunOut(t, old) })
	time.Sleep(500 * time.Millisecond)
	missing := r.firstMissing(r.hash)
	attempt, stop := context.WithTimeout(ctx, keeperHang)
	defer stop()
	if _, err := s.fetch(attempt, s.jobs, r.spec, psid(0), missing); err != nil {
		t.Fatalf("the viewer's next request, for segment %d, after the held worker's lease ran out: %v", missing, err)
	}
	var id int64
	var worker string
	var start int
	if err := r.pool.QueryRow(ctx, "SELECT id, worker, start_segment FROM transcode_jobs WHERE spec_hash = $1 AND id > $2 ORDER BY id LIMIT 1", r.hash, old).Scan(&id, &worker, &start); err != nil {
		t.Fatalf("no replacement job after job %d: %v", old, err)
	}
	if worker != free.id {
		t.Errorf("the replacement runs on %q, want the free worker %s", worker, free.id)
	}
	if start != missing {
		t.Errorf("the replacement starts at segment %d, want %d, the first not stored", start, missing)
	}
	held.release()
	waitFor(t, "the held worker's job to end", keeperHang, func() bool { return r.jobState(t, old) != "running" })
	time.Sleep(2 * time.Second) // What a worker that goes on would store.
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == held.id && p.at.After(lease.Add(putAllowance)) {
			t.Errorf("worker %s began a put of %s at %v, %v after its job's lease ran out (%v)", p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(lease).Round(time.Millisecond), lease.Format("15:04:05.000"))
		}
	}
	if got := r.jobState(t, old); got != "cancelled" {
		t.Errorf("the held worker's job ended %s, want cancelled", got)
	}
	r.noFailed(t)
	theReasonIsTheLease(t, held, 1)
}

// A hold that covers a whole renewal attempt and ends well inside the lease
// changes nothing: the same job row, worker and ffmpeg, no job cancelled, the
// lease renewed again afterwards. The hold begins as the renewal is sent (the
// attempt gives up after half a heartbeat, 1.5 s) and is released a little past
// that. The criterion asks for a release two heartbeats before the lease can
// lapse, which no hold of a whole attempt can do at the table's ratios (the
// attempt begins a heartbeat after the last renewal and ends 1.5 s later, and two
// heartbeats before the lease is 4 s after it); this one is released 5.4 s before.
func TestAHoldOfOneRenewalAttemptChangesNothing(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to run with its ffmpeg and a segment", keeperHang, func() bool {
		_, ok := r.ffmpegOf(w)
		return ok && r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND next_segment >= 1") == 1
	})
	rowsBefore := r.jobRows(t)
	ffmpegBefore, _ := r.ffmpegOf(w)
	job := r.jobOf(t, r.hash)
	held := make(chan struct{})
	w.keep.OnStatement(isTheRenewal, func() { w.hold(); close(held) }, nil) // The keeper's connection carries the renewals.
	select {
	case <-held:
	case <-time.After(keeperHang):
		t.Fatal("the worker never sent a renewal")
	}
	time.Sleep(200 * time.Millisecond)
	leaseBefore := r.leaseOf(t, job)
	time.Sleep(1500*time.Millisecond + 100*time.Millisecond) // The attempt's bound, and a little more.
	w.release()
	waitFor(t, "the lease to be renewed again", keeperHang, func() bool { return r.leaseOf(t, job).After(leaseBefore) })
	time.Sleep(time.Second)
	if got := r.jobRows(t); len(got) != len(rowsBefore) || got[0] != rowsBefore[0] {
		t.Errorf("job rows were %+v and are %+v: one missed renewal changed a job", rowsBefore, got)
	}
	if p, ok := r.ffmpegOf(w); !ok || p.Pid != ffmpegBefore.Pid || p.Start != ffmpegBefore.Start {
		t.Errorf("ffmpeg was %+v and is %+v (running %v)", ffmpegBefore, p, ok)
	}
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up after one missed renewal: %v", lines)
	}
}

// Every round trip of the worker's connections longer by 1 s (above the
// renewal's bound of half a second, a third of a heartbeat and more, and under
// half a heartbeat), for the whole film and so for longer than two leases: the
// job stays one row on one worker with one ffmpeg, its lease is never found run
// out, it ends done, no second job starts for the spec, and every segment is
// stored. A renewal that waits for its connection longer than its bound would
// give up every time and the job would be cut at the first lease.
func TestASlowDatabaseCostsNoJobItsLeaseOrItsWorker(t *testing.T) {
	slowAt(t, "2") // The film, cut short (slowFilmSeconds), still lasts more than two leases at 1 s a round trip.
	// The pool as production opens it, not the kit's exec mode: there the first use
	// of a statement on a connection is a prepare and an execute, two round trips,
	// and at 1 s each a renewal bounded at 1.5 s that is not sent in one round trip
	// gives up every time and the keeper cuts a healthy job at 9.5 s. A build must
	// send its renewal in one round trip on any connection (pgx.QueryExecModeExec).
	s := newStack(t, 1, productionPool)
	r, w, ctx := s.r, s.workers[0], context.Background()
	count := r.useSlowFilm(t) // Before the first request.
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	s.play(s.jobs, r.spec, psid(0), 1, count)
	// A third of the heartbeat, the least the criterion asks: a connection made
	// meanwhile (a new one takes several round trips) must still fit the fenced
	// put's check of half a lease (5 s), which JS-105 does not change.
	const delay = time.Second
	began := time.Now()
	w.delay(delay) // The delay begins with the job running; TestASlowDatabaseSetBeforeTheClaim... sets it before the claim.
	t.Cleanup(func() { w.delay(0) })
	// The keeper's connection, made before the delay, has its renewal statement
	// prepared already, so every renewal on it is one round trip whatever the
	// build sends. Made again under the delay it prepares first, which is where a
	// renewal that is not sent in one round trip shows.
	w.keep.Sever()

	var expired atomic.Int64
	var mu sync.Mutex
	seen := map[[2]string]bool{}
	sampling, stopSampling := context.WithCancel(ctx)
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for sampling.Err() == nil {
			var n int
			if r.pool.QueryRow(ctx, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at < now()").Scan(&n) == nil && n > 0 {
				expired.Add(int64(n))
			}
			mu.Lock()
			for _, p := range r.ffmpegs() {
				seen[[2]string{strconv.Itoa(p.Pid), p.Start}] = true
			}
			mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	waitFor(t, "the film to be done through a slow database", 6*time.Minute, func() bool { return r.jobState(t, job) != "running" })
	stopSampling()
	<-sampled
	notDoneInsideTwoLeases(t, r.jobState(t, job), began)
	if got := r.jobState(t, job); got != "done" {
		t.Errorf("the job ended %s through a slow database, want done", got)
	}
	if n := expired.Load(); n > 0 {
		t.Errorf("a running job's lease was found run out %d times", n)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); n != 1 {
		t.Errorf("%d jobs for the spec, want 1: %+v", n, r.jobRows(t))
	}
	if worker := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND worker = $2", r.hash, w.id); worker != 1 {
		t.Errorf("the job did not stay on worker %s", w.id)
	}
	mu.Lock()
	if len(seen) != 1 {
		t.Errorf("%d different ffmpeg processes ran, want 1: %v", len(seen), seen)
	}
	mu.Unlock()
	stored := r.storedSegments(r.hash)
	if len(stored) != count || stored[0] != 0 || stored[len(stored)-1] != count-1 {
		t.Errorf("stored segments %v, want 0 to %d", stored, count-1)
	}
	r.noFailed(t)
	if lines := interruptedLines(w); len(lines) > 0 {
		t.Errorf("the worker gave a job up through a slow database: %v", lines)
	}
}

// A worker stopped (SIGSTOP) past its lease gives its job up as cancelled, never
// failed, when it continues, and begins no put after the lease ran out (JS-066,
// JS-104) with the lease kept by the lease keeper: its timer fires as the
// process goes on, not after another heartbeat. The lease is read once the worker
// is stopped, where nothing can renew it.
func TestAWorkerStoppedPastItsLeaseGivesTheJobUpCancelledAndStoresNothingAfter(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, w, ctx := s.r, s.workers[0], context.Background()
	if _, err := s.fetch(ctx, s.jobs, r.spec, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	job := r.jobOf(t, r.hash)
	waitFor(t, "the job to have stored a segment", keeperHang, func() bool {
		return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= 1
	})
	w.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
	defer w.cmd.Process.Signal(syscall.SIGCONT)
	time.Sleep(200 * time.Millisecond)
	lease := r.leaseOf(t, job)
	waitFor(t, "the lease to run out while the worker is stopped", keeperHang, func() bool { return r.leaseRunOut(t, job) })
	time.Sleep(time.Second)               // Its ffmpeg has made segments meanwhile, which wait for it.
	w.cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
	waitFor(t, "the job to end after the worker went on", keeperHang, func() bool { return r.jobState(t, job) != "running" })
	time.Sleep(2 * time.Second)
	if got := r.jobState(t, job); got != "cancelled" {
		t.Errorf("a worker stopped past its lease ended its job %s, want cancelled", got)
	}
	r.noFailed(t)
	for _, p := range putsOf(t, s.putlog) {
		if p.worker == w.id && p.at.After(lease.Add(putAllowance)) {
			t.Errorf("worker %s began a put of %s at %v, %v after its job's lease ran out (%v)", p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(lease).Round(time.Millisecond), lease.Format("15:04:05.000"))
		}
	}
	waitFor(t, "the worker's ffmpeg to be gone", keeperHang, func() bool { _, ok := r.ffmpegOf(w); return !ok })
}
