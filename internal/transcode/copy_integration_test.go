//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"strings"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"

	"jellyscale/internal/testenv"
)

// capturedKeyframes are those of the fixture in compat/captures/transcode-12.4.md.
const capturedKeyframes = "0,3,7,13,18,26,31,39,44,52,57,65,70,78,83"

// The tests read their sources as fast as they can; the one that tests the
// pacing puts it back. The rate is 0, no limit, and not a large number: on
// ffmpeg 9.0 a rate above 5 doesn't mean faster. A 90 s film took 18 s at 5
// every time, 8 to 65 s at 20 and 39 to 53 s at 500, against 0.1 s at 0. With
// twice as many busy loops as cores, the 30 s waits below timed out in 13 of 13
// runs at 500 and none of 16 at 0; with one per core they passed at both.
func init() { copyReadRate = "0" }

// copySource is a 90 second film of H.264 with B-frames and AC3 audio, with
// the captured irregular keyframes.
func copySource(t *testing.T, name string, extra ...string) (path string, source playback.Source) {
	t.Helper()
	return delayedSource(t, name, delays{video: 90, audio: 90}, extra...)
}

// delays say when each stream starts and how long it lasts, in seconds, in a
// film made by delayedSource.
type delays struct{ videoStart, audioStart, video, audio float64 }

// delayedSource is copySource with streams that start late or end early, as
// the index and the scan read them.
func delayedSource(t *testing.T, name string, d delays, extra ...string) (path string, source playback.Source) {
	return codecSource(t, name, h264Fixture, d, extra...)
}

// h264Fixture, hevcFixture and av1Fixture are the video encodes of a test film.
// The H.264 one has the captured irregular keyframes. The others take the
// encoders' defaults for what a real film has: x265 cuts open GOPs (CRA frames
// with leading pictures) and SVT-AV1 forward keyframes, a keyframe every 72
// frames instead of the captured times, because forcing one makes an IDR.
var (
	h264Fixture = []string{"-c:v", "libx264", "-preset", "veryfast", "-x264-params", "bframes=2:scenecut=0", "-force_key_frames", capturedKeyframes}
	hevcFixture = []string{"-c:v", "libx265", "-preset", "veryfast", "-x265-params", "log-level=error:keyint=72"}
	av1Fixture  = []string{"-c:v", "libsvtav1", "-preset", "10", "-g", "72"}
)

// codecSource is delayedSource with the video encoded by video.
func codecSource(t *testing.T, name string, video []string, d delays, extra ...string) (path string, source playback.Source) {
	t.Helper()
	testenv.FFmpeg(t)
	dir := t.TempDir()
	path = filepath.Join(dir, name)
	seconds := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	run(t, "ffmpeg", append([]string{"-nostdin", "-v", "error", "-y",
		"-itsoffset", seconds(d.videoStart), "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001:duration=" + seconds(d.video),
		"-itsoffset", seconds(d.audioStart), "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=" + seconds(d.audio),
		"-map", "0:v", "-map", "1:a"}, slices.Concat(video, []string{"-c:a", "ac3"}, extra, []string{path})...)...)
	container := strings.TrimPrefix(filepath.Ext(name), ".")
	keyframes, err := library.ReadKeyframes(context.Background(), store.NewPosix(dir), name, 1<<40, container)
	if err != nil {
		t.Fatal(err)
	}
	end, err := library.ReadVideoEnd(context.Background(), path, keyframes[len(keyframes)-1])
	if err != nil {
		t.Fatal(err)
	}
	probe, err := library.ProbeInput(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(probe.Streams)
	streams, err := playback.ParseStreams(raw)
	if err != nil {
		t.Fatal(err)
	}
	return path, playback.Source{RunTimeTicks: probe.RunTimeTicks, Keyframes: keyframes, VideoEnd: end, Streams: streams}
}

var copySpec = playback.Spec{AudioIndex: 1, AudioBitrate: 128_000, Copy: true}

// copySegments runs a copied stream from segment start to the end of the file
// and returns the directory holding its segments, as the worker would build them.
func copySegments(t *testing.T, input string, source playback.Source, start int) (string, playback.Layout) {
	t.Helper()
	layout := source.Layout(true)
	dir := t.TempDir()
	video, audio := copyStart(source.Keyframes, layout, start)
	cmd, err := CopyCommand(context.Background(), input, copySpec, source.VideoCodec(), video, audio, dir)
	if err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg from segment %d: %v\n%s", start, err, errOut.String())
	}
	gather := newGatherer(dir, layout, source.Keyframes[0])
	for n := start; n < layout.Count(); n++ {
		if built, err := gather.segment(context.Background(), n, true); err != nil || !built {
			t.Fatalf("segment %d of a run from %d: built %v: %v", n, start, built, err)
		}
	}
	return dir, layout
}

