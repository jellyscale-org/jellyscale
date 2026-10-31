package library

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// A file whose audio outlasts its video must report where the video ends, in
// both containers: mp4 says so per stream, Matroska only in a tag.
func TestProbeReportsVideoDuration(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	for _, name := range []string{"long-audio.mp4", "long-audio.mkv"} {
		file := filepath.Join(t.TempDir(), name)
		out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=5",
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", file).CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		var video int64
		for _, s := range probe.Streams {
			if s["Type"] == "Video" {
				video, _ = s["DurationTicks"].(int64)
			}
		}
		if video < 19_000_000 || video > 21_000_000 {
			t.Errorf("%s: video DurationTicks %d, want about 20000000", name, video)
		}
		if probe.RunTimeTicks < 45_000_000 {
			t.Errorf("%s: RunTimeTicks %d should stay the format's", name, probe.RunTimeTicks)
		}
	}
}

// The H.264 profile a browser sends rules out level 5.3, interlaced and HDR
// files, so the probe has to record what shows it.
func TestProbeRecordsLevelFieldOrderAndRange(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("no ffmpeg")
	}
	cases := []struct {
		name      string
		args      []string
		level     int
		interlace bool
		rangeType string
	}{
		{"plain", nil, 10, false, "SDR"},
		{"level", []string{"-level", "5.2"}, 52, false, "SDR"},
		{"interlaced", []string{"-vf", "interlace=scan=tff", "-flags", "+ilme+ildct"}, -1, true, "SDR"},
		{"hdr", []string{"-x264-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc"}, 10, false, "HDR10"},
		{"hlg", []string{"-x264-params", "colorprim=bt2020:transfer=arib-std-b67:colormatrix=bt2020nc"}, 10, false, "HLG"},
	}
	for _, c := range cases {
		file := filepath.Join(t.TempDir(), "v.mkv")
		args := append([]string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x92:rate=10:duration=1",
			"-c:v", "libx264", "-preset", "ultrafast"}, c.args...)
		if out, err := exec.Command("ffmpeg", append(args, file)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", c.name, err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		video := probe.Streams[0]
		if (video["Level"] != c.level && c.level >= 0) || video["IsInterlaced"] != c.interlace || video["VideoRangeType"] != c.rangeType {
			t.Errorf("%s: level %v interlaced %v range %v, want %d %v %s", c.name, video["Level"], video["IsInterlaced"], video["VideoRangeType"], c.level, c.interlace, c.rangeType)
		}
	}
}

// Two tracks in one language must read differently in a client's menu.
func TestSubtitleLabelsTellTracksApart(t *testing.T) {
	for _, c := range []struct {
		language, codec     string
		isDefault, isForced bool
		want                string
	}{
		{"eng", "subrip", true, false, "eng - Default - SUBRIP"},
		{"eng", "subrip", false, true, "eng - Forced - SUBRIP"},
		{"", "ass", false, false, "ASS"},
	} {
		if got := displaySubtitle(c.language, c.codec, c.isDefault, c.isForced); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}
