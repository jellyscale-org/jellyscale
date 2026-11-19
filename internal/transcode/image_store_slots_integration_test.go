//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// The extraction of a film's image tracks (JS-089) is background work that
// nobody waits on, and its read is the whole film, so it has slots of its own
// as chapter pictures do: with more jobs than a worker has subtitle slots held
// open, a viewer's text track is still extracted at once, and no more than
// imageSlots of them run.
func TestImageTrackJobsHaveSlotsOfTheirOwn(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	const films = subtitleSlots + 1
	for i := range films {
		id := uuid.NewString()
		r.exec(t, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto)
			SELECT $1, library_id, 'Movie', $2, $2, $2, '{}' FROM items LIMIT 1`, id, fmt.Sprint("Burnt ", i))
		r.exec(t, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
			VALUES ($1, $1, 'film.mkv', 1, $2, 'mkv', '[{"Type":"Video"},{"Type":"Subtitle","Index":1,"IsImageSubtitleStream":true}]')`, id, fmt.Sprint("etag-", i))
		film, found, err := r.items.Source(context.Background(), id, "")
		if err != nil || !found {
			t.Fatalf("source %d: %v %v", i, found, err)
		}
		a, err := store.Open(film.AssetStoreURI)
		if err != nil {
			t.Fatal(err)
		}
		if err := StartImageTracks(context.Background(), r.pool, a, id, film); err != nil {
			t.Fatal(err)
		}
	}

	// Each image extraction's ffmpeg waits for a file the test writes, so the jobs
	// are still running when the track is asked for, whatever the machine.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *matroska*) while [ ! -e %q ]; do sleep 0.1; done;; esac\nexec %q \"$@\"\n", gate, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	open := func() { os.WriteFile(gate, nil, 0o644) } //nolint:errcheck // Opened twice is fine.
	t.Cleanup(open)
	r.worker(t, func(w *Worker) { w.SubtitleTimeout = 2 * time.Minute })
	j := r.jobs(t)

	running := func() int {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'images' AND state = 'running'")
	}
	waitFor(t, "image jobs running", 20*time.Second, func() bool { return running() > 0 })
	waitFor(t, "the slots of the image jobs to be full", 20*time.Second, func() bool { return running() >= imageSlots })

	// A track that waited behind the image jobs would wait for the gate, which only
	// this test opens: the minute only stops that hang.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := FetchSubtitle(ctx, r.pool, j.Hub, assets, r.spec.Item, source, 2); err != nil {
		t.Fatalf("the text track was not extracted while image jobs were held: %v", err)
	}
	if n := running(); n != imageSlots {
		t.Errorf("%d image jobs running when the track was done, want %d", n, imageSlots)
	}
	open()
	waitFor(t, "every image job to end", time.Minute, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'images' AND state IN ('queued', 'running')") == 0
	})
}

// A film whose whole read runs past the extraction's time limit fails its job
// and would fail every time: the next plays, each a seek that asks again, do not
// queue another whole-film read within the failure memory, nor after it (the
// time limit is not the film's to change, so the job stays failed for good
// until the daily sweep of finished jobs or the file's change). An ordinary
// failure is queued again once the memory has passed, which
// TestAnExtractionThatEndsForAReasonNotTheFilmsStoresNothingAndIsTakenUpAgain holds.
func TestAnImageExtractionThatRanOutOfTimeIsNotQueuedAgainByTheNextPlays(t *testing.T) {
	media := t.TempDir()
	cueFilm{codec: "copy", seconds: 20, noise: "1M", cues: []testenv.Cue{top(6, 15)}}.write(t, media, "film")
	s := newScanned(t, media, t.TempDir())
	var logged lockedBuffer
	s.log = slog.New(slog.NewJSONHandler(&logged, nil))
	slow := newMeters(s, 0)
	slow.film.hold.Store(true) // Reads wait until the extraction's time limit ends them.
	stop := s.roleWorker(t, "subtitles", slow, func(w *Worker) { w.SubtitleTimeout = 3 * time.Second })
	defer stop()
	s.askForTracks(t, s.pool)
	waitFor(t, "the extraction to run out of time", time.Minute, func() bool { return s.jobRowsIn(t, "failed") == 1 })
	// The log line is the operator's only sight of it: it names the item, the source, the
	// reason and when the film may be read again (decision D-002).
	var line struct {
		Msg, Item, Source, Reason string
		Retry                     string `json:"retry_not_before"`
	}
	found := false
	for _, raw := range strings.Split(logged.String(), "\n") {
		if json.Unmarshal([]byte(raw), &line) == nil && strings.Contains(line.Msg, "ran out of time") {
			found = true
		}
	}
	retry, err := time.Parse(time.RFC3339, line.Retry)
	if !found || line.Item != s.spec.Item || line.Source != s.source(t).ID || !strings.Contains(line.Reason, "longer than") || err != nil {
		t.Errorf("the log has no line that names the item (%s), the source (%s), the time limit and the retry time a day on: found %v, %+v", s.spec.Item, s.source(t).ID, found, line)
	}
	// The retry time is when the sweep lets the failed row go: its end and the memory of a day.
	var ended time.Time
	if err := s.pool.QueryRow(context.Background(), "SELECT ended_at FROM subtitle_jobs").Scan(&ended); err != nil {
		t.Fatal(err)
	}
	if retry.Sub(ended.Add(failedJobMemory)).Abs() > time.Second {
		t.Errorf("the log says the film is retried from %v, and the failed job ended at %v: the memory is %v", retry, ended, failedJobMemory)
	}
	opens := slow.film.opens.Load()
	if opens == 0 {
		t.Fatal("the extraction never read the film")
	}
	for range 3 {
		s.askForTracks(t, s.pool)
	}
	s.forgetFailures(t) // The memory of an ordinary failure, 30 seconds, has passed.
	for range 3 {
		s.askForTracks(t, s.pool)
	}
	time.Sleep(3 * time.Second) // Long enough for a worker to claim a job queued meanwhile.
	if rows := s.extractionRows(t); len(rows) != 1 || !strings.HasSuffix(rows[0], " failed true") {
		t.Errorf("jobs after the plays that followed the time limit: %v, want the one failed job, for good", rows)
	}
	if got := slow.film.opens.Load(); got != opens {
		t.Fatalf("the film was opened %d times, %d before the plays that followed the time limit: it was read again", got, opens)
	}
	// The memory is a day (decision D-002): the daily sweep of finished jobs ends it, and
	// only a play starts a new attempt, one however many plays.
	s.sweptRows(t)
	time.Sleep(3 * time.Second)
	if rows := s.extractionRows(t); len(rows) != 0 || slow.film.opens.Load() != opens {
		t.Errorf("after the memory lapsed, with no play, rows %v and %d opens (was %d): an attempt started without a play", rows, slow.film.opens.Load(), opens)
	}
	for range 3 {
		s.askForTracks(t, s.pool)
	}
	waitFor(t, "the one attempt to run out of time again", time.Minute, func() bool { return s.jobRowsIn(t, "failed") == 1 })
	for range 3 {
		s.askForTracks(t, s.pool)
	}
	time.Sleep(3 * time.Second)
	if rows := s.extractionRows(t); len(rows) != 1 || !strings.HasSuffix(rows[0], " failed true") {
		t.Errorf("jobs after the plays that followed the attempt: %v, want the one failed job", rows)
	}
	if got := slow.film.opens.Load(); got != opens+1 {
		t.Errorf("the film was opened %d times, want %d: one attempt after the memory lapsed", got, opens+1)
	}
}

// lockedBuffer is a log's destination that a test reads while a worker writes.
type lockedBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}
