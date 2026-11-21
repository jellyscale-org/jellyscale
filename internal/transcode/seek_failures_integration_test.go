//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// cutFilm makes the rig's film one whose file ends at 40 % (about 36 seconds of its 90): a seek past the cut is the film's own
// failure, a segment that truly cannot be made (the video has no frame there). The film has its audio, as a film cut short by a
// failed copy has, and its audio is not what fails.
func (r *rig) cutFilm(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	whole := readFile(t, filepath.Join(film(t), "film.mkv"))
	if err := os.WriteFile(filepath.Join(dir, "film.mkv"), whole[:len(whole)*4/10], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE libraries SET media_store_uri = $1, asset_store_uri = $1", "file://"+dir); err != nil {
		t.Fatal(err)
	}
}

// cutSegment is a segment of the rig's film past its cut: 60 seconds in.
const cutSegment = 15

func (r *rig) failedJobs(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'failed'", r.hash)
}

// However many requests, viewers and stream replicas ask for a segment that cannot be made, and however long they go on
// asking, a request never starts a job while the last failure is remembered (30 seconds for the film and quality); the first
// request after that starts one, and one only. A film replaced by another file is not refused for the old file's failure.
func TestARequestStartsNoJobWhileAFailureIsRememberedHoweverManyAsk(t *testing.T) {
	r := newRig(t, nil)
	r.cutFilm(t)
	r.worker(t, nil)
	replicas := []*Jobs{r.jobs(t), r.jobs(t)}
	ask := func(n int) (answers []error) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				v := r.viewer(psid(i % 4)) // Four viewers: the play sessions of several tabs and devices.
				err := replicas[i%2].Watch(ctx, v, r.hash, n)
				if err == nil {
					_, err = bytesOf(replicas[i%2].Segment(ctx, r.spec, r.hash, v, n, n))
				}
				mu.Lock()
				answers = append(answers, err)
				mu.Unlock()
			}()
		}
		wg.Wait()
		return answers
	}
	check := func(what string, answers []error) {
		t.Helper()
		for _, err := range answers {
			var failed FailedError
			if !errors.As(err, &failed) {
				t.Fatalf("%s: a request was answered %v, want the failure", what, err)
			}
		}
	}
	check("the seek", ask(cutSegment))
	if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); jobs != 1 || r.failedJobs(t) != 1 {
		t.Fatalf("20 requests for one segment made %d jobs (%d failed), want the one that failed: %+v", jobs, r.failedJobs(t), r.jobsOf(t, r.hash))
	}
	// Other segments, other viewers, on and on: no job while the failure is remembered.
	for _, n := range []int{cutSegment + 1, cutSegment + 5, cutSegment, 22} {
		check(fmt.Sprintf("segment %d within the failure memory", n), ask(n))
	}
	if got := r.failedJobs(t); got != 1 {
		t.Errorf("%d failed jobs, want the one, no job having been started for any later request: %+v", got, r.jobsOf(t, r.hash))
	}
	// The memory is 30 seconds (failureMemory): the failure is aged past it, and the next burst starts one job.
	if _, err := r.pool.Exec(context.Background(), "UPDATE transcode_jobs SET ended_at = now() - make_interval(secs => $1) WHERE state = 'failed'", failureMemory.Seconds()+1); err != nil {
		t.Fatal(err)
	}
	before := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND start_segment = $2", r.hash, cutSegment)
	check("the first requests after the memory", ask(cutSegment))
	if started := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND start_segment = $2", r.hash, cutSegment) - before; started != 1 {
		t.Errorf("20 requests after the failure was forgotten started %d jobs at segment %d, want one: %+v", started, cutSegment, r.jobsOf(t, r.hash))
	}
	// A film replaced by another file (its etag is the spec's) is not refused for the old file's failure.
	if _, err := r.pool.Exec(context.Background(), "UPDATE libraries SET media_store_uri = $1, asset_store_uri = $1", "file://"+film(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET object_etag = 'etag2'"); err != nil {
		t.Fatal(err)
	}
	r.spec.ETag = "etag2"
	r.hash = r.spec.Hash()
	if _, err := r.fetchAt(t, replicas[0], psid(0), cutSegment, cutSegment); err != nil {
		t.Errorf("the film replaced by another file is refused for the old one's failure: %v", err)
	}
	if reason, err := replicas[1].Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || reason != "" {
		t.Errorf("PlaybackInfo refuses the replaced film: %q, %v", reason, err)
	}
}