// videoMD5 hashes the video packets as they are in the file, so any change in
// their bytes shows.
func videoMD5(t *testing.T, file string) string {
	t.Helper()
	return run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-map", "0:v:0", "-c", "copy", "-f", "md5", "-")
}

// audioDifference decodes the first audio stream of two files and returns the
// largest difference between their samples, as a fraction of full scale, and
// where in the first it is. Samples after limit seconds are ignored.
func audioDifference(t *testing.T, a, b string, limit float64) (worst, at float64) {
	t.Helper()
	decode := func(file string) []int16 {
		raw := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-map", "0:a:0", "-f", "s16le", "-ac", "1", "-ar", "48000", "-")
		samples := make([]int16, len(raw)/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16([]byte(raw[2*i : 2*i+2])))
		}
		return samples
	}
	x, y := decode(a), decode(b)
	if len(x) != len(y) {
		t.Fatalf("%d audio samples against %d", len(x), len(y))
	}
	for i := range x {
		if time := float64(i) / 48000; time < limit-tsOffset {
			if d := math.Abs(float64(x[i])-float64(y[i])) / 32768; d > worst {
				worst, at = d, time
			}
		}
	}
	return worst, at
}

// A copied stream is cut on the source's keyframes, and a run that begins at
// any segment splices with the segments of a run from the start: the video is
// the same bytes at the same times, and the audio packets are on the same grid
// (the encoder's bytes differ, since it starts warm instead of continuing, and
// audioDifference measures by how much).
func TestCopiedRestartSplicesWithEarlierSegments(t *testing.T) {
	for _, c := range copiedFilms {
		t.Run(c.codec+" "+c.name, func(t *testing.T) {
			input, source := codecSource(t, c.name, c.video, delays{video: 90, audio: 90}, c.extra...)
			if got := source.VideoCodec(); got != c.codec {
				t.Fatalf("the fixture is %s, want %s", got, c.codec)
			}
			whole, layout := copySegments(t, input, source, 0)
			count := layout.Count()
			if c.codec == "h264" && count != 14 {
				t.Fatalf("%d segments for the captured keyframes, want 14 (7 6 5 8 5 8 5 8 5 8 5 8 5 7)", count)
			}
			if count < 10 {
				t.Fatalf("%d segments, want enough to restart at 1, 4, 8 and the last", count)
			}
			// The sample entry is pinned: hev1 and hvc1 differ in the init
			// segment, and the one ffmpeg writes depends on its version and the
			// input's tag, so workers would not splice.
			if init := readFile(t, filepath.Join(whole, "init.mp4")); c.codec == "hevc" && (!bytes.Contains(init, []byte("hvc1")) || bytes.Contains(init, []byte("hev1"))) {
				t.Errorf("the init segment of an HEVC copy is not hvc1")
			}
			full := join(t, filepath.Join(whole, "init.mp4"), segmentFiles(whole, 0, count)...)
			last, _ := layout.Segment(count - 1)
			// Matroska keeps milliseconds, so a 23.976 fps film has decode times that
			// repeat; that is the source's, and a splice must add no warning to it.
			warnings := func(file string) string {
				out := run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", file, "-f", "null", "-")
				return regexp.MustCompile(` @ 0x[0-9a-f]+`).ReplaceAllString(out, "")
			}
			baseline := warnings(full)
			endOfAudio := tsOffset + float64(last)/ticksPerSecond - 1 // The last segment's padded AAC frame is no seam.
			// Restarts at three places, one at the last segment, and a far seek.
			for _, start := range []int{1, 4, 8, count - 1} {
				resumed, _ := copySegments(t, input, source, start)
				if a, b := readFile(t, filepath.Join(whole, "init.mp4")), readFile(t, filepath.Join(resumed, "init.mp4")); !bytes.Equal(a, b) {
					t.Errorf("restart %d: init segments differ", start)
				}
				spliced := join(t, filepath.Join(whole, "init.mp4"),
					append(segmentFiles(whole, 0, start), segmentFiles(resumed, start, count)...)...)
				if a, b := videoMD5(t, full), videoMD5(t, spliced); a != b {
					t.Errorf("restart %d: the video differs from a run from the start: %s against %s", start, a, b)
				}
				compare(t, fmt.Sprintf("restart %d video times", start),
					times(t, full, "v:0", "packet=pts_time"), times(t, spliced, "v:0", "packet=pts_time"), 0)
				compare(t, fmt.Sprintf("restart %d audio", start),
					before(times(t, full, "a:0", "packet=pts_time"), endOfAudio), before(times(t, spliced, "a:0", "packet=pts_time"), endOfAudio), 0.0005)
				// The encoder starts warm at a restart, so the audio's bytes differ.
				// Decoded, the difference is bounded: 0.31% to 0.36% of full
				// scale (-49 dB) on the fixture's tone across ffmpeg 6.1, 8.1 and 9.0.
				worst, at := audioDifference(t, full, spliced, endOfAudio)
				t.Logf("restart %d: the decoded audio differs by at most %.4f of full scale, at %.2f s", start, worst, at)
				if worst > 0.01 {
					t.Errorf("restart %d: the decoded audio differs by %.4f of full scale at %.2f s", start, worst, at)
				}
				if got := warnings(spliced); got != baseline {
					t.Errorf("restart %d: decoder warnings differ from a run from the start:\n%s\nagainst\n%s", start, got, baseline)
				}
			}
		})
	}
}

