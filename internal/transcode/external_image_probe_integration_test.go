//go:build integration

package transcode

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"jellyscale/internal/testenv"
)

// The pre-burn probe of an image file hides it only when ffprobe itself says
// the file is bad. A probe that is killed by a signal, or runs past its
// 10 seconds, says nothing about the file: the good file is still listed and
// the pick is left to ffmpeg, which reads it.
func TestFailedProbeOfAGoodImageFileDoesNotHideItOrFailThePick(t *testing.T) {
	good := testenv.PGS(1920, 1080, testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120})
	real, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, onCount string }{
		{"killed by a signal", "kill -9 $$"},
		{"slower than its limit", "sleep 11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.FFmpeg(t)
			shim := t.TempDir()
			script := "#!/bin/sh\ncase \" $* \" in *-count_packets*) " + tc.onCount + " ;; esac\nexec " + real + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(shim, "ffprobe"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			r := newRig(t, nil)
			r.useBesideImage(t, supEntry, map[string][]byte{"damaged.sup": good})
			r.worker(t, nil)
			j := r.jobs(t)
			if err, _ := r.burn(t, j, 0); err != nil {
				t.Fatalf("the pick failed: %v", err)
			}
			if listed, marked := r.subtitleState(t); marked || !listed {
				t.Errorf("hidden (listed %v, marked %v), though the file is good", listed, marked)
			}
		})
	}
}
