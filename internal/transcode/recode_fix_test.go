package transcode

import (
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// JS-133: an AV1 copy's fallback is an SVT-AV1 encode (compat/captures/av1-fallback-12.4.md: an H.264 segment cannot
// follow an AV1 SourceBuffer). How it is cut on the playlist's layout is not fixed here: SVT-AV1 in ffmpeg 6.1 ignores
// -force_key_frames, so one command with forced keyframes cannot do it there, and the integration checks
// (av1_recode_integration_test.go) hold the build to the layout with a shim that makes every ffmpeg behave so.
func TestAnAV1FallbackIsAnSVTAV1Encode(t *testing.T) {
	spec := playback.Spec{AudioIndex: 1, AudioBitrate: 128_000, Width: 3840, Height: 2160, Copy: true}.Recoded("av1", "")
	encode, err := recodeArgs("av1", spec)
	if err != nil {
		t.Fatalf("an AV1 fallback cannot be made: %v", err)
	}
	line := strings.Join(encode, " ")
	if !slices.Contains(encode, "libsvtav1") || strings.Contains(line, "libx264") || strings.Contains(line, "libx265") || slices.Contains(encode, "copy") {
		t.Errorf("the fallback of an AV1 copy is not an SVT-AV1 encode: %s", line)
	}
	if spec.Width != 1920 || spec.Height != 1080 {
		t.Errorf("the fallback of a 4K copy is %dx%d, want 1920x1080", spec.Width, spec.Height)
	}
}