// copiedFilms are the films whose copies must splice: each codec in each
// container. The HEVC MP4 carries the hev1 tag, which a copy would keep without
// the pin.
var copiedFilms = []struct {
	name, codec string
	video       []string
	extra       []string
}{
	{"film.mkv", "h264", h264Fixture, nil},
	{"film.mp4", "h264", h264Fixture, nil},
	{"film.mkv", "hevc", hevcFixture, nil},
	{"film.mp4", "hevc", hevcFixture, []string{"-tag:v", "hev1"}},
	{"film.mkv", "av1", av1Fixture, nil},
	{"film.mp4", "av1", av1Fixture, nil},
}

// However the streams are placed in time, the playlist's last entry is the
// length of the real last segment, to within a frame: neither the container's
// duration, nor its tags, nor the audio, say where the video ends.
func TestCopiedPlaylistEndsWhereTheVideoDoes(t *testing.T) {
	const frame = 417_000 // One frame at 23.976 fps, in ticks.
	for _, name := range []string{"film.mkv", "film.mp4"} {
		for label, d := range map[string]delays{
			"video late":               {videoStart: 0.5, video: 90, audio: 90},
			"audio late":               {audioStart: 0.5, video: 90, audio: 90},
			"everything late":          {videoStart: 0.5, audioStart: 0.5, video: 90, audio: 90},
			"video shorter than audio": {videoStart: 1.5, audioStart: 1.5, video: 80, audio: 90},
		} {
			t.Run(name+", "+label, func(t *testing.T) {
				input, source := delayedSource(t, name, d)
				dir, layout := copySegments(t, input, source, 0)
				last := layout.Count() - 1
				_, length := layout.Segment(last)
				// The segment as ffmpeg wrote it: from its keyframe to the end
				// of its last frame.
				pts := times(t, join(t, filepath.Join(dir, "init.mp4"), segmentFiles(dir, last, last+1)...), "v:0", "packet=pts_time")
				real := int64((pts[len(pts)-1] - pts[0] + 1001.0/24000) * ticksPerSecond)
				if diff := length - real; diff < -frame || diff > frame {
					t.Errorf("the last playlist entry is %.3f s, the segment is %.3f s", float64(length)/ticksPerSecond, float64(real)/ticksPerSecond)
				}
			})
		}
	}
}

