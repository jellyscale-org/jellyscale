package transcode

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/library"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
)

var updateGolden = flag.Bool("update-command-golden", false, "rewrite testdata/command.golden from the code under test")

type goldenCase struct {
	name  string
	p     probes
	spec  playback.Spec
	start int
	ass   string
	image *playback.Stream
}

var (
	// A 24000/1001 source at 0 and one at 7.5 s, which is not a whole number of
	// frames; their restart at segment 5 starts at 20 s (+7.5 s) on the clock.
	atZero  = probes{start: "0.000000", fps: "24000/1001", first: "20.020000", later: "20.061708", grid: "-1024"}
	shifted = probes{start: "7.500000", fps: "24000/1001", first: "27.520000", later: "27.561708", grid: "358976"}
	early   = probes{start: "-0.500000", fps: "24000/1001", first: "20.020000", later: "20.061708", grid: "-25024"}
	// The same sources restarted at segment 1, whose first frame is 4 s in.
	atZero1  = probes{start: "0.000000", fps: "24000/1001", first: "4.004000", later: "4.045708", grid: "-1024"}
	shifted1 = probes{start: "7.500000", fps: "24000/1001", first: "11.504000", later: "11.545708", grid: "358976"}
	early1   = probes{start: "-0.500000", fps: "24000/1001", first: "4.004000", later: "4.045708", grid: "-25024"}

	base    = playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 640, Height: 360}
	copied  = playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 640, Height: 360, AudioCopy: "flac", Copy: true}
	silent  = playback.Spec{AudioIndex: -1, VideoBitrate: 1_500_000, Width: 640, Height: 360}
	toneMap = playback.Spec{AudioIndex: 1, VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 640, Height: 360, ToneMap: "smpte2084"}
)

func withSubtitle(s playback.Spec, index int) playback.Spec { s.Subtitle = &index; return s }

func imageTrack(open func(float64) (float64, error)) *playback.Stream {
	return &playback.Stream{Index: 2, Width: 1920, Height: 1080, IsImageSubtitleStream: media.Ptr(true), OpenAt: open}
}

func goldenCases() []goldenCase {
	var cases []goldenCase
	sources := []struct {
		name  string
		p, p1 probes
	}{{"at0", atZero, atZero1}, {"at7.5", shifted, shifted1}, {"atminus0.5", early, early1}}
	audios := []struct {
		name string
		spec playback.Spec
	}{{"reencoded", base}, {"copied", copied}, {"noaudio", silent}}
	for _, src := range sources {
		for _, a := range audios {
			for _, start := range []int{0, 5} {
				cases = append(cases, goldenCase{name: fmt.Sprintf("%s/%s/segment%d", src.name, a.name, start), p: src.p, spec: a.spec, start: start})
			}
		}
		for _, start := range []int{0, 5} {
			cases = append(cases, goldenCase{name: fmt.Sprintf("%s/tonemapped/segment%d", src.name, start), p: src.p, spec: toneMap, start: start})
		}
	}
	// Every source, the negative start too: the subtitles are delayed by the
	// start itself (-0.5 s moves the cues), not by max(start, 0), and an image
	// track beside the film gets -itsoffset for a negative start as well.
	for _, src := range sources {
		for _, start := range []int{0, 5} {
			cases = append(cases,
				goldenCase{name: fmt.Sprintf("%s/burned-text/segment%d", src.name, start), p: src.p, spec: withSubtitle(base, 2), start: start, ass: sampleASS},
				goldenCase{name: fmt.Sprintf("%s/beside-image/segment%d", src.name, start), p: src.p, spec: withSubtitle(base, 2), start: start,
					image: &playback.Stream{Index: 2, Width: 1920, Height: 1080, IsImageSubtitleStream: media.Ptr(true), File: "/scratch/cues.sup"}},
				goldenCase{name: fmt.Sprintf("%s/beside-image-no-canvas/segment%d", src.name, start), p: src.p, spec: withSubtitle(base, 2), start: start,
					image: &playback.Stream{Index: 2, IsImageSubtitleStream: media.Ptr(true), File: "/scratch/cues.sup"}},
			)
		}
		// The first frame is at 20.02 (+7.5): a cue that began 3 s before it, one
		// that is not showing, and cue times that are not known.
		for _, c := range []struct {
			name string
			open func(float64) (float64, error)
		}{
			{"cue-began-before-the-cut", func(t float64) (float64, error) { return t - 3, nil }},
			{"no-cue-showing", func(t float64) (float64, error) { return t, nil }},
			{"cue-times-unknown", nil},
			{"cue-lookup-says-unknown", func(float64) (float64, error) { return 0, library.ErrUnknownPicture }},
		} {
			for _, start := range []int{0, 5} {
				cases = append(cases, goldenCase{name: fmt.Sprintf("%s/embedded-image-%s/segment%d", src.name, c.name, start), p: src.p, spec: withSubtitle(base, 2), start: start, image: imageTrack(c.open)})
			}
		}
		// A restart at segment 1 with a cue that began 5 s before its first
		// frame, so before the file's first second: the earlier open is clamped
		// at 0, never a negative -ss.
		cases = append(cases, goldenCase{name: src.name + "/embedded-image-cue-began-in-the-first-second/segment1", p: src.p1, spec: withSubtitle(base, 2), start: 1,
			image: imageTrack(func(t float64) (float64, error) { return t - 5, nil })})
	}
	return cases
}

