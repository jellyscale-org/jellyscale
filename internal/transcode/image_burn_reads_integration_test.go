//go:build integration

package transcode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// What a run reads of the film before its first segment is what a seek costs
// the store (from S3, the bytes and the wait). These tests count it through a
// worker's job, on a film of 2 Mbps noise (250 kB a second) that the store
// serves at 32 times that (so a build that reads the whole film is slow to
// fail, not slow to measure). The count when the first segment appears is
// compared with the same run without the track, repeated for its spread. A run
// without the track reads about 7 MB at any cut: ffmpeg's probe of the head.
// No check is on a duration (the benchmark in docs/transcoding.md records those).
const (
	readRate   = 8_000_000
	perSecond  = 2_000_000 / 8
	readMargin = 256 << 10 // Container and probe reads that differ by a block or two.
	// What the track itself costs a run that has no cue to read: origin/main
	// read 1.1 to 1.25 MB more than the same run without the track (measured
	// for PGS and VobSub), and a look-back of ten seconds would add 2.5 MB.
	trackCost = 1536 << 10
)

// A run that begins where no cue is showing, 50 s or more after the last one
// ended, reads no more of the film before its first segment than the same run
// without the track (and what the track itself costs, trackCost), whichever way the cue ended: a PGS clear
// packet, a VobSub duration, a DVB clear packet that carries a duration. The
// first run of the film that begins mid-film is among them, and so are the
// later ones, so a read made once per film shows in the first and a read made
// every run in all.
func TestARunWhereNoCueIsShowingReadsNoMoreThanOneWithoutTheTrack(t *testing.T) {
	for _, tc := range imageCases {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			cueFilm{codec: tc.codec, remux: tc.remux, seconds: 100, noise: "2M", cues: []testenv.Cue{top(5, 15)}}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			const cut = 20                                      // 80 s: the cue ended 65 s before.
			burnt := []int64{s.seek(t, cut, 1, true, readRate)} // The first mid-film run of the film.
			var plain []int64
			for range 3 {
				plain = append(plain, s.seek(t, cut, 1, false, readRate))
			}
			burnt = append(burnt, s.seek(t, cut, 1, true, readRate), s.seek(t, cut, 1, true, readRate))
			_, most, width := spread(plain...)
			t.Logf("bytes read before segment %d: without the track %v (spread %d); burnt in, the first run of the film and two later ones %v", cut, plain, width, burnt)
			for i, got := range burnt {
				if got > most+width+trackCost {
					t.Errorf("burnt run %d read %d bytes before its first segment, %d more than the most a run without the track did (%d, spread %d): nothing is showing, so it should cost no more than the track itself does (%d)", i+1, got, got-most, most, width, int64(trackCost))
				}
			}
		})
	}
}

// insideLimit is the most a burnt run may read before its first segment, given
// what the same run without the track read, inside a cue age seconds old: the
// film from the cue's first packet to the cut, and the keyframe before the
// packet, with a third more for the second input and what the video decodes
// ahead, and 3 MB for the connection that seeks to the packet (measured on
// origin/main-like designs: 1.1 to 1.6 times the span).
// filmPosition is the byte position in the film (the one file named film.* in
// media that is not a subtitle) of its first video packet at or after second,
// or the film's size if there is none.
func filmPosition(t *testing.T, media string, second int) int64 {
	t.Helper()
	films, _ := filepath.Glob(filepath.Join(media, "film.*"))
	var film string
	for _, f := range films {
		if ext := filepath.Ext(f); ext == ".mkv" || ext == ".ts" {
			film = f
		}
	}
	info, err := os.Stat(film)
	if err != nil {
		t.Fatalf("no film in %s (%v): %v", media, films, err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-read_intervals", fmt.Sprintf("%d%%", second),
		"-show_entries", "packet=pts_time,pos", "-of", "csv=p=0", film).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", film, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var at float64
		var pos int64
		if _, err := fmt.Sscanf(strings.Replace(line, ",", " ", 1), "%f %d", &at, &pos); err == nil && at >= float64(second) {
			return pos
		}
	}
	return info.Size()
}

func insideLimit(plain []int64, age int) int64 {
	_, most, width := spread(plain...)
	return most + width + int64(age+3)*perSecond*13/10 + 3<<20
}

