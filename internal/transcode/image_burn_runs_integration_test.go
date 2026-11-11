//go:build integration

package transcode

import (
	"fmt"
	"os"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-071: a burnt image subtitle in every run. The film's cues are a bar in the
// top half from 6 to 10 seconds and one in the bottom half from 22 to 30, and
// its keyframes are two seconds apart, so a run that begins at segment 6 (24 s)
// or 7 (28 s) begins inside the bottom cue, after its packet. Each test is a
// worker's job (processes for the failover and the helpers) on a film the scan
// has read, and runs for PGS, VobSub and DVB.
var shortCues = []testenv.Cue{top(6, 10), bottom(22, 30)}

// shortFilm writes the 40 s film of those cues and returns the rig on it.
func shortFilm(t *testing.T, codec string) *scannedFilm {
	t.Helper()
	media := t.TempDir()
	cueFilm{codec: codec, seconds: 40, cues: shortCues}.write(t, media, "film")
	return newScanned(t, media, t.TempDir())
}

// expectShort fails the test for each second of a joined run (from film second
// first) that shows other than the cues call for.
func expectShort(t *testing.T, what string, halves [][2]int, first int) {
	t.Helper()
	judged := 0
	for i, half := range halves {
		if want, ok := showing(shortCues, first+i); ok {
			judged++
			if got := picture(half); got != want {
				t.Errorf("%s, second %d: bright pixels top %d, bottom %d, which is %q; want %q", what, first+i, half[0], half[1], got, want)
			}
		}
	}
	if judged < 4 {
		t.Errorf("%s: only %d seconds could be judged", what, judged)
	}
}

// A viewer who seeks into a cue (the job begins at segment 6, 24 seconds in,
// after the cue's packet at 22) sees the cue from the first frame the job
// makes, and the segments have a run from zero's frames.
func TestASeekIntoABurntImageCueShowsItFromTheFirstFrame(t *testing.T) {
	for _, tc := range imageCodecs {
		t.Run(tc.name, func(t *testing.T) {
			s := shortFilm(t, tc.codec)
			s.fromZero(t, 40)
			s.seek(t, 6, 2, true, 0)
			if jobs := s.jobsOf(t, s.hash); len(jobs) != 1 || jobs[0].Start != 6 {
				t.Fatalf("the seek was not a job that began at segment 6: %+v", jobs)
			}
			// Segments 6 and 7 are 24 to 32 seconds: the bar until 30, then none.
			expectShort(t, "after a seek to segment 6", s.halvesFrom(t, 6, 8), 24)
			for n := 6; n < 8; n++ {
				compare(t, fmt.Sprintf("segment %d's frames against a run from zero's", n), s.refTimes[n], video(t, s.segments, s.hash, n), 0.001)
			}
		})
	}
}

// A failover (the worker dies after segment 5, and the replacement begins at
// the first missing segment, 6, inside the cue) draws the cue from its first
// frame, and the film splices: the segments the first worker made and the
// replacement's show the bar throughout, and the replacement's have a run from
// zero's frames.
func TestAFailoverInsideABurntImageCueShowsItFromTheFirstFrame(t *testing.T) {
	for _, tc := range imageCodecs {
		t.Run(tc.name, func(t *testing.T) {
			s := shortFilm(t, tc.codec)
			s.fromZero(t, 40)
			s.fresh(t)
			path := os.Getenv("PATH")
			slow(t) // The first worker is at segment 5 and no further when it is killed.
			segments := s.ownStore(t)
			// No helpers: the replacement has to be the one job that begins at segment 6.
			a := s.workerProcess(t, "worker-a", segments, "JELLYSCALE_TEST_NO_HELPERS=1")
			j := s.jobs(t)
			for n := 0; n <= 5; n++ {
				if _, err := s.fetchAt(t, j, psid(0), n, 0); err != nil {
					t.Fatalf("segment %d: %v", n, err)
				}
			}
			a.Process.Kill() //nolint:errcheck
			s.workerProcess(t, "worker-b", segments, "JELLYSCALE_TEST_NO_HELPERS=1")
			for n := 6; n <= 7; n++ {
				if _, err := s.fetchAt(t, j, psid(0), n, 0); err != nil {
					t.Fatalf("segment %d after the kill: %v", n, err)
				}
			}
			restarted := false
			for _, job := range s.jobsOf(t, s.hash) {
				if job.Worker == "worker-b" && job.Start == 6 {
					restarted = true
				}
			}
			if !restarted {
				t.Fatalf("no job of worker-b began at segment 6, so the test did not restart inside the cue: %+v", s.jobsOf(t, s.hash))
			}
			t.Setenv("PATH", path) // The pictures are looked at at full speed.
			expectShort(t, "after a failover at segment 6", s.halvesFrom(t, 0, 8), 0)
			for n := 6; n <= 7; n++ {
				compare(t, fmt.Sprintf("segment %d's frames against a run from zero's", n), s.refTimes[n], video(t, s.segments, s.hash, n), 0.001)
			}
		})
	}
}

// A slow re-encode with a burnt image subtitle, inside the film or in a file
// beside it, gets helpers as the same film without the track does, whose chunks
// begin inside a cue (chunks of two segments begin at segments 2, 4, 6 and 8,
// which are 8, 16, 24 and 32 seconds in: inside the top cue, outside, inside
// the bottom cue, outside), and the film is the same as one worker's: every cue
// is on screen at its time, and every segment has the frames, at the times, of
// a single worker's run from zero, whichever run made it.
func TestABurntImageSubtitleGetsHelpers(t *testing.T) {
	const segments = 10
	for _, tc := range append(imageCodecs, struct{ name, codec string }{"PGS in a file beside the film", "beside"}) {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			cueFilm{codec: tc.codec, seconds: 40, cues: shortCues, beside: tc.codec == "beside"}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			if tc.codec == "beside" {
				s.besideTrack(t)
			}
			// One worker at full speed: no helper can start. This is the reference.
			s.fromZero(t, 40)
			if jobs := s.jobsOf(t, s.hash); len(jobs) != 1 {
				t.Fatalf("the reference run took %d jobs, want the one: %+v", len(jobs), jobs)
			}
			s.fresh(t)
			path := os.Getenv("PATH")
			slow(t)
			s.workers(t, 2, 2, 2, s.ownStore(t))
			j := s.jobs(t)
			s.playThrough(t, j, psid(0), 0, segments)
			t.Setenv("PATH", path) // The pictures are looked at at full speed.
			inCue, outside := false, false
			for _, job := range s.jobsOf(t, s.hash) {
				if job.Next <= job.Start {
					continue
				}
				helper := s.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper AND id = $1", job.ID) == 1
				switch {
				case helper && (job.Start == 2 || job.Start == 6):
					inCue = true
				case helper && (job.Start == 4 || job.Start == 8):
					outside = true
				}
			}
			if !inCue || !outside {
				t.Errorf("no helper's chunk began inside a cue (%v) or where nothing is showing (%v): %+v", inCue, outside, s.jobsOf(t, s.hash))
			}
			for n := 0; n < segments; n++ {
				compare(t, fmt.Sprintf("segment %d's frames against a single worker's", n), s.refTimes[n], video(t, s.segments, s.hash, n), 0.001)
			}
			expectShort(t, "with helpers", s.halvesFrom(t, 0, segments), 0)
		})
	}
}
