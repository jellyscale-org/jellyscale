//go:build integration

package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-088: a film recovers from a failed segment without the viewer acting. A job that fails for a cause that is not
// the file's (the transcode store refusing a put, the source store cutting a read, ffmpeg killed by a signal) is
// made again for the request that waited on it, whatever the job was (a re-encode, a copy, a burn-in), while a job
// that fails for the file's own (ffmpeg refusing its data, its input ending early) is not (DOMAINS invariants 40, 45).
//
// These checks name nothing the build must add: they ask Jobs.Segment and Jobs.Unavailable, read transcode_jobs
// and the jobs-ended counter, and look at the ffmpeg command lines a shim wrote. A request waits at most
// recoveryWait, which is below failureMemory (30 s) on purpose: a build that answers the retry by waiting out the
// memory of the failure it just recorded gets ErrTimeout here, as it would hang the player's ten seconds. The time
// the right build took is logged, never judged.

const recoveryWait = 25 * time.Second

// recoveryFreshJobs is the bound the task states (3), written out and not read from maxRestarts, so that a build
// that raises the constant fails here.
const recoveryFreshJobs = 3

// recoveryCutAfter is where a cut store stops reading the film: inside the first segment of every kind of job below.
const recoveryCutAfter = 40_000

// A cause is a way a job fails that is not the file's. arm makes it happen to the first n jobs of the rig (n 0 is
// every job), and returns what the first failed job's recorded error must hold, which names the cause.
type failureCause struct {
	name string
	arm  func(t *testing.T, r *rig, change *[]func(*Worker), n int) (mustHold string)
}

var recoveryCauses = []failureCause{
	{"the transcode store refuses a put", armPutRefused},
	{"the source store cuts a read", armSourceCut},
	{"ffmpeg is killed by a signal", armFFmpegKilled},
}

// recoveryRefusingStore refuses its first `left` puts (every put when always) the way a store whose endpoint is down does,
// and holds each refused put at `hold`, when it is set, until the test closes it.
type recoveryRefusingStore struct {
	store.Store
	left   atomic.Int32
	always bool
	hold   chan struct{} // Nil: refuse at once.
	puts   atomic.Int32
}

func (s *recoveryRefusingStore) Put(ctx context.Context, key string, body io.Reader) error {
	s.puts.Add(1)
	if s.always || s.left.Add(-1) >= 0 {
		if s.hold != nil {
			select {
			case <-s.hold:
			case <-ctx.Done():
			}
		}
		return errors.New("put " + key + ": dial tcp 127.0.0.1:9: connect: connection refused")
	}
	return s.Store.Put(ctx, key, body)
}

func armPutRefused(t *testing.T, r *rig, change *[]func(*Worker), n int) string {
	t.Helper()
	refusing := &recoveryRefusingStore{Store: r.segments, always: n == 0}
	refusing.left.Store(int32(n))
	r.segments = refusing
	*change = append(*change, func(w *Worker) { w.Segments, w.SegmentsURI = refusing, transcodeStore })
	return transcodeStore
}

// recoveryCuttingMedia is the film's store when it is an object store: no path on this machine, so ffmpeg reads
// through the worker's loopback server, which serves the source's size (the rig's size is 1, so armSourceCut sets
// the file's own). A cut handle fails every read of the film at or past `after` bytes, a ranged reopen included,
// for as long as the handle lives: ffmpeg resumes a body that was cut once on a ranged reopen past the cut, and
// so a fault that spared the reopen would sometimes not fail the job at all. A worker takes one handle for each
// job (Worker.Media is asked once in a transcode job), and the first n handles that read the film are cut: the
// job that holds one fails with a store error, the fresh job that follows takes a clean one. Other keys, the
// asset objects the subtitle jobs write, are never cut.
type recoveryCuttingMedia struct {
	store.Store
	film   string
	after  int64
	always bool
	left   *atomic.Int32 // Handles still to be cut, shared by every handle of the worker.
	once   sync.Once
	cut    bool
}

func (c *recoveryCuttingMedia) LocalPath(string) (string, bool) { return "", false }

func (c *recoveryCuttingMedia) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := c.Store.Open(ctx, key, offset)
	if err != nil || key != c.film {
		return body, err
	}
	c.once.Do(func() { c.cut = c.always || c.left.Add(-1) >= 0 })
	if !c.cut {
		return body, nil
	}
	return &recoveryCutBody{ReadCloser: body, at: offset, after: c.after}, nil
}

type recoveryCutBody struct {
	io.ReadCloser
	at, after int64 // The film's offset of the next byte, and where reads stop.
}

