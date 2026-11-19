//go:build integration

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-089: runs while the track is not stored, runs that cannot read it, films
// with no cue points, the previous release's films, and the sweep.

// heldExtraction asks for the extraction as a play does and holds its read of the
// film (a worker of its own runs only extractions, and its first read of the
// film waits), so that runs meanwhile are runs while the track is being
// extracted. release lets it go on.
func (s *scannedFilm) heldExtraction(t *testing.T) (release func()) {
	t.Helper()
	held := newMeters(s, 0)
	held.film.gate, held.film.first = make(chan struct{}), &atomic.Bool{}
	s.roleWorker(t, "subtitles", held)
	s.askForTracks(t, s.pool)
	waitFor(t, "the extraction to begin its read of the film", 60*time.Second, held.film.gated.Load)
	return func() { close(held.film.gate) }
}

// While the track is not stored (here, an extraction held in the middle of its
// read) no transcode job waits for it or starts a read that grows with the
// film: a run from zero, a seek and a failover restart each make their first
// segment within JS-071's read bounds and draw JS-071's pictures.
func TestNoRunWaitsForTheExtractionAndEachDrawsAsJS071Does(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", dim: true, cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	const cut = 28 // 112 s: the cue is 56 s old.
	var plain, zero []int64
	for range 3 {
		plain = append(plain, s.seek(t, cut, 1, false, readRate))
		zero = append(zero, s.seek(t, 0, 1, false, readRate))
	}
	s.fresh(t)
	s.burnt()
	release := s.heldExtraction(t)
	defer release()

	_, most, width := spread(zero...)
	got := s.seekWhileHeld(t, 0, 1)
	if got.film > most+width+4<<20 {
		t.Errorf("a run from zero while the extraction was held read %d bytes before its first segment, over JS-071's bound (%d without the track, spread %d, and 4 MB)", got.film, most, width)
	}
	got = s.seekWhileHeld(t, cut, 2)
	if limit := insideLimit(plain, 56); got.film > limit {
		t.Errorf("a seek into a cue 56 s old while the extraction was held read %d bytes before its first segment, over JS-071's bound of %d", got.film, limit)
	}
	expectPicture(t, "a seek while the extraction was held", s.halvesFrom(t, cut, cut+2), 1, "bottom")
	if got := s.storedObjects(); len(got) != 0 {
		t.Fatalf("the track was stored while its extraction was held: %v", got)
	}
}

// seekWhileHeld is seekStored on a worker that makes transcodes only, so the
// held extraction (another worker's) is not in its way.
func (s *scannedFilm) seekWhileHeld(t *testing.T, cut, count int) stored {
	t.Helper()
	// Not s.fresh, which forgets every worker too: the held extraction's worker is
	// alive, and a worker forgotten while it lives ends its extraction (invariant
	// 43), which is then run again with no read held.
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE transcode_jobs, transcode_viewers"); err != nil {
		t.Fatal(err)
	}
	s.segments = store.NewPosix(t.TempDir())
	s.burnt()
	m := newMeters(s, readRate)
	m.film.cut.Store(filmPosition(t, s.media, (cut+1)*4))
	stop := s.roleWorker(t, "transcode", m)
	defer stop()
	j := s.jobs(t)
	served, err := s.firstSegment(t, j, m.film.read.Load, cut, 0)
	if err != nil {
		t.Fatalf("segment %d while the extraction was held: %v", cut, err)
	}
	for n := cut + 1; n < cut+count; n++ {
		if _, err := s.fetchAt(t, j, psid(0), n, cut); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	return stored{film: m.film.before.Load(), served: served, assets: m.assets.read.Load()}
}

