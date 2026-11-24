package playback

import (
	"testing"
)

// hdrFallback is the spec of the encode that stands in for an HDR copy whose index was found wrong, spelled as
// internal/api's goOn spells it today: Recoded(codec, the source's tone map). If the build spells it another way
// (another argument, a field of the source), this is the one line of this file, and of its twin in internal/transcode, to
// follow it; nothing else here depends on the spelling.
func hdrFallback(copy Spec, codec, transfer string) Spec { return copy.Recoded(codec, transfer) }

// The fallback of an HDR copy is an encode of its own: it never shares segments with the 8-bit fallbacks (SDR, or a tone
// mapped one), with the copy, with another transfer's, or with the other codecs'.
func TestAnHDRFallbackIsAnEncodeOfItsOwn(t *testing.T) {
	copied := Spec{Item: "a", Source: "b", ETag: "e", AudioIndex: 1, Width: 1920, Height: 1080, Copy: true}
	for _, codec := range []string{"hevc", "av1"} {
		pq, hlg, sdr := hdrFallback(copied, codec, PQ), hdrFallback(copied, codec, HLG), hdrFallback(copied, codec, "")
		for name, other := range map[string]Spec{"the copy": copied, "an SDR fallback": sdr, "an HLG fallback": hlg, "the other codec's": hdrFallback(copied, map[string]string{"hevc": "av1", "av1": "hevc"}[codec], PQ)} {
			if pq.Hash() == other.Hash() {
				t.Errorf("the %s HDR10 fallback shares a hash with %s", codec, name)
			}
		}
		if pq.Copy || pq.Recode != codec {
			t.Errorf("the %s fallback is Copy %v Recode %q", codec, pq.Copy, pq.Recode)
		}
	}
	// A 4K copy's fallback is 1920x1080 as before.
	for _, codec := range []string{"hevc", "av1"} {
		got := hdrFallback(Spec{Width: 3840, Height: 2160, Copy: true}, codec, PQ)
		if got.Width != 1920 || got.Height != 1080 || got.SourceHeight != 2160 {
			t.Errorf("a 4K %s HDR copy falls back to %dx%d from %d lines, want 1920x1080 from 2160", codec, got.Width, got.Height, got.SourceHeight)
		}
	}
}

// The SDR fallbacks keep the hash they had apart from the version: a spec that is no HDR copy's carries nothing new.
func TestAnSDRFallbacksSpecIsWhatItWas(t *testing.T) {
	copied := Spec{Item: "i", Source: "s", ETag: "e", AudioIndex: 1, Width: 1280, Height: 720, VideoBitrate: 1_000_000, Copy: true}
	for _, codec := range []string{"h264", "hevc", "av1"} {
		got := copied.Recoded(codec, "")
		if got.ToneMap != "" || got.Copy || got.Recode != codec {
			t.Errorf("the %s SDR fallback is %+v", codec, got)
		}
	}
}

// A worker of the release before (output version 6) never claims the fallback job of an HDR copy: a job records the
// OutputVersion of the replica that queued it and a worker claims only its own (invariant 59), so the version is raised.
func TestOutputVersionIsRaisedForTheHDRFallback(t *testing.T) {
	if OutputVersion <= 6 {
		t.Errorf("OutputVersion = %d, want more than 6: a worker that makes an 8-bit or tone mapped fallback of an HDR copy must not claim its job", OutputVersion)
	}
}

// A 4K HDR source is the dearer to fall back from: it is decoded at full size before it is scaled down.
func TestA4KHDRFallbackCostsNoLessThanA1080pOne(t *testing.T) {
	for _, codec := range []string{"hevc", "av1"} {
		a := hdrFallback(Spec{Width: 1920, Height: 1080, Copy: true}, codec, PQ).Cost()
		b := hdrFallback(Spec{Width: 3840, Height: 2160, Copy: true}, codec, PQ).Cost()
		if b < a {
			t.Errorf("a 4K %s HDR fallback costs %.2f, less than a 1080p one's %.2f", codec, b, a)
		}
	}
}