func (b *recoveryCutBody) Read(p []byte) (int, error) {
	if b.at >= b.after {
		return 0, errors.New("read tcp 127.0.0.1:9: i/o timeout")
	}
	n, err := b.ReadCloser.Read(p[:min(int64(len(p)), b.after-b.at)])
	b.at += int64(n)
	return n, err
}

func armSourceCut(t *testing.T, r *rig, change *[]func(*Worker), n int) string {
	t.Helper()
	var key string
	if err := r.pool.QueryRow(context.Background(), "SELECT store_key FROM media_sources").Scan(&key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(film(t), key))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 2*recoveryCutAfter {
		t.Fatalf("the film is %d bytes, too small to cut inside it", info.Size())
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	left := new(atomic.Int32)
	left.Store(int32(n))
	*change = append(*change, func(w *Worker) {
		opened := w.Media
		w.Media = func(uri string) (store.Store, error) {
			inner, err := opened(uri)
			if err != nil {
				return nil, err
			}
			return &recoveryCuttingMedia{Store: inner, film: key, after: recoveryCutAfter, always: n == 0, left: left}, nil
		}
	})
	return "file://" + film(t)
}

// armFFmpegKilled puts an ffmpeg on the PATH that kills itself with SIGKILL, as the first n runs that write
// segments, and runs the real one after. The commands it saw are in the file it returns.
func armFFmpegKilled(t *testing.T, r *rig, change *[]func(*Worker), n int) string {
	t.Helper()
	film(t) // Made at full speed, before the shim is in the way.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "runs.log")
	if n == 0 {
		n = 1 << 20
	}
	script := "#!/bin/sh\ncase \"$*\" in\n*\"-f hls\"*)\n  echo \"$*\" >>" + log + "\n  if [ \"$(wc -l <" + log + ")\" -le " + strconv.Itoa(n) + " ]; then kill -KILL $$; fi;;\nesac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("JELLYSCALE_TEST_FFMPEG_RUNS", log)
	return "killed"
}

// recoveryKinds of job a viewer's request can wait on. prepare makes the rig's film and spec; segment is the first one the
// viewer asks for, and far one that needs a job of its own.
type recoveryKind struct {
	name         string
	prepare      func(t *testing.T, r *rig)
	first, far   int
	isCopy, burn bool
}

var recoveryKinds = []recoveryKind{
	{name: "a re-encode", prepare: func(t *testing.T, r *rig) {}, first: 0, far: 14},
	{name: "a copy", prepare: func(t *testing.T, r *rig) { r.copied(t, "film.mkv") }, first: 0, far: 6, isCopy: true},
	{name: "a job that burns in a subtitle", prepare: func(t *testing.T, r *rig) {
		testenv.Font(t)
		r.useBurnFilm(t)
		index := 2
		r.spec.Subtitle = &index
		r.hash = r.spec.Hash()
		r.warmBurn(t)
	}, first: 0, far: 8, burn: true},
}

// warmBurn has the track extracted and the film's fonts listed, which the first burn of a track waits for and which
// read the film through the same store the transcode's ffmpeg does, so that a fault of the source store lands on the
// job's own read and not on an extraction whose failure is a subtitle job's (invariant 43). It plays another spec
// of the film with the same track (a lower video bitrate: a spec of its own, so none of its segments answers the
// test's) with its own worker, then leaves the rig as it was: no job, no viewer.
func (r *rig) warmBurn(t *testing.T) {
	t.Helper()
	spec, hash := r.spec, r.hash
	defer func() { r.spec, r.hash = spec, hash }()
	r.spec.VideoBitrate = 1_000_000
	r.hash = r.spec.Hash()
	stop := r.worker(t, nil)
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatalf("the warming burn: %v", err)
	}
	r.idle(t)
	stop()
	if _, err := r.pool.Exec(context.Background(), "TRUNCATE transcode_jobs, transcode_viewers, transcode_nodes"); err != nil {
		t.Fatal(err)
	}
}

// failedJobs is the jobs-ended counter's failed series.
func failedJobs(t *testing.T) float64 { return counterValue(t, jobsEnded.WithLabelValues("failed")) }

func (r *rig) states(t *testing.T) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT state FROM transcode_jobs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	return states
}