// A film that lists no cue points for the track (MPEG-TS; Matroska with no Cues
// or with Cues for the video only), once the track is stored, draws a cue older
// than the 10 s look-back from a mid-film run's first frame, and the run reads
// no more of the film than the same run without the track. Until it is stored,
// the look-back and its miss are JS-071's (its own checks hold that).
func TestWithTheTrackStoredAFilmWithNoCuePointsDrawsACueOlderThanTheLookBack(t *testing.T) {
	for _, tc := range lookbackCases {
		t.Run(tc.name, func(t *testing.T) {
			cues := []testenv.Cue{top(12, 38)}
			media := t.TempDir()
			film := tc.film
			film.seconds, film.noise, film.cues = 100, "2M", cues
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			_, outside := lookbackCuts()
			s.storeTrack(t, 1, 0)
			plain := []int64{s.seekStored(t, outside, 1, false, readRate).film, s.seekStored(t, outside, 1, false, readRate).film, s.seekStored(t, outside, 1, false, readRate).film}
			_, most, width := spread(plain...)
			for range 2 {
				got := s.seekStored(t, outside, 1, true, readRate)
				t.Logf("a run at segment %d, %d s into a cue past the %v s look-back, read %d bytes of the film (without the track %v) and %d from the asset store", outside, outside*4-12, cueLookback, got.film, plain, got.assets)
				if got.film > most+width+readMargin {
					t.Errorf("a run at segment %d read %d bytes of the film, %d more than without the track", outside, got.film, got.film-most)
				}
			}
		})
	}
	// The pictures, on black films of the same shapes.
	for _, tc := range lookbackCases {
		t.Run(tc.name+", the picture", func(t *testing.T) {
			cues := []testenv.Cue{top(12, 38)}
			media := t.TempDir()
			film := tc.film
			film.seconds, film.cues = 60, cues
			film.write(t, media, "film")
			s := newScanned(t, media, t.TempDir())
			reference := s.fromZero(t, 60)
			checkReference(t, reference, cues)
			s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
			_, outside := lookbackCuts()
			s.seekStored(t, outside, 2, true, 0)
			got := s.halvesFrom(t, outside, outside+2)
			expectPicture(t, "a run that begins past the look-back, with the track stored, at its first frames", got, 1, "top")
			likeReference(t, "a run that begins past the look-back, with the track stored", reference, got, outside, edgesOf(cues)...)
		})
	}
}

