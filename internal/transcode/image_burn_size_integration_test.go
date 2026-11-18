//go:build integration

package transcode

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-131: the burn-in kit's films are 640x360, and every run over them asks for
// a picture of the film's own size. Asking for 1280x720 made ffmpeg scale each
// film up and encode four times the pixels, which cost 655 CPU-s over three
// tests where 445 do the same.

// filmSize is the first video stream's size.
func filmSize(t *testing.T, path string) (int, int) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%dx%d", &w, &h); err != nil {
		t.Fatalf("ffprobe said %q: %v", out, err)
	}
	return w, h
}

func TestTheBurnInKitAsksForTheSizeOfTheFilmsItWrites(t *testing.T) {
	testenv.FFmpeg(t)
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 8, cues: []testenv.Cue{top(1, 3)}}.write(t, media, "film")
	width, height := filmSize(t, filepath.Join(media, "film.mkv"))
	if width != 640 || height != 360 {
		t.Fatalf("the kit's film is %dx%d, not the 640x360 this check is written for", width, height)
	}
	t.Run("a film scanned by this release", func(t *testing.T) {
		s := newScanned(t, media, t.TempDir())
		if s.spec.Width != width || s.spec.Height != height {
			t.Errorf("newScanned asks for %dx%d over a film of %dx%d", s.spec.Width, s.spec.Height, width, height)
		}
	})
	t.Run("a film scanned by the previous release", func(t *testing.T) {
		s := scannedByThePreviousRelease(t, media, t.TempDir())
		if s.spec.Width != width || s.spec.Height != height {
			t.Errorf("scannedByThePreviousRelease asks for %dx%d over a film of %dx%d", s.spec.Width, s.spec.Height, width, height)
		}
	})
	t.Run("a rig outside the kit keeps its own spec", func(t *testing.T) {
		// Only the kit's films are 640x360; the tests of the other kits ask for the size they always did.
		r := newRig(t, nil)
		if r.spec.Width != 1280 || r.spec.Height != 720 {
			t.Errorf("newRig asks for %dx%d, want the 1280x720 the rest of the package's tests use", r.spec.Width, r.spec.Height)
		}
	})
}
