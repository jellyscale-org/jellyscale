package playback

import (
	"slices"
	"testing"
)

func tallSource(codec, rangeType string, height int) Source {
	return Source{Streams: []Stream{{Type: "Video", Codec: codec, VideoRangeType: rangeType, Width: height * 16 / 9, Height: height}}}
}

// The ceiling lowered the picture only where the bitrate would have carried more lines without it: a cap that carries
// exactly 1080 lines gives 1080 with or without the ceiling, so the cap lowered it and the ceiling did not.
func TestOutputSaysTheCeilingLoweredThePictureOnlyWhereTheBitrateCarriedMore(t *testing.T) {
	for _, c := range []struct {
		name    string
		height  int
		bitrate int64 // The video's share of the cap, as Decide gives it.
		want    int
		lowered bool
	}{
		{"2160 at Auto", 2160, Unlimited, 1080, true},
		{"1440 at Auto", 1440, Unlimited, 1080, true},
		{"2160 under a cap that carries 1440", 2160, 4_800_000, 1080, true},
		{"2160 under a cap that carries exactly 1080", 2160, 3_000_000 - 192_000, 1080, false},
		{"1440 under a cap that carries exactly 1080", 1440, 3_000_000 - 192_000, 1080, false},
		{"1080 at Auto", 1080, Unlimited, 1080, false},
		{"2160 under a cap that carries only 720", 2160, 1_200_000 - 192_000, 720, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := tallSource("hevc", "SDR", c.height).Output(Decision{VideoBitrate: c.bitrate})
			if out.Height != c.want || out.CeilingLowered != c.lowered || out.Codec != "h264" || out.Range != "SDR" {
				t.Errorf("got %+v, want a %d line H.264 SDR picture, ceiling lowered %v", out, c.want, c.lowered)
			}
		})
	}
}

// A copy reads as the file's own codec, height and range, whatever a re-encode would give: an HEVC HDR10 film is not said to be
// H.264 SDR.
func TestOutputOfACopyIsTheFilesOwnVideo(t *testing.T) {
	for _, c := range []struct{ codec, rangeType, want string }{
		{"hevc", "HDR10", "HDR10"}, {"hevc", "HLG", "HLG"}, {"hevc", "HDR10Plus", "HDR10Plus"}, {"av1", "SDR", "SDR"},
		{"hevc", "DOVIWithHDR10", "HDR10"}, // A Dolby Vision file is sent as its base layer.
	} {
		out := tallSource(c.codec, c.rangeType, 2160).Output(Decision{Copy: true})
		if out.Codec != c.codec || out.Height != 2160 || out.Range != c.want || out.ToneMapped || out.CeilingLowered {
			t.Errorf("%s %s: got %+v, want the file's own video in %s", c.codec, c.rangeType, out, c.want)
		}
	}
}

// A re-encode of a codec that is never copied names the codec, even where the client lists it and only the audio or the
// container forced the stream: a copy could have had those reasons, so nothing else would say the video was not sent as it is.
func TestReencodeOfANeverCopiedCodecNamesWhyTheVideoWasNotSent(t *testing.T) {
	profile := capturedProfile(t)
	for _, codec := range []string{"vp9", "mpeg4", "vc1"} {
		for _, c := range []struct{ name, container, audio string }{{"audio", "mkv", "ac3"}, {"container", "avi", "aac"}} {
			t.Run(codec+" "+c.name, func(t *testing.T) {
				source := film(c.container, codec, c.audio)
				decision := Decide(Request{DeviceProfile: profile}, source, true)
				if decision.DirectPlay || decision.Copy || !slices.Contains(decision.Why(), "VideoCodecNotSupported") {
					t.Errorf("got %+v, want a re-encode naming VideoCodecNotSupported", decision)
				}
			})
		}
	}
}