// A run that cannot read the stored track for a reason that is not the film's
// (the asset store fails once, at each of the first opens, or the object is
// gone) stores no segment without the cue a run from zero draws, is not
// remembered as the film's failure, and the next request draws it. The film is
// MPEG-TS, whose cue 20 s old a run can draw only from the stored track.
func TestARunThatCannotReadTheStoredTrackStoresNoSegmentWithoutTheCue(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "dvbsub", container: "ts", seconds: 60, cues: []testenv.Cue{top(12, 38)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.fromZero(t, 60)
	s.storeTrack(t, 1, 0) // On a worker of its own: fromZero's has ended.
	rows := s.extractionRows(t)
	_, outside := lookbackCuts()
	const second = 1 // Seconds 4 to 8 of the segment: inside the cue.

	segmentsMade := func(what string) (made int) {
		for n := outside; n < outside+2; n++ {
			if body, err := s.segments.Open(context.Background(), SegmentKey(s.hash, n), 0); err == nil {
				body.Close()
				made++
				if n == outside {
					expectPicture(t, what+": segment "+itoa(n), s.halvesFrom(t, n, n+1), second, "top")
				}
			}
		}
		return made
	}
	for opens := int64(1); opens <= 4; opens++ {
		what := "when Open " + itoa(int(opens)) + " of the stored track fails once"
		s.fresh(t)
		s.burnt()
		failing := newMeters(s, 0)
		failing.assets.failing.Store(opens)
		stop := s.metered(t, failing)
		_, err := s.firstSegment(t, s.jobs(t), failing.film.read.Load, outside, 0)
		t.Logf("%s: the request ended with %v after %d opens of the asset store", what, err, failing.assets.opens.Load())
		segmentsMade(what)
		stop()
		s.noExtraction(t, what, rows)

		s.fresh(t)
		s.spec.VideoBitrate++ // Another spec: the first one's failure memory does not stand in the way.
		s.burnt()
		healthy := newMeters(s, 0)
		stop = s.metered(t, healthy)
		if _, err := s.firstSegment(t, s.jobs(t), healthy.film.read.Load, outside, 0); err != nil {
			t.Errorf("%s: a later request with the store well failed: %v", what, err)
		} else if segmentsMade(what+", and the next request") == 0 {
			t.Errorf("%s: a later request with the store well stored no segment", what)
		}
		stop()
		s.spec.VideoBitrate--
	}

	// The object is gone: no segment lacks the cue, and the asset store's own
	// object is asked for again by the next play, which stores the track once more.
	for key := range s.storedObjects() {
		if err := os.Remove(filepath.Join(s.assets, key)); err != nil {
			t.Fatal(err)
		}
	}
	s.fresh(t)
	s.burnt()
	gone := newMeters(s, 0)
	stop := s.metered(t, gone)
	_, err := s.firstSegment(t, s.jobs(t), gone.film.read.Load, outside, 0)
	t.Logf("with the object gone the request ended with %v", err)
	segmentsMade("with the object gone")
	stop()
	s.forgetFailures(t)
	stop = s.metered(t, newMeters(s, 0))
	defer stop()
	s.askForTracks(t, s.pool)
	s.waitStored(t, 1)
}

// The read of the stored track ends with its job: when the last viewer stops,
// the asset store's reads the job began are closed soon after, though the store
// never answers.
func TestTheReadOfTheStoredTrackEndsWithItsJob(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 60, noise: "1M", cues: []testenv.Cue{top(4, 56)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.storeTrack(t, 1, 0)
	s.fresh(t)
	s.burnt()
	m := newMeters(s, 0)
	m.assets.hold.Store(true)
	stop := s.metered(t, m)
	defer stop()
	j := s.jobs(t)
	go s.firstSegment(t, j, m.film.read.Load, 10, 0) //nolint:errcheck
	waitFor(t, "the job to be waiting on the asset store", 60*time.Second, func() bool { return m.assets.held.Load() > 0 })
	if err := j.Stop(context.Background(), s.user, psid(0), "device", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Second) // A hang bound with room: the job's reads end within a heartbeat or two.
	if open := m.assets.held.Load() + m.assets.live.Load(); open > 0 {
		t.Errorf("%d reads of the asset store were still open 10 s after the job was stopped", open)
	}
}

// A film the previous release scanned, with no new scan, has its tracks stored
// by a play and a mid-film run then draws the cue 56 s old and reads no more of
// the film than a run without the track.
func TestAFilmThePreviousReleaseScannedHasItsTrackStoredWithoutANewScan(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 128, noise: "2M", dim: true, cues: []testenv.Cue{top(4, 8), bottom(56, 120)}}.write(t, media, "film")
	s := scannedByThePreviousRelease(t, media, t.TempDir())
	var item, source, etag string
	if err := s.pool.QueryRow(context.Background(), "SELECT item_id::text, id::text, object_etag FROM media_sources").Scan(&item, &source, &etag); err != nil {
		t.Fatal(err)
	}
	s.spec.Item, s.spec.Source, s.spec.ETag = db.CompactID(item), db.CompactID(source), etag // What a PlaybackInfo reads: no scan.
	s.track = 2
	s.burnt()
	s.storeTrack(t, 1, 0)
	const cut = 28
	plain := []int64{s.seekStored(t, cut, 1, false, readRate).film, s.seekStored(t, cut, 1, false, readRate).film}
	_, most, width := spread(plain...)
	got := s.seekStored(t, cut, 2, true, readRate)
	if got.film > most+width+readMargin {
		t.Errorf("a run at segment %d of the film the previous release scanned read %d bytes of the film, %d more than without the track", cut, got.film, got.film-most)
	}
	expectPicture(t, "a run at segment 28 of the film the previous release scanned", s.halvesFrom(t, cut, cut+2), 1, "bottom")
}

// A scan starts no extraction (the first scan of a film, and the next), and
// stores nothing: the asset store holds only what a scan has always put there.
func TestAScanStartsNoExtraction(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 40, cues: shortCues}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	stop := s.metered(t, newMeters(s, 0)) // A worker is there to take one if it were queued.
	defer stop()
	s.rescan(t)
	time.Sleep(3 * time.Second)
	if rows := s.extractionRows(t); len(rows) != 0 {
		t.Errorf("a scan queued extractions: %v", rows)
	}
	if got := s.storedObjects(); len(got) != 0 {
		t.Errorf("a scan stored %v", got)
	}
}

