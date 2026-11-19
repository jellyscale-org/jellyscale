//go:build integration

package transcode

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-089: what is drawn from the stored track. Each check makes the reference
// first, a run from zero (which draws as JS-071 does, from the film), then
// waits for the track to be stored, then measures runs that draw from it.

const (
	frameW, frameH = 160, 90
	frameBytes     = frameW * frameH * 3
)

// frame is one decoded video frame of a joined run: its time and its pixels as
// 160x90 RGB.
type frame struct {
	at  float64
	rgb []byte
}

// frames decodes every frame of the file, each with its time.
func frames(t *testing.T, file string) []frame {
	t.Helper()
	at := times(t, file, "v:0", "frame=pts_time")
	raw := run(t, "ffmpeg", "-nostdin", "-v", "error", "-i", file, "-an", "-fps_mode", "passthrough",
		"-vf", "scale="+strconv.Itoa(frameW)+":"+strconv.Itoa(frameH)+",format=rgb24", "-f", "rawvideo", "-")
	if len(raw) != len(at)*frameBytes {
		t.Fatalf("%s: %d frames by their times and %d bytes of pixels", file, len(at), len(raw))
	}
	out := make([]frame, len(at))
	for i := range at {
		out[i] = frame{at[i], []byte(raw[i*frameBytes : (i+1)*frameBytes])}
	}
	return out
}

// bar is where the picture's bright pixels are: the box that holds them, how
// many there are, and their mean colour.
type bar struct {
	n                    int
	x0, y0, x1, y1       int
	mean                 [3]float64
	top, bottom, visible bool
}

func barIn(f frame) bar {
	b := bar{x0: frameW, y0: frameH, x1: -1, y1: -1}
	var sum [3]float64
	for i := 0; i < frameW*frameH; i++ {
		px := f.rgb[i*3 : i*3+3]
		if max(px[0], px[1], px[2]) <= 128 {
			continue
		}
		x, y := i%frameW, i/frameW
		b.n++
		b.x0, b.y0, b.x1, b.y1 = min(b.x0, x), min(b.y0, y), max(b.x1, x), max(b.y1, y)
		for c := range sum {
			sum[c] += float64(px[c])
		}
		if y < frameH/2 {
			b.top = true
		} else {
			b.bottom = true
		}
	}
	if b.n > 0 {
		for c := range sum {
			b.mean[c] = sum[c] / float64(b.n)
		}
	}
	b.visible = b.n >= 100 // A bar is about 500 pixels at this size; a stray one is not a cue.
	return b
}

// state says what a frame shows: "", "top" or "bottom".
func (b bar) state() string {
	switch {
	case !b.visible:
		return ""
	case b.top:
		return "top"
	}
	return "bottom"
}

// sameBar fails the test unless the frame draws the cue where, as large as and
// in the colours that the reference does, compared on its pixels: the box
// within a pixel, the mean colour within 12, and at most 1% of the picture
// differing by more than 64 in a channel (an encode is not the same bytes
// twice, but a cue drawn at another place or in another palette is).
func sameBar(t *testing.T, what string, want, got frame) {
	t.Helper()
	w, g := barIn(want), barIn(got)
	if !w.visible {
		t.Fatalf("%s: the reference frame at %.3f s shows no cue (%d bright pixels): the check would compare two black pictures", what, want.at, w.n)
	}
	if !g.visible {
		t.Errorf("%s: the frame at %.3f s shows no cue where a run from zero draws one (%d pixels in %d..%d, %d..%d)", what, got.at, w.n, w.x0, w.x1, w.y0, w.y1)
		return
	}
	for i, d := range []int{g.x0 - w.x0, g.y0 - w.y0, g.x1 - w.x1, g.y1 - w.y1} {
		if d < -1 || d > 1 {
			t.Errorf("%s: at %.3f s the cue's box is x %d..%d y %d..%d and a run from zero draws x %d..%d y %d..%d (edge %d differs by %d)", what, got.at, g.x0, g.x1, g.y0, g.y1, w.x0, w.x1, w.y0, w.y1, i, d)
			break
		}
	}
	for c := range 3 {
		if math.Abs(g.mean[c]-w.mean[c]) > 12 {
			t.Errorf("%s: at %.3f s the cue's mean colour is %.0f,%.0f,%.0f and a run from zero draws %.0f,%.0f,%.0f", what, got.at, g.mean[0], g.mean[1], g.mean[2], w.mean[0], w.mean[1], w.mean[2])
			break
		}
	}
	differing := 0
	for i := 0; i < frameW*frameH; i++ {
		for c := range 3 {
			if d := int(want.rgb[i*3+c]) - int(got.rgb[i*3+c]); d > 64 || d < -64 {
				differing++
				break
			}
		}
	}
	if differing*100 > frameW*frameH {
		t.Errorf("%s: at %.3f s %d of %d pixels differ from a run from zero's by more than 64 in a channel", what, got.at, differing, frameW*frameH)
	}
}

