//go:build integration

package transcode

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// JS-106. A store Put that is slow is not a lost job: the lease keeper (JS-105) holds the lease
// while the put runs, and the put's bytes count as the job's work coming round, so a put that
// moves keeps the job and one that does not is given up a lease after its last byte. These checks
// run a real worker process whose store is slow as slow_put_kit_integration_test.go says, at
// production's lease (10 s) and heartbeat (3 s): a put of one and a half leases is 15 s, and the
// kit reads in pieces a second apart, never still for a lease. A shorter lease would make the
// slow put short, and what it could hide is a wait of a few heartbeats that a lease of 10 s
// counts in tens of seconds, such as a stall limit that a put is counted against.
//
// Nothing here judges how long anything took. A put is slow by construction (fifteen pieces and a
// pause between each, counted in the kit's log); what is compared is the state Postgres holds.

const (
	slowLease     = 10 * time.Second
	slowHeartbeat = 3 * time.Second
	slowPutLength = "15s" // One and a half leases.
)

func slowStack(t *testing.T) (*stack, *outageWorker, string) {
	t.Helper()
	slow(t) // ffmpeg reads at the speed of the film, so it is still running when the job is judged.
	dir := t.TempDir()
	s := newStack(t, 1, "JELLYSCALE_TEST_LEASE="+slowLease.String(), "JELLYSCALE_TEST_HEARTBEAT="+slowHeartbeat.String(), slowPutEnv(dir))
	return s, s.workers[0], dir
}

// jobWatch samples the spec's job rows and the worker's ffmpeg until it is stopped, and says what
// it saw that a job kept through a slow put does not show: another job row, another owner, a lease
// that Postgres had as run out, another ffmpeg.
type jobWatch struct {
	mu       sync.Mutex
	jobs     map[int64]bool
	problems []string
	first    ffmpegProcess
	seen     int
	stop     func()
}

