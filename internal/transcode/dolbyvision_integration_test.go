//go:build integration

package transcode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// A re-encode of a Dolby Vision film is plain H.264, so its init segment must
// not carry the source's Dolby Vision configuration. ffmpeg 6.1 copies it into
// the output when told -strict -2, and Chrome then never starts the video.
func TestReencodedDolbyVisionCarriesNoDolbyVisionBox(t *testing.T) {
	work := t.TempDir()
	source := filepath.Join(work, "p5.mp4")
	if err := testenv.MakeHDRFilm(source, testenv.HDRFilm{Base: "pq", DVProfile: 5, Seconds: 4}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(source); !bytes.Contains(data, []byte("dvcC")) {
		t.Fatal("the source has no dvcC box, so this test proves nothing")
	}
	for _, audio := range []string{"", "aac"} {
		dir := t.TempDir()
		spec := playback.Spec{AudioIndex: 1, VideoBitrate: 1_000_000, Width: 640, Height: 360, AudioCopy: audio}
		cmd, err := Command(context.Background(), source, spec, 0, dir, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, out)
		}
		init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		for _, box := range []string{"dvcC", "dvvC"} {
			if bytes.Contains(init, []byte(box)) {
				t.Errorf("audio copy %q: the H.264 init segment carries a %s box", audio, box)
			}
		}
	}
}
