//go:build integration

package transcode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// ffmpegStates is the kernel's state letter of each ffmpeg writing into the
// rig's scratch: T is stopped.
func (r *rig) ffmpegStates() []string {
	entries, _ := os.ReadDir("/proc")
	var states []string
	for _, entry := range entries {
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || !strings.Contains(string(raw), r.scratch) || !strings.HasPrefix(filepath.Base(strings.SplitN(string(raw), "\x00", 2)[0]), "ffmpeg") {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		after := string(stat[bytes.LastIndexByte(stat, ')')+2:])
		states = append(states, after[:1])
	}
	return states
}

func (r *rig) paused() bool {
	states := r.ffmpegStates()
	return len(states) == 1 && states[0] == "T"
}

func (r *rig) next(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT COALESCE(max(next_segment), 0) FROM transcode_jobs WHERE state = 'running'")
}

func (r *rig) stored(n int) bool {
	body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, n), 0)
	if err == nil {
		body.Close()
	}
	return err == nil
}

// keepWatching asks for nothing in particular on the spec, as a playlist
// request does, until the test ends or stop is called.
func (r *rig) keepWatching(t *testing.T, j *Jobs, psid string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			j.Watch(ctx, r.viewer(psid), r.hash, -1) //nolint:errcheck
			time.Sleep(300 * time.Millisecond)
		}
	}()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// A job makes at most the lead beyond the furthest segment a viewer asked for,
// then its ffmpeg stops, and it goes on as requests come. While viewers keep
// asking, being stopped is no stall, no idleness and no abandonment of its
// scratch directory; once they stop asking it ends as any job does.
func TestAJobPausesAtItsLeadAndResumes(t *testing.T) {
	old := scratchIdle
	scratchIdle = 2 * time.Second
	defer func() { scratchIdle = old }()
	r := newRig(t, nil)
	var logs logBuffer
	r.worker(t, func(w *Worker) {
		w.Log = logs.logger()
		w.Lead = 12 * time.Second // Segments 0 to 2.
		w.Stall = 2 * time.Second
		w.Idle = 5 * time.Second
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to stop at its lead", 20*time.Second, func() bool { return r.paused() && r.next(t) == 3 })
	stopWatching := r.keepWatching(t, j, psid(0))
	// Longer than the stall check, the idle timer and the scratch sweep allow.
	other := &Worker{Scratch: r.scratch}
	for end := time.Now().Add(7 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		other.clearScratch()
	}
	if !r.paused() || r.next(t) != 3 || r.stored(3) {
		t.Fatalf("the job did not hold at its lead: paused %v, next %d, segment 3 stored %v", r.paused(), r.next(t), r.stored(3))
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'"); n != 1 {
		t.Fatalf("%d jobs running while paused and requested, want 1", n)
	}
	// Requests move the bound: asking for segment 4 lets the job make up to 6,
	// and it is the same job and the same ffmpeg process.
	if _, err := r.fetch(t, j, psid(0), 4); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to stop at its new lead", 20*time.Second, func() bool { return r.paused() && r.next(t) == 7 })
	if r.stored(7) {
		t.Error("segment 7 is past the lead and was stored")
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Errorf("%d jobs, want the one that paused", n)
	}
	if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
		t.Fatalf("%d jobs failed:\n%s", failed, logs.String())
	}
	// Nobody asks: the idle timer ends it, as for a job that is not paused.
	stopWatching()
	waitFor(t, "the idle timer to end the paused job", 20*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	if state := mustScalar(t, r, "SELECT state FROM transcode_jobs"); state != "cancelled" {
		t.Errorf("the job is %s, want cancelled", state)
	}
}

// Another viewer's segment, in a part of the film this job does not serve,
// does not carry it on: that viewer has a job of its own.
func TestAViewerFarAheadDoesNotMoveAnotherJobsLead(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) { w.Lead = 12 * time.Second })
	first, second := r.jobs(t), r.jobs(t) // Two stream replicas.
	if _, err := r.fetch(t, first, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(t, second, psid(1), 18); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both jobs to stop at their leads", 20*time.Second, func() bool {
		return len(r.ffmpegStates()) == 2 && r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND next_segment IN (3, 21)") == 2
	})
	time.Sleep(2 * time.Second)
	if n := r.count(t, "SELECT next_segment FROM transcode_jobs WHERE start_segment = 0"); n != 3 {
		t.Errorf("the job at the start made up to segment %d for a viewer at 18, want 3", n)
	}
	// And the one viewer's request does move its own: segment 2 is within the
	// first job's reach.
	if _, err := r.fetch(t, first, psid(0), 2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first job to follow its viewer", 20*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE start_segment = 0 AND next_segment = 5") == 1
	})
}

