package api

import (
	"testing"

	"jellyscale/internal/playback"
)

// A copy of H.264, HEVC or AV1 goes on as an encode (JS-133 built the AV1 one), and enters the path that logs and
// counts a fallback.
func TestACopyOfEachCodecFallsBack(t *testing.T) {
	for codec, want := range map[string]bool{"h264": true, "hevc": true, "av1": true} {
		req := hlsRequest{media: playback.Source{Streams: []playback.Stream{{Type: "Video", Codec: codec}}}}
		req.useDropped([]int64{0, 50_000_000}, 100_000_000)
		if req.fellBack != want {
			t.Errorf("%s copy with its index dropped: fellBack %v, want %v", codec, req.fellBack, want)
		}
		if len(req.media.Keyframes) != 2 {
			t.Errorf("%s: the playlist is not cut from the dropped index", codec)
		}
	}
}