// at is the frame whose time is within 1 ms of the second, or fails the test.
func at(t *testing.T, all []frame, second float64) frame {
	t.Helper()
	for _, f := range all {
		if math.Abs(f.at-second) < 0.0205 { // Half a frame at 23.976 a second.
			return f
		}
	}
	t.Fatalf("no frame near %.3f s in %d frames (%.3f to %.3f)", second, len(all), all[0].at, all[len(all)-1].at)
	return frame{}
}

// referenceFrames is every frame of the run from zero that fromZero just made.
func (s *scannedFilm) referenceFrames(t *testing.T, seconds int) []frame {
	t.Helper()
	return frames(t, s.storedFrom(t, 0, (seconds+3)/4))
}

// With the track stored, every run that begins mid-film draws what a run from
// zero draws at and after its cut: the showing cue from its first frame until it
// ends, nothing in a gap between cues, before the first or after the last, the
// later of two cues. The cuts are those of JS-071's check of the same name, and
// a run begins in each kind of place.
func TestWithTheTrackStoredAMidFilmRunDrawsWhatAFilmFromZeroDrawsAtItsCut(t *testing.T) {
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
			s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
			for _, cut := range cuts {
				count := min(3, 30-cut)
				got := s.seekStored(t, cut, count, true, 0)
				likeReference(t, "a run from segment "+itoa(cut)+" with the track stored", reference, s.halvesFrom(t, cut, cut+count), cut, edgesOf(cues)...)
				if want, judged := showing(cues, cut*4+1); judged {
					expectPicture(t, "a run from segment "+itoa(cut)+" with the track stored, at its first frames", s.halvesFrom(t, cut, cut+count), 1, want)
					if want != "" && got.assets <= 0 {
						t.Errorf("a run from segment %d, where a cue is showing, read nothing from the asset store: it did not draw from the stored track", cut)
					}
				}
			}
		})
	}
}

// A PGS picture built by two packets is drawn whole by a run that begins after
// the later packet, from the stored track.
func TestWithTheTrackStoredAMidFilmRunDrawsAPGSPictureBuiltByTwoPackets(t *testing.T) {
	for _, remux := range []string{"", "default"} {
		t.Run("remux="+remux, func(t *testing.T) {
			media := t.TempDir()
			cueFilm{codec: "copy", remux: remux, seconds: 60, sup: testenv.PGSAdded(1920, 1080, top(6, 30), bottom(14, 30), 30)}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			reference := s.fromZero(t, 60)
			for at, want := range map[int]string{3: "", 10: "top", 20: "both", 27: "both", 34: ""} {
				expectPicture(t, "a run from zero", reference, at, want)
			}
			s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
			for _, cut := range []int{3, 4, 5, 6} {
				s.seekStored(t, cut, 2, true, 0)
				likeReference(t, "a run from segment "+itoa(cut)+" with the track stored", reference, s.halvesFrom(t, cut, cut+2), cut, 6, 14, 30)
			}
		})
	}
}

