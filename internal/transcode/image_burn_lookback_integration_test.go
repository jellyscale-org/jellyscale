//go:build integration

package transcode

import (
	"testing"

	"jellyscale/internal/testenv"
)

// A film that lists no times for its image track without a whole-film read
// starts no whole-film read either. A run that begins mid-film opens
// cueLookback seconds early, so a cue that began within that is drawn from the
// run's first frame and one that began earlier is missed until the next: the
// one remaining gap, pinned on both sides here (DOMAINS 51 states it). The
// names the checks use are cueLookback, the look-back in seconds, which the
// build keeps or renames with these checks.
//
// Three kinds of film list no times for the track. Zero points for the track
// is not "the track never shows anything": a build that reads it so takes a
// different path from MPEG-TS (the container is Matroska and its index was
// read), and every mid-film run on such a film then draws nothing, with no
// look-back. So a Matroska file is among the cases twice: written without
// Cues at all, and with Cues for the video only. Both are made with mkvmerge,
// which also compresses the track's blocks as a real remux does.
var lookbackCases = []struct {
	name string
	film cueFilm
}{
	{"MPEG-TS", cueFilm{codec: "dvbsub", container: "ts"}},
	{"Matroska with no Cues", cueFilm{codec: "copy", remux: "nocues"}},
	{"Matroska with Cues for the video only", cueFilm{codec: "copy", remux: "videocues"}},
}

// The cue is 26 seconds from 12 s.
func lookbackCuts() (inside, outside int) {
	inside = int(12+cueLookback-3) / 4
	outside = int(12+cueLookback+4)/4 + 1
	return inside, outside
}

func TestAMidFilmRunOfAFilmWithNoCueTimesDrawsACueWithinTheLookBackAndMissesAnOlderOne(t *testing.T) {
	for _, tc := range lookbackCases {
		t.Run(tc.name, func(t *testing.T) {
			cues := []testenv.Cue{top(12, 38)}
			media := t.TempDir()
			film := tc.film
			film.seconds, film.cues = 60, cues
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			reference := s.fromZero(t, 60)
			checkReference(t, reference, cues)
			inside, outside := lookbackCuts()
			if age := outside*4 - 12; float64(age) <= cueLookback || outside*4 >= 36 {
				t.Fatalf("the cut at segment %d is %d s into a cue that ends at 38 s: not past the look-back of %v s", outside, age, cueLookback)
			}
			s.seek(t, inside, 2, true, 0)
			got := s.halvesFrom(t, inside, inside+2)
			expectPicture(t, "a run that begins within the look-back, at its first frames", got, 1, "top")
			likeReference(t, "a run that begins within the look-back", reference, got, inside, edgesOf(cues)...)

			s.seek(t, outside, 2, true, 0)
			got = s.halvesFrom(t, outside, outside+2)
			// The remaining gap: the cue began before the look-back reaches, so the run
			// does not see its packet and draws nothing until the next cue (there is none).
			expectPicture(t, "a run that begins past the look-back, at its first frames", got, 1, "")
		})
	}
}

// The same films as noise: the run past the look-back reads at most the look-back
// more than a run without the track, and no whole-film read comes first, for
// the first mid-film run of the film and a later one.
func TestAMidFilmRunOfAFilmWithNoCueTimesReadsAtMostTheLookBackMore(t *testing.T) {
	for _, tc := range lookbackCases {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			film := tc.film
			film.seconds, film.noise, film.cues = 100, "2M", []testenv.Cue{top(12, 38)}
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			_, outside := lookbackCuts()
			burnt := []int64{s.seek(t, outside, 1, true, readRate)}
			plain := []int64{s.seek(t, outside, 1, false, readRate), s.seek(t, outside, 1, false, readRate)}
			burnt = append(burnt, s.seek(t, outside, 1, true, readRate))
			_, most, width := spread(plain...)
			// The look-back, the keyframe before it, a third more for the second input,
			// and 3 MB for the connection that seeks (as in the cue-span check).
			limit := most + width + int64(cueLookback+3)*perSecond*13/10 + 3<<20
			t.Logf("a run at segment %d read %v bytes with the track burnt in (the first run of the film, then a later one) and %v without it; the limit is %d", outside, burnt, plain, limit)
			for _, got := range burnt {
				if got > limit {
					t.Errorf("a run at segment %d read %d bytes, %d more than without the track: over the look-back of %v s (%d bytes with the allowance)", outside, got, got-most, cueLookback, limit-most-width)
				}
			}
		})
	}
}
