package playback

import "testing"

// A copy whose index was found wrong goes on as an encode, which never shares segments with the copy (JS-088), and
// the encode is priced as one, whatever the copy was.
func TestRecodedSpecIsAnEncodeOfItsOwn(t *testing.T) {
	copied := Spec{Item: "a", Source: "b", ETag: "e", AudioIndex: 1, Width: 1920, Height: 1080, Copy: true, VideoBitrate: 0}
	h264, hevc := copied.Recoded("h264", ""), copied.Recoded("hevc", "")
	if h264.Copy || h264.Hash() == copied.Hash() || h264.Hash() == hevc.Hash() {
		t.Errorf("hashes: copy %s, h264 %s, hevc %s", copied.Hash(), h264.Hash(), hevc.Hash())
	}
	if copied.Cost() != CopyCost || h264.Cost() != 0.8 || hevc.Cost() != 3.2 {
		t.Errorf("costs: copy %v, h264 %v, hevc %v", copied.Cost(), h264.Cost(), hevc.Cost())
	}
	if h264.VideoBitrate <= 0 {
		t.Errorf("an encode of a file that names no bitrate has none to hold to: %d", h264.VideoBitrate)
	}
}
