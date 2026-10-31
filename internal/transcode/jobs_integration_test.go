package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/store"
)

func psid(i int) string { return fmt.Sprintf("%032x", i+1) }

// Any replica serves any segment, and concurrent requests for one missing
// segment, from replicas and viewers alike, start one job.
func TestAnyReplicaServesAnySegmentAndOneJobStarts(t *testing.T) {
	targets := map[string]store.Store{"posix": nil}
	for name, s := range s3Segments(t) {
		targets[name] = s
	}
	for name, segments := range targets {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, segments)
			r.worker(t, nil)
			replicas := []*Jobs{r.jobs(t), r.jobs(t)}

			const viewers = 8
			results := make([][]byte, viewers)
			errs := make([]error, viewers)
			var wg sync.WaitGroup
			for i := range viewers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = r.fetch(t, replicas[i%2], psid(i), 3)
				}()
			}
			wg.Wait()
			for i := range viewers {
				if errs[i] != nil {
					t.Fatalf("viewer %d: %v", i, errs[i])
				}
				if !isMP4Segment(results[i]) || !bytes.Equal(results[i], results[0]) {
					t.Fatalf("viewer %d got %d bytes that differ from viewer 0's", i, len(results[i]))
				}
			}
			if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); jobs != 1 {
				t.Fatalf("%d concurrent requests for one segment started %d jobs", viewers, jobs)
			}
			if started := r.count(t, "SELECT start_segment FROM transcode_jobs WHERE spec_hash = $1", r.hash); started != 3 {
				t.Fatalf("the job started at segment %d, want 3", started)
			}
			if procs := r.ffmpegProcesses(); len(procs) != 1 {
				t.Fatalf("%d ffmpeg processes: %v", len(procs), procs)
			}

			// Either replica now serves the init segment and the next one without a new job.
			init, err := r.fetch(t, replicas[1], psid(0), -1)
			if err != nil || !bytes.Contains(init[:32], []byte("ftyp")) {
				t.Fatalf("init segment: %d bytes, %v", len(init), err)
			}
			if next, err := r.fetch(t, replicas[0], psid(1), 4); err != nil || !isMP4Segment(next) {
				t.Fatalf("segment 4: %d bytes, %v", len(next), err)
			}
			if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); jobs != 1 {
				t.Fatalf("serving the next segment started another job: %d", jobs)
			}
		})
	}
}

// A seek past where the job has got starts a job there, and the one it
// replaces stops: its ffmpeg exits.
func TestSeekPastTheEncodedPointReplacesTheJob(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if procs := r.ffmpegProcesses(); len(procs) != 1 || segmentStart(procs[0]) != "0" {
		t.Fatalf("expected one ffmpeg at segment 0: %v", procs)
	}
	if _, err := r.fetch(t, j, psid(0), 20); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first ffmpeg to exit", 10*time.Second, func() bool {
		procs := r.ffmpegProcesses()
		return len(procs) == 1 && segmentStart(procs[0]) == "20"
	})
	if cancelled := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'cancelled'"); cancelled != 1 {
		t.Fatalf("%d cancelled jobs", cancelled)
	}
}

// A request within reach of the running job waits for it rather than restart.
func TestNearbySegmentWaitsForTheRunningJob(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(t, j, psid(0), 2); err != nil {
		t.Fatal(err)
	}
	if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs"); jobs != 1 {
		t.Fatalf("%d jobs for two nearby segments", jobs)
	}
}

