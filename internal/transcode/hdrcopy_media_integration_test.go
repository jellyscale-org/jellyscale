//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// scanned reads a film as the scan would store it, for a copy.
func scanned(t *testing.T, path, container string) playback.Source {
	t.Helper()
	ctx := context.Background()
	keyframes, err := library.ReadKeyframes(ctx, store.NewPosix(filepath.Dir(path)), filepath.Base(path), 1<<40, container)
	if err != nil {
		t.Fatal(err)
	}
	end, err := library.ReadVideoEnd(ctx, path, keyframes[len(keyframes)-1])
	if err != nil {
		t.Fatal(err)
	}
	probe, err := library.ProbeInput(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(probe.Streams)
	streams, err := playback.ParseStreams(raw)
	if err != nil {
		t.Fatal(err)
	}
	return playback.Source{Container: container, RunTimeTicks: probe.RunTimeTicks, Keyframes: keyframes, VideoEnd: end, Streams: streams}
}

// firstTwoCPUs is a CPU list of two cores this process may run on. A machine
// with fewer fails the test: a timing on one core says nothing about two.
func firstTwoCPUs(t *testing.T) string {
	t.Helper()
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Fatal(err)
	}
	var cpus []string
	for cpu := 0; cpu < 1024 && len(cpus) < 2; cpu++ {
		if set.IsSet(cpu) {
			cpus = append(cpus, strconv.Itoa(cpu))
		}
	}
	if len(cpus) < 2 {
		t.Fatalf("this process may run on %d CPUs, the test needs two", len(cpus))
	}
	return strings.Join(cpus, ",")
}

