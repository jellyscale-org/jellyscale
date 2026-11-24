//go:build integration

package transcode

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/mediatool"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-177 (D-064, D-066, D-070): a worker learns what it holds from the jobs it runs. These checks run the real worker, its
// real ffmpeg and a real Postgres in this process, and read what an operator and a replica read: the worker's row, its
// /metrics, its log and PlaybackInfo's answer. Real processes and real contention are in
// cmd/jellyscale/capacity_learning_integration_test.go; the estimator's arithmetic is capacity_learn_test.go's (its
// header lists the estimator's names).
//
// What the build adds besides the estimator (the names these checks imply):
//
//	type Kernel interface {
//		Readable() error                    // nil when the scheduler's figures can be read; asked once, at the worker's start
//		Allotment() float64                 // the CPUs the worker may use: cpu.max (fractional) when there is one, else GOMAXPROCS(0) (Go already folds the affinity into it), and never more than GOMAXPROCS(0)
//		Demand(pids []int) (float64, error) // CPU-seconds per second those processes wanted over the last 10 s: on a CPU plus on a run queue
//	}
//	Worker.Kernel Kernel                    // nil is the kernel's own figures; a test sets its own (fakeKernel, capacity_kernel_fake_test.go,
//	                                        // which is untagged so that the unit tier injects the allotment instead of reading the host's cpu.max)
//	Worker.cores() is the START figure (the lesser of Kernel.Allotment() and the ceiling, Cores or Slots); registration writes
//	the figure the worker HOLDS: after a move, or after the reaper forgot the worker and it registers again, the new row carries
//	the learned figure and not the start (TestAWorkerThatTheReaperForgotRegistersAgainWithTheFigureItHolds)
//	the log "transcode capacity moved" (attributes before, after, source), once for a move of more than a fifth from the figure
//	last logged; "transcode capacity cannot be learned", once at start; "transcode capacity timed rise failed" (attributes
//	before, after), which from the second since a confirmed rise also names transcodeCores
//	jellyscale_transcode_capacity_source{source="assumed"|"learned"|"ceiling"}: 1 for the source in force, 0 for the others
//	jellyscale_transcode_capacity_unreadable: 1 when the worker cannot read the kernel's figures
//	jellyscale_transcode_unhelped_jobs_total{capacity="assumed"|"learned"|"ceiling",reason=...}, with the reason own_extraction
//	a mediatool.Run that is not a transcode's is what a worker counts as its own extraction (process-wide, in mediatool)
//
// Where a check lets the kernel's figures be real (a nil Kernel) the CPU a job wants is what the machine gives it, which is
// how the negatives are shown: a job that waits for its input wants little of any machine. Where a check needs a CPU that
// is starved it injects the figures (fakeKernel) and holds the job's speed with a paced ffmpeg, so that the figure learned is
// the same on a 2-CPU runner and on 16 CPUs; the real starved worker is the process-level check.
//
// Timings. The rig's heartbeat is 200 ms, so a sample can come every 200 ms and not every 3 s: a check that needs no sample
// to count in some span must keep every job in it younger than the 15 s a job waits before it counts, or give it none that
// the injected CPU would make count. The design's gaps are in units of the lease, which a check shortens with Lease
// (the timed rise's check, 2 s, so that its first gap is 60 s and not 5 minutes). No check passes or fails on how long
// something took, but for a lower bound that a slow machine cannot break: a timed rise does not come before its gap.

// i5Speed is the i5-6500's speed on the 3.2 row (JS-154, JS-177's measurement): what a job makes on a worker throttled to it.
const i5Speed = 0.88

// learnHang bounds the wait for a figure to move: 120 s, 2.7 times the design's 45 s from a job's claim to the figure's move
// (docs/transcoding.md, section 4). It is a bound on a hang, not a bar: no check asserts that a move came within the 45 s, and
// a closer bound (1.5 times, 67 s) would be a duration measured on the machine that decides (docs/testing.md), which a
// runner whose heartbeats are late would break. 2.7 times is the least that still stops a sampler on a 30 s ticker, which
// needs 210 s, from passing every convergence wait; a ticker of 10 s (70 s) it does not stop, which is why the sampler's
// cadence is a check of its own, by mechanism (TestTheSamplerTakesOneSampleEveryHeartbeat), in a multiple of the heartbeat.
const learnHang = 2 * time.Minute

