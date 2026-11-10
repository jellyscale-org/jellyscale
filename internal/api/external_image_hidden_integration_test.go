//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"jellyscale/internal/library"
	"jellyscale/internal/testenv"
)

// An external image file that a worker found unreadable (library.MarkUnreadable,
// which the burn-in's worker calls) is hidden as an unreadable text file is
// (TestBadSubtitleFileCostsOneJobPerFingerprint): out of the film's tracks,
// over rescans, the other tracks keep their numbers, and an edited file is
// listed again at its index. For a .sup and for a .sub and .idx pair, on POSIX
// and S3.
func TestUnreadableExternalImageFileStaysHiddenUntilItChanges(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets(t)...) {
		t.Run("backend="+target, func(t *testing.T) {
			f, _ := imageFilesEnv(t, target)
			fingerprint := func(index int) string {
				return scalar[string](t, f.pool, `SELECT s->>'Fingerprint' FROM media_sources, jsonb_array_elements(streams) s WHERE (s->>'Index')::int = $1`, index)
			}
			for _, c := range []struct {
				index int
				name  string
			}{{5, "film.en.sup"}, {6, "film.es."}} {
				if err := library.MarkUnreadable(context.Background(), f.pool, f.text, c.index, fingerprint(c.index), ""); err != nil {
					t.Fatal(err)
				}
			}
			check := func(why string) {
				t.Helper()
				tracks := f.tracksByIndex(t)
				if tracks[5] != nil || tracks[6] != nil {
					t.Errorf("%s: a file marked unreadable is listed: %s", why, summary(f.subtitles(t)))
				}
				if tracks[3] == nil || tracks[7] == nil || tracks[8] == nil || tracks[9] == nil || len(tracks) != 6 {
					t.Errorf("%s: the other tracks did not keep their numbers: %s", why, summary(f.subtitles(t)))
				}
				if status, _, _ := f.get(t, 0, f.path(f.text, 5, 0, "Stream.pgssub")); status != 404 {
					t.Errorf("%s: the hidden .sup is still served: %d", why, status)
				}
			}
			check("after the mark")
			f.scan(t)
			check("after a rescan")
			f.scan(t)
			check("after another")

			// Edited, each is another file: listed again at its index.
			f.put(t, "film.en.sup", string(pgsFile(901)))
			idx, sub := testenv.VobSub(720, 480, testenv.Cue{Start: 1, End: 2, X: 247, Y: 213, W: 225, H: 53})
			f.put(t, "film.es.idx", string(idx))
			f.put(t, "film.es.sub", string(sub))
			f.scan(t)
			tracks := f.tracksByIndex(t)
			if tracks[5] == nil || tracks[6] == nil || !strings.Contains(summary(f.subtitles(t)), "5=film.en.sup") {
				t.Errorf("after the files were edited: %s", summary(f.subtitles(t)))
			}
		})
	}
}
