//go:build integration

package transcode

import (
	"os"
	"path/filepath"
	"testing"

	"jellyscale/internal/testenv"
)

var imageCodecs = []struct{ name, codec string }{{"PGS", "copy"}, {"VobSub", "dvdsub"}, {"DVB", "dvbsub"}}

// imageCases adds a PGS track as a Blu-ray remux has it: remuxed by mkvmerge,
// which stores the track's blocks compressed (ffmpeg's are raw), so a build
// that reads a packet's bytes where they lie sees what ffmpeg-made files never
// show it.
var imageCases = []struct{ name, codec, remux string }{
	{"PGS", "copy", ""}, {"VobSub", "dvdsub", ""}, {"DVB", "dvbsub", ""}, {"PGS remuxed by mkvmerge", "copy", "default"},
}

// showing is the picture the cues call for in the film's second at, and false
// within a second and a half of a cue's edge, where it is not judged.
func showing(cues []testenv.Cue, at int) (string, bool) {
	for _, cue := range cues {
		for _, edge := range []float64{cue.Start, cue.End} {
			if float64(at) > edge-1.5 && float64(at) < edge+1.5 {
				return "", false
			}
		}
		if float64(at) > cue.Start && float64(at) < cue.End {
			if cue.Y < 540 {
				return "top", true
			}
			return "bottom", true
		}
	}
	return "", true
}

// edgesOf lists the times at which the cues begin and end.
func edgesOf(cues []testenv.Cue) (edges []float64) {
	for _, cue := range cues {
		edges = append(edges, cue.Start, cue.End)
	}
	return edges
}

// checkReference fails the test for a second of a run from zero that does not
// show what the cues call for: a test that compared two black films would pass.
func checkReference(t *testing.T, reference [][2]int, cues []testenv.Cue) {
	t.Helper()
	bars := 0
	for at := range reference {
		if want, judged := showing(cues, at); judged {
			expectPicture(t, "a run from zero", reference, at, want)
			if want != "" {
				bars++
			}
		}
	}
	if bars < 10 {
		t.Fatalf("a run from zero shows a bar in %d seconds only", bars)
	}
}

// A run that begins at any segment draws what a run from zero draws there, for
// a film with several cues of different ages: a short one, a gap, a cue that
// ends, a gap, and a last one that began before the cuts at segments 12 and
// later (51 seconds old at segment 24 for PGS and VobSub; a DVB cue lasts 30
// seconds at most as ffmpeg writes it, so its last cue is shorter). The cuts
// are a worker's jobs on a film the scan has read, so the cue times reach the
// run as they do in production (JS-071).
func TestAMidFilmRunDrawsTheCueAFilmFromZeroShowsAtItsCut(t *testing.T) {
	for _, tc := range imageCases {
		t.Run(tc.name, func(t *testing.T) {
			cues := []testenv.Cue{top(6, 14), bottom(18, 34), top(45, 108)}
			cuts := []int{1, 2, 4, 5, 9, 11, 12, 20, 24, 26, 28}
			if tc.codec == "dvbsub" {
				cues = []testenv.Cue{top(6, 14), bottom(18, 34), top(45, 65)}
				cuts = []int{1, 2, 4, 5, 9, 11, 12, 14, 16, 18, 24}
			}
			media := t.TempDir()
			cueFilm{codec: tc.codec, remux: tc.remux, seconds: 120, cues: cues}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			reference := s.fromZero(t, 120)
			checkReference(t, reference, cues)
			// 1: before the first cue. 2, 5: inside the short cues. 4, 9: the gaps
			// after them. 11, 12: just before and just after the last cue began.
			for _, cut := range cuts {
				count := min(3, 30-cut)
				s.seek(t, cut, count, true, 0)
				likeReference(t, "a run from segment "+itoa(cut), reference, s.halvesFrom(t, cut, cut+count), cut, edgesOf(cues)...)
				if want, judged := showing(cues, cut*4+1); judged {
					expectPicture(t, "a run from segment "+itoa(cut)+" at its first frames", s.halvesFrom(t, cut, cut+count), 1, want)
				}
				// The segments have the frames, at the times, that the same run without
				// the track makes (a restart's segments can differ by a frame from a
				// run from zero's at 23.976 frames a second, with or without a track:
				// that is not this task's).
				burntTimes := map[int][]float64{}
				for n := cut; n < cut+count; n++ {
					burntTimes[n] = video(t, s.segments, s.hash, n)
				}
				s.seek(t, cut, count, false, 0)
				for n := cut; n < cut+count; n++ {
					compare(t, "segment "+itoa(n)+" of a run from segment "+itoa(cut)+" with the track against the same run without it", video(t, s.segments, s.hash, n), burntTimes[n], 0.001)
				}
			}
		})
	}
}

