//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-133: the fallback of an AV1 copy whose keyframe index was found wrong is an AV1 encode (SVT-AV1) that splices on the
// playlist's layout, whichever worker made which segment. These checks make the film, the source and the spec as the
// worker's own path does, and name nothing the build adds beyond Spec.Recode "av1" and the codec name its command takes.

// av1Keyframes are the keyframes of the film (seconds): gaps of 5, 6, 5, 7, 6, 7, 5, 6, 6 and a film of 60 s, so its copy's
// layout differs from the 4 s grid of an encode in count and in every start after the first.
var av1Keyframes = []int{0, 5, 11, 16, 23, 29, 36, 41, 47, 53, 60}

// av1Film writes, into the shared film directory, an AV1 film at rate (a frame rate as ffmpeg writes it) with AC3
// audio, and returns its file name. SVT-AV1 in ffmpeg 6.1 ignores -force_key_frames, so the picture is encoded in
// pieces that each start at a keyframe time and joined (test/fixtures/e2e.sh does the same).
func av1Film(t *testing.T, name, rate string) string {
	t.Helper()
	testenv.FFmpeg(t)
	frameAt := func(seconds int) int { // The first frame at or after the time.
		num, den := 25, 1
		if rate == "24000/1001" {
			num, den = 24000, 1001
		}
		return (seconds*num + den - 1) / den
	}
	pieces := t.TempDir()
	var list strings.Builder
	for i := 0; i+1 < len(av1Keyframes); i++ {
		piece := filepath.Join(pieces, strconv.Itoa(i)+".mkv")
		run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate="+rate+":duration=60",
			"-vf", fmt.Sprintf("trim=start_frame=%d:end_frame=%d,setpts=PTS-STARTPTS", frameAt(av1Keyframes[i]), frameAt(av1Keyframes[i+1])),
			"-c:v", "libsvtav1", "-preset", "10", "-g", "2000", "-pix_fmt", "yuv420p", piece)
		fmt.Fprintf(&list, "file '%s'\n", piece)
	}
	if err := os.WriteFile(filepath.Join(pieces, "list"), []byte(list.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "concat", "-safe", "0", "-i", filepath.Join(pieces, "list"),
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60", "-map", "0:v", "-map", "1:a", "-c:v", "copy", "-c:a", "ac3",
		filepath.Join(film(t), name))
	return name
}

