//go:build integration

package transcode

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jellyscale/internal/playback"

	"jellyscale/internal/testenv"
)

// hdrParams tags a 10-bit HEVC file as BT.2020 with the given transfer function,
// as an HDR10 or HLG release is. PQ carries the mastering display and light
// level an HDR10 file has.
func hdrParams(transfer string) string {
	params := "log-level=error:colorprim=bt2020:colormatrix=bt2020nc:transfer=" + transfer
	if transfer == playback.PQ {
		params += ":master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400"
	}
	return params
}

// hdrPatch makes a flat picture at a 10-bit limited-range luma code and chroma
// codes; 512 for both is grey.
func hdrPatch(t *testing.T, dir, transfer string, code, cb, cr, seconds int) string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("patch-%s-%d-%d-%d.mkv", transfer, code, cb, cr))
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("nullsrc=size=640x360:rate=24:duration=%d,format=yuv420p10le,geq=lum=%d:cb=%d:cr=%d", seconds, code, cb, cr),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds),
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "8", "-x265-params", hdrParams(transfer), "-c:a", "aac", path)
	return path
}

// meanLuma is the average of the 8-bit luma of every frame of a stream.
func meanLuma(t *testing.T, file string) float64 { return meanStat(t, file, "YAVG") }

// meanStat is the average over every frame of one of signalstats' values, such
// as YAVG, UAVG or VAVG.
func meanStat(t *testing.T, file, key string) float64 {
	t.Helper()
	out := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-an",
		"-vf", "signalstats,metadata=print:key=lavfi.signalstats."+key+":file=-", "-f", "null", "-")
	var sum float64
	var n int
	for _, line := range strings.Split(out, "\n") {
		if _, value, ok := strings.Cut(line, "lavfi.signalstats."+key+"="); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				t.Fatal(err)
			}
			sum += v
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no frames measured in %s", file)
	}
	return sum / float64(n)
}

// An HDR picture comes out SDR BT.709, tagged so, with the light levels an SDR
// display shows them at. PQ code 573 is 203 nits, the level HDR10 puts diffuse
// white at, and 509 is 100 nits; HLG code 721 is its reference white (75%).
// Passed through unmapped, PQ comes out at luma 143 and 127 (the control), which
// is what a viewer sees as washed out. HLG is made to look right on an SDR
// display as it is, so its luma is the same either way, and the tags and the
// primaries are what map it. The ranges are what hable at
// npl=100 gives, measured on ffmpeg 6.1.1, 8.1.3 and 9.0.2 (182, 151 and 182)
// with room for the encoder.
func TestToneMapMakesSDRBT709(t *testing.T) {
	testenv.FFmpeg(t)
	work := t.TempDir()
	for _, c := range []struct {
		name, transfer string
		code           int
		low, high      float64
		control        bool
	}{
		{"PQ 203 nits", playback.PQ, 573, 175, 190, true},
		{"PQ 100 nits", playback.PQ, 509, 144, 158, true},
		{"HLG reference white", playback.HLG, 721, 175, 190, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			source := hdrPatch(t, work, c.transfer, c.code, 512, 512, 9)
			spec := testSpec
			spec.ToneMap = c.transfer
			dir := t.TempDir()
			cmd, err := Command(context.Background(), source, spec, 0, dir, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, out)
			}
			joined := join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, 0, 2)...)
			tags := strings.TrimSpace(run(t, "ffprobe", "-v", "error", "-select_streams", "v:0",
				"-show_entries", "stream=pix_fmt,color_space,color_primaries,color_transfer,color_range", "-of", "csv=p=0:s=,", joined))
			tags = strings.TrimSuffix(tags, ",")
			if tags != "yuv420p,tv,bt709,bt709,bt709" {
				t.Errorf("pix_fmt, range, matrix, primaries, transfer = %q, want yuv420p,tv,bt709,bt709,bt709", tags)
			}
			if got := meanLuma(t, joined); got < c.low || got > c.high {
				t.Errorf("mean luma %.1f, want %.0f to %.0f", got, c.low, c.high)
			}

			if !c.control {
				return
			}
			// The control: the same film encoded as before is outside the range.
			plain := t.TempDir()
			cmd, err = Command(context.Background(), source, testSpec, 0, plain, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg: %v\n%s", err, out)
			}
			if got := meanLuma(t, join(t, filepath.Join(plain, "init.mp4"), segmentFiles(plain, 0, 2)...)); got >= c.low && got <= c.high {
				t.Errorf("the film without tone mapping has luma %.1f, inside %.0f to %.0f: the range tells nothing", got, c.low, c.high)
			}
		})
	}
}