// paced puts an ffmpeg on the PATH that reads each transcode's input at the rate in its file (the speed the job makes: 1 is
// the film's own), read at each run, and records every ffmpeg run's arguments. It returns the setter of the rate and the
// record of the runs.
func paced(t *testing.T, rate float64) (set func(float64), runs func() string) {
	t.Helper()
	film(t) // Made at full speed, before the shim is in the way.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	ratePath, runsPath := filepath.Join(dir, "rate"), filepath.Join(dir, "runs")
	set = func(rate float64) {
		if err := os.WriteFile(ratePath, []byte(strconv.FormatFloat(rate, 'f', -1, 64)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set(rate)
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %s\ncase \"$*\" in\n*\"-f hls\"*) exec %s -readrate \"$(cat %s)\" \"$@\";;\nesac\nexec %s \"$@\"\n", runsPath, real, ratePath, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return set, func() string {
		raw, _ := os.ReadFile(runsPath)
		return string(raw)
	}
}

// learner starts a worker that declares nothing, with the kernel given, and returns it with its log.
func (r *rig) learner(t *testing.T, kernel Kernel, change func(*Worker)) (*Worker, *logBuffer) {
	t.Helper()
	var w *Worker
	logs := &logBuffer{}
	r.worker(t, func(worker *Worker) {
		w = worker
		worker.Cores, worker.Slots = 0, 0 // The rig declares two slots, and more for a dear spec; nothing is declared here.
		worker.Kernel = kernel
		worker.Log = logs.logger()
		if change != nil {
			change(worker)
		}
	})
	return w, logs
}

func (r *rig) nodeRow(t *testing.T, id string) (cores float64, slots int, session string, found bool) {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT cores::float8, slots, session FROM transcode_nodes WHERE id = $1", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&cores, &slots, &session); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	return cores, slots, session, found
}

func (r *rig) rowCores(t *testing.T, id string) float64 {
	t.Helper()
	cores, _, _, _ := r.nodeRow(t, id)
	return cores
}

func deviceOf(n int) string { return fmt.Sprintf("device-%d", n) }

func (r *rig) viewerOf(n int) Viewer {
	return Viewer{PlaySessionID: psid(n), DeviceID: deviceOf(n), UserID: r.user}
}

// playAs starts a play of the spec as the viewer number n, on a device of its own: the job's first segment is made, and the
// viewer goes on asking. stop is the viewer leaving, which cancels the job.
func (r *rig) playAs(t *testing.T, spec playback.Spec, n int) (stop func()) {
	t.Helper()
	stop, err := r.playErr(r.jobs(t), spec, n)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return stop
}

// playErr is playAs for a goroutine: it reports an error instead of failing the test, and registers no cleanup.
func (r *rig) playErr(j *Jobs, spec playback.Spec, n int) (stop func(), err error) {
	r.spec, r.hash = spec, spec.Hash()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := j.Watch(ctx, r.viewerOf(n), spec.Hash(), 0); err != nil {
		return nil, fmt.Errorf("play %d: %v", n, err)
	}
	if _, err := bytesOf(j.Segment(ctx, spec, spec.Hash(), r.viewerOf(n), 0, 0)); err != nil {
		return nil, fmt.Errorf("the first segment of play %d: %v", n, err)
	}
	keep, leave := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for keep.Err() == nil {
			j.Watch(keep, r.viewerOf(n), spec.Hash(), -1) //nolint:errcheck
			time.Sleep(300 * time.Millisecond)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			leave()
			<-done
			j.Stop(context.Background(), r.user, psid(n), deviceOf(n), "") //nolint:errcheck
		})
	}, nil
}

// waitLogLines waits for at least n lines of the log that contain the text, and returns every one then: a build that logs a
// moment after it writes the row, or the other way, does not fail a check for the order.
func waitLogLines(t *testing.T, logs *logBuffer, containing string, n int) []string {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d log lines containing %q", n, containing), learnHang, func() bool {
		return len(logLinesWith(logs.String(), containing)) >= n
	})
	return logLinesWith(logs.String(), containing)
}

