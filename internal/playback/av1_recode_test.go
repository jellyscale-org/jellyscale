package playback

import (
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-133: an AV1 copy whose keyframe index is found wrong goes on as an AV1 encode (SVT-AV1) cut on the playlist's layout.
// These checks name Spec.Recode "av1", which the build adds beside "h264" and "hevc" (JS-088).

// The observation that settled the fallback's codec (compat/captures/av1-fallback-12.4.md) is on disk, and the choice
// the build makes follows it: H.264 only if both directions showed frames, no video.error and no dialog.
func TestTheFallbacksCodecFollowsTheRecordOfWhatJellyfinWebDid(t *testing.T) {
	raw, err := os.ReadFile("../../compat/captures/av1-fallback-12.4.json")
	if err != nil {
		t.Fatalf("the observation is not recorded beside segment-errors-12.4.md: %v", err)
	}
	var record struct {
		Script   string `json:"script"`
		Choice   string `json:"choice"`
		Observed map[string]map[string]struct {
			Frames     int     `json:"frames"`
			VideoError *string `json:"videoError"`
			Dialog     bool    `json:"dialog"`
		} `json:"observed"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("../../compat/captures/" + strings.TrimPrefix(record.Script, "compat/captures/")); err != nil || record.Script == "" {
		t.Errorf("the script that reproduces the observation (%q) is not beside the record: %v", record.Script, err)
	}
	if md, err := os.ReadFile("../../compat/captures/av1-fallback-12.4.md"); err != nil || !strings.Contains(string(md), "av01") {
		t.Errorf("the record's prose is missing or does not name the master playlist's AV1 codec: %v", err)
	}
	h264, ok := record.Observed["h264"]
	if !ok {
		t.Fatalf("the record has no row for an H.264 fallback: %+v", record.Observed)
	}
	works := true
	for _, direction := range []string{"forward", "back"} {
		d, ok := h264[direction]
		if !ok {
			t.Fatalf("the H.264 row has no %q direction", direction)
		}
		if d.Frames == 0 || d.VideoError != nil || d.Dialog {
			works = false
		}
	}
	want := "av1"
	if works {
		want = "h264"
	}
	if record.Choice != want {
		t.Errorf("the record names %q, the rule (H.264 only if both directions showed frames with no video.error and no dialog) gives %q", record.Choice, want)
	}
	// The build follows the record, for every AV1 copy.
	copied := Spec{Item: "a", Source: "b", ETag: "e", AudioIndex: 1, Width: 1920, Height: 1080, Copy: true}
	if !CanRecode("av1") {
		t.Fatal("an AV1 copy cannot go on as an encode: CanRecode(\"av1\") is false")
	}
	for _, tone := range []string{"", "hdr10"} {
		if got := copied.Recoded(record.Choice, tone).Recode; got != record.Choice {
			t.Errorf("the AV1 fallback encodes %q, the record chose %q", got, record.Choice)
		}
	}
}

// The AV1 fallback is an encode of its own: it never shares segments with the copy or with the other codecs' fallbacks.
func TestAnAV1FallbackIsAnEncodeOfItsOwn(t *testing.T) {
	copied := Spec{Item: "a", Source: "b", ETag: "e", AudioIndex: 1, Width: 1920, Height: 1080, Copy: true}
	av1 := copied.Recoded("av1", "")
	if av1.Copy || av1.Recode != "av1" {
		t.Fatalf("the AV1 fallback is Copy %v Recode %q", av1.Copy, av1.Recode)
	}
	for name, other := range map[string]Spec{"the copy": copied, "the H.264 fallback": copied.Recoded("h264", ""), "the HEVC fallback": copied.Recoded("hevc", "")} {
		if av1.Hash() == other.Hash() {
			t.Errorf("the AV1 fallback shares a hash with %s", name)
		}
	}
	if av1.VideoBitrate <= 0 {
		t.Errorf("an encode of a file that names no bitrate has none to hold to: %d", av1.VideoBitrate)
	}
}

// A worker of the release before (output version 5) never claims the AV1 fallback's job: a job records the OutputVersion
// of the replica that queued it and a worker claims only its own (invariant 59), so the version is raised.
func TestOutputVersionIsRaisedForTheAV1Fallback(t *testing.T) {
	if OutputVersion <= 5 {
		t.Errorf("OutputVersion = %d, want more than 5: a worker that cannot make the AV1 fallback must not claim its job", OutputVersion)
	}
}

// The fallback's size: capped at 1080 lines like every re-encode, the aspect ratio kept, a smaller copy as it is.
func TestAnAV1FallbackIsCappedAt1080LinesAndKeepsTheAspectRatio(t *testing.T) {
	for _, c := range []struct {
		name          string
		width, height int
		w, h          int
	}{
		{"4K", 3840, 2160, 1920, 1080},
		{"wider than 16:9, over 1080 lines", 4096, 1716, 2576, 1080},
		{"1080 lines", 1920, 1080, 1920, 1080},
		{"720 lines", 1280, 720, 1280, 720},
		{"360 lines", 640, 360, 640, 360},
	} {
		got := Spec{Width: c.width, Height: c.height, Copy: true}.Recoded("av1", "")
		if got.Width != c.w || got.Height != c.h {
			t.Errorf("%s: the fallback is %dx%d, want %dx%d", c.name, got.Width, got.Height, c.w, c.h)
		}
		if got.Height > 1080 || got.Width%2 != 0 || got.Height%2 != 0 {
			t.Errorf("%s: %dx%d is over 1080 lines or odd", c.name, got.Width, got.Height)
		}
		if ratio, want := float64(got.Width)/float64(got.Height), float64(c.width)/float64(c.height); math.Abs(ratio-want) > 0.005 {
			t.Errorf("%s: aspect %.4f, the copy's is %.4f", c.name, ratio, want)
		}
		if got.SourceHeight != c.height {
			t.Errorf("%s: the price is read from the source's %d lines, the spec holds %d", c.name, c.height, got.SourceHeight)
		}
	}
}

// docs/transcoding.md records what the AV1 fallback's own ffmpeg command used for each second of film (user plus system
// CPU, and wall-clock), for a 1080p and a 4K 24 fps AV1 source, on a named machine with the ffmpeg and encoder
// versions, and Cost is at least the CPU of each row. The rows are lines of the form
//
//	AV1 fallback measured on: <machine>, ffmpeg <version>, SVT-AV1 <version>
//	AV1 fallback row: <1080p|4k> <sdr|tonemap> cpu <seconds of CPU per second of film> s/s wall <seconds per second of film> s/s
//
// (a tonemap row for each source is optional; the SDR row of each is required). The live figures are measured by
// TestTheAV1FallbacksCommandsAreMeasuredOnA1080pAndA4KSource in internal/transcode and logged there, not judged: a CPU time
// depends on the machine a check runs on.
func TestTheAV1FallbacksPriceCoversTheCPUItsFfmpegUses(t *testing.T) {
	raw, err := os.ReadFile("../../docs/transcoding.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !regexp.MustCompile(`(?m)^AV1 fallback measured on: .*ffmpeg [0-9n][^ ,]*.*SVT-AV1 v?[0-9][^ ,]*`).MatchString(text) {
		t.Errorf("docs/transcoding.md has no line 'AV1 fallback measured on: <machine>, ffmpeg <version>, SVT-AV1 <version>'")
	}
	rows := map[string]struct{ cpu, wall float64 }{}
	for _, m := range regexp.MustCompile(`(?m)^AV1 fallback row: (1080p|4k) (sdr|tonemap) cpu ([0-9.]+) s/s wall ([0-9.]+) s/s`).FindAllStringSubmatch(text, -1) {
		cpu, _ := strconv.ParseFloat(m[3], 64)
		wall, _ := strconv.ParseFloat(m[4], 64)
		rows[m[1]+" "+m[2]] = struct{ cpu, wall float64 }{cpu, wall}
	}
	spec := map[string]func(toneMap string) Spec{
		"1080p": func(tone string) Spec { return Spec{Width: 1920, Height: 1080, Copy: true}.Recoded("av1", tone) },
		"4k":    func(tone string) Spec { return Spec{Width: 3840, Height: 2160, Copy: true}.Recoded("av1", tone) },
	}
	for _, size := range []string{"1080p", "4k"} {
		sdr, ok := rows[size+" sdr"]
		if !ok || sdr.cpu <= 0 {
			t.Errorf("docs/transcoding.md has no 'AV1 fallback row: %s sdr cpu <n> s/s wall <n> s/s'", size)
			continue
		}
		for _, tone := range []string{"sdr", "tonemap"} {
			row, ok := rows[size+" "+tone]
			if !ok {
				continue
			}
			toneMap := map[string]string{"sdr": "", "tonemap": "hdr10"}[tone]
			if cost := spec[size](toneMap).Cost(); cost < row.cpu {
				t.Errorf("a %s %s AV1 fallback costs %.2f cores, its ffmpeg used %.2f s of CPU for each second of film", size, tone, cost, row.cpu)
			}
			if row.wall <= 0 || row.wall > 1 {
				t.Errorf("%s %s: the encode took %.2f s of wall-clock for each second of film, it must keep up with the film (at most 1)", size, tone, row.wall)
			}
		}
		if sdrCost, toneCost := spec[size]("").Cost(), spec[size]("hdr10").Cost(); toneCost < sdrCost {
			t.Errorf("%s: a tone-mapped AV1 fallback costs %.2f, less than the SDR one's %.2f", size, toneCost, sdrCost)
		}
	}
	// The 4K source is the dearer: it is decoded at full size before it is scaled down.
	if a, b := spec["1080p"]("").Cost(), spec["4k"]("").Cost(); b < a {
		t.Errorf("a 4K AV1 fallback costs %.2f, less than a 1080p one's %.2f", b, a)
	}
}