// MPEG-2 video with a VobSub track, as a DVD rip has it.
func TestWithTheTrackStoredAMidFilmRunOfAnMPEG2FilmWithVobSubDrawsTheCueShowing(t *testing.T) {
	cues := []testenv.Cue{top(6, 14), bottom(18, 90)}
	media := t.TempDir()
	cueFilm{codec: "dvdsub", mpeg2: true, seconds: 100, cues: cues}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	reference := s.fromZero(t, 100)
	checkReference(t, reference, cues)
	s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
	for _, cut := range []int{2, 5, 12, 20, 23} {
		count := min(2, 25-cut)
		s.seekStored(t, cut, count, true, 0)
		likeReference(t, "a run from segment "+itoa(cut)+" with the track stored", reference, s.halvesFrom(t, cut, cut+count), cut, edgesOf(cues)...)
		if want, judged := showing(cues, cut*4+1); judged {
			expectPicture(t, "a run from segment "+itoa(cut)+" with the track stored, at its first frames", s.halvesFrom(t, cut, cut+count), 1, want)
		}
	}
}

// A cue drawn from the stored track is the picture a run from zero draws: the
// same place, size and colours, on each track's own canvas (PGS on 1920x1080
// and on 1280x720, whose coordinates are not the same; VobSub, whose colours
// are its palette's; DVB), compared on the pixels of frames inside the cue.
func TestACueDrawnFromTheStoredTrackIsThePictureARunFromZeroDraws(t *testing.T) {
	small := testenv.Cue{Start: 6, End: 34, X: 440, Y: 70, W: 400, H: 80}
	for _, tc := range []struct {
		name string
		film cueFilm
	}{
		{"PGS", cueFilm{codec: "copy", cues: []testenv.Cue{bottom(6, 34)}}},
		{"PGS on a 1280x720 canvas", cueFilm{codec: "copy", sup: testenv.PGS(1280, 720, small)}},
		{"VobSub", cueFilm{codec: "dvdsub", cues: []testenv.Cue{top(6, 34)}}},
		{"DVB", cueFilm{codec: "dvbsub", cues: []testenv.Cue{bottom(6, 34)}}},
		{"PGS remuxed by mkvmerge", cueFilm{codec: "copy", remux: "default", cues: []testenv.Cue{top(6, 34)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			film := tc.film
			film.seconds = 40
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			s.fromZero(t, 40)
			reference := s.referenceFrames(t, 40)
			s.storeTrack(t, 1, 0)          // On a worker of its own: fromZero's has ended.
			s.seekStored(t, 4, 3, true, 0) // 16 s: inside the cue.
			run := frames(t, s.storedFrom(t, 4, 7))
			for _, second := range []float64{17, 20, 24, 27} { // Of the film; a segment's times are tsOffset later.
				sameBar(t, "a run from segment 4 with the track stored", at(t, reference, tsOffset+second), at(t, run, tsOffset+second))
			}
		})
	}
}

// transitions are the times at which what the frames show changes, from the
// frame whose time is at least from to the one whose time is below to.
func transitions(all []frame, from, to float64) (found []string) {
	last := "?"
	for _, f := range all {
		if f.at < from || f.at >= to {
			continue
		}
		state := barIn(f).state()
		if state != last && last != "?" {
			found = append(found, strconv.FormatFloat(f.at, 'f', 3, 64)+" "+state)
		}
		last = state
	}
	return found
}

// A cue drawn from the stored track appears and ends on the same video frame as
// in a run from zero, frame by frame at both edges, for a film whose clock
// starts at zero (Matroska, as ffmpeg writes it) and one whose does not
// (MPEG-TS). The cues are 6 to 14 s and 18 to 34 s, and the runs cover each
// edge: the run from segment 1 sees three, the one from 4 the cue's start at
// 18, and the one from 6 its end at 34. Under ffmpeg 6.1 (test/ffmpeg61.sh) and
// 9, whichever is on the PATH: the criterion names both, and CI runs the first.
func TestACueDrawnFromTheStoredTrackAppearsAndEndsOnTheSameFrameAsInARunFromZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		film cueFilm
	}{
		{"Matroska PGS", cueFilm{codec: "copy"}},
		{"Matroska VobSub", cueFilm{codec: "dvdsub"}},
		{"MPEG-TS DVB", cueFilm{codec: "dvbsub", container: "ts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			film := tc.film
			film.seconds, film.cues = 40, []testenv.Cue{top(6, 14), bottom(18, 34)}
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			s.fromZero(t, 40)
			reference := s.referenceFrames(t, 40)
			if edges := transitions(reference, 0, 1e9); len(edges) < 4 {
				t.Fatalf("a run from zero has %d changes of picture, want the cues' four edges at least: %v", len(edges), edges)
			}
			s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
			compared := 0
			for _, c := range []struct{ cut, count int }{{1, 4}, {4, 4}, {6, 3}} {
				s.seekStored(t, c.cut, c.count, true, 0)
				got := frames(t, s.storedFrom(t, c.cut, c.cut+c.count))
				// From the run's second frame to the start of its last second, so that a
				// run's own first and last frames (cut by the segment) are not judged.
				from, to := got[1].at, got[len(got)-1].at-0.5
				want, have := transitions(reference, from, to), transitions(got, from, to)
				if strings.Join(want, ",") != strings.Join(have, ",") {
					t.Errorf("a run from segment %d with the track stored changes its picture at %v; a run from zero does at %v (time, then what is shown)", c.cut, have, want)
				}
				compared += len(want)
			}
			if compared < 5 { // 6, 14 and 18 s in the first run, 18 in the second, 34 in the third.
				t.Errorf("only %d cue edges were compared", compared)
			}
		})
	}
}

