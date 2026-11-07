//go:build integration

package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestPlaybackInfoCountsItsDecisions(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	reencode, direct := playbackDecisions.WithLabelValues("reencode"), playbackDecisions.WithLabelValues("direct_play")
	reencodes, directs, refusals := counterValue(t, reencode), counterValue(t, direct), counterValue(t, playbackRefusals)
	delta := func(what string, c prometheus.Counter, was *float64, want float64) {
		t.Helper()
		if got := counterValue(t, c); got != *was+want {
			t.Errorf("%s: %v, was %v, want %v more", what, got, *was, want)
		}
		*was = counterValue(t, c)
	}

	e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	delta("a transcode", reencode, &reencodes, 1)
	delta("refusals after a transcode", playbackRefusals, &refusals, 0)

	e.playbackInfo(t, 0, e.item, map[string]any{"MediaSourceId": source}) // No profile.
	delta("direct play", direct, &directs, 1)
	delta("a transcode counted as direct play", reencode, &reencodes, 0)

	mustExec(t, e.pool, "UPDATE transcode_nodes SET seen_at = now() - interval '1 minute'")
	e.playbackInfo(t, 0, e.item, transcodeProfileBody(t, source))
	delta("a refusal", playbackRefusals, &refusals, 1)
	delta("a refused source counted as a transcode", reencode, &reencodes, 0)
}

func TestPlaybackInfoCountsACopy(t *testing.T) {
	e, _, source := transcodeEnv(t, "h264", "ac3")
	mustExec(t, e.pool, "UPDATE media_sources SET keyframes = '{0,30000000,70000000,130000000,180000000,260000000,310000000,390000000,440000000,520000000,570000000,650000000,700000000,780000000,830000000}', video_end = 900000000, size = 25000000")
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = 900000000")
	copies := playbackDecisions.WithLabelValues("copy")
	before := counterValue(t, copies)
	e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source})
	if got := counterValue(t, copies); got != before+1 {
		t.Errorf("copies %v, was %v", got, before)
	}
}