// The sweep removes the stored track of the previous version of a replaced
// file and of a removed film, and never the track of a current file: not one
// that is stored while the sweep is listing, and not while the film's
// extraction is queued or running. The tracks are stored by real extractions of
// real films, so the key is whatever the build gives it.
func TestTheAssetSweepRemovesAReplacedFilesAndARemovedFilmsTrackAndKeepsACurrentOne(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 40, cues: shortCues}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	s.storeTrack(t, 1, 0)
	var first string
	for key := range s.storedObjects() {
		first = key
	}
	var cache store.Cache
	sweeper := func() *Worker {
		return &Worker{Pool: s.pool, Segments: s.segments, Log: s.log, Media: cache.Open, Retention: 24 * time.Hour}
	}
	sweep := func() {
		t.Helper()
		if err := sweeper().Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// A current file's track stays, also while the film's extraction is queued or running.
	sweep()
	if _, ok := s.storedObjects()[first]; !ok {
		t.Fatal("the sweep removed the track of a current file")
	}
	for _, state := range []string{"queued", "running"} {
		if _, err := s.pool.Exec(context.Background(), `UPDATE subtitle_jobs SET state = $1, worker = 'another', ended_at = NULL,
			lease_expires_at = CASE WHEN $1 = 'running' THEN now() + interval '1 hour' END`, state); err != nil {
			t.Fatal(err)
		}
		sweep()
		if _, ok := s.storedObjects()[first]; !ok {
			t.Errorf("the sweep removed the track of a current file while its extraction was %s", state)
		}
	}
	if _, err := s.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET state = 'done', ended_at = now()"); err != nil {
		t.Fatal(err)
	}

	// The file is replaced: the new file's track is stored, and the old file's goes.
	s.replace(t, cueFilm{codec: "copy", seconds: 40, cues: []testenv.Cue{bottom(8, 30)}})
	s.forgetFailures(t)
	stop := s.metered(t, newMeters(s, 0))
	s.askForTracks(t, s.pool)
	s.waitStored(t, 2)
	stop()
	sweep()
	left := s.storedObjects()
	if _, ok := left[first]; ok || len(left) != 1 {
		t.Errorf("after the file was replaced and the sweep ran, the asset store holds %v: want the new file's track only, not %s", left, first)
	}

	// The film is removed: its track goes.
	if _, err := s.pool.Exec(context.Background(), "DELETE FROM items"); err != nil {
		t.Fatal(err)
	}
	sweep()
	if got := s.storedObjects(); len(got) != 0 {
		t.Errorf("after the film was removed and the sweep ran, the asset store holds %v", got)
	}
}

