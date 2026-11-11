//go:build integration

package transcode

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// Two runs of one film that begin at once on two workers (a helper and a seek,
// or two helpers) each stay within one run's bound, and neither waits for a
// read the other makes: the worker whose read comes first is held (its store
// serves nothing until the test lets it), and the other worker's run must make
// its first segment meanwhile. The film is new to both, so neither has times
// that the other stored; the bound is the one of the single-run check.
func TestTwoMidFilmRunsAtOnceOnTwoWorkersEachStayWithinOneRunsBound(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	const young, old = 16, 28 // 8 and 56 s into the cue.
	plain := map[int][]int64{}
	for _, cut := range []int{young, old} {
		plain[cut] = []int64{s.seek(t, cut, 1, false, readRate), s.seek(t, cut, 1, false, readRate)}
	}

	s.fresh(t)
	s.burnt()
	gate, first := make(chan struct{}), &atomic.Bool{}
	a, b := newMeter(media, readRate), newMeter(media, readRate)
	for _, m := range []*meter{a, b} {
		m.gate, m.first = gate, first
	}
	s.worker(t, a)
	s.worker(t, b)
	j := s.jobs(t)
	type result struct {
		read int64
		err  error
	}
	heldRun := make(chan result, 1)
	go func() {
		read, err := s.firstSegment(t, j, func() int64 {
			if a.gated.Load() {
				return a.before.Load()
			}
			return b.before.Load()
		}, young, 0)
		heldRun <- result{read, err}
	}()
	waitFor(t, "the first run's worker to open the film", 60*time.Second, first.Load)
	// Each run is measured as seek measures the runs without the track: the bytes
	// it read from below the end of its first segment's place in the film, which
	// ffmpeg's read-ahead cannot move (JS-071's lead correction). The held
	// worker's reads wait for the gate, so its position is set before any count.
	heldMeter, freeMeter := a, b
	if b.gated.Load() {
		heldMeter, freeMeter = b, a
	}
	heldMeter.cut.Store(filmPosition(t, media, (young+1)*4))
	freeMeter.cut.Store(filmPosition(t, media, (old+1)*4))
	// The second run is the other worker's. Its reads are its own, so it must
	// finish while the first worker's are held.
	other := func() *meter {
		if a.gated.Load() {
			return b
		}
		return a
	}
	read, err := s.firstSegment(t, j, func() int64 { return other().before.Load() }, old, 1)
	if err != nil {
		t.Fatalf("the second run made no first segment while the first worker's reads were held: %v", err)
	}
	select {
	case r := <-heldRun:
		t.Fatalf("the held run made its segment (%d bytes, %v) with its reads held: the test did not hold it", r.read, r.err)
	default:
	}
	close(gate)
	held := <-heldRun
	if held.err != nil {
		t.Fatalf("the held run: %v", held.err)
	}
	t.Logf("two runs at once: the segment at %d (age 56 s) read %d bytes on the free worker, %d without the track; the segment at %d (age 8 s) read %d on the held worker, %d without the track", old, read, plain[old], young, held.read, plain[young])
	if limit := insideLimit(plain[old], 56); read > limit {
		t.Errorf("the run at segment %d, 56 s into the cue, read %d bytes before its first segment on its worker, over the single-run bound of %d", old, read, limit)
	}
	if limit := insideLimit(plain[young], 8); held.read > limit {
		t.Errorf("the run at segment %d, 8 s into the cue, read %d bytes before its first segment on its worker, over the single-run bound of %d", young, held.read, limit)
	}
}

// If learning where the cue begins takes a read that fails for a reason that
// is not the film's, the run does not go on as if nothing were showing: no
// segment is stored without the cue a run from zero draws, and the failure is
// not remembered as the film's, so the next request (here of another spec, so
// that the job failure memory of the first does not stand in the way) draws it.
// Each Open of the film, from the first on, fails once in turn, so whichever
// read the build makes first is among them; the film is 40 s with a cue from 4
// to 36 s, and the runs are at 32 s.
func TestAFailedReadOfTheCueTimesNeverLeavesASegmentWithoutTheCue(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 40, cues: []testenv.Cue{bottom(4, 36)}}.write(t, media, "film")
	const cut = 8
	drawn := func(s *scannedFilm, what string) (stored int) {
		for n := cut; n < cut+2; n++ {
			if body, err := s.segments.Open(context.Background(), SegmentKey(s.hash, n), 0); err != nil {
				continue
			} else {
				body.Close()
			}
			if body, err := s.segments.Open(context.Background(), SegmentKey(s.hash, -1), 0); err != nil {
				continue
			} else {
				body.Close()
			}
			stored++
			// Seconds 32 to 34 of the film are inside the cue, in segment 8's first seconds.
			if n == cut {
				expectPicture(t, what+": segment 8", s.halvesFrom(t, n, n+1), 1, "bottom")
			}
		}
		return stored
	}
	for opens := int64(1); opens <= 8; opens++ {
		s := newScanned(t, media, t.TempDir())
		s.fresh(t)
		s.burnt()
		failing := newMeter(media, 0)
		failing.failing.Store(opens)
		stop := s.worker(t, failing)
		j := s.jobs(t)
		_, err := s.firstSegment(t, j, failing.read.Load, cut, 0)
		what := "when Open " + itoa(int(opens)) + " of the film fails once"
		t.Logf("%s: the request ended with %v; %d opens", what, err, failing.opens.Load())
		drawn(s, what)
		stop()

		// The store is well again; another spec (a new hash) is asked for.
		s.fresh(t)
		s.spec.VideoBitrate++
		s.burnt()
		healthy := newMeter(media, 0)
		stop = s.worker(t, healthy)
		if _, err := s.firstSegment(t, s.jobs(t), healthy.read.Load, cut, 0); err != nil {
			t.Errorf("%s: a later request with the store well failed: %v", what, err)
		} else if drawn(s, what+", and the next request") == 0 {
			t.Errorf("%s: a later request with the store well stored no segment", what)
		}
		stop()
	}
}

// The read ends with its job: when the last viewer stops, every read that the
// job began beyond those of the same run without the track (the cue times' read,
// whatever it is and wherever it runs) is over soon after, with its body
// closed, though the store never answers. origin/main leaves the reads of the
// run's own first probe hanging the same, which this does not count.
func TestEveryReadOfAMidFilmRunEndsWithItsJob(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 60, noise: "1M", cues: []testenv.Cue{top(4, 56)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	left := func(burnt bool) int64 {
		s.fresh(t)
		if burnt {
			s.burnt()
		} else {
			s.plain()
		}
		m := newMeter(media, 0)
		m.hold.Store(true) // The store never answers.
		stop := s.worker(t, m)
		defer stop()
		j := s.jobs(t)
		go s.firstSegment(t, j, m.read.Load, 10, 0) //nolint:errcheck
		waitFor(t, "the job to be waiting on the store", 60*time.Second, func() bool { return m.held.Load() > 0 })
		time.Sleep(2 * time.Second) // Whatever else the job starts reading starts by now.
		if err := j.Stop(context.Background(), s.user, psid(0), "device", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second) // The job's reads end within a heartbeat or two; this is a hang bound with room.
		return m.held.Load() + m.live.Load()
	}
	plain := left(false)
	if burnt := left(true); burnt > plain {
		t.Errorf("%d reads of the film were still open 10 s after the job was stopped with the track burnt in, against %d without it: a read the track's cues cost outlives its job", burnt, plain)
	}
}