// Every way of ending a transcode ends the real process.
func TestKillPathsEndFFmpeg(t *testing.T) {
	slow(t)
	cases := map[string]struct {
		change func(*Worker)
		end    func(t *testing.T, r *rig, j *Jobs)
	}{
		"stop from the player": {end: func(t *testing.T, r *rig, j *Jobs) {
			if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
				t.Fatal(err)
			}
		}},
		"DELETE ActiveEncodings, sent twice": {end: func(t *testing.T, r *rig, j *Jobs) {
			for range 2 {
				if err := j.Stop(context.Background(), r.user, "", "device", ""); err != nil {
					t.Fatal(err)
				}
			}
		}},
		"a closed tab sends nothing": {change: func(w *Worker) { w.Idle = 2 * time.Second }, end: func(*testing.T, *rig, *Jobs) {}},
		"the lease is lost": {end: func(t *testing.T, r *rig, j *Jobs) {
			// Another worker's name on the job: ours no longer holds the lease.
			if _, err := r.pool.Exec(context.Background(), "UPDATE transcode_jobs SET worker = 'someone-else'"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.worker(t, c.change)
			j := r.jobs(t)
			if _, err := r.fetch(t, j, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			if len(r.ffmpegProcesses()) != 1 {
				t.Fatal("ffmpeg isn't running")
			}
			c.end(t, r, j)
			waitFor(t, "ffmpeg to exit", 15*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
			// A stopped viewer's late request must not start work again.
			if name == "stop from the player" {
				if err := j.Watch(context.Background(), r.viewer(psid(0)), r.hash); !errors.Is(err, ErrStopped) {
					t.Fatalf("late request from a stopped viewer: %v", err)
				}
			}
		})
	}
}

// Two viewers on one spec share the job, and stopping one leaves the other's.
func TestStoppingOneOfTwoViewersLeavesTheOther(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	for i := range 2 {
		if _, err := r.fetch(t, j, psid(i), 0); err != nil {
			t.Fatal(err)
		}
	}
	if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs"); jobs != 1 {
		t.Fatalf("two viewers on a spec started %d jobs", jobs)
	}
	if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	// Several heartbeats pass; the job must still be there.
	time.Sleep(time.Second)
	if len(r.ffmpegProcesses()) != 1 {
		t.Fatal("stopping one viewer killed the other's ffmpeg")
	}
	if _, err := r.fetch(t, j, psid(1), 5); err != nil {
		t.Fatalf("the remaining viewer lost its stream: %v", err)
	}
	if err := j.Stop(context.Background(), r.user, psid(1), "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ffmpeg to exit once nobody watches", 15*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
}

// Quitting a worker stops its ffmpeg, so none outlives it.
func TestShuttingTheWorkerDownEndsFFmpeg(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	stop := r.worker(t, nil)
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatal(err)
	}
	stop()
	if procs := r.ffmpegProcesses(); len(procs) != 0 {
		t.Fatalf("ffmpeg outlived its worker: %v", procs)
	}
}

// A viewer whose transcode can't start gets an error, not a hang, and an
// operator can read why.
func TestFailuresAreReportedNotHung(t *testing.T) {
	t.Run("ffmpeg fails", func(t *testing.T) {
		r := newRig(t, nil)
		r.worker(t, nil)
		// The file on disk is not media.
		dir := strings.TrimPrefix(mustScalar(t, r, "SELECT media_store_uri FROM libraries"), "file://")
		bad := dir + "-bad"
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(bad) })
		if err := os.WriteFile(bad+"/film.mkv", []byte("not a movie"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(context.Background(), "UPDATE libraries SET media_store_uri = $1", "file://"+bad); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		_, err := r.fetch(t, r.jobs(t), psid(0), 0)
		var failed FailedError
		if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "ffmpeg") {
			t.Fatalf("got %v", err)
		}
		if time.Since(began) > 20*time.Second {
			t.Fatalf("took %v to report", time.Since(began))
		}
		if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); reason == "" {
			t.Fatal("the job row keeps no reason")
		}
	})
	t.Run("the file changed", func(t *testing.T) {
		r := newRig(t, nil)
		r.worker(t, nil)
		if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET object_etag = 'newer'"); err != nil {
			t.Fatal(err)
		}
		_, err := r.fetch(t, r.jobs(t), psid(0), 0)
		var failed FailedError
		if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "changed") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("no worker", func(t *testing.T) {
		r := newRig(t, nil)
		j := r.jobs(t)
		j.QueueTimeout = time.Second
		if _, err := r.fetch(t, j, psid(0), 0); !errors.Is(err, ErrNoWorker) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a segment past the end", func(t *testing.T) {
		r := newRig(t, nil)
		r.worker(t, nil)
		j := r.jobs(t)
		j.WaitTimeout = 15 * time.Second
		// Segments is the API's check; here the job runs off the end of the file.
		if _, err := r.fetch(t, j, psid(0), 500); err == nil {
			t.Fatal("a segment past the end of the file came back")
		}
	})
}