// scanAV1Source is the film as the scan reads it: its index, the video's end and its streams.
func scanAV1Source(t *testing.T, name string) (path string, source playback.Source) {
	t.Helper()
	path = filepath.Join(film(t), name)
	keyframes, err := library.ReadKeyframes(context.Background(), store.NewPosix(film(t)), name, 1<<40, "mkv")
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

// av1Source is scanAV1Source for the film av1Film makes, whose copy layout differs from the 4 s grid.
func av1Source(t *testing.T, name string) (path string, source playback.Source) {
	t.Helper()
	path, source = scanAV1Source(t, name)
	if len(source.Keyframes) != len(av1Keyframes)-1 {
		t.Fatalf("the scan read %d keyframes of the film, want the %d it was made with: the fixture does not hold its keyframes", len(source.Keyframes), len(av1Keyframes)-1)
	}
	layout, grid := source.Layout(true), playback.EncodedLayout(source.Ticks())
	if layout.Count() != len(av1Keyframes)-1 || layout.Count() == grid.Count() {
		t.Fatalf("the copy's layout has %d segments, the encode's grid %d: the film must differ in count", layout.Count(), grid.Count())
	}
	for n := 1; n < min(layout.Count(), grid.Count()); n++ {
		if layout.Starts[n] == grid.Starts[n] {
			t.Fatalf("segment %d starts where the 4 s grid's does: the film must differ in every start after the first", n)
		}
	}
	return path, source
}

// ignoreForcedKeyFrames puts in front of ffmpeg a shim that drops -force_key_frames and its value from each command that
// encodes with libsvtav1. That is what SVT-AV1 in Ubuntu 24.04's ffmpeg 6.1 does (it puts a keyframe at 0 and nowhere
// else, so the hls muxer writes one fragment), and CI runs that ffmpeg: with the shim a build that cuts the fallback on
// the playlist's layout by forcing keyframes fails on any ffmpeg, here as it fails in the merge queue. A build that
// cuts each piece with an ffmpeg of its own (each piece starts on a keyframe on any ffmpeg) passes on both. Call it
// before slowAt, which wraps whatever ffmpeg it finds.
func ignoreForcedKeyFrames(t *testing.T) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := `#!/bin/sh
case " $* " in *libsvtav1*) ;; *) exec ` + real + ` "$@" ;; esac
n=$#
skip=
for a in "$@"; do
  if [ -n "$skip" ]; then skip=; continue; fi
  if [ "$a" = -force_key_frames ]; then skip=1; continue; fi
  set -- "$@" "$a"
done
shift $n
exec ` + real + ` "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// av1Fallback is the spec of the encode that stands in for the AV1 copy of the film.
func av1Fallback(base playback.Spec) playback.Spec {
	base.AudioIndex, base.AudioBitrate, base.Width, base.Height, base.Copy = 1, 128_000, 640, 360, true
	return base.Recoded("av1", "")
}

// recodeSegments is the fallback of the film name as a viewer who asks for segments from segment start to the end of
// the film, in order, gets it: a rig of its own with a real worker and a store of its own, so the first request starts
// the job at segment start as a restart does. Whatever commands the worker runs (one, or one for each segment) is its
// business; the check reads what it stored. The directory it returns holds init.mp4 and n.m4s for each segment made.
func recodeSegments(t *testing.T, name string, start int) (string, playback.Layout) {
	t.Helper()
	_, source := av1Source(t, name)
	count := source.Layout(true).Count()
	order := make([]int, 0, count-start)
	for n := start; n < count; n++ {
		order = append(order, n)
	}
	return fallbackRun(t, name, order)
}

// fallbackRun fetches the segments in order from a worker and returns where the segments the viewer was given are,
// copied under the names join and segmentFiles use.
func fallbackRun(t *testing.T, name string, order []int) (string, playback.Layout) {
	t.Helper()
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	_, source := r.av1Fallback(t, name)
	layout := source.Layout(true)
	stop := r.worker(t, nil)
	j := r.jobs(t)
	for _, n := range order {
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d of the fallback, asked in the order %v: %v", n, order, err)
		}
	}
	if _, err := r.fetchAt(t, j, psid(0), -1, order[0]); err != nil {
		t.Fatalf("the fallback's init segment: %v", err)
	}
	stop() // The run's job is over before the next run's rig truncates the tables.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "init.mp4"), readFile(t, filepath.Join(segments, SegmentKey(r.hash, -1))), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range order {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.m4s", n)), readFile(t, filepath.Join(segments, SegmentKey(r.hash, n))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, layout
}

// A fallback is cut on the playlist's layout whichever worker made which segment: asked from the start, and asked the
// bad segment first and the ones before it afterwards (a second run, which starts at segment 0, joins the first's), every
// segment starts on a keyframe, starts within 0.05 s of the playlist's start and lasts within 0.1 s of its EXTINF, holds
// the frames of its length to within 1, and the segments together hold the film's frames, none skipped or played twice.
// The shim makes SVT-AV1 ignore -force_key_frames as it does in ffmpeg 6.1 (see ignoreForcedKeyFrames).
func TestAnAV1FallbackSegmentsKeepThePlaylistsLayoutInEveryOrder(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, rate := range []string{"24000/1001", "25"} {
		name := av1Film(t, "layout-"+strings.ReplaceAll(rate, "/", "-")+".mkv", rate)
		input, source := av1Source(t, name)
		layout := source.Layout(true)
		count := layout.Count()
		fps := 25.0
		if rate == "24000/1001" {
			fps = 24000.0 / 1001
		}
		filmFrames := len(times(t, input, "v:0", "frame=pts_time"))
		bad := count / 2
		orders := map[string][]int{"from the start": nil, "the bad segment first": nil}
		for n := range count {
			orders["from the start"] = append(orders["from the start"], n)
		}
		for n := bad; n < count; n++ {
			orders["the bad segment first"] = append(orders["the bad segment first"], n)
		}
		for n := range bad {
			orders["the bad segment first"] = append(orders["the bad segment first"], n)
		}
		for label, order := range orders {
			t.Run(rate+" fps, "+label, func(t *testing.T) {
				dir, _ := fallbackRun(t, name, order)
				init := filepath.Join(dir, "init.mp4")
				total := 0
				for n := range count {
					file := join(t, init, segmentFiles(dir, n, n+1)...)
					start, length := layout.Segment(n)
					packets := strings.Fields(run(t, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", file))
					if len(packets) == 0 {
						t.Errorf("segment %d holds no video", n)
						continue
					}
					if !strings.Contains(strings.SplitN(packets[0], ",", 2)[1], "K") {
						t.Errorf("segment %d does not start on a keyframe: %s", n, packets[0])
					}
					pts := times(t, file, "v:0", "frame=pts_time")
					total += len(pts)
					want := tsOffset + float64(source.Keyframes[0]+start)/ticksPerSecond
					if d := pts[0] - want; math.Abs(d) > 0.05 {
						t.Errorf("segment %d starts %.3f s from the playlist's start", n, d)
					}
					if d := pts[len(pts)-1] + 1/fps - pts[0] - float64(length)/ticksPerSecond; math.Abs(d) > 0.1 {
						t.Errorf("segment %d lasts %.3f s from its EXTINF", n, d)
					}
					if d := float64(len(pts)) - float64(length)/ticksPerSecond*fps; math.Abs(d) > 1.0001 {
						t.Errorf("segment %d holds %d frames, %.1f from its length's", n, len(pts), d)
					}
				}
				if total != filmFrames {
					t.Errorf("the segments hold %d frames, the film %d: some were skipped or played twice", total, filmFrames)
				}
			})
		}
	}
}

// A fallback run that begins at segment 1, at a middle segment and at the last one, as a restart begins, splices with
// the earlier segments of a run from the start: the same frames at the same times (within 1 ms), the audio packets on
// the same grid (within 0.5 ms), no decoder warning the run from the start does not have across the join, and the same
// init segment, at 24000/1001 and 25 fps. Run it under test/ffmpeg61.sh for Ubuntu 24.04's ffmpeg 6.1 (the criterion
// names it and ffmpeg 9): its SVT-AV1 ignores -force_key_frames, which a build that relies on it fails here on any
// ffmpeg, through the shim of ignoreForcedKeyFrames.
func TestAnAV1FallbackRestartSplicesWithEarlierSegments(t *testing.T) {
	ignoreForcedKeyFrames(t)
	for _, rate := range []string{"24000/1001", "25"} {
		t.Run(rate+" fps", func(t *testing.T) {
			name := av1Film(t, "restart-"+strings.ReplaceAll(rate, "/", "-")+".mkv", rate)
			whole, layout := recodeSegments(t, name, 0)
			count := layout.Count()
			full := join(t, filepath.Join(whole, "init.mp4"), segmentFiles(whole, 0, count)...)
			if init := readFile(t, filepath.Join(whole, "init.mp4")); !bytes.Contains(init, []byte("av01")) {
				t.Errorf("the init segment of the fallback of an AV1 copy is not AV1's")
			}
			last, _ := layout.Segment(count - 1)
			warnings := func(file string) string {
				out := run(t, "ffmpeg", "-nostdin", "-v", "warning", "-i", file, "-f", "null", "-")
				return regexp.MustCompile(` @ 0x[0-9a-f]+`).ReplaceAllString(out, "")
			}
			baseline := warnings(full)
			endOfAudio := tsOffset + float64(last)/ticksPerSecond - 1 // The last segment's padded AAC frame is no seam.
			for _, start := range []int{1, count / 2, count - 1} {
				resumed, _ := recodeSegments(t, name, start)
				if a, b := readFile(t, filepath.Join(whole, "init.mp4")), readFile(t, filepath.Join(resumed, "init.mp4")); !bytes.Equal(a, b) {
					t.Errorf("restart %d: init segments differ", start)
				}
				spliced := join(t, filepath.Join(whole, "init.mp4"), append(segmentFiles(whole, 0, start), segmentFiles(resumed, start, count)...)...)
				compare(t, fmt.Sprintf("restart %d video times (and their number: no frame skipped or played twice)", start),
					times(t, full, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
				compare(t, fmt.Sprintf("restart %d audio packet times", start),
					before(times(t, full, "a:0", "packet=pts_time"), endOfAudio), before(times(t, spliced, "a:0", "packet=pts_time"), endOfAudio), 0.0005)
				if got := warnings(spliced); got != baseline {
					t.Errorf("restart %d: decoder warnings differ from a run from the start:\n%s\nagainst\n%s", start, got, baseline)
				}
			}
		})
	}
}

// fallbackRig is a rig whose film is the AV1 copy after its index was found wrong: the index is in
// dropped_keyframes, the spec is the AV1 fallback of the copy.
func (r *rig) av1Fallback(t *testing.T, name string) (input string, source playback.Source) {
	t.Helper()
	input, source = scanAV1Source(t, name)
	r.useFile(t, name)
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = '{}', dropped_keyframes = $1, video_end = $2, size = $3",
		source.Keyframes, source.VideoEnd, mustSize(t, input)); err != nil {
		t.Fatal(err)
	}
	r.spec = av1Fallback(r.spec)
	r.hash = r.spec.Hash()
	return input, source
}

// A worker killed in the middle of an AV1 fallback is replaced by another, which makes the rest from the first missing
// segment, and the request that waited is answered with the segment, not an error (invariant 22). The segments of the
// two workers join as a run from the start's do: the init segment's bytes, every frame and audio packet at its time.
func TestAKilledWorkerMakingAnAV1FallbackIsReplacedAndTheSegmentsJoin(t *testing.T) {
	ignoreForcedKeyFrames(t)
	name := av1Film(t, "failover.mkv", "24000/1001")
	reference, layout := recodeSegments(t, name, 0) // Before this test's rig: a rig truncates the tables of the one before.
	count := layout.Count()
	segments := t.TempDir()
	r := newRig(t, store.NewPosix(segments))
	r.av1Fallback(t, name)
	ffmpeg := testenv.FFmpeg(t) // Before the shim, which would slow the check of the result.
	slowAt(t, "2")
	a := r.workerProcess(t, "worker-a", segments)
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if got := r.owner(t); got != "worker-a" {
		t.Fatalf("the job is worker-a's, not %q", got)
	}
	r.workerProcess(t, "worker-b", segments)
	owned := r.watchOwners(t)
	for n := 1; n < count; n++ {
		if n == 3 {
			a.Process.Kill() //nolint:errcheck // The viewer has segment 2 and waits for 3, which worker-a is making.
		}
		if _, err := r.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d: the request that waited was answered with an error: %v", n, err)
		}
	}
	if got := r.owner(t); got != "worker-b" {
		t.Errorf("the job is %q's, want worker-b's", got)
	}
	owned.check(t)
	stored := t.TempDir()
	for n := range count {
		body, err := os.ReadFile(filepath.Join(segments, SegmentKey(r.hash, n)))
		if err != nil {
			t.Fatalf("segment %d was not stored: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(stored, fmt.Sprintf("%d.m4s", n)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	init := readFile(t, filepath.Join(segments, SegmentKey(r.hash, -1)))
	if !bytes.Equal(init, readFile(t, filepath.Join(reference, "init.mp4"))) {
		t.Error("the init segment differs from an uninterrupted run's")
	}
	initFile := filepath.Join(stored, "init.mp4")
	if err := os.WriteFile(initFile, init, 0o644); err != nil {
		t.Fatal(err)
	}
	whole := join(t, filepath.Join(reference, "init.mp4"), segmentFiles(reference, 0, count)...)
	spliced := join(t, initFile, segmentFiles(stored, 0, count)...)
	compare(t, "video", times(t, whole, "v:0", "frame=pts_time"), times(t, spliced, "v:0", "frame=pts_time"), 0.001)
	last, _ := layout.Segment(count - 1)
	end := tsOffset + float64(last)/ticksPerSecond - 1
	compare(t, "audio", before(times(t, whole, "a:0", "packet=pts_time"), end), before(times(t, spliced, "a:0", "packet=pts_time"), end), 0.0005)
	if warnings := bytes.TrimSpace([]byte(run(t, ffmpeg, "-nostdin", "-v", "warning", "-i", spliced, "-f", "null", "-"))); len(warnings) > 0 {
		t.Errorf("decoder warnings:\n%s", warnings)
	}
}

// A job of the AV1 fallback records the OutputVersion of the replica that queued it, which is above the release before's
// (5): that release's worker claims only jobs of its own version, so it never takes this one (invariant 59).
func TestAnAV1FallbackJobIsQueuedAtAnOutputVersionTheReleaseBeforeDoesNotMake(t *testing.T) {
	r := newRig(t, nil)
	r.spec = av1Fallback(r.spec)
	r.hash = r.spec.Hash()
	id, _, err := r.jobs(t).ensure(context.Background(), r.spec, r.hash, psid(0), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.versionOf(t, id); got <= 5 || got != playback.OutputVersion {
		t.Errorf("the AV1 fallback's job is queued at output version %d, want the replica's %d, above the release before's 5", got, playback.OutputVersion)
	}
}

// The price of the fallback is measured with the commands the worker runs for it, whichever they are: the CPU time of
// the ffmpeg processes it ran (user plus system, from the test process's children) for each second of film, for a
// 1080p and a 4K 24 fps AV1 source, logged beside what Cost says; docs/transcoding.md's rows must cover it
// (internal/playback's TestTheAV1FallbacksPriceCoversTheCPUItsFfmpegUses). It asserts only that the fallback runs on
// the sources the rows name and encodes with SVT-AV1: what a CPU takes depends on the machine a check runs on, so the
// figures are logged, to be copied into the docs by whoever measures them.
func TestTheAV1FallbacksCommandsAreMeasuredOnA1080pAndA4KSource(t *testing.T) {
	testenv.FFmpeg(t)
	const seconds = 6
	for _, size := range []struct {
		name          string
		width, height int
	}{{"1080p", 1920, 1080}, {"4k", 3840, 2160}} {
		t.Run(size.name, func(t *testing.T) {
			name := size.name + "-av1.mkv"
			run(t, "ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=24:duration=%d", size.width, size.height, seconds),
				"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=48000:duration=%d", seconds), "-map", "0:v", "-map", "1:a",
				"-c:v", "libsvtav1", "-preset", "10", "-g", "2000", "-pix_fmt", "yuv420p", "-c:a", "ac3", filepath.Join(film(t), name))
			ffmpegLog := filepath.Join(t.TempDir(), "ffmpeg.log")
			real := testenv.FFmpeg(t)
			shim := t.TempDir()
			if err := os.WriteFile(filepath.Join(shim, "ffmpeg"), []byte("#!/bin/sh\necho \"$*\" >>"+ffmpegLog+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			r := newRig(t, nil)
			r.av1Fallback(t, name)
			r.spec = playback.Spec{Item: r.spec.Item, Source: r.spec.Source, ETag: r.spec.ETag, AudioIndex: 1, AudioBitrate: 128_000,
				Width: size.width, Height: size.height, Copy: true}.Recoded("av1", "")
			r.hash = r.spec.Hash()
			stop := r.worker(t, nil)
			j := r.jobs(t)
			var before, after syscall.Rusage
			syscall.Getrusage(syscall.RUSAGE_CHILDREN, &before) //nolint:errcheck
			began := time.Now()
			layout := r.layoutOf(t, name)
			for n := range layout.Count() {
				if _, err := r.fetch(t, j, psid(0), n); err != nil {
					t.Fatalf("segment %d: %v", n, err)
				}
			}
			wall := time.Since(began)
			stop()
			syscall.Getrusage(syscall.RUSAGE_CHILDREN, &after) //nolint:errcheck
			cpu := time.Duration(after.Utime.Nano()-before.Utime.Nano()) + time.Duration(after.Stime.Nano()-before.Stime.Nano())
			t.Logf("AV1 fallback row: %s sdr cpu %.2f s/s wall %.2f s/s (the price is %.2f cores; %d CPUs, %s)",
				size.name, cpu.Seconds()/seconds, wall.Seconds()/seconds, r.spec.Cost(), numCPU(), ffmpegVersion(t))
			if log := readFile(t, ffmpegLog); !strings.Contains(string(log), "libsvtav1") {
				t.Errorf("no ffmpeg of the fallback encoded with SVT-AV1:\n%s", log)
			}
		})
	}
}

// layoutOf is the copy layout of the film as the rig's fallback was set up with.
func (r *rig) layoutOf(t *testing.T, name string) playback.Layout {
	t.Helper()
	_, source := scanAV1Source(t, name)
	return source.Layout(true)
}

func numCPU() int { return int(math.Max(1, float64(len(cpuIDs())))) }

func cpuIDs() []string {
	raw, _ := os.ReadFile("/proc/cpuinfo")
	return regexp.MustCompile(`(?m)^processor\s*:`).FindAllString(string(raw), -1)
}

func ffmpegVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-version").Output()
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return first
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
