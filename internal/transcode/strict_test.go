package transcode

import (
	"context"
	"slices"
	"testing"

	"jellyscale/internal/playback"
)

// -strict -2 is global: on ffmpeg 6.1 it also lets the MP4 muxer copy a Dolby
// Vision source's dvcC box into a re-encoded H.264 stream, which Chrome won't
// play (TestReencodedDolbyVisionCarriesNoDolbyVisionBox shows it with 6.1).
// FLAC and Opus go into MP4 without it from 6.1 on.
func TestNoGlobalStrictFlag(t *testing.T) {
	for _, audio := range []string{"", "opus", "flac"} {
		spec := playback.Spec{AudioIndex: 1, VideoBitrate: 1_000_000, Width: 640, Height: 360, AudioCopy: audio}
		cmd, err := Command(context.Background(), "in.mp4", spec, 0, t.TempDir(), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(cmd.Args, "-strict") {
			t.Errorf("audio copy %q: ffmpeg is given -strict: %v", audio, cmd.Args)
		}
	}
}
