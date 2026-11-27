//go:build integration

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-089: with the image track stored (see the kit), a seek into a showing cue
// costs the film's store no more than any seek: the cue's age is read from the
// asset store, where the whole track is a few kilobytes.

// ageCases are the films of the cue-age checks: a short cue early, and a cue
// that begins at 56 s, which is 8 s old at segment 16 and 56 s old at segment
// 28 (DVB's longest, as ffmpeg writes it, is 28 s: 24 s old at segment 20), and
// a PGS cue that is never cleared.
var ageCases = []struct {
	name       string
	film       cueFilm
	young, old int // The segments of the two cuts.
	ageOld     int
}{
	{"PGS", cueFilm{codec: "copy", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}, 16, 28, 56},
	{"VobSub", cueFilm{codec: "dvdsub", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}, 16, 28, 56},
	{"DVB", cueFilm{codec: "dvbsub", cues: []testenv.Cue{top(4, 8), bottom(56, 84)}}, 16, 20, 24},
	{"PGS remuxed by mkvmerge", cueFilm{codec: "copy", remux: "default", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}, 16, 28, 56},
	{"PGS never cleared", cueFilm{codec: "copy", sup: pgsNeverCleared(bottom(56, 0))}, 16, 28, 56},
}

// With the track stored a seek into a cue 8 s old, one 56 s old (24 s for DVB)
// and one that is never cleared reads no more of the film before its first
// segment than the same seek without the track, within the spread of repeated
// runs; the older cue's read is not above the younger's; and what the cue costs
// instead is read from the asset store, the stored track at most once, the same
// for both ages. JS-071's bound grew with the cue's age (0.3 MB for each second).
func TestWithTheTrackStoredASeekIntoALongCueReadsNoMoreOfTheFilmThanAnySeek(t *testing.T) {
	for _, tc := range ageCases {
		t.Run(tc.name, func(t *testing.T) {
			media := t.TempDir()
			film := tc.film
			film.seconds, film.noise = 128, "2M"
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			track := s.storeTrack(t, 1, 0)
			t.Logf("the extraction read %d bytes of the film in all (with the play that asked for it) and stored %d bytes", track.film, track.size)

			cuts := []int{tc.young, tc.old}
			plain := map[int][]int64{}
			burnt := map[int][]stored{}
			for _, cut := range cuts {
				for range 3 {
					plain[cut] = append(plain[cut], s.seekStored(t, cut, 1, false, readRate).film)
				}
			}
			for _, cut := range cuts {
				burnt[cut] = []stored{s.seekStored(t, cut, 1, true, readRate), s.seekStored(t, cut, 1, true, readRate)}
			}
			var youngWorst, oldWorst stored
			for _, cut := range cuts {
				_, most, width := spread(plain[cut]...)
				limit := most + width + readMargin
				for _, got := range burnt[cut] {
					t.Logf("segment %d: burnt in %d bytes of the film (without the track %v, limit %d), %d from the asset store (the track is %d)", cut, got.film, plain[cut], limit, got.assets, track.size)
					if got.film > limit {
						t.Errorf("a seek to segment %d with the track stored read %d bytes of the film before its first segment, %d more than the most a seek without the track did (%d, spread %d); %s", cut, got.film, got.film-most, most, width, got.reads)
					}
					if got.assets <= 0 || got.assets > track.size {
						t.Errorf("a seek to segment %d read %d bytes from the asset store, want the stored track (%d bytes) once at most and more than none", cut, got.assets, track.size)
					}
					if cut == tc.young && got.film > youngWorst.film {
						youngWorst = got
					}
					if cut == tc.old && got.film > oldWorst.film {
						oldWorst = got
					}
				}
			}
			_, _, youngWidth := spread(plain[tc.young]...)
			_, _, oldWidth := spread(plain[tc.old]...)
			if oldWorst.film > youngWorst.film+youngWidth+oldWidth+readMargin {
				t.Errorf("the seek into the older cue (%d s) read %d bytes of the film and the one into the younger cue (8 s) %d: an older cue must not cost more\nthe older cue's seek, %s\nthe younger cue's seek, %s", tc.ageOld, oldWorst.film, youngWorst.film, oldWorst.reads, youngWorst.reads)
			}
			// Only the track's own read shows in the cue-age difference of the two.
			if a, b := burnt[tc.young][0].assets, burnt[tc.old][0].assets; a-b > readMargin || b-a > readMargin {
				t.Errorf("the asset store served %d bytes for the cue 8 s old and %d for the one %d s old: it should be the same", a, b, tc.ageOld)
			}
		})
	}
}