func (s *stack) watchJob(t *testing.T, w *outageWorker) *jobWatch {
	jw := &jobWatch{jobs: map[int64]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	problem := func(format string, args ...any) {
		jw.mu.Lock()
		if len(jw.problems) < 5 {
			jw.problems = append(jw.problems, time.Now().Format("15:04:05.000")+" "+sprintf(format, args...))
		}
		jw.mu.Unlock()
	}
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			rows, err := s.r.pool.Query(ctx, `SELECT id, COALESCE(worker, ''), state, lease_expires_at > now() FROM transcode_jobs WHERE spec_hash = $1`, s.r.hash)
			for err == nil && rows.Next() {
				var id int64
				var worker, state string
				var leaseStands bool
				if rows.Scan(&id, &worker, &state, &leaseStands) != nil {
					break
				}
				jw.mu.Lock()
				jw.jobs[id] = true
				jw.mu.Unlock()
				if state != "queued" && (state != "running" || worker != w.id || !leaseStands) {
					problem("job %d is %s on %q, its lease standing: %v", id, state, worker, leaseStands)
				}
			}
			if err == nil {
				rows.Close()
			}
			if p, ok := s.r.ffmpegOf(w); ok {
				jw.mu.Lock()
				if jw.seen == 0 {
					jw.first = p
				} else if p.Pid != jw.first.Pid || p.Start != jw.first.Start {
					problem("ffmpeg is %+v, was %+v", p, jw.first)
				}
				jw.seen++
				jw.mu.Unlock()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	jw.stop = func() { cancel(); <-done }
	t.Cleanup(jw.stop)
	return jw
}

func sprintf(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}

// A put that is slow and never still keeps the job: the same job row, worker, ffmpeg and lease,
// every put of the upload stored, the request that waited answered.
func TestASlowButMovingPutKeepsItsJob(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		wants int // Segments recorded when the case is judged.
	}{
		{"the first segment's put", "steady first " + slowPutLength, 2},
		{"the init segment's put", "steady init " + slowPutLength, 2},
		{"every put: init, the first segment and the next", "steady all " + slowPutLength, 2},
		// The store reads the whole body, seeks back to its start and reads it again over one and
		// a half leases: the body is one that seeks, and the reads of it are what is counted.
		{"a put that reads all, seeks back and reads it again", "seekback first " + slowPutLength, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, w, dir := slowStack(t)
			r := s.r
			setPuts(t, dir, w, c.mode)
			watch := s.watchJob(t, w)
			served := make(chan error, 1)
			go func() { _, err := s.fetch(context.Background(), s.jobs, r.spec, psid(0), 0); served <- err }()
			waitFor(t, "the job to run", keeperHang, func() bool {
				return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running'", r.hash) == 1
			})
			job := r.jobOf(t, r.hash)
			// Until the segments are recorded, or the job has ended: what ended it is the finding.
			waitFor(t, "the job to record its second segment or end", keeperHang, func() bool {
				return r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job) >= c.wants || r.jobState(t, job) != "running"
			})
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("the request that waited on the segment was not served: %v", err)
				}
			case <-time.After(keeperHang):
				t.Error("the request that waited on the segment was never answered")
			}
			if got := r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job); got < c.wants {
				t.Errorf("the job recorded %d segments, want %d", got, c.wants)
			}
			watch.stop()
			watch.mu.Lock()
			defer watch.mu.Unlock()
			if len(watch.jobs) != 1 {
				t.Errorf("the spec had %d job rows, want the one", len(watch.jobs))
			}
			if watch.seen == 0 {
				t.Error("the worker's ffmpeg was never seen")
			}
			for _, p := range watch.problems {
				t.Errorf("the job was not kept through the slow put: %s", p)
			}
			if got := strings.Count(w.log.String(), `msg="transcode started"`); got != 1 {
				t.Errorf("the worker logged %d 'transcode started' lines for the spec, want 1:\n%s", got, w.log.String())
			}
			if got := r.jobState(t, job); got != "running" {
				t.Errorf("the job is %s, want running", got)
			}
			r.noFailed(t)
			// The slow puts were slow, and were handed a body that seeks.
			slowed, reads := 0, 0
			for _, e := range slowEvents(t, dir, w) {
				switch e.event {
				case "begin":
					slowed++
				case "read":
					reads++
				case "seekable":
					if e.detail != "true" {
						t.Errorf("the put of %s was handed a body that cannot seek", e.key)
					}
				}
			}
			if slowed == 0 || reads < 10*slowed {
				t.Errorf("the store was slowed for %d puts with %d reads of the body: the case did not slow what it says", slowed, reads)
			}
			stored := r.storedSegments(r.hash)
			if len(stored) < 2 || stored[0] != 0 || stored[1] != 1 {
				t.Errorf("segments stored: %v, want 0 and 1", stored)
			}
			if _, err := r.segments.Stat(context.Background(), SegmentKey(r.hash, -1)); err != nil {
				t.Errorf("the init segment was not stored: %v", err)
			}
		})
	}
}

// startedAts are the times the worker logged that a transcode started.
func startedAts(w *outageWorker) []time.Time {
	var found []time.Time
	re := regexp.MustCompile(`time=(\S+) .*msg="transcode started"`)
	for _, line := range strings.Split(w.log.String(), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			if at, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				found = append(found, at)
			}
		}
	}
	return found
}

