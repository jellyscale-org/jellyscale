//go:build integration

package transcode

import (
	"testing"

	"jellyscale/internal/testenv"
)

// What the scan reads of a film is paid once per film, but a library has
// thousands, and every one that has an image track is read by the first scan
// after a release: moving the cue times' whole-film read from the run into the
// scan leaves every job-side check green and costs the store a full read of
// each film (ffprobe over the packets of the track reads 86% of a 547 MB film;
// ARCHITECTURE rule 6 and ReadKeyframes exist to avoid that). So the scan is
// counted here, through the store the library reads the film from.

// scanRead is what the first scan of a film of the given length reads,
// with the image track in the film or without it.
func scanRead(t *testing.T, film cueFilm, seconds int, withTrack bool) int64 {
	t.Helper()
	media := t.TempDir()
	film.seconds, film.noise, film.cues, film.noTrack = seconds, "1M", []testenv.Cue{top(4, 20), bottom(30, 50)}, !withTrack
	film.write(t, media, "film")
	m := newMeter(media, 0)
	newScannedThrough(t, media, t.TempDir(), m)
	return m.read.Load()
}

// scanTrackCost is the most the scan may read more for a film's image track
// than for the same film without it. The scan's own reads vary by up to 6 MB
// from run to run on a busy machine (its frame grab reads ahead until it is
// stopped; JS-071's lead correction: a 3 MB bound failed under load with
// nothing wrong, the "extra" coming out as -6 MB), while the read it guards
// against, the track's packets over the whole film, adds the film's size: 32 MB
// for the 256 s film, and 24 MB more than for the 64 s one. 12 MB lies between.
const scanTrackCost = 12 << 20

// The scan reads no more of a film because it has an image track, and what it
// reads does not grow with the film: in a film four times as long (1 Mbps
// noise, 64 s and 256 s) the extra is the same. The cases include a Matroska
// file with Cues (a build can take cue points from them), one without, MPEG-2
// video (an index the scan does not read), and MPEG-TS (no index at all).
func TestTheScanReadsNoMoreOfAFilmForItsImageTrackAndNothingThatGrowsWithTheFilm(t *testing.T) {
	for _, tc := range []struct {
		name string
		film cueFilm
	}{
		{"PGS", cueFilm{codec: "copy"}},
		{"PGS remuxed by mkvmerge", cueFilm{codec: "copy", remux: "default"}},
		{"PGS with no Cues", cueFilm{codec: "copy", remux: "nocues"}},
		{"VobSub with MPEG-2 video", cueFilm{codec: "dvdsub", mpeg2: true}},
		{"DVB in MPEG-TS", cueFilm{codec: "dvbsub", container: "ts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[int]int64{}
			for _, seconds := range []int{64, 256} {
				with, without := scanRead(t, tc.film, seconds, true), scanRead(t, tc.film, seconds, false)
				extra[seconds] = with - without
				t.Logf("a %d s film: the first scan read %d bytes with the image track and %d without it", seconds, with, without)
			}
			if extra[256] > scanTrackCost {
				t.Errorf("the scan of a 256 s film read %d bytes more for its image track, over the %d the track itself costs", extra[256], int64(scanTrackCost))
			}
			if diff := extra[256] - extra[64]; diff > scanTrackCost || diff < -scanTrackCost {
				t.Errorf("the scan read %d bytes more for the image track of the 256 s film and %d more for the 64 s one: what it reads grows with the film (by %d)", extra[256], extra[64], diff)
			}
		})
	}
}
