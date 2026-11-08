//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// Parallel transcoding (JS-024): while a job runs below about 1.2x real time,
// helper jobs on other workers encode later chunks of the same spec. A helper
// is an ordinary row in transcode_jobs, claimed like any other, so these tests
// name no column the task does not: they read the spec's rows (worker,
// start_segment, next_segment, state) and the segments in the store.
//
// They use two names the task implies and the build must add, so they do not
// compile until it has: Worker.ChunkSegments (segments in a helper's chunk)
// and Worker.HelpBelow (the encode speed, in film seconds per second, under
// which a job gets helpers; 1.2 when zero). Worker processes read both, and the
// slot count, lead and idle time, from the environment (helperSettings).

// filmSegments is the 90 second film in 4 second segments; its 2 second tail
// is a segment of its own.
const filmSegments = 23

// helperSettings is what a worker process of these tests takes from its
// environment, on top of what workerProcess sets.
func helperSettings(w *Worker) {
	number := func(name string) (int, bool) {
		n, err := strconv.Atoi(os.Getenv(name))
		return n, err == nil
	}
	if n, ok := number("JELLYSCALE_TEST_SLOTS"); ok {
		w.Slots = n
	}
	if n, ok := number("JELLYSCALE_TEST_CHUNK"); ok {
		w.ChunkSegments = n
	}
	if n, ok := number("JELLYSCALE_TEST_LEAD_SECONDS"); ok {
		w.Lead = time.Duration(n) * time.Second
	}
	if n, ok := number("JELLYSCALE_TEST_IDLE_SECONDS"); ok {
		w.Idle = time.Duration(n) * time.Second
	}
	if v, err := strconv.ParseFloat(os.Getenv("JELLYSCALE_TEST_HELP_BELOW"), 64); err == nil {
		w.HelpBelow = v
	}
}

// helperFleet is worker processes started by a test, by id.
type helperFleet struct {
	cmds map[string]*exec.Cmd
	ids  []string
}

// ownStore gives the rig a segment store in a directory the worker processes
// can be told about.
func (r *rig) ownStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	r.segments = store.NewPosix(dir)
	return dir
}

// workers starts n worker processes with slots each, chunks of chunk segments,
// writing into dir, and waits until every one holds its lock.
func (r *rig) workers(t *testing.T, n, slots, chunk int, dir string, env ...string) *helperFleet {
	t.Helper()
	f := &helperFleet{cmds: map[string]*exec.Cmd{}}
	all := append([]string{"JELLYSCALE_TEST_SLOTS=" + strconv.Itoa(slots), "JELLYSCALE_TEST_CHUNK=" + strconv.Itoa(chunk),
		"JELLYSCALE_TEST_HEARTBEAT=200ms", "JELLYSCALE_TEST_LEASE=3s"}, env...)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("helper-w%d", i)
		f.cmds[id] = r.workerProcess(t, id, dir, all...)
		f.ids = append(f.ids, id)
	}
	return f
}

// kill is SIGKILL, as a crashed node.
func (f *helperFleet) kill(id string) { f.cmds[id].Process.Signal(syscall.SIGKILL) } //nolint:errcheck

// stop ends every worker gracefully and waits until every process is gone, so
// a later part of the test is not served by a worker of an earlier one.
func (f *helperFleet) stop(t *testing.T) {
	t.Helper()
	for _, cmd := range f.cmds {
		cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	}
	waitFor(t, "the workers to exit", 20*time.Second, func() bool {
		for _, cmd := range f.cmds {
			if syscall.Kill(cmd.Process.Pid, 0) == nil {
				return false
			}
		}
		return true
	})
}

// job is a row of the spec's jobs.
type job struct {
	ID          int64
	Worker      string
	Start, Next int
	State       string
}