// A film that goes back to an earlier version has that version's stored track
// as an orphan in the asset store until it does: the sweep lists the store while
// the file is at its second version, reads the state (the track is nobody's),
// and then, held there, the file is the first again (the scan's etag is the
// first version's, as it is for a file put back with its own size and time) and
// either the first version's job row says done and the track is still where it
// was, or the row is gone and a real extraction puts the object again. Neither
// may be deleted by the sweep that listed it, nor by the next. This is
// JS-078's put-after-read race (asset_sweep_put_after_read_integration_test.go,
// held at the same statement) with the stored image track as the asset: the
// writer is a real extraction of a real film, and the key is the one the store
// was seen to hold. A build that names the new asset in the sweep's set of what a
// file keeps but not in its test of whether a key is the current version's
// (assetWanted, which names subtitles.Key and SupKey), passes the check
// above and fails this one.
func TestTheStoredTrackOfAFileChangedBackIsNeverDeletedByTheSweepThatListedItAnOrphan(t *testing.T) {
	for _, extracted := range []bool{false, true} {
		name := "its job row says done and the object is there"
		if extracted {
			name = "a worker extracts it again after the sweep read its state"
		}
		t.Run(name, func(t *testing.T) {
			media := t.TempDir()
			first := cueFilm{codec: "copy", seconds: 40, cues: shortCues}
			first.write(t, media, "film")
			path := filepath.Join(media, "film.mkv")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			s := newScanned(t, media, t.TempDir())
			s.storeTrack(t, 1, 0)
			var k1 string
			for key := range s.storedObjects() {
				k1 = key
			}

			// The file is replaced by one with other cues, whose track is stored too.
			s.replace(t, cueFilm{codec: "copy", seconds: 40, cues: []testenv.Cue{bottom(8, 30)}})
			s.forgetFailures(t)
			stop := s.metered(t, newMeters(s, 0))
			s.askForTracks(t, s.pool)
			s.waitStored(t, 2)
			stop()

			// The sweep lists both, reads the state and is held.
			var cache store.Cache
			spy := &spyStore{Store: store.NewPosix(s.assets)}
			cache.Put("file://"+s.assets, spy)
			proxy := testenv.NewDBProxy(t, s.rig.dsn)
			pool, err := db.Open(context.Background(), withParam(t, proxy.DSN(), "default_query_exec_mode", "simple_protocol"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			t.Cleanup(letGo)
			proxy.OnStatement(func(sent []byte) bool {
				return len(sent) > 0 && sent[0] == 'Q' && strings.Contains(string(sent), "kind, source_id FROM subtitle_jobs")
			}, nil, func() { close(reached); <-release })
			w := &Worker{Pool: pool, Segments: s.segments, Log: s.log, Media: cache.Open, Retention: 24 * time.Hour}
			swept := make(chan error, 1)
			go func() { swept <- w.Sweep(context.Background()) }()
			select {
			case <-reached:
			case err := <-swept:
				t.Fatalf("the sweep ended (%v) without being held after its state read", err)
			case <-time.After(time.Minute):
				t.Fatal("timed out waiting to hold the sweep after its state read")
			}
			if n := spy.deleteCount(); n != 0 {
				t.Fatalf("held after its state read, the sweep had deleted %d objects", n)
			}

			// The file is the first one again: its bytes, size and time, so its etag.
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			s.rescan(t)
			if extracted {
				if _, err := s.pool.Exec(context.Background(), "DELETE FROM subtitle_jobs WHERE state = 'done'"); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(s.assets, k1)); err != nil {
					t.Fatal(err)
				}
				stop := s.metered(t, newMeters(s, 0))
				s.askForTracks(t, s.pool)
				waitFor(t, "the extraction of the first version again", 45*time.Second, func() bool {
					_, there := s.storedObjects()[k1]
					return there && s.jobRowsIn(t, "queued")+s.jobRowsIn(t, "running") == 0
				})
				stop()
			}
			letGo()
			if err := <-swept; err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			if _, ok := s.storedObjects()[k1]; !ok {
				t.Errorf("the sweep that listed %s an orphan deleted it after the file went back to the version it belongs to", k1)
			}
			if err := (&Worker{Pool: s.pool, Segments: s.segments, Log: s.log, Media: cache.Open, Retention: 24 * time.Hour}).Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := s.storedObjects()
			if _, ok := got[k1]; !ok {
				t.Errorf("the next sweep deleted %s, the track of the file as it now is", k1)
			}
			if len(got) != 1 {
				t.Errorf("after the next sweep the asset store holds %v: want the first version's track only, the second version's having gone", got)
			}
			// No job row says the track failed.
			for _, row := range s.extractionRows(t) {
				if strings.Contains(row, " failed") {
					t.Errorf("a job row says failed after the sweep: %q", row)
				}
			}
		})
	}
}
