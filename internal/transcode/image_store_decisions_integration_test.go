//go:build integration

package transcode

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// The product owner's decisions on the stored image track (JS-089): D-003, the
// whole-film read is paid once per file version; D-002, a film too big to read
// in the time limit is not read again by anything but a play, once a day.

// sweptRows ages every finished subtitle job by two days and runs the sweep of
// jobs, as the daily sweep does.
func (s *scannedFilm) sweptRows(t *testing.T) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET ended_at = ended_at - interval '2 days' WHERE ended_at IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: s.pool, Scratch: t.TempDir()}
	if err := w.sweep(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
}

// noReadAgain asks as a play does and says the film was neither queued nor read.
func (s *scannedFilm) noReadAgain(t *testing.T, what string) {
	t.Helper()
	m := newMeters(s, 0)
	stop := s.metered(t, m)
	defer stop()
	before := s.extractionRows(t)
	s.askForTracks(t, s.pool)
	time.Sleep(3 * time.Second) // Long enough for a worker to claim what was queued.
	if after := s.extractionRows(t); len(after) != len(before) || s.jobRowsIn(t, "queued")+s.jobRowsIn(t, "running") != 0 {
		t.Errorf("%s: a play queued an extraction: rows %v before, %v after", what, before, after)
	}
	if got, opens := m.film.read.Load(), m.film.opens.Load(); got != 0 || opens != 0 {
		t.Errorf("%s: a play read the film (%d bytes, %d opens)", what, got, opens)
	}
}

// D-003: a film whose tracks are stored, or are recorded as not extractable, is
// never read whole again after the daily sweep has removed its finished job.
func TestAFilmWhoseTracksAreStoredOrLeftOutIsNotReadWholeAgainAfterTheSweepOfJobs(t *testing.T) {
	for _, phantom := range []bool{false, true} {
		name := "tracks stored"
		if phantom {
			name = "a track left out"
		}
		t.Run(name, func(t *testing.T) {
			media := t.TempDir()
			cueFilm{codec: "copy", seconds: 30, noise: "1M", cues: []testenv.Cue{top(6, 25)}}.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			if phantom { // A track the film has no stream for, which ffmpeg names and the job leaves out.
				if _, err := s.pool.Exec(context.Background(), `UPDATE media_sources SET streams = streams || jsonb_build_array((SELECT elem || '{"Index": 9}'::jsonb
					FROM jsonb_array_elements(streams) elem WHERE elem->>'Index' = '2'))`); err != nil {
					t.Fatal(err)
				}
			}
			s.fresh(t)
			s.burnt()
			stop := s.metered(t, newMeters(s, 0))
			s.askForTracks(t, s.pool)
			s.waitStored(t, 1)
			stop()
			s.sweptRows(t)
			if phantom && s.jobRowsIn(t, "done") != 1 {
				t.Errorf("the sweep removed the record of the track the film can't give: %v", s.extractionRows(t))
			}
			s.noReadAgain(t, name)
		})
	}
}

// D-002: a film whose image tracks could not be read in the time limit (its job
// failed for good, as the worker records it) draws the cue showing at a seek
// from its first frame through JS-071's path, as TestNoRunWaitsForTheExtractionAndEachDrawsAsJS071Does
// holds for a track not stored yet, and reads no more of the film than JS-071 does.
func TestAFilmWhoseExtractionRanOutOfTimeDrawsAsJS071Does(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", dim: true, cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	const cut = 28 // 112 s: the cue is 56 s old.
	var plain []int64
	for range 3 {
		plain = append(plain, s.seek(t, cut, 1, false, readRate))
	}
	s.fresh(t)
	s.burnt()
	src := s.source(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, permanent, error, ended_at)
		VALUES ($1, 'images', $2, $3, 0, 'failed', true, 'reading the file took longer than the time limit', now())`, subtitles.ImagesKey(src), s.spec.Item, src.ID); err != nil {
		t.Fatal(err)
	}
	got := s.seekWhileHeld(t, cut, 2)
	if limit := insideLimit(plain, 56); got.film > limit {
		t.Errorf("a seek into a cue 56 s old, the film's extraction having run out of time, read %d bytes before its first segment, over JS-071's bound of %d", got.film, limit)
	}
	expectPicture(t, "a seek, the film's extraction having run out of time", s.halvesFrom(t, cut, cut+2), 1, "bottom")
	if got := s.storedObjects(); len(got) != 0 {
		t.Fatalf("a track was stored for a film whose extraction ran out of time: %v", got)
	}
}

// D-004: the request a viewer waits on for a picture, the first segment of a play
// that burns an image track, is answered while the film's image track job is held
// running: the play's requests (which ask for the job, as the playlists do) wait
// for nothing, and a worker makes the segment. A build whose ask waited for the job
// hangs here until the bound, which opens the gate.
func TestTheFirstSegmentOfAPlayIsAnsweredWhileTheImageTrackJobIsHeld(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 60, noise: "1M", cues: []testenv.Cue{top(6, 50)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.fresh(t)
	s.burnt()
	held := newMeters(s, 0)
	held.film.gate, held.film.first = make(chan struct{}), &atomic.Bool{}
	s.roleWorker(t, "subtitles", held)
	var once sync.Once
	release := func() { once.Do(func() { close(held.film.gate) }) }
	defer release()

	ask := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- StartImageTracks(context.Background(), s.pool, store.NewPosix(s.assets), s.spec.Item, s.source(t))
		}()
		return done
	}
	answered := func(done <-chan error, what string) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
		case <-time.After(60 * time.Second): // Only the hang bound.
			release()
			t.Fatalf("%s waited for the image track job, which is held", what)
		}
	}
	first := ask()
	waitFor(t, "the extraction to begin its read of the film", 60*time.Second, held.film.gated.Load)
	answered(first, "a play's request for the film's image tracks")
	answered(ask(), "a second request of the play, with the job running")
	if got := s.jobRowsIn(t, "running"); got != 1 {
		t.Fatalf("%d jobs running, want the held image track job", got)
	}
	got := s.seekWhileHeld(t, 0, 1) // The first segment: its own transcode worker makes it.
	if got.served <= 0 {
		t.Errorf("the first segment was made from %d bytes of the film", got.served)
	}
	if got := s.storedObjects(); len(got) != 0 {
		t.Errorf("the track was stored while its extraction was held: %v", got)
	}
}
