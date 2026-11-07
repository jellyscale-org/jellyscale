package transcode

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// clock is a progress's time, moved by hand.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func newProgress() (*progress, *clock) {
	c := &clock{at: time.Unix(1000, 0)}
	return &progress{now: c.now, began: c.at}, c
}

// emit writes a block as ffmpeg does, in pieces that cut a line in two.
func emit(t *testing.T, p *progress, film float64) {
	t.Helper()
	text := fmt.Sprintf("frame=1\nout_time_us=%d\nspeed=1.0x\nprogress=continue\n", int64(film*1e6))
	for _, piece := range []string{text[:11], text[11:]} {
		if _, err := p.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProgressSpeedIsFilmPerRunningSecond(t *testing.T) {
	p, c := newProgress()
	if _, ok := p.speed(); ok {
		t.Fatal("a job with no output has a speed")
	}
	emit(t, p, 0)
	c.at = c.at.Add(500 * time.Millisecond)
	emit(t, p, 0.4)
	if _, ok := p.speed(); ok {
		t.Fatal("under a second of running is not a speed yet")
	}
	// 0.8x for ten seconds, the UHD case.
	for i := 1; i <= 20; i++ {
		c.at = c.at.Add(500 * time.Millisecond)
		emit(t, p, 0.4+0.4*float64(i))
	}
	speed, ok := p.speed()
	if !ok || speed < 0.79 || speed > 0.81 {
		t.Fatalf("speed %v, %v; want 0.8", speed, ok)
	}
	// It follows the job: now it encodes 3 seconds of film per second.
	for i := 1; i <= 40; i++ {
		c.at = c.at.Add(500 * time.Millisecond)
		emit(t, p, 8.4+1.5*float64(i))
	}
	if speed, _ := p.speed(); speed < 2.9 || speed > 3.1 {
		t.Fatalf("speed %v; want 3 once the job has sped up", speed)
	}
}

// ffmpeg is stopped at the lead limit for as long as the viewers are behind, and
// that must not read as a slow encode: the next slice starts helpers for jobs
// below 1.2x.
func TestProgressSpeedIgnoresPausedTime(t *testing.T) {
	p, c := newProgress()
	emit(t, p, 0)
	for i := 1; i <= 4; i++ {
		c.at = c.at.Add(time.Second)
		emit(t, p, 2*float64(i)) // 2x.
	}
	p.pause(true)
	if p.pausedValue() != 1 {
		t.Fatal("paused job not reported paused")
	}
	c.at = c.at.Add(5 * time.Minute)
	if speed, _ := p.speed(); speed < 1.99 || speed > 2.01 {
		t.Fatalf("speed %v while paused; want the 2 it had", speed)
	}
	p.pause(false)
	if p.pausedValue() != 0 {
		t.Fatal("resumed job reported paused")
	}
	for i := 1; i <= 4; i++ {
		c.at = c.at.Add(time.Second)
		emit(t, p, 8+2*float64(i))
	}
	if speed, _ := p.speed(); speed < 1.99 || speed > 2.01 {
		t.Fatalf("speed %v after the pause; want 2", speed)
	}
}

// An ffmpeg that stops writing and is not paused is stuck, and must not keep
// the speed it had.
func TestProgressSpeedFallsWhenNoProgressArrives(t *testing.T) {
	p, c := newProgress()
	emit(t, p, 0)
	for i := 1; i <= 20; i++ {
		c.at = c.at.Add(500 * time.Millisecond)
		emit(t, p, 0.5*float64(i)) // 1x.
	}
	if speed, _ := p.speed(); speed < 0.99 || speed > 1.01 {
		t.Fatalf("speed %v before the stall; want 1", speed)
	}
	c.at = c.at.Add(10 * time.Second)
	if speed, ok := p.speed(); !ok || speed > 0.6 {
		t.Fatalf("speed %v, %v after 10 seconds without progress; want it halved or less", speed, ok)
	}
	c.at = c.at.Add(5 * time.Minute)
	if speed, _ := p.speed(); speed > 0.05 {
		t.Fatalf("speed %v after minutes without progress; want about 0", speed)
	}
}

func TestProgressIgnoresWhatIsNotATime(t *testing.T) {
	p, _ := newProgress()
	p.Write([]byte("out_time_us=N/A\nout_time_us=\nout_time=00:00:01\n")) //nolint:errcheck
	if len(p.samples) != 0 {
		t.Fatalf("samples from lines that carry no time: %v", p.samples)
	}
}

// histogramCount reads the count of the one series with these labels.
func histogramCount(t *testing.T, vec *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	observer, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatal(err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestObserveSegmentSplitsByOutcome(t *testing.T) {
	for _, tc := range []struct {
		err           error
		source        string
		outcome, kind string
		copied        bool
	}{
		{nil, "store", "ok", "reencode", false},
		{nil, "started", "ok", "copy", true},
		{ErrTimeout, "waited", "timeout", "reencode", false},
		{FailedError{"x"}, "started", "failed", "reencode", false},
		{ErrBusy, "", "refused", "copy", true},
		{errors.New("boom"), "", "error", "reencode", false},
	} {
		source := tc.source
		if source == "" {
			source = "none"
		}
		before := histogramCount(t, segmentWait, source, tc.kind, tc.outcome)
		observeSegment(time.Now(), tc.copied, tc.source, tc.err)
		if got := histogramCount(t, segmentWait, source, tc.kind, tc.outcome); got != before+1 {
			t.Errorf("%v from %q: %d observations, was %d", tc.err, tc.source, got, before)
		}
	}
}

func TestSlowSegmentRequestsAreStalls(t *testing.T) {
	defer func(was time.Duration) { stallAfter = was }(stallAfter)
	stallAfter = 50 * time.Millisecond
	stalls := segmentStalls.WithLabelValues("copy")
	refused := segmentRefused.WithLabelValues("no_worker")
	before, refusedBefore := counterValue(t, stalls), counterValue(t, refused)
	observeSegment(time.Now(), true, "store", nil)
	if counterValue(t, stalls) != before {
		t.Fatal("a quick request counted as a stall")
	}
	observeSegment(time.Now().Add(-time.Second), true, "waited", ErrNoWorker)
	if got := counterValue(t, stalls); got != before+1 {
		t.Fatalf("stalls %v, was %v", got, before)
	}
	if got := counterValue(t, refused); got != refusedBefore+1 {
		t.Fatalf("refusals %v, was %v", got, refusedBefore)
	}
}

// Both the api's Watch and Segment's own refuse a stopped viewer; neither adds
// to the other's count, and observeSegment adds none.
func TestObserveSegmentDoesNotCountAStoppedRefusal(t *testing.T) {
	stopped := segmentRefused.WithLabelValues("stopped")
	before := counterValue(t, stopped)
	observeSegment(time.Now(), false, "waited", ErrStopped)
	if got := counterValue(t, stopped); got != before {
		t.Fatalf("observeSegment counted a stopped refusal (%v, was %v); Watch and ensure do", got, before)
	}
	if err := refusedAsStopped(); !errors.Is(err, ErrStopped) || counterValue(t, stopped) != before+1 {
		t.Fatalf("refusedAsStopped: %v, count %v, was %v", err, counterValue(t, stopped), before)
	}
}