func (r *rig) jobsOf(t *testing.T, hash string) []job {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT id, COALESCE(worker, ''), start_segment, next_segment, state
		FROM transcode_jobs WHERE spec_hash = $1 ORDER BY id`, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.Worker, &j.Start, &j.Next, &j.State); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	return jobs
}

func running(jobs []job) (live []job) {
	for _, j := range jobs {
		if j.State == "running" {
			live = append(live, j)
		}
	}
	return live
}

// helperMaking is a running job of the spec that does not start at segment 0
// and has made something: a helper at work.
func (r *rig) helperMaking(t *testing.T) (job, bool) {
	t.Helper()
	for _, j := range running(r.jobsOf(t, r.hash)) {
		if j.Start > 0 && j.Next > j.Start {
			return j, true
		}
	}
	return job{}, false
}

// activeJobs counts every job that holds or waits for a slot, of any spec.
func (r *rig) activeJobs(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')")
}

// play is a viewer that asks for segments from..to-1 in turn, the next when the
// last has come, as a player at the end of its buffer does. It returns how long
// the slowest took and how long all took.
func (r *rig) play(ctx context.Context, j *Jobs, psid string, from, to int) (slowest, total time.Duration, err error) {
	began := time.Now()
	for n := from; n < to; n++ {
		asked := time.Now()
		if err := j.Watch(ctx, r.viewer(psid), r.hash, n); err != nil {
			return slowest, time.Since(began), err
		}
		body, err := j.Segment(ctx, r.spec, r.hash, r.viewer(psid), n, 0)
		if err != nil {
			return slowest, time.Since(began), fmt.Errorf("segment %d after %v: %w", n, time.Since(asked).Round(time.Millisecond), err)
		}
		if !isMP4Segment(body) {
			return slowest, time.Since(began), fmt.Errorf("segment %d is not a segment", n)
		}
		slowest = max(slowest, time.Since(asked))
	}
	return slowest, time.Since(began), nil
}

// playThrough is play that fails the test on an error.
func (r *rig) playThrough(t *testing.T, j *Jobs, psid string, from, to int) (slowest, total time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	slowest, total, err := r.play(ctx, j, psid, from, to)
	if err != nil {
		t.Fatal(err)
	}
	return slowest, total
}

// playInBackground is a viewer playing the film from the start until the test
// ends. Its errors are ignored: the test looks at the jobs.
func (r *rig) playInBackground(t *testing.T, j *Jobs, psid string) (slowest chan time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	slowest = make(chan time.Duration, 1)
	go func() {
		defer close(done)
		worst, _, err := r.play(ctx, j, psid, 0, filmSegments)
		if err == nil {
			slowest <- worst
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return slowest
}

// video is the times of the segment's video frames, read from the init
// segment and the segment together, and it fails the test if they do not
// decode cleanly. A re-encode is not the same bytes twice (x264's threads and
// rate control), so what must match a single worker's run is where every frame
// falls: chunks from different runs then meet as a restart's segments do, to
// within the 1 ms a restart's times move (TestAKilledWorkerIsReplacedWithinSeconds).
func video(t *testing.T, s store.Store, hash string, n int) []float64 {
	t.Helper()
	read := func(key string) []byte {
		body, err := readSegment(context.Background(), s, key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		return body
	}
	file := filepath.Join(t.TempDir(), "s.mp4")
	if err := os.WriteFile(file, append(read(SegmentKey(hash, -1)), read(SegmentKey(hash, n))...), 0o644); err != nil {
		t.Fatal(err)
	}
	if warnings := strings.TrimSpace(run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", file, "-map", "0:v:0", "-f", "null", "-")); warnings != "" {
		t.Errorf("segment %d does not decode cleanly on its own:\n%s", n, warnings)
	}
	return times(t, file, "v:0", "frame=pts_time")
}

var (
	baselineOnce  sync.Once
	baselineVideo [][]float64
)

// baseline is the video of each segment of the film made by one worker with
// one slot at full speed: no helper can start, so it is an unassisted run. It
// is made once. Call it before slow(t), which would make it 90 seconds long.
func baseline(t *testing.T) [][]float64 {
	t.Helper()
	baselineOnce.Do(func() {
		r := newRig(t, nil)
		workerSet := r.workers(t, 1, 1, 6, r.ownStore(t))
		defer workerSet.stop(t)
		j := r.jobs(t)
		r.playThrough(t, j, psid(90), 0, filmSegments)
		if jobs := r.jobsOf(t, r.hash); len(jobs) != 1 {
			t.Errorf("the baseline run took %d jobs, want the one: %+v", len(jobs), jobs)
		}
		for n := 0; n < filmSegments; n++ {
			baselineVideo = append(baselineVideo, video(t, r.segments, r.hash, n))
		}
	})
	if len(baselineVideo) != filmSegments {
		t.Fatal("the baseline run failed in an earlier test")
	}
	return baselineVideo
}

// A slow job gets helpers on other workers, each encoding a different range,
// and the stream assembles from the store: three worker processes, a film that
// takes 90 seconds on one, and every segment's video equal to one worker's.
func TestThreeWorkersEncodeOneFilmInChunksAndItsVideoEqualsOneWorkers(t *testing.T) {
	want := baseline(t)
	slow(t) // A worker reads at the speed of the film: below 1.2x, so helpers start.
	r := newRig(t, nil)
	r.workers(t, 3, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	_, total := r.playThrough(t, j, psid(0), 0, filmSegments)
	t.Logf("%d segments of a 90 second film in %v with 3 workers, each reading at the film's speed", filmSegments, total.Round(100*time.Millisecond))

	jobs := r.jobsOf(t, r.hash)
	t.Run("each worker encoded a part", func(t *testing.T) {
		workers := map[string]bool{}
		for _, j := range jobs {
			if j.Next > j.Start {
				workers[j.Worker] = true
			}
		}
		if len(workers) != 3 {
			t.Errorf("%d workers made segments, want 3: %+v", len(workers), jobs)
		}
	})
	t.Run("the chunks do not overlap, so no segment was encoded twice", func(t *testing.T) {
		made := make([]int, filmSegments)
		for _, j := range jobs {
			for n := j.Start; n < min(j.Next, filmSegments); n++ {
				made[n]++
			}
		}
		for n, times := range made {
			if times != 1 {
				t.Errorf("segment %d was made by %d jobs: %+v", n, times, jobs)
			}
		}
	})
	t.Run("it is faster than one worker, which takes the length of the film", func(t *testing.T) {
		if total > 63*time.Second {
			t.Errorf("took %v for a film that one worker reading at its speed makes in 90 s", total)
		}
	})
	t.Run("every segment has one worker's frames at one worker's times", func(t *testing.T) {
		for n := 0; n < filmSegments; n++ {
			compare(t, fmt.Sprintf("segment %d's frames against a single worker's", n), want[n], video(t, r.segments, r.hash, n), 0.001)
		}
	})
	t.Run("nothing is left running", func(t *testing.T) {
		waitFor(t, "every job to end and every ffmpeg to be gone", 20*time.Second, func() bool {
			return len(running(r.jobsOf(t, r.hash))) == 0 && len(r.ffmpegProcesses()) == 0
		})
	})
}

// A helper that dies mid-chunk is replaced from the first segment of its chunk
// that is missing, and the viewer, who reaches that chunk later, never waits
// longer than a player does before it gives up on a request (10 seconds).
func TestAKilledHelperIsReplacedAndTheViewerKeepsPlaying(t *testing.T) {
	want := baseline(t)
	slow(t)
	r := newRig(t, nil)
	workerSet := r.workers(t, 3, 2, 6, r.ownStore(t))
	j := r.jobs(t)

	played := r.playInBackground(t, j, psid(0))

	var victim job
	waitFor(t, "a helper to be encoding a later chunk", 40*time.Second, func() bool {
		var found bool
		victim, found = r.helperMaking(t)
		return found
	})
	workerSet.kill(victim.Worker)

	select {
	case slowest := <-played:
		if slowest > 9*time.Second {
			t.Errorf("the viewer waited %v for one segment; a player gives up after 10 s", slowest)
		}
	case <-time.After(150 * time.Second):
		t.Fatal("the viewer never got the whole film, or a segment failed")
	}
	replaced := false
	for _, j := range r.jobsOf(t, r.hash) {
		if j.ID > victim.ID && j.Worker != victim.Worker && j.Start >= victim.Start && j.Start < victim.Start+6 && j.Next > j.Start {
			replaced = true
		}
	}
	if !replaced {
		t.Errorf("no job on another worker made segments from the dead helper's chunk (it started at %d): %+v", victim.Start, r.jobsOf(t, r.hash))
	}
	for n := 0; n < filmSegments; n++ {
		compare(t, fmt.Sprintf("segment %d's frames against a single worker's, after the helper was killed", n), want[n], video(t, r.segments, r.hash, n), 0.001)
	}
}

// A helper takes a slot only if another stays free for a viewer who has not
// asked yet.
func TestHelpersNeverTakeTheLastFreeSlot(t *testing.T) {
	slow(t)
	t.Run("three slots in all: a main job and one helper, and the third stays free for a new viewer", func(t *testing.T) {
		r := newRig(t, nil)
		r.workers(t, 3, 1, 4, r.ownStore(t))
		j := r.jobs(t)
		r.playInBackground(t, j, psid(0))
		waitFor(t, "a helper to run beside the main job", 40*time.Second, func() bool { return r.activeJobs(t) >= 2 })
		// The three workers decide at the same moment, in separate processes; a
		// race that gave two helpers shows as three jobs.
		for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if n := r.activeJobs(t); n > 2 {
				t.Fatalf("%d jobs queued or running on 3 slots, so a helper took the last free one: %+v", n, r.jobsOf(t, r.hash))
			}
		}
		// Another stream of the same file, started now, gets that slot.
		other := r.spec
		other.VideoBitrate = 900_000
		hash := other.Hash()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		viewer := r.viewer(psid(1))
		if err := j.Watch(ctx, viewer, hash, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Segment(ctx, other, hash, viewer, 0, 0); err != nil {
			t.Fatalf("the new viewer was refused (%v) though a slot stayed free for it", err)
		}
	})
	t.Run("two slots in all: the main job takes one and no helper starts", func(t *testing.T) {
		r := newRig(t, nil)
		r.workers(t, 2, 1, 4, r.ownStore(t))
		j := r.jobs(t)
		r.playInBackground(t, j, psid(0))
		waitFor(t, "the main job to run", 30*time.Second, func() bool { return len(running(r.jobsOf(t, r.hash))) == 1 })
		for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if jobs := r.jobsOf(t, r.hash); len(jobs) != 1 {
				t.Fatalf("a helper was started with only the last free slot to run on: %+v", jobs)
			}
		}
	})
}

// Every kill path ends the helpers with the main job (invariant 25).
func TestStoppedEndsEveryHelper(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 4, r.ownStore(t))
	j := r.jobs(t)
	r.playInBackground(t, j, psid(0))
	waitFor(t, "two jobs to run", 40*time.Second, func() bool { return len(running(r.jobsOf(t, r.hash))) >= 2 })
	if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "every job to be cancelled and every ffmpeg gone", 8*time.Second, func() bool {
		return len(running(r.jobsOf(t, r.hash))) == 0 && len(r.ffmpegProcesses()) == 0
	})
	// A helper claimed a moment ago must not start work for a stopped viewer.
	time.Sleep(3 * time.Second)
	if live := r.activeJobs(t); live != 0 || len(r.ffmpegProcesses()) != 0 {
		t.Fatalf("%d jobs and %d ffmpeg after Stopped", live, len(r.ffmpegProcesses()))
	}
}

func TestHelpersEndWhenNoRequestComesInTheIdleTime(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 4, r.ownStore(t), "JELLYSCALE_TEST_IDLE_SECONDS=4")
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil { // And then the tab is closed.
		t.Fatal(err)
	}
	waitFor(t, "a helper to run", 40*time.Second, func() bool { return len(running(r.jobsOf(t, r.hash))) >= 2 })
	waitFor(t, "every job to end and every ffmpeg to be gone", 15*time.Second, func() bool {
		return len(running(r.jobsOf(t, r.hash))) == 0 && len(r.ffmpegProcesses()) == 0
	})
}

// Two viewers share a spec and its jobs (invariant 38): stopping one leaves
// the other's, helpers included.
func TestStoppingOneOfTwoViewersLeavesTheHelpers(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 4, r.ownStore(t))
	j := r.jobs(t)
	r.playInBackground(t, j, psid(0))
	r.keepWatching(t, j, psid(1))
	waitFor(t, "a helper to run", 40*time.Second, func() bool { _, found := r.helperMaking(t); return found })
	if err := j.Stop(context.Background(), r.user, psid(1), "", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if len(running(r.jobsOf(t, r.hash))) < 2 {
		t.Fatalf("stopping the viewer who only watched ended the jobs the other one needs: %+v", r.jobsOf(t, r.hash))
	}
}

// A helper's chunk is inside the read-ahead (the lead, invariant 57): nothing
// is stored past the lead beyond the furthest segment asked for, and moving
// the request moves the limit.
func TestHelpersStayWithinTheReadAhead(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// 24 seconds is 6 segments: segments 0 to 5 may be made after asking for 0.
	r.workers(t, 3, 2, 2, r.ownStore(t), "JELLYSCALE_TEST_LEAD_SECONDS=24")
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	waitFor(t, "a helper to encode inside the lead", 40*time.Second, func() bool { _, found := r.helperMaking(t); return found })
	time.Sleep(12 * time.Second) // A job at 1x makes 3 more segments in that time.
	for n := 6; n < filmSegments; n++ {
		if r.stored(n) {
			t.Fatalf("segment %d is stored with the viewer at segment 0 and a lead of 6 segments: %+v", n, r.jobsOf(t, r.hash))
		}
	}
	if _, err := r.fetch(t, j, psid(0), 5); err != nil {
		t.Fatal(err)
	}
	time.Sleep(12 * time.Second)
	for n := 11; n < filmSegments; n++ {
		if r.stored(n) {
			t.Fatalf("segment %d is stored with the viewer at segment 5 and a lead of 6 segments: %+v", n, r.jobsOf(t, r.hash))
		}
	}
}

// A seek into a helper's chunk is served by the helper (invariant 37): no job
// starts for a segment a running one is about to make.
func TestASeekIntoAHelpersChunkJoinsTheHelper(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.workers(t, 3, 2, 6, r.ownStore(t))
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	var helper job
	waitFor(t, "a helper to make its first segment", 40*time.Second, func() bool {
		var found bool
		helper, found = r.helperMaking(t)
		return found
	})
	before := len(r.jobsOf(t, r.hash))
	asked := time.Now()
	if _, err := r.fetch(t, j, psid(0), helper.Next+1); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(asked); took > 9*time.Second {
		t.Errorf("a seek into the helper's chunk took %v", took)
	}
	for _, started := range r.jobsOf(t, r.hash)[before:] {
		if started.Start >= helper.Start && started.Start <= helper.Next+1 {
			t.Errorf("a seek inside a running helper's chunk started a job at %d: %+v", started.Start, r.jobsOf(t, r.hash))
		}
	}
}

// The measured speedup that goes in the PR: the same film, one CPU for each
// worker's ffmpeg (taskset), one, two and three worker processes, and helpers
// wanted whatever the speed (HelpBelow is high), because on this machine a CPU
// alone is faster than real time. It shows that more workers finish the film
// sooner, and logs the numbers. Each worker gets a CPU on its own physical
// core: two hyperthreads of one core are not two CPUs' worth of encoding (a
// 4-vCPU CI runner has two cores).
func TestMoreWorkersFinishTheFilmSooner(t *testing.T) {
	real := testenv.FFmpeg(t)
	film(t)
	if _, err := exec.LookPath("taskset"); err != nil {
		t.Fatal("this test pins each worker's ffmpeg to a CPU with taskset")
	}
	cpus := oneCPUPerCore(t)
	if len(cpus) < 2 {
		t.Fatalf("this test needs at least two physical cores, found CPUs %v", cpus)
	}
	t.Logf("one CPU per physical core: %v", cpus)
	timings := map[int]time.Duration{}
	for _, n := range []int{1, 2, 3} {
		r := newRig(t, nil)
		dir := r.ownStore(t)
		set := &helperFleet{cmds: map[string]*exec.Cmd{}}
		for i := 0; i < n; i++ {
			bin := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\nexec taskset -c %d %s \"$@\"\n", cpus[i%len(cpus)], real)
			if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			id := fmt.Sprintf("speed-w%d", i)
			set.cmds[id] = r.workerProcess(t, id, dir, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"JELLYSCALE_TEST_SLOTS=2", "JELLYSCALE_TEST_CHUNK=4", "JELLYSCALE_TEST_HELP_BELOW=1000",
				"JELLYSCALE_TEST_HEARTBEAT=200ms", "JELLYSCALE_TEST_LEASE=3s")
		}
		j := r.jobs(t)
		_, timings[n] = r.playThrough(t, j, psid(0), 0, filmSegments)
		t.Logf("%d worker(s), each ffmpeg on 1 CPU: %v for %d segments", n, timings[n].Round(10*time.Millisecond), filmSegments)
		set.stop(t)
	}
	t.Logf("speedup over one worker: 2 workers %.2fx, 3 workers %.2fx", float64(timings[1])/float64(timings[2]), float64(timings[1])/float64(timings[3]))
	// With cores to spare for Postgres, the workers and this test, two workers
	// take at most 80% of one's time and three at most 60%. A CI runner has two
	// cores in all, which the encodes share with everything else: there two and
	// three workers measure 1.2x to 1.35x, so the bar is a 10% gain.
	most := map[int]time.Duration{2: timings[1] * 80 / 100, 3: timings[1] * 60 / 100}
	if len(cpus) < 4 {
		most = map[int]time.Duration{2: timings[1] * 90 / 100, 3: timings[1] * 90 / 100}
	}
	for _, n := range []int{2, 3} {
		if timings[n] > most[n] {
			t.Errorf("%d workers on %d cores took %v, one took %v: no real gain (at most %v)", n, len(cpus), timings[n], timings[1], most[n])
		}
	}
}

// oneCPUPerCore lists one logical CPU for each physical core this process may
// run on, by the kernel's topology.
func oneCPUPerCore(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("taskset", "-pc", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	// "pid 123's current affinity list: 0-3,8"
	list := strings.TrimSpace(string(out[strings.LastIndex(string(out), ":")+1:]))
	seen := map[string]bool{}
	var cpus []int
	for _, part := range strings.Split(list, ",") {
		lo, hi, found := strings.Cut(part, "-")
		if !found {
			hi = lo
		}
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil {
			t.Fatalf("taskset affinity list %q", list)
		}
		for cpu := a; cpu <= b; cpu++ {
			dir := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/topology/", cpu)
			pkg, err1 := os.ReadFile(dir + "physical_package_id")
			core, err2 := os.ReadFile(dir + "core_id")
			if err1 != nil || err2 != nil {
				t.Fatalf("no topology for CPU %d: %v %v", cpu, err1, err2)
			}
			key := strings.TrimSpace(string(pkg)) + "/" + strings.TrimSpace(string(core))
			if !seen[key] {
				seen[key] = true
				cpus = append(cpus, cpu)
			}
		}
	}
	return cpus
}

// A job nobody helps runs one ffmpeg from where it began to the end of the
// film. A new ffmpeg at each chunk start would cost a fast viewer a restart
// every 16 seconds, and a burnt PGS cue that runs across a chunk start would
// vanish there (invariant 51).
func TestAJobNobodyHelpsIsOneFFmpeg(t *testing.T) {
	film(t)
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "encodes")
	// Only an encode passes -progress; probes and checks do not.
	script := "#!/bin/sh\ncase \" $* \" in *\" -progress \"*) echo run >> " + log + ";; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newRig(t, nil)
	r.workers(t, 1, 2, 4, r.ownStore(t), "JELLYSCALE_TEST_HELP_BELOW=0.001")
	j := r.jobs(t)
	r.playThrough(t, j, psid(0), 0, filmSegments)
	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(body), "run"); runs != 1 {
		t.Errorf("%d ffmpeg encodes made the %d segments of one unhelped job, want 1", runs, filmSegments)
	}
}

// A helper's failure is not the viewer's: a worker that could not make a later
// chunk leaves the viewer's own job to make it, where a failure of the viewer's
// job is remembered and refused for a while (the film is broken).
func TestAFailedHelperDoesNotFailTheViewer(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, nil)
	raw, err := json.Marshal(r.spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, state, error, ended_at)
		VALUES ($1, $2, 'helper', 4, 4, true, 'failed', 'the helper could not read the film', now())`, r.hash, raw); err != nil {
		t.Fatal(err)
	}
	j := r.jobs(t)
	if reason, err := j.Unavailable(context.Background(), r.hash, r.user, "device"); err != nil || reason != "" {
		t.Errorf("with a failed helper, playback is unavailable: %q, %v", reason, err)
	}
	if _, err := r.fetch(t, j, psid(0), 4); err != nil {
		t.Errorf("the segment of a failed helper's chunk: %v", err)
	}
}