// A PGS picture built by two packets (a line added while the earlier one stays
// on screen) is drawn whole by a run that begins after the later packet, as a
// run from zero draws it: the top bar from 6 s, the bottom one added at 14 s,
// both until 30 s. A run that read only the later packet would draw the bottom
// bar alone.
func TestAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets(t *testing.T) {
	for _, remux := range []string{"", "default"} { // The second is mkvmerge's file, whose packets are compressed.
		t.Run("remux="+remux, func(t *testing.T) {
			media := t.TempDir()
			cueFilm{codec: "copy", remux: remux, seconds: 60, sup: testenv.PGSAdded(1920, 1080, top(6, 30), bottom(14, 30), 30)}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			reference := s.fromZero(t, 60)
			for at, want := range map[int]string{3: "", 10: "top", 20: "both", 27: "both", 34: ""} {
				expectPicture(t, "a run from zero", reference, at, want)
			}
			for _, cut := range []int{3, 4, 5, 6} { // 12 s (before the second line), 16, 20 and 24.
				s.seek(t, cut, 2, true, 0)
				likeReference(t, "a run from segment "+itoa(cut), reference, s.halvesFrom(t, cut, cut+2), cut, 6, 14, 30)
			}
		})
	}
}

// A film whose index the scan does not read today (MPEG-2 video with a VobSub
// track in Matroska, as a DVD rip has) meets the same: a run draws the cue
// showing at its cut, whenever it began.
func TestAMidFilmRunOfAnMPEG2FilmWithVobSubDrawsTheCueShowing(t *testing.T) {
	cues := []testenv.Cue{top(6, 14), bottom(18, 90)}
	media := t.TempDir()
	cueFilm{codec: "dvdsub", mpeg2: true, seconds: 100, cues: cues}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	reference := s.fromZero(t, 100)
	checkReference(t, reference, cues)
	for _, cut := range []int{2, 5, 12, 20, 23} { // The last two are 54 and 74 seconds into the long cue.
		count := min(2, 25-cut)
		s.seek(t, cut, count, true, 0)
		likeReference(t, "a run from segment "+itoa(cut), reference, s.halvesFrom(t, cut, cut+count), cut, edgesOf(cues)...)
		if want, judged := showing(cues, cut*4+1); judged {
			expectPicture(t, "a run from segment "+itoa(cut)+" at its first frames", s.halvesFrom(t, cut, cut+count), 1, want)
		}
	}
}

// replace puts a new film at the old one's place, as a library owner does, and
// has the library scanned again.
func (s *scannedFilm) replace(t *testing.T, f cueFilm) {
	t.Helper()
	dir := t.TempDir()
	f.write(t, dir, "film")
	if err := os.Rename(filepath.Join(dir, "film.mkv"), filepath.Join(s.media, "film.mkv")); err != nil {
		t.Fatal(err)
	}
	s.rescan(t)
}

// After a film's file is replaced by one whose cues fall at other times, a
// run that begins mid-film draws by the new file's cues, never the old file's:
// at 80 s the first file has a cue showing, the second has none, the third has
// one that began at 60 s.
func TestAfterTheFilmIsReplacedAMidFilmRunDrawsByTheNewFilesCues(t *testing.T) {
	media := t.TempDir()
	film := func(cue testenv.Cue) cueFilm {
		return cueFilm{codec: "copy", seconds: 100, cues: []testenv.Cue{cue}}
	}
	film(top(20, 100)).write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	const cut = 20 // 80 s.
	for _, step := range []struct {
		name string
		cue  testenv.Cue
		want string
	}{
		{"the first file", top(20, 100), "top"},
		{"the second, whose cue ended at 40 s", bottom(8, 40), ""},
		{"the third, whose cue began at 60 s", bottom(60, 100), "bottom"},
	} {
		if step.name != "the first file" {
			s.replace(t, film(step.cue))
		}
		s.seek(t, cut, 2, true, 0)
		expectPicture(t, "a run at 80 s in "+step.name, s.halvesFrom(t, cut, cut+2), 1, step.want)
		expectPicture(t, "a run at 80 s in "+step.name, s.halvesFrom(t, cut, cut+2), 5, step.want)
	}
}

// And what it reads: a film whose cue began 108 s before the cut is replaced by
// one whose cue began 16 s before it, and the run reads the film from the new
// cue's packet, not the old one's.
func TestAfterTheFilmIsReplacedAMidFilmRunReadsByTheNewFilesCues(t *testing.T) {
	media := t.TempDir()
	long := cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: []testenv.Cue{top(4, 120)}}
	short := cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: []testenv.Cue{top(96, 128)}}
	long.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	const cut = 28 // 112 s.
	first := s.seek(t, cut, 1, true, readRate)
	later := s.seek(t, cut, 1, true, readRate)
	s.replace(t, short)
	plain := []int64{s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate)}
	burnt := []int64{s.seek(t, cut, 1, true, readRate), s.seek(t, cut, 1, true, readRate)}
	limit := insideLimit(plain, 16)
	t.Logf("the old file's runs read %d and %d bytes (a cue 108 s old); the new file's read %v with the track and %v without it; the limit for a cue 16 s old is %d", first, later, burnt, plain, limit)
	for _, got := range burnt {
		if got > limit {
			t.Errorf("a run after the file was replaced read %d bytes before its first segment, over the %d that a cue 16 s old allows: it read by the old file's cue", got, limit)
		}
	}
}