func logLinesWith(text, containing string) []string {
	var found []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, containing) {
			found = append(found, line)
		}
	}
	return found
}

// jobSpeed is the speed the worker shows on /metrics for its running job, or 0.
func jobSpeed(t *testing.T, w *Worker) float64 {
	t.Helper()
	m := speedLine.FindStringSubmatch(scrape(t, w))
	if m == nil {
		return 0
	}
	speed, _ := strconv.ParseFloat(m[1], 64)
	return speed
}

var afterAttr = regexp.MustCompile(`after=([0-9.e+-]+)`)

// watchFigure fails if the worker's row leaves want within d, and if no job was running to give it samples.
func watchFigure(t *testing.T, r *rig, w *Worker, want float64, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if got := r.rowCores(t, w.ID); got != want {
			t.Fatalf("the figure moved from %v to %v", want, got)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", w.ID); n == 0 {
		t.Fatal("no job was running to show it")
	}
	if got := value(scrape(t, w), `jellyscale_transcode_cores`); got != want {
		t.Errorf("/metrics says %v cores, want %v", got, want)
	}
}

// Criteria 6, 7, 8 and 14 in one run (each guarantee costs the same convergence of about 25 s, so they share it): a lone undeclared worker throttled to the i5-6500's speed (the 3.2 row at
// 0.88x) learns what it holds from its first 4K HDR job: its figure falls from its start of 4 to the job's price times its
// speed over 1.2, within a fifth, while the job goes on (the same row, the same running ffmpeg, no job ended); the row,
// /metrics and the log all say it, with the source; and nothing ran a benchmark to find out. Mutants: a figure that lives
// only in memory and never reaches the row; a lowering that ends the job; a move that is neither logged nor counted as learned.
func TestALoneWorkerThrottledToTheI5LearnsItsFigureAndIsRefusedWhatItCannotHold(t *testing.T) {
	_, runs := paced(t, i5Speed)
	r := newRig(t, nil)
	four := cheapOutput(t, r.costSpec(t, "hdr4k"))
	w, logs := r.learner(t, starvedKernel(), nil)
	if cores, slots, _, _ := r.nodeRow(t, w.ID); cores != 4 || slots != 4 {
		t.Fatalf("the row of an undeclared worker on 4 CPUs has %v cores and %d slots, want its start of 4 and 4", cores, slots)
	}
	text := scrape(t, w)
	if value(text, `jellyscale_transcode_cores`) != 4 || value(text, `jellyscale_transcode_capacity_source{source="assumed"}`) != 1 {
		t.Errorf("at start /metrics has cores %v and the source assumed %v, want 4 and 1", value(text, `jellyscale_transcode_cores`), value(text, `jellyscale_transcode_capacity_source{source="assumed"}`))
	}
	stop := r.playAs(t, four, 0)
	defer stop()
	job := r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'")
	waitFor(t, "the worker's figure to fall", learnHang, func() bool { return r.rowCores(t, w.ID) < 3.6 })
	speed := jobSpeed(t, w)
	if speed == 0 {
		t.Fatal("the worker shows no job speed")
	}
	cores, slots, _, _ := r.nodeRow(t, w.ID)
	holds := 3.2 * speed / 1.2
	if math.Abs(cores-holds) > holds/5 {
		t.Errorf("the row says %.3f cores and the job at %.3fx holds %.3f: want it within a fifth", cores, speed, holds)
	}
	if slots != int(math.Ceil(cores)) {
		t.Errorf("slots %d beside %.3f cores: a replica of the release before counts a worker by slots, which are the cores rounded up", slots, cores)
	}
	// Lowering the figure ends no running job.
	if still := r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'"); still != job {
		t.Errorf("the job running was %d and is %d", job, still)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('cancelled', 'failed')"); n != 0 {
		t.Errorf("%d jobs were cancelled or failed by the lowering", n)
	}
	// The operator sees what admission counts, and where it comes from.
	waitFor(t, "/metrics to show the row's figure and its source", learnHang, func() bool {
		text := scrape(t, w)
		return math.Abs(value(text, `jellyscale_transcode_cores`)-r.rowCores(t, w.ID)) < 1e-6 &&
			value(text, `jellyscale_transcode_capacity_source{source="learned"}`) == 1 && value(text, `jellyscale_transcode_capacity_source{source="assumed"}`) == 0
	})
	moved := waitLogLines(t, logs, "transcode capacity moved", 1)
	if len(moved) != 1 {
		t.Fatalf("%d lines say the capacity moved, want one, for a move of more than a fifth (4 to %.2f): %v", len(moved), cores, moved)
	}
	if after, err := strconv.ParseFloat(afterAttr.FindStringSubmatch(moved[0])[1], 64); err != nil || math.Abs(after-r.rowCores(t, w.ID)) > 1e-6 || !strings.Contains(moved[0], "before=4") {
		t.Errorf("the line does not give the figure before (4) and after (the row's %v): %s", r.rowCores(t, w.ID), moved[0])
	}
	for _, run := range logLinesWith(runs(), "") {
		if strings.Contains(run, "lavfi") || strings.Contains(run, "testsrc") || strings.Contains(run, "nullsrc") {
			t.Errorf("the worker ran a benchmark: %s", run)
		}
	}

	// Criterion 7 (and the claim of 8): once converged, a second 4K HDR play on the lone worker is refused at PlaybackInfo,
	// with the job's cost and the learned capacity in the words and no job queued, for every ask; a play the worker holds
	// (the 0.8 row) is admitted; and the worker claims no 3.2 job from the queue, because every claim counts the figure the
	// worker learned (a claim that counted the start of 4 takes it).
	learned := r.rowCores(t, w.ID)
	other := r.costSpec(t, "hdr4k")
	for i := range 5 {
		reason, text := r.refusal(t, other, deviceOf(0))
		if reason != playback.NoWorkerWithFreeCores {
			t.Fatalf("ask %d: reason %q (%s), want %s", i, reason, text, playback.NoWorkerWithFreeCores)
		}
		if !strings.Contains(text, "3.2") || !strings.Contains(text, strconv.FormatFloat(learned, 'g', -1, 64)[:3]) {
			t.Errorf("ask %d: the refusal %q gives the job's cost (3.2) and the capacity the worker learned (%.3f)", i, text, learned)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1", other.Hash()); n != 0 {
		t.Errorf("%d jobs were queued for the refused play", n)
	}
	if reason, text := r.refusal(t, r.costSpec(t, "sdr1080"), deviceOf(0)); reason != "" {
		t.Errorf("the 0.8 core play that the worker holds is refused (%s): %s", reason, text)
	}
	// A 3.2 job that stands in the queue, for a viewer of another device, stays unclaimed on a worker that holds 2.3.
	stop()
	queued := r.addJob(t, other, "queued", "", false, 0)
	r.watchedBy(t, other, "another device")
	time.Sleep(3 * time.Second) // A queued job is dead after 6 s; the worker looks every second and at each hint.
	if got := r.state(t, queued); got == "running" {
		t.Errorf("the worker claimed a 3.2 core job at a figure of %.2f", r.rowCores(t, w.ID))
	}
	// The queued job above is the test's own artefact: a play admitted before convergence whose job was never claimed. At the
	// 0.65 share a learned 2.3 can host a shared play, so a queued 3.2 job still counts on the lone worker until it is dead,
	// which it is after the queue timeout; a 0.8 play asked for inside that window would be refused for its cores. So wait,
	// with a hang bound and no sleep, until the job has ended; the criterion's meaning (a play the worker holds is admitted)
	// is the check above and the next play's.
	waitFor(t, "the queued job to end", learnHang, func() bool { return r.state(t, queued) != "queued" })
	// Criterion 14: the worker's capacity is learned now, and a slow job of its own that gets no helper is counted under that
	// source (the line and the counter name it).
	stopSlow := r.playAs(t, r.costSpec(t, "sdr1080"), 3)
	waitFor(t, "the slow job to be counted under the learned source", learnHang, func() bool {
		return value(scrape(t, w), `jellyscale_transcode_unhelped_jobs_total{capacity="learned",reason="lone_worker"}`) >= 1
	})
	stopSlow()

	// The design's "a re-registration writes the figure the worker holds": the reaper forgets the worker (its row is deleted)
	// and it registers again. The new row, with a session of its own, carries the learned figure and not the start of 4, and
	// /metrics agrees with it, with the source learned. Mutant: registration that writes the start figure (Worker.cores()) and
	// leaves the estimator holding the learned one, so that nothing corrects the row.
	waitFor(t, "the jobs to end", learnHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
	learned = r.rowCores(t, w.ID)
	_, _, old, _ := r.nodeRow(t, w.ID)
	r.exec(t, "DELETE FROM transcode_nodes WHERE id = $1", w.ID) // What the reaper does to a worker it forgets.
	waitFor(t, "the worker to register again", learnHang, func() bool {
		_, _, session, found := r.nodeRow(t, w.ID)
		return found && session != old
	})
	if cores, slots, _, _ := r.nodeRow(t, w.ID); math.Abs(cores-learned) > 1e-6 || slots != int(math.Ceil(learned)) {
		t.Errorf("the worker registered again with %v cores and %d slots, want the %v it learned (and its slots)", cores, slots, learned)
	}
	text = scrape(t, w)
	if got := value(text, `jellyscale_transcode_cores`); math.Abs(got-r.rowCores(t, w.ID)) > 1e-6 || value(text, `jellyscale_transcode_capacity_source{source="learned"}`) != 1 {
		t.Errorf("/metrics says %v cores, learned %v; the row says %v", got, value(text, `jellyscale_transcode_capacity_source{source="learned"}`), r.rowCores(t, w.ID))
	}
}

// Criterion 5: a job under 1.2x whose cause is not the CPU leaves the figure alone. A source that is read slowly: the worker
// reads the kernel's own figures (nothing injected), and a job that waits for its input wants little of any machine. POSIX
// here, with ffmpeg's input paced as a slow disk or mount does; S3 is the next check. A speed-only estimator lowers it
// within a minute.
func TestAJobSlowedByItsSourceReadLeavesTheFigureAlone(t *testing.T) {
	paced(t, 0.5)
	r := newRig(t, nil)
	w, logs := r.learner(t, nil, nil)
	start := r.rowCores(t, w.ID)
	stop := r.playAs(t, r.costSpec(t, "sdr1080"), 0)
	defer stop()
	watchFigure(t, r, w, start, 18*time.Second)
	if got := logLinesWith(logs.String(), "transcode capacity moved"); len(got) != 0 {
		t.Errorf("the capacity moved for a job that waited: %v", got)
	}
}

// slowReads is a store whose reads come at a set rate, so that the loopback input behind it starves ffmpeg of input and the
// CPU it wants stays low: an S3 source that is slow.
type slowReads struct {
	store.Store
	bytesPerSecond float64
}

type throttledBody struct {
	io.ReadCloser
	bytesPerSecond float64
}

func (b throttledBody) Read(p []byte) (int, error) {
	if len(p) > 8192 {
		p = p[:8192]
	}
	n, err := b.ReadCloser.Read(p)
	time.Sleep(time.Duration(float64(n) / b.bytesPerSecond * float64(time.Second)))
	return n, err
}

func (s slowReads) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := s.Store.Open(ctx, key, offset)
	if err != nil {
		return nil, err
	}
	return throttledBody{ReadCloser: body, bytesPerSecond: s.bytesPerSecond}, nil
}

