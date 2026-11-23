//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-089's checks share this kit. The track is stored by a worker's extraction
// that a play asks for, never by the test: a test starts a burnt run (which is
// all a client's playback does that the worker sees), waits for the asset store
// to hold the track, and then asks for the runs it measures, each on a worker of
// its own (so "after every worker has restarted" holds in all of them).
//
// Nothing here names how the build stores the track. The films hold no text
// track, no font and no .sup copy is asked for, so every object under
// subtitles/ in the film's asset store is the extracted image track.

// storedObjects lists the keys under subtitles/ of the asset store, with their sizes.
func (s *scannedFilm) storedObjects() map[string]int64 {
	found := map[string]int64{}
	root := filepath.Join(s.assets, "subtitles")
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { //nolint:errcheck // None yet is an empty listing.
		if err == nil && d.Type().IsRegular() && !strings.Contains(d.Name(), ".tmp") {
			if info, err := d.Info(); err == nil {
				rel, _ := filepath.Rel(s.assets, path)
				found[rel] = info.Size()
			}
		}
		return nil
	})
	return found
}

// storedSize is the bytes of every stored object.
func (s *scannedFilm) storedSize() (total int64) {
	for _, size := range s.storedObjects() {
		total += size
	}
	return total
}

// extractionRows is the film's subtitle jobs, by state.
func (s *scannedFilm) extractionRows(t *testing.T) (rows []string) {
	t.Helper()
	found, err := s.pool.Query(context.Background(), "SELECT key || ' ' || state || ' ' || permanent::text FROM subtitle_jobs ORDER BY created_at")
	if err != nil {
		t.Fatal(err)
	}
	defer found.Close()
	for found.Next() {
		var row string
		if err := found.Scan(&row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

// jobRowsIn counts the subtitle jobs in the state ("" for any).
func (s *scannedFilm) jobRowsIn(t *testing.T, state string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM subtitle_jobs WHERE $1 = '' OR state = $1", state).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// waitStored waits until the asset store holds want objects and no extraction
// is queued or running, and returns them. A build that never stores the track
// fails here.
func (s *scannedFilm) waitStored(t *testing.T, want int) map[string]int64 {
	t.Helper()
	waitFor(t, "the track to be stored by a worker's extraction", 45*time.Second, func() bool {
		return len(s.storedObjects()) >= want && s.jobRowsIn(t, "queued")+s.jobRowsIn(t, "running") == 0
	})
	if got := s.storedObjects(); len(got) != want {
		t.Fatalf("the asset store holds %d objects after the extraction, want %d: %v", len(got), want, got)
	}
	return s.storedObjects()
}

// forgetTrack is the film as it was before any play: nothing stored, no job.
func (s *scannedFilm) forgetTrack(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(s.assets, "subtitles")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE subtitle_jobs"); err != nil {
		t.Fatal(err)
	}
}

// meters of a worker: the film's store and the asset store.
type meters struct{ film, assets *meter }

func newMeters(s *scannedFilm, rate int) meters {
	return meters{film: newMeter(s.media, rate), assets: newMeter(s.assets, 0)}
}

// open is the worker's Media: the film and the assets through their meters.
func (s *scannedFilm) open(m meters) func(string) (store.Store, error) {
	return func(uri string) (store.Store, error) {
		switch strings.TrimPrefix(uri, "file://") {
		case s.media:
			return m.film, nil
		case s.assets:
			return m.assets, nil
		}
		return store.Open(uri)
	}
}

// metered is scannedFilm.worker with the asset store through a meter too.
func (s *scannedFilm) metered(t *testing.T, m meters, change ...func(*Worker)) (stop func()) {
	t.Helper()
	return s.rig.worker(t, func(w *Worker) {
		w.Slots, w.NoHelpers, w.Lead = 1, true, 8*time.Second
		w.Media = s.open(m)
		for _, c := range change {
			c(w)
		}
	})
}

// roleWorker is a worker that runs only its transcode jobs ("transcode") or
// only its extractions ("subtitles"), so that a test can hold the one and let
// the other go on. It is rig.worker's body with one loop.
func (s *scannedFilm) roleWorker(t *testing.T, role string, m meters, change ...func(*Worker)) (stop func()) {
	t.Helper()
	return s.rig.keep(func() func() { return s.startRoleWorker(t, role, m, change...) })
}

func (s *scannedFilm) startRoleWorker(t *testing.T, role string, m meters, change ...func(*Worker)) (stop func()) {
	t.Helper()
	r := s.rig
	pool, err := db.Open(context.Background(), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	w := &Worker{ID: uuid.NewString(), Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: r.segments, Log: r.log, Media: media.Open,
		Scratch: r.scratch, Slots: 1, Idle: 60 * time.Second, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond, Lead: 8 * time.Second, NoHelpers: true}
	w.Media = s.open(m)
	for _, c := range change {
		c(w)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Hub = &Hub{Pool: pool, Log: r.log}
	go w.Hub.Run(ctx)
	var running sync.WaitGroup
	running.Add(2)
	go func() { defer running.Done(); w.Present(ctx, "worker", r.storeURI, 200*time.Millisecond) }()
	go func() {
		defer running.Done()
		if role == "subtitles" {
			w.RunSubtitles(ctx)
		} else {
			w.Run(ctx)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			running.Wait()
			pool.Close()
		})
	}
	t.Cleanup(stop)
	waitFor(t, "worker "+w.ID+" to register", 30*time.Second, func() bool { return r.locked(w.ID) })
	return stop
}

// quiet waits until the meter has served nothing for two seconds and returns
// what it served in all (a hang bound of two minutes).
func quiet(t *testing.T, m *meter) int64 {
	t.Helper()
	last, since := m.read.Load(), time.Now()
	for time.Since(since) < 2*time.Second {
		if time.Since(since) > 2*time.Minute {
			t.Fatal("the film is still being read after two minutes")
		}
		time.Sleep(250 * time.Millisecond)
		if now := m.read.Load(); now != last {
			last, since = now, time.Now()
		}
	}
	return last
}

// extracted is what storing the track cost the film's store and the asset store.
type extracted struct {
	film    int64 // Served by the store in all, by the play that asked and the extraction.
	size    int64 // The stored track's bytes.
	objects int
}

// askForTracks is the one place these checks name how a play asks for the
// extraction: the api's request (PlaybackInfo, the playlists) calls it as it
// calls StartFonts, without waiting, once for each request. The name and
// arguments are implied by the task (StartFonts is the model) and are what the
// build has to provide or the lead has to change here. The pool is the
// caller's, so that two api replicas are two pools.
func (s *scannedFilm) askForTracks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := StartImageTracks(context.Background(), pool, store.NewPosix(s.assets), s.spec.Item, s.source(t)); err != nil {
		t.Fatalf("asking for the film's image tracks: %v", err)
	}
}

// storeTrack asks as a play does, on a worker with its stores metered, waits
// for the extraction, and ends the worker. want is the number of objects it
// leaves (one per image track). Runs measured afterwards are on workers of
// their own.
func (s *scannedFilm) storeTrack(t *testing.T, want int, rate int) extracted {
	t.Helper()
	s.fresh(t)
	s.burnt()
	m := newMeters(s, rate)
	stop := s.metered(t, m)
	defer stop()
	s.askForTracks(t, s.pool)
	objects := s.waitStored(t, want)
	return extracted{film: quiet(t, m.film), size: s.storedSize(), objects: len(objects)}
}

// stored is what a measured run read.
type stored struct {
	film   int64 // Bytes of the film from below the end of the first segment's place in it (meter.before).
	served int64 // Everything the film's store served by the first segment.
	assets int64 // What the asset store served by the first segment.
	// assetsAll is what it had served when the last of the segments asked for was
	// stored, across every ffmpeg the job started for them.
	assetsAll int64
}

// seekStored is scannedFilm.seek on a worker of its own with the asset store
// metered, for a film whose track is stored. It asks for count segments from cut.
func (s *scannedFilm) seekStored(t *testing.T, cut, count int, burnt bool, rate int, change ...func(*Worker)) stored {
	t.Helper()
	s.fresh(t)
	if burnt {
		s.burnt()
	} else {
		s.plain()
	}
	m := newMeters(s, rate)
	m.film.cut.Store(filmPosition(t, s.media, (cut+1)*4))
	stop := s.metered(t, m, change...)
	defer stop()
	j := s.jobs(t)
	served, err := s.firstSegment(t, j, m.film.read.Load, cut, 0)
	if err != nil {
		t.Fatalf("segment %d: %v", cut, err)
	}
	got := stored{film: m.film.before.Load(), served: served, assets: m.assets.read.Load()}
	for n := cut + 1; n < cut+count; n++ {
		if _, err := s.fetchAt(t, j, psid(0), n, cut); err != nil {
			t.Fatalf("segment %d: %v", n, err)
		}
	}
	got.assetsAll = m.assets.read.Load()
	return got
}

// noExtraction fails the test if an extraction was queued since before was
// taken (the rows then), and for every one of them if the film was read by one.
func (s *scannedFilm) noExtraction(t *testing.T, what string, before []string) {
	t.Helper()
	if after := s.extractionRows(t); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("%s started an extraction: the subtitle jobs were %v and are %v", what, before, after)
	}
}

// pgsNeverCleared is a PGS track whose one cue is not cleared: testenv.PGS
// without its closing display set (a composition, a window and an end segment,
// which are 24, 14 and 13 bytes), so the cue shows until the film ends.
func pgsNeverCleared(cue testenv.Cue) []byte {
	sup := testenv.PGS(1920, 1080, cue)
	return sup[:len(sup)-51]
}

// specJSON is the spec as a job row holds it.
func (s *scannedFilm) specJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(s.spec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
