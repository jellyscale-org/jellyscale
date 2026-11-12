//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"jellyscale/internal/library"
	"jellyscale/internal/metrics"
	"jellyscale/internal/testenv"
)

// scrape is /metrics as the worker serves it.
func scrape(t *testing.T, w *Worker) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler(w).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 200 {
		t.Fatalf("/metrics answered %d", recorder.Code)
	}
	return recorder.Body.String()
}

// value is the number after the first line that starts with series, or -1.
func value(text, series string) float64 {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			n, _ := strconv.ParseFloat(rest, 64)
			return n
		}
	}
	return -1
}

var speedLine = regexp.MustCompile(`(?m)^jellyscale_transcode_job_speed_ratio\{job="\d+",kind="reencode"\} (\S+)$`)

// A worker shows its running job while it runs and nothing of it afterwards: the
// speed is film per running second, held (not dragged down) while the job is
// paused at its lead, and the series goes with the job whichever way it ends.
func TestAWorkerReportsItsJobWhileItRuns(t *testing.T) {
	slow(t) // Real time: the job encodes one second of film per second.
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(worker *Worker) {
		w = worker
		worker.Lead = 12 * time.Second
		worker.Idle = 5 * time.Second
		worker.Slots = 2
	})
	startedBefore := counterValue(t, jobsStarted)
	cancelledBefore := counterValue(t, jobsEnded.WithLabelValues("interrupted"))
	doneBefore := counterValue(t, jobsEnded.WithLabelValues("done"))
	j := r.jobs(t)
	before := histogramCount(t, segmentWait, "started", "reencode", "ok")
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if got := histogramCount(t, segmentWait, "started", "reencode", "ok"); got < before+1 {
		t.Errorf("the request that started the job was not counted as having started it (%d, was %d)", got, before)
	}
	stopWatching := r.keepWatching(t, j, psid(0))

	var text string
	waitFor(t, "a speed", 20*time.Second, func() bool { text = scrape(t, w); return speedLine.MatchString(text) })
	// What the speed is on this machine is its own to say (TestAWorkersShownSpeed...
	// in mechanism_integration_test.go checks it against what the worker
	// recorded); here it is shown, and a number.
	shownBefore, err := strconv.ParseFloat(speedLine.FindStringSubmatch(text)[1], 64)
	if err != nil || shownBefore == 0 || math.IsNaN(shownBefore) {
		t.Errorf("speed %q for a running job", speedLine.FindStringSubmatch(text)[1])
	}
	for series, want := range map[string]float64{
		`jellyscale_transcode_slots`:                         2,
		`jellyscale_transcode_slots_busy`:                    1,
		`jellyscale_transcode_jobs_running{kind="reencode"}`: 1,
		`jellyscale_transcode_jobs_running{kind="copy"}`:     0,
		`jellyscale_transcode_queue_depth`:                   0,
	} {
		if got := value(text, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if got := counterValue(t, jobsStarted); got != startedBefore+1 {
		t.Errorf("jobs started %v, was %v", got, startedBefore)
	}

	// At its lead the job stops, for as long as it takes; its speed is the
	// speed it had and not what a stopped process averages.
	waitFor(t, "the job to pause", 30*time.Second, func() bool { return r.paused() })
	time.Sleep(time.Second) // Whatever ffmpeg had already written when it stopped is read by now.
	text = scrape(t, w)
	pausedAt := speedLine.FindStringSubmatch(text)
	if pausedAt == nil {
		t.Fatal("the job lost its speed when it paused")
	}
	time.Sleep(5 * time.Second)
	text = scrape(t, w)
	if got := value(text, `jellyscale_transcode_job_paused{job="`+r.jobID(t)+`"}`); got != 1 {
		t.Errorf("a paused job reports paused %v", got)
	}
	match := speedLine.FindStringSubmatch(text)
	if match == nil {
		t.Fatal("the paused job lost its speed")
	}
	// The speed is over the job's running time, so six seconds paused change
	// nothing; a speed that fell would be pausing counted as slow encoding.
	shownHeld, _ := strconv.ParseFloat(match[1], 64)
	shownPaused, _ := strconv.ParseFloat(pausedAt[1], 64)
	if math.Abs(shownHeld-shownPaused) > 1e-9*math.Max(shownHeld, shownPaused) {
		t.Errorf("speed %v after 6 seconds paused, %v when it paused: pausing counted as slow encoding", shownHeld, shownPaused)
	}

	// Nobody asks any more: the job is cut short, and the series is gone.
	stopWatching()
	waitFor(t, "the job to end", 30*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
	waitFor(t, "the series to go", 5*time.Second, func() bool {
		text = scrape(t, w)
		return !strings.Contains(text, "jellyscale_transcode_job_speed_ratio{") && !strings.Contains(text, "jellyscale_transcode_job_paused{")
	})
	if got := value(text, `jellyscale_transcode_slots_busy`); got != 0 {
		t.Errorf("busy slots %v after the job", got)
	}
	if got := value(text, `jellyscale_transcode_jobs_running{kind="reencode"}`); got != 0 {
		t.Errorf("jobs running %v after the job", got)
	}
	// Cancelled when it was left behind is a job that ended well.
	if ended := counterValue(t, jobsEnded.WithLabelValues("done")) - doneBefore + counterValue(t, jobsEnded.WithLabelValues("interrupted")) - cancelledBefore; ended != 1 {
		t.Errorf("%v jobs counted as ended, want 1", ended)
	}
}

// The queue is the cluster's, read from Postgres when asked.
func TestTheQueueDepthIsReadAtScrapeTime(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(worker *Worker) {
		w = worker
		worker.Slots = 1
		worker.Lead = 12 * time.Second
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	// A second spec waits for the only slot.
	copyOf := *r
	other := &copyOf
	other.spec.VideoBitrate = 1_000_000
	other.hash = other.spec.Hash()
	go other.fetch(t, j, psid(1), 0) //nolint:errcheck
	waitFor(t, "a queued job", 10*time.Second, func() bool { return value(scrape(t, w), `jellyscale_transcode_queue_depth`) == 1 })
	if got := value(scrape(t, w), `jellyscale_transcode_slots_busy`); got != 1 {
		t.Errorf("busy slots %v with the one slot taken", got)
	}
}

// jobID is the ID of the one running job.
func (r *rig) jobID(t *testing.T) string {
	t.Helper()
	return strconv.Itoa(r.count(t, "SELECT id FROM transcode_jobs WHERE state = 'running'"))
}

// A worker's look that finds a dead worker counts it and its jobs once; a look
// that finds nothing counts nothing.
func TestReapsAreCounted(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	workers, jobs := reaped.WithLabelValues("workers"), reaped.WithLabelValues("jobs")
	workersBefore, jobsBefore := counterValue(t, workers), counterValue(t, jobs)
	look := func() error {
		dead, cut, _, err := Reap(ctx, r.pool, "")
		logReaped(r.log, dead, cut)
		return err
	}
	if err := look(); err != nil {
		t.Fatal(err)
	}
	if counterValue(t, workers) != workersBefore || counterValue(t, jobs) != jobsBefore {
		t.Fatal("a look at a healthy cluster counted a reap")
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ('dead-x', 'worker', '', 2, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ('h', '{}', 'o', 0, 0, 'running', 'dead-x', now() + interval '10 seconds')`); err != nil {
		t.Fatal(err)
	}
	if err := look(); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(t, workers) - workersBefore; got != 1 {
		t.Errorf("%v workers counted, want 1", got)
	}
	if got := counterValue(t, jobs) - jobsBefore; got != 1 {
		t.Errorf("%v jobs counted, want 1", got)
	}
}

// A request from a viewer who was stopped is refused before any segment is
// read, and still counted.
func TestAStoppedViewersRequestIsCountedAsRefused(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	j := r.jobs(t)
	stopped := segmentRefused.WithLabelValues("stopped")
	before := counterValue(t, stopped)
	if err := j.Watch(ctx, r.viewer(psid(0)), r.hash, 0); err != nil {
		t.Fatal(err)
	}
	if counterValue(t, stopped) != before {
		t.Fatal("a viewer who is watching was counted as refused")
	}
	if err := j.Stop(ctx, r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := j.Watch(ctx, r.viewer(psid(0)), r.hash, 0); !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
	if got := counterValue(t, stopped) - before; got != 1 {
		t.Errorf("%v refusals counted, want 1", got)
	}
}

// A viewer stopped while a Segment request waits is one refusal, though Segment
// both watches and refuses.
func TestAStoppedWaitingRequestIsCountedOnce(t *testing.T) {
	r := newRig(t, nil)
	j := r.jobs(t)
	stopped := segmentRefused.WithLabelValues("stopped")
	before := counterValue(t, stopped)
	result := make(chan error, 1)
	go func() {
		_, err := j.Segment(context.Background(), r.spec, r.hash, r.viewer(psid(0)), 0, 0) // No worker to make it.
		result <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	if err := j.Stop(context.Background(), r.user, psid(0), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrStopped) {
		t.Fatalf("got %v, want ErrStopped", err)
	}
	if got := counterValue(t, stopped) - before; got != 1 {
		t.Errorf("%v stopped refusals counted, want 1", got)
	}
}

// A request that finds a job already there, from another viewer, waited on it;
// the request that made the job started it.
func TestARequestThatJoinsAJobIsCountedAsHavingWaited(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	waitedBefore := histogramCount(t, segmentWait, "waited", "reencode", "ok")
	startedBefore := histogramCount(t, segmentWait, "started", "reencode", "ok")
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if got := histogramCount(t, segmentWait, "started", "reencode", "ok"); got != startedBefore+1 {
		t.Fatalf("started %d, was %d", got, startedBefore)
	}
	r.keepWatching(t, j, psid(0))
	// Another viewer, and a segment the running job has not stored yet.
	if _, err := r.fetch(t, j, psid(1), 2); err != nil {
		t.Fatal(err)
	}
	if got := histogramCount(t, segmentWait, "waited", "reencode", "ok"); got != waitedBefore+1 {
		t.Errorf("waited %d, was %d", got, waitedBefore)
	}
	if r.count(t, "SELECT count(*) FROM transcode_jobs") != 1 {
		t.Error("the second viewer started a job of its own")
	}
}

// A job whose ffmpeg fails is counted as failed, and the request that waited on
// it as a failed one.
func TestAFailedJobIsCounted(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, nil)
	dir := strings.TrimPrefix(mustScalar(t, r, "SELECT media_store_uri FROM libraries"), "file://")
	bad := dir + "-bad"
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(bad) })
	if err := os.WriteFile(bad+"/film.mkv", []byte("not a movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.exec(t, "UPDATE libraries SET media_store_uri = $1", "file://"+bad)
	failed := jobsEnded.WithLabelValues("failed")
	before := counterValue(t, failed)
	waitsBefore := histogramCount(t, segmentWait, "started", "reencode", "failed")
	var failure FailedError
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); !errors.As(err, &failure) {
		t.Fatalf("got %v", err)
	}
	if got := counterValue(t, failed) - before; got != 1 {
		t.Errorf("%v failed jobs counted, want 1", got)
	}
	if got := histogramCount(t, segmentWait, "started", "reencode", "failed"); got != waitsBefore+1 {
		t.Errorf("failed waits %d, was %d", got, waitsBefore)
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

// A subtitle extraction shows in the gauge, by its own label, while it runs and
// not after.
func TestARunningExtractionIsCounted(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	countExtractions(t, "sleep 3 >/dev/null 2>&1")
	r.worker(t, nil)
	j := r.jobs(t)
	subtitles, chapters := extractions.WithLabelValues("subtitles"), extractions.WithLabelValues("chapter_images")
	done := make(chan error, 1)
	go func() {
		_, err := FetchSubtitle(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2)
		done <- err
	}()
	waitFor(t, "the extraction to show", 20*time.Second, func() bool { return gaugeValue(t, subtitles) == 1 })
	if got := gaugeValue(t, chapters); got != 0 {
		t.Errorf("chapter jobs %v while a subtitle runs", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the extraction to leave the gauge", 5*time.Second, func() bool { return gaugeValue(t, subtitles) == 0 })
}

// A chapter-picture job is counted under its own label, and a worker that stops
// and cuts it off still takes it out.
func TestARunningChapterJobIsCounted(t *testing.T) {
	r := newRig(t, nil)
	r.exec(t, `UPDATE media_sources SET streams = '[{"Type":"Video"}]', chapters = '[{"StartPositionTicks":0,"Name":"a"}]'`)
	r.exec(t, "UPDATE libraries SET chapter_images = true")
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *image2pipe*) exec sleep 60;; esac\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stop := r.worker(t, nil)
	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	subtitles, chapters := extractions.WithLabelValues("subtitles"), extractions.WithLabelValues("chapter_images")
	waitFor(t, "the chapter job to show", 20*time.Second, func() bool { return gaugeValue(t, chapters) == 1 })
	if got := gaugeValue(t, subtitles); got != 0 {
		t.Errorf("subtitle extractions %v while a chapter job runs", got)
	}
	stop()
	if got := gaugeValue(t, chapters); got != 0 {
		t.Errorf("chapter jobs %v after the worker stopped", got)
	}
}

// A worker that finds its own row among the dead counts it too, as the
// replicas that find others do.
func TestAWorkerReapedByItsOwnLookIsCounted(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	workers := reaped.WithLabelValues("workers")
	w := &Worker{ID: "lost-lock", Pool: r.pool, Log: r.log}
	r.exec(t, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ($1, 'worker', '', 2, true)`, w.ID)
	before := counterValue(t, workers)
	// A node is forgotten on a second look, a grace after the first found it dead.
	if w.lookForTheDead(ctx) {
		t.Fatal("the first look already forgot the worker")
	}
	time.Sleep(reapGrace)
	if !w.lookForTheDead(ctx) {
		t.Fatal("a worker whose row is among the dead did not find itself lost")
	}
	if got := counterValue(t, workers) - before; got != 1 {
		t.Errorf("%v workers counted, want 1", got)
	}
}