// killingFFmpeg is an ffmpeg that dies by SIGKILL when the command line holds marker, the first kills times, and is
// the real one otherwise: a kernel's OOM killer, or an operator.
func killingFFmpeg(t *testing.T, marker string, kills int) (counted func() int) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *\" %s \"*)\n  n=$(cat %s 2>/dev/null || echo 0)\n  echo $((n + 1)) > %s\n  if [ \"$n\" -lt %d ]; then kill -9 $$; fi;;\nesac\nexec %s \"$@\"\n", marker, count, count, kills, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		raw, _ := os.ReadFile(count)
		var n int
		fmt.Sscan(string(raw), &n)
		return n
	}
}

// A seek's job that ends for a cause that is not the film's is never remembered as a film that cannot be played: ffmpeg
// (the probe of the audio, or the encode) killed by a signal is made again for the waiting request, at most 3 times.
func TestASeeksJobKilledBySignalsIsMadeAgainAndNotRemembered(t *testing.T) {
	for _, c := range []struct {
		name, marker string
	}{
		{"the probe of the audio", "framecrc"},
		{"the encode", "-hls_segment_type"},
	} {
		t.Run(c.name+" is killed twice", func(t *testing.T) {
			counted := killingFFmpeg(t, c.marker, 2)
			r := newRig(t, nil)
			r.worker(t, nil)
			j := r.jobs(t)
			if _, err := r.fetchAt(t, j, psid(0), 9, 9); err != nil {
				t.Fatalf("a seek whose first two runs were killed: %v (jobs %+v)", err, r.jobsOf(t, r.hash))
			}
			if counted() < 3 {
				t.Fatalf("the shim saw %d runs of the command, so the kills did not happen", counted())
			}
			if again := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'failed' AND again", r.hash); again != 2 {
				t.Errorf("%d jobs ended failed as not the film's, want 2: %+v", again, r.jobsOf(t, r.hash))
			}
			if reason, err := j.Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || reason != "" {
				t.Errorf("the film is refused after kills that were not its own: %q, %v", reason, err)
			}
		})
		t.Run(c.name+" is killed every time", func(t *testing.T) {
			killingFFmpeg(t, c.marker, 1000)
			r := newRig(t, nil)
			r.worker(t, nil)
			_, err := r.fetchAt(t, r.jobs(t), psid(0), 9, 9)
			var failed FailedError
			if !errors.As(err, &failed) {
				t.Fatalf("a seek whose every run is killed was answered %v, want the failure", err)
			}
			// The first job and at most three made again for the request (maxRestarts).
			if jobs := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", r.hash); jobs != 1+maxRestarts {
				t.Errorf("%d jobs were made for the request, want %d: %+v", jobs, 1+maxRestarts, r.jobsOf(t, r.hash))
			}
		})
	}
}

// A job cut by a stopping worker is cancelled, not failed, and the next request restarts it: the film is not refused.
func TestASeeksJobCutByAStoppingWorkerIsCancelledAndTheNextRequestRestartsIt(t *testing.T) {
	slowAt(t, "1")
	r := newRig(t, nil)
	stop := r.worker(t, nil)
	j := r.jobs(t)
	if _, err := r.fetchAt(t, j, psid(0), 9, 9); err != nil {
		t.Fatal(err)
	}
	stop()
	rows := r.jobsOf(t, r.hash)
	if len(rows) != 1 || rows[0].State != "cancelled" {
		t.Fatalf("the job of the worker that stopped is %+v, want cancelled", rows)
	}
	r.worker(t, nil)
	if reason, err := j.Unavailable(context.Background(), r.spec, r.user, "device"); err != nil || reason != "" {
		t.Errorf("the film is refused after a worker stopped: %q, %v", reason, err)
	}
	if _, err := r.fetchAt(t, j, psid(0), 11, 9); err != nil {
		t.Fatalf("the next request after the worker stopped: %v", err)
	}
	if rows := r.jobsOf(t, r.hash); len(rows) != 2 || rows[1].State == "failed" || rows[1].Start != 11 {
		t.Errorf("the next request made %+v, want a second job at segment 11 that did not fail", rows)
	}
	_ = playback.SegmentSeconds
}