// HLG is made to look right on an SDR display as it is, so a grey patch has the
// same luma mapped or not, and only the conversion from BT.2020 to BT.709
// primaries is left to show the mapping did something. A saturated BT.2020
// colour is where it shows: without the filter the picture keeps its BT.2020
// tags and chroma, which an SDR display shows wrongly less saturated.
// The control is the same film encoded as before.
func TestToneMapConvertsHLGPrimaries(t *testing.T) {
	testenv.FFmpeg(t)
	source := hdrPatch(t, t.TempDir(), playback.HLG, 500, 400, 800, 9)
	chroma := func(spec playback.Spec) (u, v float64) {
		dir := t.TempDir()
		cmd, err := Command(context.Background(), source, spec, 0, dir, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		joined := join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, 0, 2)...)
		return meanStat(t, joined, "UAVG"), meanStat(t, joined, "VAVG")
	}
	mapped := testSpec
	mapped.ToneMap = playback.HLG
	mu, mv := chroma(mapped)
	pu, pv := chroma(testSpec)
	t.Logf("mapped U %.1f V %.1f, unmapped U %.1f V %.1f", mu, mv, pu, pv)
	if diff := math.Abs(mu-pu) + math.Abs(mv-pv); diff < 20 {
		t.Errorf("the mapping changed the chroma by %.1f, want at least 20", diff)
	}
}

// A restart splices with the segments of an earlier run: the same frame times,
// the same init segment, no decoder warnings, and no step in brightness at the
// seam, as a different peak or a state the filter kept from one frame to the
// next would make. The source has its own HDR10 or HLG colour tags and a
// changing picture.
func TestToneMappedRestartSplicesWithEarlierSegments(t *testing.T) {
	testenv.FFmpeg(t)
	work := t.TempDir()
	const total = 10 // 40 seconds in 4 second segments
	for _, transfer := range []string{playback.PQ, playback.HLG} {
		t.Run(transfer, func(t *testing.T) {
			source := filepath.Join(work, transfer+".mkv")
			run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24000/1001:duration=40",
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
				"-vf", "format=yuv420p10le", "-c:v", "libx265", "-preset", "ultrafast", "-g", "240", "-x265-params", hdrParams(transfer), "-c:a", "flac", source)
			spec := testSpec
			spec.ToneMap = transfer
			build := func(start int) string {
				dir := t.TempDir()
				cmd, err := Command(context.Background(), source, spec, start, dir, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, out)
				}
				return dir
			}
			whole := build(0)
			full := join(t, filepath.Join(whole, "init.mp4"), segmentFiles(whole, 0, total)...)
			for _, restart := range []int{1, 5, 9} {
				resumed := build(restart)
				spliced := join(t, filepath.Join(whole, "init.mp4"),
					append(segmentFiles(whole, 0, restart), segmentFiles(resumed, restart, total)...)...)
				compare(t, fmt.Sprintf("restart %d video", restart),
					times(t, full, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
				if warnings := strings.TrimSpace(run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-")); warnings != "" {
					t.Errorf("restart %d: decoder warnings:\n%s", restart, warnings)
				}
				if a, b := readFile(t, filepath.Join(whole, "init.mp4")), readFile(t, filepath.Join(resumed, "init.mp4")); string(a) != string(b) {
					t.Errorf("restart %d: init segments differ", restart)
				}
				// The brightness of every frame is the same in both, to what the
				// encoder's own rate control moves.
				a, b := frameLuma(t, full), frameLuma(t, spliced)
				if len(a) != len(b) {
					t.Fatalf("restart %d: %d frames against %d", restart, len(a), len(b))
				}
				worst := 0.0
				for i := range a {
					worst = math.Max(worst, math.Abs(a[i]-b[i]))
				}
				if worst > 1.5 {
					t.Errorf("restart %d: a frame differs in mean luma by %.2f from the run from zero", restart, worst)
				}
			}
		})
	}
}

// frameLuma is the 8-bit mean luma of each frame.
func frameLuma(t *testing.T, file string) []float64 {
	t.Helper()
	out := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-an",
		"-vf", "signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-", "-f", "null", "-")
	var list []float64
	for _, line := range strings.Split(out, "\n") {
		if _, value, ok := strings.Cut(line, "lavfi.signalstats.YAVG="); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, v)
		}
	}
	return list
}