// A job that viewers have all seeked away from gives its slot back rather than
// stay paused until they leave the film. A viewer's own seek already replaces
// the job it owns; this is the one another viewer joined, which the owner's
// seek leaves running for it.
func TestAJobLeftBehindBySeeksGivesBackItsSlot(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) {
		w.Lead = 12 * time.Second
		w.Slots = 3
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(t, j, psid(1), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to stop at its lead", 20*time.Second, func() bool { return r.paused() && r.next(t) == 4 })
	for i, n := range []int{12, 22} {
		if _, err := r.fetch(t, j, psid(i), n); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the job left behind to end", 20*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE start_segment = 0 AND state = 'running'") == 0
	})
	if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
		t.Errorf("%d jobs failed", failed)
	}
}

// A viewer that has fetched only a playlist frees the job while it is seen,
// and the bound comes back once it has gone.
func TestAPlaylistOnlyViewerFreesTheJobOnlyWhileItIsSeen(t *testing.T) {
	slow(t) // Freed, a fast encode would reach the end before the viewer goes.
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) {
		w.Lead = 12 * time.Second
		w.Idle = 5 * time.Second
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	waitFor(t, "the job to stop at its lead", 40*time.Second, func() bool { return r.paused() && r.next(t) == 3 })
	if err := j.Watch(context.Background(), r.viewer(psid(1)), r.hash, -1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the free viewer to let the job past its lead", 20*time.Second, func() bool { return r.next(t) > 3 })
	waitFor(t, "the job to pause at its lead again", 40*time.Second, r.paused)
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'"); n != 1 {
		t.Errorf("%d jobs running, want the one that paused again", n)
	}
}

// The bound counts a viewer by the segment it asked for last, so one that seeks
// back lowers it, and a viewer that has asked for none (an old replica never
// records one) frees the job: with no number to bound it by, a job paused at
// its lead could never be moved on.
func TestFurthestIsTheLastSegmentAskedFor(t *testing.T) {
	r := newRig(t, nil)
	w := &Worker{Pool: r.pool, Idle: time.Minute}
	job := claimed{Hash: r.hash, Start: 0}
	ctx := context.Background()
	furthest := func() (int, bool) {
		t.Helper()
		n, _, free, err := w.furthest(ctx, job, 10)
		if err != nil {
			t.Fatal(err)
		}
		return n, free
	}
	for _, c := range []struct {
		name    string
		psid    string
		segment int
		want    int
		free    bool
	}{
		{"a segment", psid(0), 8, 8, false},
		{"a seek back counts by the last segment, not the greatest", psid(0), 2, 2, false},
		{"a request that names no segment leaves it", psid(0), -1, 2, false},
		{"a viewer that has asked for none frees the job", psid(1), -1, 2, true},
	} {
		if err := r.jobs(t).Watch(ctx, r.viewer(c.psid), r.hash, c.segment); err != nil {
			t.Fatal(err)
		}
		if n, free := furthest(); n != c.want || free != c.free {
			t.Errorf("%s: furthest %d and free %v, want %d and %v", c.name, n, free, c.want, c.free)
		}
	}
}

