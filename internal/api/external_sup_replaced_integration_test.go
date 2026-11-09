//go:build integration

package api

import (
	"testing"
)

// A .sup replaced between scans is sent as the file it is now, whatever its
// size: the size listed at the last scan must not cut or pad it.
func TestReplacedSupIsServedWhole(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f, _ := imageFilesEnv(t, target)
			for _, c := range []struct {
				index int
				name  string
				size  int
			}{{5, "film.en.sup", 3000}, {9, "film.sup", 20}, {5, "film.en.sup", 40}} {
				replaced := string(pgsFile(100 + c.size%7))
				replaced += string(make([]byte, 0))
				if c.size > len(replaced) {
					replaced += string(make([]byte, c.size-len(replaced)))
				}
				f.put(t, c.name, replaced)
				if status, _, body := f.get(t, 0, f.path(f.text, c.index, 0, "Stream.pgssub")); status != 200 || body != replaced {
					t.Errorf("%s replaced by %d bytes, no rescan: %d, %d bytes", c.name, len(replaced), status, len(body))
				}
			}
		})
	}
}
