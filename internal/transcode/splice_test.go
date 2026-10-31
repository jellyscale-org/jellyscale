package transcode

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not installed", name)
		}
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, errOut.String())
	}
	return out.String() + errOut.String()
}

// makeSource writes a 40 second clip with a 440 Hz tone, keyframes every ten
// seconds so a restart has to decode forward to its cut.
func makeSource(t *testing.T, dir, name, rate, audioRate, extra string) string {
	t.Helper()
	path := filepath.Join(dir, name+".mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate="+rate+":duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate="+audioRate+":duration=40",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "240", "-c:a", "flac", path)
	if extra == "" {
		return path
	}
	shifted := filepath.Join(dir, name+"-shifted.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-i", path, "-c", "copy", "-output_ts_offset", extra, shifted)
	return shifted
}

var testSpec = playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 640, Height: 360}

// encode runs the real command from segment start and returns its directory.
func encode(t *testing.T, input string, start int) string {
	t.Helper()
	dir := t.TempDir()
	cmd, err := Command(context.Background(), input, testSpec, start, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, errOut.String())
	}
	return dir
}

// join stitches an init segment and media segments into one fragmented MP4,
// the way a player sees them.
func join(t *testing.T, init string, segments ...string) string {
	t.Helper()
	var all []byte
	for _, name := range append([]string{init}, segments...) {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, data...)
	}
	path := filepath.Join(t.TempDir(), "joined.mp4")
	if err := os.WriteFile(path, all, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// times lists a stream's presentation times, sorted.
func times(t *testing.T, file, stream, entries string) []float64 {
	t.Helper()
	out := run(t, "ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", entries, "-of", "csv=p=0", file)
	var list []float64
	for _, field := range strings.Fields(out) {
		if v, err := strconv.ParseFloat(strings.TrimRight(field, ","), 64); err == nil {
			list = append(list, v)
		}
	}
	sort.Float64s(list)
	return list
}

func compare(t *testing.T, what string, want, got []float64, limit float64) {
	t.Helper()
	if len(want) != len(got) {
		for i := 0; i < min(len(want), len(got)); i++ {
			if math.Abs(want[i]-got[i]) > limit {
				t.Logf("first difference at %d: %.5f against %.5f; want %.5f got %.5f", i, want[i], got[i], want[max(i-3, 0):min(i+3, len(want))], got[max(i-3, 0):min(i+3, len(got))])
				break
			}
		}
		t.Errorf("%s: %d against %d (last %.4f against %.4f)", what, len(want), len(got), want[len(want)-1], got[len(got)-1])
		return
	}
	worst := 0.0
	for i := range want {
		worst = math.Max(worst, math.Abs(want[i]-got[i]))
	}
	if worst > limit {
		t.Errorf("%s: worst difference %.3f ms, limit %.3f ms", what, worst*1000, limit*1000)
	}
}

func before(list []float64, limit float64) []float64 {
	i := sort.SearchFloat64s(list, limit)
	return list[:i]
}

func segmentFiles(dir string, from, to int) []string {
	var files []string
	for n := from; n < to; n++ {
		files = append(files, filepath.Join(dir, fmt.Sprintf("%d.m4s", n)))
	}
	return files
}

// A fresh ffmpeg started at segment N must splice with the segments an earlier
// run made, with no discontinuity: same frames at the same times, and audio
// packets on the same grid.
func TestRestartSplicesWithEarlierSegments(t *testing.T) {
	needFFmpeg(t)
	work := t.TempDir()
	sources := map[string]string{
		"ntsc":    makeSource(t, work, "ntsc", "24000/1001", "48000", ""),
		"pal":     makeSource(t, work, "pal", "25", "48000", ""),
		"44k":     makeSource(t, work, "44k", "24000/1001", "44100", ""),
		"shifted": makeSource(t, work, "shifted", "24000/1001", "48000", "7.5"),
	}
	const total = 10 // 40 seconds in 4 second segments
	for name, source := range sources {
		t.Run(name, func(t *testing.T) {
			whole := encode(t, source, 0)
			full := join(t, filepath.Join(whole, "init.mp4"), segmentFiles(whole, 0, total)...)
			for _, restart := range []int{1, 5, 9} {
				resumed := encode(t, source, restart)
				spliced := join(t, filepath.Join(whole, "init.mp4"),
					append(segmentFiles(whole, 0, restart), segmentFiles(resumed, restart, total)...)...)
				compare(t, fmt.Sprintf("restart %d video", restart),
					times(t, full, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
				// The last segment is left out: at the end of the file a run that
				// started later flushes one more padded AAC frame, which sits
				// past the picture and is no seam.
				compare(t, fmt.Sprintf("restart %d audio", restart),
					before(times(t, full, "a:0", "packet=pts_time"), tsOffset+36), before(times(t, spliced, "a:0", "packet=pts_time"), tsOffset+36), 0.0005)
				if warnings := strings.TrimSpace(run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-")); warnings != "" {
					t.Errorf("restart %d: decoder warnings:\n%s", restart, warnings)
				}
				if a, b := readFile(t, filepath.Join(whole, "init.mp4")), readFile(t, filepath.Join(resumed, "init.mp4")); !bytes.Equal(a, b) {
					t.Errorf("restart %d: init segments differ", restart)
				}
			}
		})
	}
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Each audio index picks that stream: the output carries the tone of the track
// asked for.
func TestAudioIndexSelectsTheTrack(t *testing.T) {
	needFFmpeg(t)
	input := filepath.Join(t.TempDir(), "two-tracks.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=9",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=9",
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000:duration=9",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "ac3", input)
	for index, want := range map[int]float64{1: 440, 2: 880} {
		spec := testSpec
		spec.AudioIndex = index
		dir := t.TempDir()
		cmd, err := Command(context.Background(), input, spec, 0, dir, "")
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		joined := join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, 0, 2)...)
		pcm := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", joined, "-map", "0:a:0", "-ac", "1", "-ar", "48000", "-f", "s16le", "-")
		samples := []byte(pcm)
		crossings := 0
		for i := 4; i+1 < len(samples); i += 2 {
			prev, cur := int16(uint16(samples[i-2])|uint16(samples[i-1])<<8), int16(uint16(samples[i])|uint16(samples[i+1])<<8)
			if (prev < 0) != (cur < 0) {
				crossings++
			}
		}
		got := float64(crossings) / 2 / (float64(len(samples)/2) / 48000)
		if math.Abs(got-want) > want*0.05 {
			t.Errorf("audio index %d: %.0f Hz, want %.0f", index, got, want)
		}
	}
}

// An interlaced source comes out progressive: its fields are not left woven
// into combed frames.
func TestInterlacedSourceIsDeinterlaced(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "interlaced.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=50:duration=14",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=14",
		"-vf", "interlace=scan=tff", "-c:v", "libx264", "-preset", "ultrafast", "-flags", "+ilme+ildct", "-x264-params", "tff=1",
		"-g", "100", "-c:a", "aac", source)
	out := encode(t, source, 0)
	joined := join(t, filepath.Join(out, "init.mp4"), segmentFiles(out, 0, 1)...)
	report := run(t, "ffmpeg", "-nostdin", "-i", joined, "-vf", "idet", "-f", "null", "-")
	var tff, bff, progressive int
	// idet prints a summary when it starts, all zero, and another at the end.
	all := regexp.MustCompile(`Multi frame detection: TFF:\s*(\d+) BFF:\s*(\d+) Progressive:\s*(\d+)`).FindAllStringSubmatch(report, -1)
	if len(all) == 0 {
		t.Fatalf("idet said nothing:\n%s", report)
	}
	found := all[len(all)-1]
	tff, _ = strconv.Atoi(found[1])
	bff, _ = strconv.Atoi(found[2])
	progressive, _ = strconv.Atoi(found[3])
	if total := tff + bff + progressive; total == 0 || progressive*10 < total*9 {
		t.Fatalf("combed output: %d TFF, %d BFF, %d progressive", tff, bff, progressive)
	}
}