// A 4K HDR10 film with 5.1 TrueHD, which no browser plays, is copied by the
// worker's own ffmpeg path on two CPUs far faster than it plays (JS-025 measured
// 30 s in 0.41 s on four; a viewer's first segment waits on this), with the
// picture hvc1 and PQ and the sound stereo AAC. ffmpeg is held to two CPUs with
// taskset, as a small worker has. The read rate is the tests' unlimited one:
// production's 5x would cap the speed at 5x whatever the CPUs can do, and what
// is measured here is whether they can keep up with it.
func TestCopyOf4KHDR10WithTrueHDRunsWellAboveRealTimeOnTwoCPUs(t *testing.T) {
	testenv.FFmpeg(t)
	cpus := firstTwoCPUs(t)
	const filmSeconds = 20
	dir := t.TempDir()
	input := filepath.Join(dir, "film.mkv")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=20",
		"-map", "0:v", "-map", "1:a", "-pix_fmt", "yuv420p10le",
		"-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "keyint=48:log-level=error:"+"colorprim=bt2020:colormatrix=bt2020nc:transfer=smpte2084:hdr10=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1):max-cll=1000,400",
		"-ac", "6", "-c:a", "truehd", "-strict", "-2", input)
	source := scanned(t, input, "mkv")
	audio := source.Streams[1]
	if audio.Codec != "truehd" || audio.Channels != 6 {
		t.Fatalf("the source's sound is %s with %d channels, so this test proves nothing", audio.Codec, audio.Channels)
	}
	if got := source.Streams[0].VideoRangeType; got != "HDR10" || source.Streams[0].Width != 3840 {
		t.Fatalf("the scan labels the picture %q, %d wide", got, source.Streams[0].Width)
	}
	if !source.CanCopy() {
		t.Fatal("CanCopy is false for 4K HDR10")
	}

	layout := source.Layout(true)
	out := t.TempDir()
	video, audioStart := copyStart(source.Keyframes, layout, 0)
	command, err := CopyCommand(context.Background(), input, copySpec, source.VideoCodec(), video, audioStart, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(command.Args, " "), "-c:v copy") {
		t.Fatalf("the video is not copied: %v", command.Args)
	}
	pinned := exec.Command("taskset", append([]string{"-c", cpus}, command.Args...)...)
	var errOut bytes.Buffer
	pinned.Stderr = &errOut
	began := time.Now()
	if err := pinned.Run(); err != nil {
		t.Fatalf("ffmpeg on CPUs %s: %v\n%s", cpus, err, errOut.String())
	}
	took := time.Since(began)
	// 10x real time. The lead measured 73x on four CPUs; a CI core is slower
	// and shared, so the bar leaves a wide margin and still fails a copy that
	// decodes the picture or the TrueHD on the video's critical path.
	if limit := filmSeconds * time.Second / 10; took > limit {
		t.Errorf("copying %d s of 4K took %s on CPUs %s, over %s (10x real time)", filmSeconds, took, cpus, limit)
	}
	t.Logf("copied %d s of 4K HDR10 with TrueHD in %s on CPUs %s (%.0fx real time)", filmSeconds, took, cpus, filmSeconds/took.Seconds())

	gather := newGatherer(out, layout, source.Keyframes[0])
	for n := 0; n < layout.Count(); n++ {
		if built, err := gather.segment(context.Background(), n, true); err != nil || !built {
			t.Fatalf("segment %d: built %v: %v", n, built, err)
		}
	}
	init, err := os.ReadFile(filepath.Join(out, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(init, []byte("hvc1")) || bytes.Contains(init, []byte("hev1")) {
		t.Error("the sample entry is not hvc1")
	}
	segment, err := os.ReadFile(filepath.Join(out, "0.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	probed, err := ffprobeFrom(context.Background(), bytes.NewReader(append(init, segment...)),
		"-show_entries", "stream=codec_name,pix_fmt,width,channels,color_transfer,color_primaries", "-of", "default=nw=1", "-")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codec_name=hevc", "width=3840", "pix_fmt=yuv420p10le", "color_transfer=smpte2084", "color_primaries=bt2020", "codec_name=aac", "channels=2"} {
		if !strings.Contains(string(probed), want) {
			t.Errorf("the copy lacks %s:\n%s", want, probed)
		}
	}
	if strings.Contains(string(probed), "truehd") {
		t.Errorf("the TrueHD was copied:\n%s", probed)
	}
	text := playback.Master(playback.Spec{Copy: true, Width: 3840, Height: 2160, VideoBitrate: 20_000_000, AudioBitrate: 128_000}, source, "q")
	first := strings.Split(strings.SplitN(text, "#EXT-X-STREAM-INF:", 2)[1], "\n")[0]
	if !strings.Contains(first, "VIDEO-RANGE=PQ,") || !regexp.MustCompile(`CODECS="hvc1\.2\.\d+\.L\d+\.B0,mp4a\.40\.2"`).MatchString(first) {
		t.Errorf("master: %s", first)
	}
}

// colourOf is the colour box (colr, nclx) of the first sample entry in an init
// segment: the primaries, transfer and matrix a player reads before the first
// frame.
func colourOf(t *testing.T, init []byte) (primaries, transfer, matrix uint16) {
	t.Helper()
	at := bytes.Index(init, []byte("colrnclx"))
	if at < 0 {
		t.Fatal("the init segment has no colr box of type nclx")
	}
	body := init[at+8:]
	return binary.BigEndian.Uint16(body), binary.BigEndian.Uint16(body[2:]), binary.BigEndian.Uint16(body[4:])
}

// An AV1 HDR10 film is copied through the worker's own ffmpeg path with its
// picture still PQ: the init segment's colour box and the stream say SMPTE 2084
// on BT.2020, the sample entry is av01, and the master playlist names PQ and the
// AV1 codec string. (CI's Chrome plays AV1, so this is the HDR copy it can show;
// compat/captures/hdr-copy-12.4.md has Jellyfin's master for the same range.)
func TestCopyOfAV1HDR10KeepsPQInTheInitSegmentAndTheMaster(t *testing.T) {
	input, source := hdrSource(t, testenv.HDRFilm{Base: "av1", Seconds: 12})
	if got := source.Streams[0].VideoRangeType; got != "HDR10" || source.Streams[0].Codec != "av1" {
		t.Fatalf("the scan labels it %s %q, so this test proves nothing", source.Streams[0].Codec, got)
	}
	if !source.CanCopy() {
		t.Fatal("CanCopy is false for AV1 HDR10")
	}
	dir, layout := copySegments(t, input, source, 0)
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(init, []byte("av01")) {
		t.Error("the sample entry is not av01")
	}
	if primaries, transfer, matrix := colourOf(t, init); primaries != 9 || transfer != 16 || matrix != 9 {
		t.Errorf("the colr box says primaries %d, transfer %d, matrix %d, want 9, 16, 9 (BT.2020, PQ)", primaries, transfer, matrix)
	}
	segment, err := os.ReadFile(filepath.Join(dir, "0.m4s"))
	if err != nil {
		t.Fatal(err)
	}
	probed, err := ffprobeFrom(context.Background(), bytes.NewReader(append(init, segment...)), "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,pix_fmt,color_transfer,color_primaries", "-of", "default=nw=1", "-")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codec_name=av1", "pix_fmt=yuv420p10le", "color_transfer=smpte2084", "color_primaries=bt2020"} {
		if !strings.Contains(string(probed), want) {
			t.Errorf("the copied picture lacks %s:\n%s", want, probed)
		}
	}
	for _, box := range []string{"dvcC", "dvvC"} {
		if bytes.Contains(init, []byte(box)) {
			t.Errorf("the init segment carries a %s box", box)
		}
	}
	if layout.Count() < 2 {
		t.Fatalf("%d segments", layout.Count())
	}
	text := playback.Master(playback.Spec{Copy: true, Width: 640, Height: 360, VideoBitrate: 4_000_000, AudioBitrate: 128_000}, source, "q")
	first := strings.Split(strings.SplitN(text, "#EXT-X-STREAM-INF:", 2)[1], "\n")[0]
	if !strings.Contains(first, "VIDEO-RANGE=PQ,") || !regexp.MustCompile(`CODECS="av01\.0\.\d+M\.10,mp4a\.40\.2"`).MatchString(first) {
		t.Errorf("master: %s", first)
	}
}