// A put that stops moving is a job's end: given up a lease after its last byte (the keeper's
// stall, with no number of its own), cancelled and never failed, by its own worker while the
// lease it holds stands, the stall the reason in the row and the log, its ffmpeg gone, and no put
// begun after. That a put that returns only when its context ends, and one that does not return
// then, are the same to the job is what the cases are for; a put that returns late must begin
// nothing after it and record no segment.
func TestAPutThatStopsMovingEndsItsJobCancelledForTheStall(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		puts  int // The puts the worker began for the job: the init, and the first segment if it got there.
		first string
	}{
		{"a put of the first segment that reads nothing and returns when its context ends", "stall first 0s", 2, "/0.mp4"},
		{"a put of the first segment that stops part-way and returns when its context ends", "stallpart first 0s", 2, "/0.mp4"},
		{"a put of the init segment that reads nothing and returns when its context ends", "stall init 0s", 1, "/init.mp4"},
		{"a put of the init segment that does not return when its context ends", "hang init 25s", 1, "/init.mp4"},
		{"a put of the first segment that does not return when its context ends", "hang first 25s", 2, "/0.mp4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, w, dir := slowStack(t)
			r := s.r
			setPuts(t, dir, w, c.mode)
			ctx, stopViewer := context.WithCancel(context.Background())
			defer stopViewer()
			go s.fetch(ctx, s.jobs, r.spec, psid(0), 0) //nolint:errcheck
			waitFor(t, "the slow put to begin", keeperHang, func() bool {
				for _, e := range slowEvents(t, dir, w) {
					if e.event == "begin" {
						return true
					}
				}
				return false
			})
			// The viewer asks no more, so no replacement is started: every put of the worker is the job's.
			stopViewer()
			job := r.jobOf(t, r.hash)
			before, ok := r.ffmpegOf(w)
			if !ok {
				t.Fatal("the job has no ffmpeg while its put is slow")
			}
			var state, worker, reason string
			var lease, ended bool
			waitFor(t, "the job to end", keeperHang, func() bool {
				if err := r.pool.QueryRow(context.Background(), `SELECT state, COALESCE(worker, ''), COALESCE(error, ''), lease_expires_at > now() FROM transcode_jobs WHERE id = $1`,
					job).Scan(&state, &worker, &reason, &lease); err != nil {
					t.Fatal(err)
				}
				ended = state != "running"
				return ended
			})
			if state != "cancelled" {
				t.Errorf("the job ended %s (%s), want cancelled: a failed job refuses its spec to every viewer", state, reason)
			}
			if worker != w.id {
				t.Errorf("the row ended by worker %q, want its own, %s", worker, w.id)
			}
			if !lease {
				t.Error("the row was ended after its lease ran out: the worker did not give the job up in time, a sweep did")
			}
			if !strings.Contains(reason, "has not come round") {
				t.Errorf("the row's error is %q, want the stall as the reason", reason)
			}
			// The row is written before the ffmpeg is ended (DOMAINS 25), and the
			// log line after it, so the line is waited for rather than read at once.
			stalled := func() bool {
				for _, line := range interruptedLines(w) {
					if strings.Contains(line, "has not come round") {
						return true
					}
				}
				return false
			}
			waitFor(t, "the worker's log to give the stall as the reason", keeperHang, stalled)
			if gone, _ := r.goneBefore(t, before, r.leaseOf(t, job)); !gone {
				t.Error("the job's ffmpeg is still running")
			}
			// A put that returns later: wait for it, then for what a worker that went on would have begun.
			waitFor(t, "the put to return", keeperHang, func() bool {
				begun, ended := 0, 0
				for _, e := range slowEvents(t, dir, w) {
					switch e.event {
					case "begin":
						begun++
					case "end":
						ended++
					}
				}
				return begun == ended
			})
			time.Sleep(2 * time.Second)
			began := 0
			for _, p := range putsOf(t, s.putlog) {
				if p.worker == w.id {
					began++
				}
			}
			if began != c.puts {
				t.Errorf("the worker began %d puts for the job, want %d: it began one after the job was given up", began, c.puts)
			}
			if got := r.count(t, "SELECT next_segment FROM transcode_jobs WHERE id = $1", job); got != 0 {
				t.Errorf("the job's next segment is %d: a segment was recorded for a job that was given up", got)
			}
			if got := len(startedAts(w)); got != 1 {
				t.Errorf("%d jobs were started: a replacement was made, so the puts counted are not only the job's", got)
			}
			r.noFailed(t)
		})
	}
}