// A run that begins inside a cue reads, beyond the same run without the track,
// at most the film from that cue's first packet to the cut (and the keyframe
// before the packet: three seconds in all), for a cue 8 seconds old and one
// about 50 (24 for DVB, whose cues ffmpeg writes to last 30 seconds at most).
func TestARunInsideACueReadsOnlyTheFilmFromTheCuesFirstPacket(t *testing.T) {
	for _, tc := range imageCases {
		t.Run(tc.name, func(t *testing.T) {
			cues := []testenv.Cue{top(4, 8), bottom(56, 120)}
			old := 28 // Segment 28 is 112 s: the cue is 56 s old.
			if tc.codec == "dvbsub" {
				cues, old = []testenv.Cue{top(4, 8), bottom(56, 84)}, 20 // 80 s: 24 s old.
			}
			media := t.TempDir()
			cueFilm{codec: tc.codec, remux: tc.remux, seconds: 128, noise: "2M", cues: cues}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			cuts := []int{16, old} // 64 s: the cue is 8 s old.
			burnt := map[int][]int64{}
			plain := map[int][]int64{}
			served := map[int][]int64{}
			run := func(cut int, track bool) {
				n := s.seek(t, cut, 1, track, readRate)
				if track {
					burnt[cut] = append(burnt[cut], n)
					served[cut] = append(served[cut], s.served)
				} else {
					plain[cut] = append(plain[cut], n)
				}
			}
			for _, cut := range cuts { // The first runs of the film begin mid-film, one after the other.
				run(cut, true)
			}
			for _, cut := range cuts {
				run(cut, false)
				run(cut, false)
				run(cut, true)
			}
			for _, cut := range cuts {
				age := cut*4 - 56
				_, most, width := spread(plain[cut]...)
				limit := insideLimit(plain[cut], age)
				t.Logf("segment %d, a cue %d s old: read for the first segment without the track %v; burnt in (the first run of the film, then a later one) %v; the limit is %d; read in all by the first segment, burnt in, %v", cut, age, plain[cut], burnt[cut], limit, served[cut])
				for _, got := range burnt[cut] {
					if got > limit {
						t.Errorf("a run at segment %d, inside a cue %d s old, read %d bytes for its first segment, %d more than without the track: over the %d seconds of film from the cue's first packet to the cut (%d bytes, with the allowance)", cut, age, got, got-most, age+3, limit-most-width)
					}
				}
			}
		})
	}
}

// The first run that begins mid-film on a film reads the same, before its first
// segment and until it is done reading (so a scan of the whole film for the track's
// packets, in the foreground or the background, shows), for the same cue in a
// film four times as long: nothing it does grows with the film. The film is
// 1 Mbps noise, 64 s and 256 s, with a cue from 4 to 60 s and the run at 16 s;
// the job stops reading 8 s of film past the segment asked for.
func TestTheFirstMidFilmRunReadsTheSameInAFilmFourTimesAsLong(t *testing.T) {
	const cut = 4 // 16 s: 12 s into the cue, and the job's whole range is inside both films.
	extra, total := map[int]int64{}, map[int]int64{}
	for _, seconds := range []int{64, 256} {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: seconds, noise: "1M", cues: []testenv.Cue{top(4, 60)}}.write(t, media, "film")
		s := newScanned(t, media, t.TempDir())
		s.waitEnd = true
		burnt := s.seek(t, cut, 1, true, readRate)
		total[seconds] = s.total
		s.waitEnd = false
		plain := []int64{s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate)}
		_, most, _ := spread(plain...)
		extra[seconds] = burnt - most
		t.Logf("a %d s film: the first run that begins mid-film read %d bytes before its first segment, %d more than without the track (%v), and %d by the time the store was quiet", seconds, burnt, burnt-most, plain, total[seconds])
	}
	if diff := extra[256] - extra[64]; diff > 1<<20 || diff < -(1<<20) {
		t.Errorf("before its first segment, the first mid-film run read %d bytes more than without the track in the 256 s film and %d in the 64 s one: it grows with the film (by %d bytes)", extra[256], extra[64], diff)
	}
	if diff := total[256] - total[64]; diff > 2<<20 || diff < -(2<<20) {
		t.Errorf("by the time the store was quiet, the first mid-film run had read %d bytes of the 256 s film and %d of the 64 s one: something it does grows with the film (by %d bytes)", total[256], total[64], diff)
	}
}

// A run from segment 0 reads nothing it does not read today: origin/main read
// 1.4 to 2.8 MB more than the same run without the track (the head of the film
// holds the track's first packets), and 4 MB is the most a run may read more.
func TestARunFromZeroWithABurntImageTrackReadsNoMoreThanOneWithout(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 100, noise: "2M", cues: []testenv.Cue{top(2, 15), bottom(40, 60)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	burnt := []int64{s.seek(t, 0, 1, true, readRate), s.seek(t, 0, 1, true, readRate)}
	plain := []int64{s.seek(t, 0, 1, false, readRate), s.seek(t, 0, 1, false, readRate), s.seek(t, 0, 1, false, readRate)}
	_, most, width := spread(plain...)
	t.Logf("a run from segment 0 read %v bytes burnt in and %v without the track", burnt, plain)
	for _, got := range burnt {
		if got > most+width+4<<20 {
			t.Errorf("a run from segment 0 read %d bytes with the track burnt in, %d more than the most a run without it did (%d, spread %d)", got, got-most, most, width)
		}
	}
}

// A run whose image track is a file beside the film reads nothing more than
// one without the track, from zero and from the middle: the track is read from
// its own copy, not from the film.
func TestARunWhoseImageTrackIsAFileBesideTheFilmReadsNoMoreThanOneWithout(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 100, noise: "2M", beside: true, cues: []testenv.Cue{top(5, 15), bottom(50, 90)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.besideTrack(t)
	for _, cut := range []int{0, 20} { // 80 s is inside the second cue.
		burnt := []int64{s.seek(t, cut, 1, true, readRate), s.seek(t, cut, 1, true, readRate)}
		plain := []int64{s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate)}
		_, most, width := spread(plain...)
		slack := int64(readMargin)
		if cut == 0 {
			slack = 4 << 20 // As for a track inside the film: origin/main read 1.8 MB more.
		}
		t.Logf("a run from segment %d read %v bytes with the track beside the film burnt in and %v without it", cut, burnt, plain)
		for _, got := range burnt {
			if got > most+width+slack {
				t.Errorf("a run from segment %d read %d bytes with the track beside the film burnt in, %d more than the most a run without it did (%d, spread %d)", cut, got, got-most, most, width)
			}
		}
	}
}