// goldenRun runs one case and describes what Command made and the probes it ran.
func goldenRun(t *testing.T, c goldenCase) string {
	t.Helper()
	log := c.p.shim(t)
	dir := t.TempDir()
	cmd, err := callCommand(context.Background(), "/media/film.mkv", c.spec, c.start, dir, c.ass, c.image)
	if err != nil {
		return "error " + strings.ReplaceAll(err.Error(), dir, "<dir>") + "\n"
	}
	out := describe(cmd, dir)
	if data, err := os.ReadFile(filepath.Join(dir, "subtitles.ass")); err == nil {
		out += "subtitles.ass\n" + string(data)
	}
	for _, line := range asked(t, log) {
		out += "probe " + line + "\n"
	}
	return out
}

// CopyCommand is not changed; its arguments are held with Command's.
func goldenCopyCases() map[string]func(t *testing.T) string {
	run := func(p probes, spec playback.Spec, video, audio float64) func(t *testing.T) string {
		return func(t *testing.T) string {
			p.shim(t)
			dir := t.TempDir()
			cmd, err := CopyCommand(context.Background(), "/media/film.mkv", spec, "hevc", video, audio, dir)
			if err != nil {
				t.Fatal(err)
			}
			return describe(cmd, dir)
		}
	}
	return map[string]func(t *testing.T) string{
		"copycommand/at0/both/restart":        run(atZero, copied, 20.02, 20.02),
		"copycommand/at7.5/reencoded/restart": run(shifted, base, 27.52, 27.52),
		"copycommand/at7.5/from-the-start":    run(shifted, base, -1, -1),
	}
}

// The arguments Command makes, and the directory ffmpeg runs in, with the
// probes' answers held fixed, are those of the code before JS-113. The file is
// recorded from that code with -update-command-golden, never written by hand.
func TestCommandArgumentsAreUnchanged(t *testing.T) {
	// The golden holds the product's read rate; the integration build's init
	// sets copyReadRate to 0 for its own tests (copy_integration_test.go), so
	// this test sets the product's value and puts the other back.
	defer func(was string) { copyReadRate = was }(copyReadRate)
	copyReadRate = "5"
	got := map[string]string{}
	for _, c := range goldenCases() {
		got[c.name] = goldenRun(t, c)
	}
	for name, run := range goldenCopyCases() {
		got[name] = run(t)
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "=== %s\n%s", name, got[name])
	}
	path := filepath.Join("testdata", "command.golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantBy := map[string]string{}
	for _, section := range strings.Split(string(want), "=== ")[1:] {
		name, body, _ := strings.Cut(section, "\n")
		wantBy[name] = body
	}
	for _, name := range names {
		if wantBy[name] != got[name] {
			t.Errorf("%s changed\nwas:\n%s\nnow:\n%s", name, wantBy[name], got[name])
		}
		delete(wantBy, name)
	}
	for name := range wantBy {
		t.Errorf("%s is recorded but no longer run", name)
	}
}