// Criterion 5 on S3: the film is an object of a store that is read at 0.4 of the film's own rate, through the worker's
// loopback input. The job waits for it; the figure stays. One S3 target is enough: the guarantee is the classification of a
// waiting ffmpeg, not the store's conformance (invariant 11 has that suite), so the second target would add 30 s for nothing.
func TestAJobSlowedByAnS3SourceReadLeavesTheFigureAlone(t *testing.T) {
	media, segments := s3Segments(t), s3Segments(t)
	for name := range segments {
		t.Run(name, func(t *testing.T) {
			whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if err := media[name].Put(context.Background(), "film.mkv", strings.NewReader(string(whole))); err != nil {
				t.Fatal(err)
			}
			r := newRig(t, segments[name])
			// The rig's source row says it is 1 byte long, which the loopback input would serve; the film's own size, as r.copied
			// sets it.
			r.exec(t, "UPDATE media_sources SET size = $1", int64(len(whole)))
			slowed := slowReads{Store: media[name], bytesPerSecond: 0.4 * float64(len(whole)) / 90}
			w, _ := r.learner(t, nil, func(worker *Worker) {
				worker.Media = func(string) (store.Store, error) { return slowed, nil }
			})
			start := r.rowCores(t, w.ID)
			stop := r.playAs(t, r.costSpec(t, "sdr1080"), 0)
			defer stop()
			watchFigure(t, r, w, start, 18*time.Second)
		})
		break // One target.
	}
}