func (r *rig) idle(t *testing.T) {
	t.Helper()
	waitFor(t, "the jobs to end", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
}

// A failure that is not the file's costs the viewer nothing: the request that waited on the job is answered with
// the segment, by a fresh job, and no later request or PlaybackInfo is refused for it. The failure is still told:
// the job is recorded failed with its cause and counted.
func TestAFailureThatIsNotTheFilesIsMadeAgainForTheRequestThatWaited(t *testing.T) {
	for _, k := range recoveryKinds {
		for _, c := range recoveryCauses {
			t.Run(k.name+", "+c.name, func(t *testing.T) {
				r := newRig(t, nil)
				k.prepare(t, r)
				var change []func(*Worker)
				mustHold := c.arm(t, r, &change, 1)
				r.worker(t, func(w *Worker) {
					for _, f := range change {
						f(w)
					}
				})
				j := r.jobs(t)
				j.WaitTimeout = recoveryWait
				j.Segments = r.segments
				failedBefore := failedJobs(t)

				began := time.Now()
				body, err := r.fetch(t, j, psid(0), k.first)
				t.Logf("the request was answered after %v (err %v)", time.Since(began).Round(time.Millisecond), err)
				if err != nil || !isMP4Segment(body) {
					t.Fatalf("segment %d: %d bytes, %v; the job that failed for a cause that is not the file's was not made again", k.first, len(body), err)
				}
				// Asked at once, before the fresh job is likely to have made it: the spec's failure is not held against it.
				if _, err := r.fetch(t, j, psid(0), k.far); err != nil {
					t.Errorf("segment %d, asked after the recovered failure: %v", k.far, err)
				}
				r.idle(t)

				states := r.states(t)
				if len(states) < 2 || states[0] != "failed" || states[len(states)-1] != "done" {
					t.Fatalf("the jobs ended %v, want the first failed and the last done", states)
				}
				reason := mustScalar(t, r, "SELECT error FROM transcode_jobs ORDER BY id LIMIT 1")
				if !strings.Contains(reason, mustHold) {
					t.Errorf("the failed job's recorded error %q does not name its cause (%q)", reason, mustHold)
				}
				if got := failedJobs(t) - failedBefore; got != 1 {
					t.Errorf("jellyscale_transcode_jobs_ended_total{result=\"failed\"} rose by %v, want 1: a recovered failure stays visible", got)
				}
				// PlaybackInfo asks this before it offers a transcode.
				if reason, err := j.Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || reason != "" {
					t.Errorf("PlaybackInfo refuses the spec after a recovered failure: %q, %v", reason, err)
				}
				if k.isCopy {
					// Invariant 45: a copy is made again as a copy, and the index is kept.
					if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
						t.Errorf("the keyframe index has %d entries after the failure, want 9: it was dropped", n)
					}
					if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec->>'Copy' IS DISTINCT FROM 'true'"); n != 0 {
						t.Errorf("%d jobs of the spec are not copies: a re-encode started", n)
					}
				}
				if runs := os.Getenv("JELLYSCALE_TEST_FFMPEG_RUNS"); runs != "" && k.isCopy {
					raw, _ := os.ReadFile(runs)
					for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
						if !strings.Contains(line, "-c:v copy") {
							t.Errorf("an ffmpeg that does not copy the video ran for a copy: %s", line)
						}
					}
				}
			})
		}
	}
}

// A cause that persists is not made again for ever: one request starts at most maxRestarts fresh jobs, and then
// gets the documented failure (JS-107), which names the cause.
func TestACauseThatPersistsStartsAtMostThreeFreshJobsAndThenFails(t *testing.T) {
	for _, c := range recoveryCauses {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			var change []func(*Worker)
			mustHold := c.arm(t, r, &change, 0)
			r.worker(t, func(w *Worker) {
				for _, f := range change {
					f(w)
				}
			})
			j := r.jobs(t)
			j.WaitTimeout = recoveryWait
			j.Segments = r.segments

			_, err := r.fetch(t, j, psid(0), 0)
			var failed FailedError
			if !errors.As(err, &failed) || !strings.Contains(failed.Reason, mustHold) {
				t.Fatalf("the request got %v, want the failure that names %q", err, mustHold)
			}
			r.idle(t)
			states := r.states(t)
			if len(states) < 2 {
				t.Errorf("%d job: a cause that is not the file's was not made again", len(states))
			}
			if len(states) > 1+recoveryFreshJobs {
				t.Errorf("%d jobs for one request, want at most the first and %d fresh ones: %v", len(states), recoveryFreshJobs, states)
			}
			for _, state := range states {
				if state != "failed" {
					t.Errorf("a job ended %s though nothing could have made the segment: %v", state, states)
					break
				}
			}
		})
	}
}

