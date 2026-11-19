package playback

import "testing"

// A fallback is a re-encode, however large the copy was, and an HEVC one is
// priced from the x265 measurements (JS-088).
func TestRecodedCopyHonoursTheEncodeCapCostAndOutputVersion(t *testing.T) {
	if OutputVersion <= 5 {
		t.Errorf("OutputVersion = %d, want more than 5: an older worker must not claim a recode job", OutputVersion)
	}
	copied := Spec{Width: 3840, Height: 2160, VideoBitrate: 40_000_000, Copy: true}
	h264 := copied.Recoded("h264", "")
	if h264.Width != 1920 || h264.Height != 1080 || h264.SourceHeight != 2160 {
		t.Errorf("4K H.264 fallback is %dx%d from %d lines, want 1920x1080 from 2160", h264.Width, h264.Height, h264.SourceHeight)
	}
	hevc := copied.Recoded("hevc", "")
	if hevc.Width != 1920 || hevc.Height != 1080 || hevc.SourceHeight != 2160 {
		t.Errorf("4K HEVC fallback is %dx%d from %d lines, want 1920x1080 from 2160", hevc.Width, hevc.Height, hevc.SourceHeight)
	}
	if h264.Cost() != 1.6 || hevc.Cost() != 4.0 {
		t.Errorf("4K fallback costs: H.264 %v, HEVC %v; want 1.6 and 4.0", h264.Cost(), hevc.Cost())
	}
	hd := Spec{Width: 1920, Height: 1080, Copy: true}.Recoded("hevc", "")
	if hd.Cost() != 3.2 {
		t.Errorf("1080p HEVC fallback costs %v cores, want 3.2 (x264's 0.8 and 2.4 for x265)", hd.Cost())
	}
	small := Spec{Width: 640, Height: 360, Copy: true}.Recoded("hevc", "")
	if want := 0.8 + 2.4/9; small.Cost() < want-0.001 || small.Cost() > want+0.001 {
		t.Errorf("360p HEVC fallback costs %v cores, want %v (its pixels are a ninth of 1080p's)", small.Cost(), want)
	}
}