// slowPuts is a transcode store whose segment uploads take a set time each, one after another: the stored rate of a slow store.
type slowPuts struct {
	store.Store
	delay time.Duration
}

func (s slowPuts) Put(ctx context.Context, key string, body io.Reader) error {
	if strings.HasSuffix(key, ".mp4") && !strings.HasSuffix(key, "init.mp4") {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.Put(ctx, key, body)
}

// Criterion 5: a slowed store upload. ffmpeg writes to scratch and is not paced by uploads, so its own speed (here 2x) stays
// over 1.2 while the segments reach the store at 0.8x (5 s for each 4 s segment, one after another), and the worker's CPU is
// starved. A figure taken from the rate the segments are stored at reads that as a slow CPU and lowers; ffmpeg's own speed
// does not. The lease is 12 s so that a 5 s upload is slow and not given up (JS-106).
func TestAJobWhoseUploadsAreSlowLeavesTheFigureAlone(t *testing.T) {
	paced(t, 2)
	r := newRig(t, slowPuts{Store: newPosixSegments(t), delay: 5 * time.Second})
	w, _ := r.learner(t, starvedKernel(), func(worker *Worker) { worker.Lease = 12 * time.Second })
	start := r.rowCores(t, w.ID)
	stop := r.playAs(t, cheapOutput(t, r.costSpec(t, "sdr1080")), 0)
	defer stop()
	// The first segment came after one 5 s upload; a worker whose figure followed the stored rate would have lowered it 17 s
	// after the job's first output, at 0.8x with the CPU starved.
	watchFigure(t, r, w, start, 18*time.Second)
}

func newPosixSegments(t *testing.T) store.Store { return store.NewPosix(t.TempDir()) }

// Criterion 5: time spent paused at the lead. A job that reached its lead (4 s past the furthest segment asked for) and
// waits there, with the CPU starved and no second job: the figure stays. The job is paused before it is old enough to count
// at the rig's heartbeat, so what this shows is that a paused job's time is not read as a slow job's (a speed that counted
// the wall time the job spent stopped reads far under 1.2 and lowers the figure).
func TestAJobPausedAtItsLeadLeavesTheFigureAlone(t *testing.T) {
	paced(t, 0.8)
	r := newRig(t, nil)
	w, _ := r.learner(t, starvedKernel(), func(worker *Worker) { worker.Lead, worker.Idle = 4*time.Second, 90*time.Second })
	start := r.rowCores(t, w.ID)
	stop := r.playAs(t, r.costSpec(t, "sdr1080"), 0)
	defer stop()
	waitFor(t, "the job to pause at its lead", learnHang, func() bool { return r.paused() })
	watchFigure(t, r, w, start, 14*time.Second)
	if !r.paused() {
		t.Error("the job did not stay paused")
	}
}

// startExtraction runs a media tool as a worker's extraction does (mediatool.Run, at the lowest priority) for d, and
// returns the stop. It is what the worker counts as its own extraction.
func startExtraction(t *testing.T, d time.Duration) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mediatool.Run(ctx, "ffmpeg", []string{"-nostdin", "-v", "error", "-re", "-f", "lavfi", "-i", "anullsrc=r=48000",
			"-t", strconv.Itoa(int(d.Seconds())), "-f", "null", "-"}, mediatool.Options{Background: true}) //nolint:errcheck
	}()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}