// A failover restart begins inside the 56 s cue, on a worker that did not make
// the earlier segments, and reads no more of the film than the same restart
// without the track. The first worker makes segments 0 to 27 and ends; the
// segments from 28 are lost, as a kill after segment 27 leaves them.
func TestWithTheTrackStoredAFailoverRestartInsideALongCueReadsNoMoreOfTheFilm(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	track := s.storeTrack(t, 1, 0)
	rows := s.extractionRows(t)

	const cut = 28 // 112 s: the cue is 56 s old.
	restart := func(burnt bool) stored {
		s.fresh(t)
		dir := t.TempDir()
		s.segments = store.NewPosix(dir)
		if burnt {
			s.burnt()
		} else {
			s.plain()
		}
		first := newMeters(s, 0)
		stop := s.metered(t, first)
		j := s.jobs(t)
		for n := 0; n < cut; n++ {
			if _, err := s.fetch(t, j, psid(0), n); err != nil {
				t.Fatalf("segment %d: %v", n, err)
			}
		}
		stop() // The worker is gone; what it made past segment 27 is not there.
		for n := cut; n < cut+6; n++ {
			os.Remove(filepath.Join(dir, SegmentKey(s.hash, n)))
		}
		m := newMeters(s, readRate)
		m.film.cut.Store(filmPosition(t, media, (cut+1)*4))
		stop = s.metered(t, m)
		defer stop()
		served, err := s.firstSegment(t, s.jobs(t), m.film.read.Load, cut, 0)
		if err != nil {
			t.Fatalf("segment %d after the failover: %v", cut, err)
		}
		restarted := false
		for _, job := range s.jobsOf(t, s.hash) {
			restarted = restarted || job.Start == cut
		}
		if !restarted {
			t.Fatalf("no job began at segment %d, so the test did not restart inside the cue: %+v", cut, s.jobsOf(t, s.hash))
		}
		return stored{film: m.film.before.Load(), served: served, assets: m.assets.read.Load()}
	}
	var plain []int64
	for range 3 {
		plain = append(plain, restart(false).film)
	}
	_, most, width := spread(plain...)
	for range 2 {
		got := restart(true)
		t.Logf("a failover restart at segment %d read %d bytes of the film (without the track %v) and %d from the asset store (the track is %d)", cut, got.film, plain, got.assets, track.size)
		if got.film > most+width+readMargin {
			t.Errorf("a failover restart inside a cue 56 s old read %d bytes of the film, %d more than the most one without the track did (%d, spread %d)", got.film, got.film-most, most, width)
		}
		if got.assets <= 0 || got.assets > track.size {
			t.Errorf("a failover restart read %d bytes from the asset store, want the stored track (%d bytes) once at most and more than none", got.assets, track.size)
		}
	}
	s.noExtraction(t, "a failover restart", rows)
}

// A helper's chunk that begins inside the 56 s cue: a queued helper job of the
// spec at segment 28, which a worker with a viewer at that segment takes as it
// takes one a leader's chunk made. It reads no more of the film than the same
// chunk without the track.
func TestWithTheTrackStoredAHelpersChunkInsideALongCueReadsNoMoreOfTheFilm(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	track := s.storeTrack(t, 1, 0)
	rows := s.extractionRows(t)

	const cut = 28
	chunk := func(burnt bool) stored {
		s.fresh(t)
		if burnt {
			s.burnt()
		} else {
			s.plain()
		}
		m := newMeters(s, readRate)
		m.film.cut.Store(filmPosition(t, media, (cut+1)*4))
		// A helper's job: a viewer is at the segment, and the job is the spec's
		// helper from there. No leader's job exists, so the worker's claim is the chunk.
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, segment) VALUES ($1, $2, $3::uuid, 'device', $4)`,
			psid(0), s.hash, s.user, cut); err != nil {
			t.Fatal(err)
		}
		raw := s.specJSON(t)
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, state)
			VALUES ($1, $2, 'leader', $3, $3, true, $4, 'queued')`, s.hash, raw, cut, playback.OutputVersion); err != nil {
			t.Fatal(err)
		}
		stop := s.metered(t, m, func(w *Worker) { w.NoHelpers, w.ChunkSegments, w.Slots = false, 2, 4 })
		defer stop()
		served, err := s.firstSegment(t, s.jobs(t), m.film.read.Load, cut, 0)
		if err != nil {
			t.Fatalf("segment %d: %v", cut, err)
		}
		// Every job the spec has is the helper's, the one inserted above, and it began at the cut.
		if jobs := s.jobsOf(t, s.hash); len(jobs) != 1 || jobs[0].Start != cut || s.count(t, "SELECT count(*) FROM transcode_jobs WHERE helper AND id = $1", jobs[0].ID) != 1 {
			t.Fatalf("segment %d was not made by the helper's chunk alone: %+v", cut, jobs)
		}
		return stored{film: m.film.before.Load(), served: served, assets: m.assets.read.Load()}
	}
	var plain []int64
	for range 3 {
		plain = append(plain, chunk(false).film)
	}
	_, most, width := spread(plain...)
	for range 2 {
		got := chunk(true)
		t.Logf("a helper's chunk at segment %d read %d bytes of the film (without the track %v) and %d from the asset store (the track is %d)", cut, got.film, plain, got.assets, track.size)
		if got.film > most+width+readMargin {
			t.Errorf("a helper's chunk inside a cue 56 s old read %d bytes of the film, %d more than the most one without the track did (%d, spread %d)", got.film, got.film-most, most, width)
		}
		if got.assets <= 0 || got.assets > track.size {
			t.Errorf("a helper's chunk read %d bytes from the asset store, want the stored track (%d bytes) once at most and more than none", got.assets, track.size)
		}
	}
	s.noExtraction(t, "a helper's chunk", rows)
}

