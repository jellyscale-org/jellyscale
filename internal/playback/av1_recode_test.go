package playback

import (
	"math"
	"testing"
)

// JS-133: an AV1 copy whose keyframe index is found wrong goes on as an AV1 encode (SVT-AV1) cut on the playlist's layout.
// These checks name Spec.Recode "av1", which the build adds beside "h264" and "hevc" (JS-088).

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

// A tone-mapped AV1 fallback costs no less than an SDR one, and a 4K source is the dearer: it is decoded at full size
// before it is scaled down.
func TestAnAV1FallbackCostsMoreToToneMapAndFrom4K(t *testing.T) {
	spec := func(w, h int, tone string) Spec { return Spec{Width: w, Height: h, Copy: true}.Recoded("av1", tone) }
	for _, size := range [][2]int{{1920, 1080}, {3840, 2160}} {
		if sdr, tone := spec(size[0], size[1], "").Cost(), spec(size[0], size[1], "hdr10").Cost(); tone < sdr {
			t.Errorf("%dx%d: a tone-mapped AV1 fallback costs %.2f, less than the SDR one's %.2f", size[0], size[1], tone, sdr)
		}
	}
	if a, b := spec(1920, 1080, "").Cost(), spec(3840, 2160, "").Cost(); b < a {
		t.Errorf("a 4K AV1 fallback costs %.2f, less than a 1080p one's %.2f", b, a)
	}
}