// A viewer served by a replica that records no segment leaves its job to run on
// past the lead, where it would otherwise stop and wait for a number that never
// comes, and the viewer for a segment that never arrives.
func TestAJobIsNotStoppedForAViewerThatRecordsNoSegment(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, func(w *Worker) { w.Lead = 12 * time.Second })
	j := r.jobs(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Segment waits Watch with no segment, as an old replica's every request does.
	if _, err := bytesOf(j.Segment(ctx, r.spec, r.hash, r.viewer(psid(0)), 0, 0)); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	waitFor(t, "the job to go past its lead", 20*time.Second, func() bool { return r.next(t) > 3 })
	if r.paused() {
		t.Error("the job stopped at a lead no viewer's request could move")
	}
}

// A copy that reads from S3 is stopped, with its S3 connections held open and
// unread, for longer than any request lasts, and goes on: nothing fails, and
// every segment is what a run from the start made.
func TestAPausedCopyFromS3ResumesAndMakesTheSameSegments(t *testing.T) {
	copyReadRate = "5"
	defer func() { copyReadRate = "0" }()
	media, segments := s3Segments(t), s3Segments(t)
	for name := range segments {
		t.Run(name, func(t *testing.T) {
			whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if err := media[name].Put(context.Background(), "film.mkv", bytes.NewReader(whole)); err != nil {
				t.Fatal(err)
			}
			r := newRig(t, segments[name])
			r.copied(t, "film.mkv") // 9 segments of 10 s.
			fromS3 := func(string) (store.Store, error) { return media[name], nil }
			read := func(lead time.Duration, ask func(t *testing.T, j *Jobs)) [][]byte {
				stop := r.worker(t, func(w *Worker) {
					w.Media = fromS3
					w.Lead = lead
					w.Stall = 10 * time.Second
					w.Idle = 10 * time.Second
				})
				defer stop()
				j := r.jobs(t)
				ask(t, j)
				var all [][]byte
				for n := -1; n < 9; n++ {
					body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, n), 0)
					if err != nil {
						t.Fatalf("segment %d: %v", n, err)
					}
					data := new(bytes.Buffer)
					data.ReadFrom(body) //nolint:errcheck
					body.Close()
					all = append(all, data.Bytes())
				}
				return all
			}
			paused := read(12*time.Second, func(t *testing.T, j *Jobs) {
				if _, err := r.fetch(t, j, psid(0), 0); err != nil {
					t.Fatal(err)
				}
				waitFor(t, "the copy to stop at its lead", 30*time.Second, func() bool { return r.paused() && r.next(t) == 2 })
				r.keepWatching(t, j, psid(0))
				time.Sleep(20 * time.Second)
				if !r.paused() || r.stored(2) || r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running'") != 1 {
					t.Fatal("the copy did not hold at its lead")
				}
				for n := 1; n < 9; n++ { // As a player does, one after another.
					if _, err := r.fetch(t, j, psid(0), n); err != nil {
						t.Fatalf("segment %d after the pause: %v", n, err)
					}
				}
			})
			if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
				t.Fatalf("%d jobs failed", failed)
			}
			if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
				t.Fatalf("%d jobs, want the one that paused", n)
			}
			// A run from the start, with no bound, in a fresh store.
			waitFor(t, "the paused job to end", 30*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
			for n := -1; n < 9; n++ {
				r.segments.Delete(context.Background(), SegmentKey(r.hash, n)) //nolint:errcheck
			}
			whole2 := read(time.Hour, func(t *testing.T, j *Jobs) {
				for n := range 9 {
					if _, err := r.fetch(t, j, psid(1), n); err != nil {
						t.Fatal(err)
					}
				}
			})
			for n := range paused {
				if !bytes.Equal(paused[n], whole2[n]) {
					t.Errorf("segment %d differs between a paused run and a run from the start", n-1)
				}
			}
		})
	}
}