func mustScalar(t *testing.T, r *rig, sql string) string {
	t.Helper()
	var value string
	if err := r.pool.QueryRow(context.Background(), sql).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// Segments of specs nobody asked for lately are deleted; others stay.
func TestSweepDeletesOnlyUnusedSpecs(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	other := "ffffffffffffffffffffffffffffffff"
	for _, hash := range []string{r.hash, other} {
		for _, key := range []string{SegmentKey(hash, -1), SegmentKey(hash, 0)} {
			if err := r.segments.Put(ctx, key, strings.NewReader("data")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.pool.Exec(ctx, "INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id) VALUES ($1, $2, $3, 'd')",
			psid(0), hash, r.user); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.pool.Exec(ctx, "UPDATE transcode_viewers SET last_seen_at = now() - interval '2 hours' WHERE spec_hash = $1", other); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: r.pool, Segments: r.segments, Log: r.log, Retention: time.Hour}
	if err := w.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if objects, _ := r.segments.List(ctx, "segments/"+other); len(objects) != 0 {
		t.Errorf("the unused spec keeps %d objects", len(objects))
	}
	if objects, _ := r.segments.List(ctx, "segments/"+r.hash); len(objects) != 2 {
		t.Errorf("the recent spec has %d objects", len(objects))
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_viewers WHERE spec_hash = $1", other); n != 0 {
		t.Errorf("the unused spec keeps %d viewer rows", n)
	}
	// Sweeping again, with nothing left to do, is harmless.
	if err := w.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
}

// A worker runs at most Slots ffmpeg processes; the rest wait in the queue and
// start when a slot frees.
func TestSlotsBoundConcurrentTranscodes(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) { w.Slots = 1 })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	// A second spec, from another viewer, asks for a job of its own.
	other := r.spec
	other.AudioBitrate = 64_000
	otherHash := other.Hash()
	ctx := context.Background()
	if err := j.Watch(ctx, r.viewer(psid(1)), otherHash); err != nil {
		t.Fatal(err)
	}
	if _, err := j.ensure(ctx, other, otherHash, psid(1), 0, 0); err != nil {
		t.Fatal(err)
	}
	Notify(ctx, j.Pool, jobHint)
	time.Sleep(2 * time.Second)
	if running, queued := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'"), r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'queued'"); running != 1 || queued != 1 {
		t.Fatalf("%d running and %d queued, want 1 and 1", running, queued)
	}
	if procs := r.ffmpegProcesses(); len(procs) != 1 {
		t.Fatalf("%d ffmpeg processes with one slot", len(procs))
	}
	if err := j.Stop(ctx, r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the queued job to take the freed slot", 15*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND spec_hash = $1", otherHash) == 1 && len(r.ffmpegProcesses()) == 1
	})
}

// A Stop racing a job's insert leaves nothing running, whichever lands first
// (invariant 38). A stop that lands between Watch and ensure used to lose.
func TestStopRacingAnInsertLeavesNoJob(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	ctx := context.Background()
	for i := range 200 {
		session := psid(i)
		if err := j.Watch(ctx, r.viewer(session), r.hash); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- j.Stop(ctx, r.user, session, "", "") }()
		if _, err := j.ensure(ctx, r.spec, r.hash, session, 0, 0); err != nil && !errors.Is(err, ErrStopped) {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')"); n != 0 {
			t.Fatalf("round %d: the stopped viewer left %d jobs", i, n)
		}
	}
}

// A request still waiting when its viewer stops must not start a job again
// after the stop cancelled the one it waited on (invariant 38).
func TestStoppedViewerWaitingForASegmentStartsNoJob(t *testing.T) {
	r := newRig(t, nil) // No worker: the job stays queued.
	j := r.jobs(t)
	j.WaitTimeout = 10 * time.Second
	failed := make(chan error, 1)
	go func() {
		_, err := r.fetch(t, j, psid(0), 0)
		failed <- err
	}()
	waitFor(t, "a job", 5*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs") == 1 })
	if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-failed; !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Fatalf("%d jobs: the stopped viewer started another", n)
	}
}