// Nothing a seek reads grows with the film's length once the track is stored:
// the same cue in a film four times as long costs the film's store and the
// asset store the same (the film is 1 Mbps noise, 64 s and 256 s, a cue from 4
// to 60 s, the run at 16 s).
func TestWithTheTrackStoredASeekReadsTheSameInAFilmFourTimesAsLong(t *testing.T) {
	const cut = 4
	extra, assets := map[int]int64{}, map[int]int64{}
	for _, seconds := range []int{64, 256} {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: seconds, noise: "1M", cues: []testenv.Cue{top(4, 60)}}.write(t, media, "film")
		s := newScanned(t, media, t.TempDir())
		track := s.storeTrack(t, 1, 0)
		burnt := s.seekStored(t, cut, 1, true, readRate)
		plain := []int64{s.seekStored(t, cut, 1, false, readRate).film, s.seekStored(t, cut, 1, false, readRate).film}
		_, most, _ := spread(plain...)
		extra[seconds], assets[seconds] = burnt.film-most, burnt.assets
		t.Logf("a %d s film: the track is %d bytes; a seek read %d bytes of the film, %d more than without the track (%v), and %d from the asset store", seconds, track.size, burnt.film, burnt.film-most, plain, burnt.assets)
	}
	if diff := extra[256] - extra[64]; diff > readMargin || diff < -readMargin {
		t.Errorf("a seek read %d bytes more than without the track in the 256 s film and %d in the 64 s one: it grows with the film (by %d bytes)", extra[256], extra[64], diff)
	}
	if diff := assets[256] - assets[64]; diff > readMargin || diff < -readMargin {
		t.Errorf("a seek read %d bytes from the asset store for the 256 s film and %d for the 64 s one: it grows with the film (by %d bytes)", assets[256], assets[64], diff)
	}
}

// A track that has a cue all through the film grows with the film (here a
// full-canvas cue every 2 s, 6.5 KB of PGS each), and what a seek reads from
// the asset store must not grow with it: a build that copies the whole track
// before ffmpeg starts, as a file beside the film is copied, reads 4 times as
// much for the 256 s film as for the 64 s one before its first segment, and a
// re-encode starts an ffmpeg every 16 s (defaultChunkSegments), so a build that
// copies it per start reads it again for each. The seek is at 16 s, where the cue
// from 15.5 s shows, in both films; the 16 segments from it are four chunks.
func TestWithTheTrackStoredASeekReadsTheSameFromTheAssetStoreWhateverTheTracksSize(t *testing.T) {
	const cut, count = 4, 16
	cuesAllThrough := func(seconds int) (cues []testenv.Cue) {
		for at := 0.0; at+3.5 < float64(seconds); at += 2 {
			cues = append(cues, testenv.Cue{Start: at + 1.5, End: at + 3.4, X: 0, Y: 0, W: 1920, H: 1080})
		}
		return cues
	}
	var size, first, all [2]int64
	for i, seconds := range []int{64, 256} {
		media := t.TempDir()
		cueFilm{codec: "copy", seconds: seconds, noise: "1M", cues: cuesAllThrough(seconds)}.write(t, media, "film")
		s := newScanned(t, media, t.TempDir())
		track := s.storeTrack(t, 1, 0)
		// The 64 s film ends at segment 15: its run is the twelve segments it has from
		// the cut, and the 256 s film's the sixteen that the last check below counts.
		segments := min(count, seconds/4-cut)
		run := s.seekStored(t, cut, segments, true, readRate)
		size[i], first[i], all[i] = track.size, run.assets, run.assetsAll
		t.Logf("a %d s film: the track is %d bytes; the seek read %d bytes from the asset store before its first segment and %d in all for %d segments", seconds, size[i], first[i], all[i], segments)
	}
	// Without this the film's length would not show in what is read.
	if size[1]-size[0] < 2*readMargin {
		t.Fatalf("the track is %d bytes for the 256 s film and %d for the 64 s one: too close to show a read that grows with it", size[1], size[0])
	}
	if diff := first[1] - first[0]; diff > readMargin || diff < -readMargin {
		t.Errorf("the seek read %d bytes from the asset store before its first segment for the 256 s film and %d for the 64 s one (tracks of %d and %d bytes): it grows with the film", first[1], first[0], size[1], size[0])
	}
	// The whole track once is what one job may read; a copy for each ffmpeg of
	// the four chunks is not.
	if all[1] > size[1]+readMargin {
		t.Errorf("sixteen segments (four chunks) read %d bytes from the asset store for a track of %d bytes: it is read again for each start of ffmpeg", all[1], size[1])
	}
}