// Two requests, two viewers or two stream replicas, wait on one job as it fails. They start one fresh job between
// them. The transcode store holds the first put until both requests are on the job, and then refuses it, so the
// failure comes with both waiting; a build whose retry starts a job for each request (the mutant: the retry inserts
// a job without looking for a live one of the spec) ends with three jobs.
//
// A sleep before the test's step, which opens the hold, changes nothing a right build does: it waits for the one state
// the viewers' rows give, and the worker's lease is longer than any wait here, so that the job is not given up
// while its put is held.
func TestTwoRequestsWaitingOnAJobAsItFailsStartOneFreshJobBetweenThem(t *testing.T) {
	r := newRig(t, nil)
	refusing := &recoveryRefusingStore{Store: r.segments, hold: make(chan struct{})}
	refusing.left.Store(1)
	r.segments = refusing
	r.worker(t, func(w *Worker) { w.Segments, w.SegmentsURI, w.Lease = refusing, transcodeStore, time.Minute })
	a, b := r.jobs(t), r.jobs(t) // Two stream replicas.
	for _, j := range []*Jobs{a, b} {
		j.WaitTimeout = recoveryWait
		j.Segments = refusing
	}

	var wg sync.WaitGroup
	answers := make([]error, 2)
	for i, j := range []*Jobs{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, answers[i] = r.fetch(t, j, psid(i), 0)
		}()
	}
	// Both viewers' rows name the same job: both requests are waiting on it.
	waitFor(t, "both viewers to wait on the one job", 30*time.Second, func() bool {
		return r.count(t, `SELECT count(*) FROM transcode_viewers v WHERE v.play_session_id IN ($1, $2) AND v.job_id <> 0
			AND v.job_id = (SELECT max(id) FROM transcode_jobs)`, psid(0), psid(1)) == 2
	})
	waitFor(t, "the first put to be held at the store", 30*time.Second, func() bool { return refusing.puts.Load() == 1 })
	close(refusing.hold)
	wg.Wait()
	for i, err := range answers {
		if err != nil {
			t.Errorf("viewer %d: %v", i, err)
		}
	}
	r.idle(t)
	if states := r.states(t); len(states) != 2 || states[0] != "failed" {
		t.Fatalf("the jobs ended %v, want the failed one and one fresh job between the two requests", states)
	}
}

// What is the file's is not made again within failureMemory: ffmpeg failing on the file's own data, or the input
// ending early (invariant 40). The request is answered with the failure, and the spec is refused as before.
func TestAFailureThatIsTheFilesStartsNoFreshJob(t *testing.T) {
	t.Run("ffmpeg fails on the file's own data", func(t *testing.T) {
		r := newRig(t, nil)
		whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(film(t), "garbled.mkv"), whole[:4096], 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = 'garbled.mkv'"); err != nil {
			t.Fatal(err)
		}
		r.worker(t, nil)
		j := r.jobs(t)
		j.WaitTimeout = recoveryWait
		_, err = r.fetch(t, j, psid(0), 0)
		var failed FailedError
		if !errors.As(err, &failed) {
			t.Fatalf("got %v, want the failure", err)
		}
		// Another viewer, within failureMemory: refused with the memory of it, and no job.
		if _, err := r.fetch(t, j, psid(1), 0); !errors.As(err, &failed) || !strings.Contains(failed.Reason, "the last attempt failed") {
			t.Errorf("a second viewer got %v, want the spec refused as before", err)
		}
		r.idle(t)
		if states := r.states(t); len(states) != 1 || states[0] != "failed" {
			t.Errorf("the jobs ended %v, want one failed job and no fresh one", states)
		}
		if reason, _ := j.Unavailable(context.Background(), r.spec, r.user, "device"); !strings.Contains(reason, "the last attempt failed") {
			t.Errorf("PlaybackInfo is not refused for a file ffmpeg cannot read: %q", reason)
		}
	})
	for name, copied := range map[string]bool{"the input ends early, re-encoded": false, "the input ends early, copied": true} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			file := "cut-recovery.mkv"
			if err := os.WriteFile(filepath.Join(film(t), file), whole, 0o644); err != nil {
				t.Fatal(err)
			}
			if copied {
				r.copied(t, file) // The index is read from the whole file, as the scan did before it was cut.
			} else if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = $1", file); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(film(t), file), whole[:len(whole)*2/5], 0o644); err != nil {
				t.Fatal(err)
			}
			r.worker(t, nil)
			j := r.jobs(t)
			j.WaitTimeout = recoveryWait
			if _, err := r.fetch(t, j, psid(0), 0); err != nil {
				t.Fatalf("segment 0 is whole and should be served: %v", err)
			}
			waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
			next := r.count(t, "SELECT next_segment FROM transcode_jobs")
			var failed FailedError
			if _, err := r.fetch(t, j, psid(0), next); !errors.As(err, &failed) {
				t.Errorf("the segment the cut input never gave (%d) was answered with %v, want the failure", next, err)
			}
			r.idle(t)
			if states := r.states(t); len(states) != 1 || states[0] != "failed" {
				t.Errorf("the jobs ended %v, want the one failed job and no fresh one within failureMemory", states)
			}
			if copied {
				if n := r.count(t, "SELECT cardinality(keyframes) FROM media_sources"); n != 9 {
					t.Errorf("the index has %d keyframes after an input that ended early, want 9", n)
				}
			}
		})
	}
}