// Each probe is started at most once per call, and only when the call needs it.
func TestCommandStartsTheProbesItNeeds(t *testing.T) {
	timing, frame, grid := "ffprobe timing", "ffprobe frame ", "ffmpeg grid"
	for _, tc := range []struct {
		name  string
		p     probes
		spec  playback.Spec
		start int
		image *playback.Stream
		ass   string
		want  []string // a prefix match for the frame probe's interval
	}{
		{"segment 0 needs no probe", atZero, base, 0, nil, "", nil},
		{"segment 0 with copied audio needs none", atZero, copied, 0, nil, "", nil},
		{"segment 0 with a burned text track asks the timing", shifted, withSubtitle(base, 2), 0, nil, sampleASS, []string{timing}},
		{"segment 0 with an image track beside the film asks the timing", shifted, withSubtitle(base, 2), 0,
			&playback.Stream{Index: 2, IsImageSubtitleStream: media.Ptr(true), File: "/scratch/cues.sup"}, "", []string{timing}},
		{"segment 0 with an embedded image track needs none", shifted, withSubtitle(base, 2), 0, imageTrack(nil), "", nil},
		{"a restart with re-encoded audio asks the timing, the first frame and the grid", atZero, base, 5, nil, "", []string{timing, frame + "20.000000%22.000000", grid}},
		{"a restart with copied audio asks no grid", atZero, copied, 5, nil, "", []string{timing, frame + "20.000000%22.000000"}},
		{"a restart without audio asks no grid", atZero, silent, 5, nil, "", []string{timing, frame + "20.000000%22.000000"}},
		{"a restart asks the first frame at the cut plus a positive start", shifted, base, 5, nil, "", []string{timing, frame + "27.500000%29.500000", grid}},
		{"a restart asks it at the cut when the start is negative", early, base, 5, nil, "", []string{timing, frame + "20.000000%22.000000", grid}},
		{"a restart with a burned text track asks each once", shifted, withSubtitle(copied, 2), 5, nil, sampleASS, []string{timing, frame + "27.500000%29.500000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := tc.p.shim(t)
			if _, err := callCommand(context.Background(), "/media/film.mkv", tc.spec, tc.start, t.TempDir(), tc.ass, tc.image); err != nil {
				t.Fatal(err)
			}
			got := asked(t, log)
			if len(got) != len(tc.want) {
				t.Fatalf("probes started: %q, want %q", got, tc.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tc.want[i]) {
					t.Errorf("probe %d: %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A probe that fails or finds nothing makes Command return an error and no
// command, never arguments made from zero values.
func TestCommandFailsWhenAProbeFindsNothing(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		fail  string
		fps   string
		spec  playback.Spec
		start int
		image *playback.Stream
		ass   string
		ctx   context.Context
	}{
		{name: "the timing probe exits non-zero", fail: "timing", spec: base, start: 5},
		{name: "the timing probe exits non-zero for a burned track at segment 0", fail: "timing", spec: withSubtitle(base, 2), ass: sampleASS},
		{name: "the source has no video stream", fail: "nostream", spec: base, start: 5},
		{name: "the frame rate is 0", fps: "0/0", spec: base, start: 5},
		{name: "the frame rate has no denominator", fps: "25/0", spec: base, start: 5},
		{name: "the first frame probe exits non-zero", fail: "frame", spec: base, start: 5},
		{name: "no frame within 2 s of the cut", fail: "noframe", spec: base, start: 5},
		{name: "the grid run makes no packet", fail: "nopacket", spec: base, start: 5},
		{name: "the grid run exits non-zero", fail: "grid", spec: base, start: 5},
		{name: "the context has ended", spec: base, start: 5, ctx: cancelled},
		{name: "a cue lookup fails", spec: withSubtitle(base, 2), start: 5, image: imageTrack(func(float64) (float64, error) { return 0, errors.New("store down") })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := atZero
			p.fail = tc.fail
			if tc.fps != "" {
				p.fps = tc.fps
			}
			p.shim(t)
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			cmd, err := callCommand(ctx, "/media/film.mkv", tc.spec, tc.start, t.TempDir(), tc.ass, tc.image)
			if err == nil || cmd != nil {
				t.Errorf("got command %v and error %v, want an error and no command", cmd, err)
			}
		})
	}
}