// A copy reads at 5 times the film's speed, not as fast as the disk allows: in
// three seconds it copies about 15 s of film, not the whole of it. Nothing
// limits how far it gets ahead of a viewer.
func TestCopyReadsAtFiveTimesTheFilmsSpeed(t *testing.T) {
	input, source := copySource(t, "film.mkv")
	copyReadRate = "5"
	defer func() { copyReadRate = "0" }()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd, err := CopyCommand(ctx, input, copySpec, source.VideoCodec(), -1, -1, dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Run() //nolint:errcheck // Killed by the timeout.
	pieces, err := newGatherer(dir, source.Layout(true), source.Keyframes[0]).pieces(context.Background())
	if err != nil || len(pieces) == 0 {
		t.Fatalf("%d fragments: %v", len(pieces), err)
	}
	// 3 s at 5x is 15 s of film, which is 5 of the fixture's 15 keyframes.
	if done := pieces[len(pieces)-1].end; done > 40*ticksPerSecond {
		t.Errorf("ffmpeg copied %.0f s of film in 3 s", float64(done)/ticksPerSecond)
	}
}

// Every entry of the index of an HEVC or AV1 film is a packet the encoder
// marked as a random access point (an IDR or CRA, a key frame), and a decoder
// that starts at it decodes without error. The fixtures are made with the
// encoders' defaults, which cut open GOPs: a CRA has leading pictures that
// refer to the group before it, and an AV1 forward keyframe follows frames
// that are shown after it.
func TestIndexedKeyframesOfOpenGOPFilmsAreRandomAccessPoints(t *testing.T) {
	for _, c := range copiedFilms {
		if c.codec == "h264" {
			continue
		}
		t.Run(c.codec+" "+c.name, func(t *testing.T) {
			input, source := codecSource(t, c.name, c.video, delays{video: 90, audio: 90}, c.extra...)
			if len(source.Keyframes) < 10 {
				t.Fatalf("%d keyframes in the index", len(source.Keyframes))
			}
			// Packets whose flags say key frame.
			out := run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", input)
			var key []float64
			for _, line := range strings.Fields(out) {
				if time, flags, ok := strings.Cut(line, ","); ok && strings.HasPrefix(flags, "K") {
					v, _ := strconv.ParseFloat(time, 64)
					key = append(key, v)
				}
			}
			for _, k := range source.Keyframes {
				at := float64(k) / ticksPerSecond
				if !slices.ContainsFunc(key, func(v float64) bool { return math.Abs(v-at) < 0.002 }) {
					t.Errorf("the index lists %.3f s, which is not a key packet (key packets: %.3f)", at, key)
				}
			}
			// Decoding from each cut: -ss seeks to the keyframe at or before
			// the time, as the copy's run for a segment does.
			for _, k := range source.Keyframes[1:min(len(source.Keyframes), 6)] {
				at := strconv.FormatFloat(float64(k)/ticksPerSecond+0.005, 'f', 3, 64)
				if out := run(t, "ffmpeg", "-nostdin", "-v", "error", "-ss", at, "-i", input, "-map", "0:v:0", "-frames:v", "48", "-f", "null", "-"); strings.TrimSpace(out) != "" {
					t.Errorf("decoding from the keyframe at %s s: %s", at, out)
				}
			}
		})
	}
}
