//go:build integration

package library

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"jellyscale/internal/testenv"
)

// A file whose audio outlasts its video must report where the video ends, in
// both containers: mp4 says so per stream, Matroska only in a tag.
func TestProbeReportsVideoDuration(t *testing.T) {
	testenv.FFmpeg(t)
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
	testenv.FFmpeg(t)
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

// The bit depth comes from the pixel format, which is all ffprobe says of it
// for most HEVC and AV1: the master playlist of an AV1 copy names it.
func TestProbeRecordsBitDepth(t *testing.T) {
	testenv.FFmpeg(t)
	for _, c := range []struct {
		pixFmt string
		depth  int
	}{{"yuv420p", 8}, {"yuv420p10le", 10}} {
		file := filepath.Join(t.TempDir(), "v.mkv")
		if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x92:rate=10:duration=1",
			"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", c.pixFmt, file).CombinedOutput(); err != nil {
			t.Fatalf("%s: ffmpeg: %v: %s", c.pixFmt, err, out)
		}
		probe, err := ProbeInput(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		if got := probe.Streams[0]["BitDepth"]; got != c.depth {
			t.Errorf("%s: bit depth %v, want %d", c.pixFmt, got, c.depth)
		}
		if !probe.wantsKeyframes() {
			t.Errorf("%s: an HEVC file in Matroska is not asked for an index", c.pixFmt)
		}
	}
}

// PGS, VobSub and DVB tracks are recorded as image tracks under the names
// Jellyfin lists them by, and only PGS can be sent to the client as a file.
func TestProbeRecordsImageSubtitles(t *testing.T) {
	testenv.FFmpeg(t)
	dir := t.TempDir()
	sup := filepath.Join(dir, "track.sup")
	if err := os.WriteFile(sup, testenv.PGS(1920, 1080, testenv.Cue{Start: 0.2, End: 0.8, X: 10, Y: 10, W: 100, H: 40}), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "film.mkv")
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=1",
		"-fix_sub_duration", "-i", sup, "-map", "0:v", "-map", "1", "-map", "1", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:s:0", "copy", "-c:s:1", "dvdsub", "-c:s:2", "dvbsub", "-metadata:s:s:0", "language=eng", "-disposition:s:0", "default+forced", file).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	probe, err := ProbeInput(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range map[int]map[string]any{
		1: {"Codec": "PGSSUB", "DisplayTitle": "eng - Default - Forced - PGSSUB", "Language": "eng", "Width": 1920, "Height": 1080, "SupportsExternalStream": true},
		2: {"Codec": "DVDSUB", "DisplayTitle": "DVDSUB", "Width": 1920, "Height": 1080, "SupportsExternalStream": false},
		3: {"Codec": "DVBSUB", "DisplayTitle": "DVBSUB", "SupportsExternalStream": false},
	} {
		got := probe.Streams[index]
		want["Index"], want["Type"], want["IsTextSubtitleStream"], want["IsImageSubtitleStream"] = index, "Subtitle", false, true
		for key, value := range want {
			if got[key] != value {
				t.Errorf("stream %d: %s is %v, want %v (%v)", index, key, got[key], value, got)
			}
		}
		if _, has := got["Width"]; index == 3 && has {
			t.Errorf("stream 3: DVB says no size, and Width is %v", got["Width"])
		}
	}
}