// A slow put in flight does not carry the job's right to put to the put after it: the check before
// each put is asked of Postgres when that put is to begin, however long the put before it took.
// The job stops being the worker's while the init segment's put is in flight (a cancel, a cut and
// the deletion of the worker's row, a cancel just after a renewal), and its put goes on; when it
// returns no put begins, nor one more than a round trip after the job stopped being the worker's.
// The existing cases of JS-105 (lease_keeper_fence_integration_test.go) hold the same for puts
// that are not slow, and are not changed.
func TestNoPutBeginsAfterTheJobWasLostWhileASlowPutWasInFlight(t *testing.T) {
	lose := map[string]func(t *testing.T, s *stack, w *outageWorker, job int64) time.Time{
		"cancelled": func(t *testing.T, s *stack, w *outageWorker, job int64) time.Time {
			s.r.exec(t, "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1", job)
			return time.Now()
		},
		"cancelled just after a renewal": func(t *testing.T, s *stack, w *outageWorker, job int64) (at time.Time) {
			done := w.keep.OnStatement(isTheRenewal, nil, func() {
				s.r.exec(t, "UPDATE transcode_jobs SET state = 'cancelled' WHERE id = $1", job)
				at = time.Now()
			})
			select {
			case <-done:
			case <-time.After(keeperHang):
				t.Fatal("the worker sent no renewal")
			}
			return at
		},
		"the lease cut and the worker's row deleted while the pool is held": func(t *testing.T, s *stack, w *outageWorker, job int64) time.Time {
			w.db.Hold()
			t.Cleanup(w.db.Release)
			if _, err := s.r.pool.Exec(context.Background(), `WITH cut AS (UPDATE transcode_jobs SET lease_expires_at = LEAST(lease_expires_at, now() + make_interval(secs => 0.5))
					WHERE state = 'running' AND worker = $1 RETURNING id)
				DELETE FROM transcode_nodes WHERE id = $1`, w.id); err != nil {
				t.Fatal(err)
			}
			return time.Now()
		},
	}
	for name, lose := range lose {
		t.Run(name, func(t *testing.T) {
			s, w, dir := slowStack(t)
			r := s.r
			setPuts(t, dir, w, "gated init "+slowPutLength)
			ctx, stopViewer := context.WithCancel(context.Background())
			defer stopViewer()
			go s.fetch(ctx, s.jobs, r.spec, psid(0), 0) //nolint:errcheck
			// The job is lost late in the put, which then waits at its thirteenth read (a gated put) until
			// the test lets it go a second after the loss: the put outlives the loss, and the keeper's
			// next renewal (a heartbeat after the one the loss follows) comes after the put returns. Only
			// a worker that fences again before each put (not one that fenced once per upload, or looked
			// at its context alone) begins nothing. Lost at the first read, the keeper would find the
			// loss and cancel the context ten seconds before the put returns.
			waitFor(t, "the init segment's put to be at its thirteenth read", keeperHang, func() bool {
				reads := 0
				for _, e := range slowEvents(t, dir, w) {
					if e.event == "read" {
						reads++
					}
				}
				return reads >= 13
			})
			stopViewer()
			job := r.jobOf(t, r.hash)
			lostAt := lose(t, s, w, job)
			time.Sleep(time.Second)
			releaseGate(t, dir, w)
			waitFor(t, "the init segment's put to return", keeperHang, func() bool {
				begun, ended := 0, 0
				for _, e := range slowEvents(t, dir, w) {
					switch e.event {
					case "begin":
						begun++
					case "end":
						ended++
					}
				}
				return begun > 0 && begun == ended
			})
			waitFor(t, "the worker's ffmpeg to be gone", keeperHang, func() bool { _, ok := r.ffmpegOf(w); return !ok })
			time.Sleep(2 * time.Second)
			for _, p := range putsOf(t, s.putlog) {
				if p.worker == w.id && p.at.After(lostAt.Add(putAllowance)) {
					t.Errorf("worker %s began a put of %s at %v, %v after job %d stopped being its own while a slow put was in flight; a put's check allows %v",
						p.worker, p.key, p.at.Format("15:04:05.000"), p.at.Sub(lostAt).Round(time.Millisecond), job, putAllowance)
				}
			}
			w.db.Release()
			waitFor(t, "the job to end", keeperHang, func() bool { return r.jobState(t, job) != "running" })
			r.noFailed(t)
		})
	}
}