// A burnt image subtitle gets no helpers, however slow the job: a run that
// begins mid-film does not draw a cue whose packet came before it (invariant
// 51), so a helper's chunk would lose the cue showing at its start.
func TestABurntImageSubtitleGetsNoHelpers(t *testing.T) {
	dir := t.TempDir()
	imageFilm(t, dir, "film.mkv", "copy")
	slow(t)
	r := newRig(t, nil)
	streams, err := json.Marshal([]playback.Stream{{Index: 2, Type: "Subtitle", IsImageSubtitleStream: true, Width: 1920, Height: 1080}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for sql, arg := range map[string]any{
		"UPDATE libraries SET media_store_uri = $1": "file://" + dir,
		"UPDATE items SET run_time_ticks = $1":      400_000_000, // The film is 40 seconds.
		"UPDATE media_sources SET streams = $1":     streams,
	} {
		if _, err := r.pool.Exec(ctx, sql, arg); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	r.workers(t, 2, 2, 2, r.ownStore(t))
	j := r.jobs(t)
	r.playThrough(t, j, psid(0), 0, 10)
	if helpers := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper"); helpers != 0 {
		t.Errorf("a burnt PGS track's job had %d helpers, want none: %+v", helpers, r.jobsOf(t, r.hash))
	}
}

// A helper that always fails is not tried again for its chunk until the
// failure is old (failureMemory), however often the slow job beats: the
// viewer's own job makes the chunk, and the film plays through.
func TestAHelperThatAlwaysFailsIsNotRetriedOnEveryBeat(t *testing.T) {
	film(t)
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	// Paced at the film's speed, so helpers are wanted, and any encode that
	// does not begin at the start of the film (every helper's) fails.
	script := "#!/bin/sh\nprev=\nfor a in \"$@\"; do\n  if [ \"$prev\" = -ss ] && [ \"$a\" != 0.000000 ] && case \" $* \" in *\" -progress \"*) true;; *) false;; esac; then echo boom >&2; exit 1; fi\n  prev=$a\ndone\nexec " + real + " -re \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := newRig(t, nil)
	r.workers(t, 3, 1, 4, r.ownStore(t))
	j := r.jobs(t)
	_, total := r.playThrough(t, j, psid(0), 0, filmSegments)
	failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper AND state = 'failed'")
	if failed == 0 {
		t.Fatal("no helper failed: the test's ffmpeg did not fail them")
	}
	// Each chunk may fail once per failureMemory the film took to play.
	chunks := (filmSegments + 3) / 4
	if most := chunks * (int(total/failureMemory) + 1); failed > most {
		t.Errorf("%d helpers failed in %v, more than %d (one per chunk per %v): they are retried on every beat", failed, total.Round(time.Second), most, failureMemory)
	}
}