// Every segment a run makes with the stored track has the frames, at the times
// (within 1 ms), of a run from zero's (invariant 22), for one spec whose earlier
// segments were made before the track was stored and whose later ones after,
// with the cue unbroken across the seam. The first worker runs the film's
// transcodes, a second runs its extractions with the film's reads held, as an
// extraction held in the middle of its read; the first makes segments 0 to 11
// at once, which is also the check that no job waits for the extraction (JS-071's
// reads and pictures are what a run draws before the track is stored). The seam
// is segment 12, 48 s into a cue from 6 to 56 s.
func TestASpecMadeBeforeAndAfterTheTrackIsStoredHasARunFromZerosFramesAndAnUnbrokenCue(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 88, cues: []testenv.Cue{top(6, 56)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	reference := s.fromZero(t, 88)
	checkReference(t, reference, []testenv.Cue{top(6, 56)})
	refTimes := s.refTimes
	s.forgetTrack(t)

	s.fresh(t)
	s.burnt()
	segments := t.TempDir()
	s.segments = store.NewPosix(segments)
	held := newMeters(s, 0)
	held.film.gate, held.film.first = make(chan struct{}), &atomic.Bool{}
	extractor := s.roleWorker(t, "subtitles", held)
	before := newMeters(s, 0)
	leader := s.roleWorker(t, "transcode", before)
	s.askForTracks(t, s.pool)
	waitFor(t, "the extraction to begin its read of the film", 60*time.Second, held.film.gated.Load)
	j := s.jobs(t)
	const seam = 12
	for n := 0; n < seam; n++ {
		if _, err := s.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d with the extraction held: %v", n, err)
		}
	}
	if got := s.storedObjects(); len(got) != 0 {
		t.Fatalf("the track was stored while its extraction was held: %v", got)
	}
	leader()
	for n := seam; n < seam+6; n++ { // What the leader made past the seam is lost with it.
		os.Remove(filepath.Join(segments, SegmentKey(s.hash, n)))
	}
	close(held.film.gate)
	s.waitStored(t, 1)
	extractor()

	after := newMeters(s, 0)
	stop := s.metered(t, after)
	defer stop()
	j = s.jobs(t)
	for n := seam; n < seam+8; n++ {
		if _, err := s.fetch(t, j, psid(0), n); err != nil {
			t.Fatalf("segment %d after the track was stored: %v", n, err)
		}
	}
	if after.assets.read.Load() <= 0 {
		t.Errorf("the run after the seam read nothing from the asset store: it did not draw from the stored track")
	}
	for n := 0; n < seam+8; n++ {
		compare(t, "segment "+itoa(n)+" against a run from zero's", refTimes[n], video(t, s.segments, s.hash, n), 0.001)
	}
	halves := s.halvesFrom(t, 0, seam+8)
	for _, second := range []int{9, 20, 40, 45, 47, 48, 49, 51, 54} {
		expectPicture(t, "the spec made before and after the track was stored", halves, second, "top")
	}
}
