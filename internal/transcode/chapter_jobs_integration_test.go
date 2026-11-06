//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// Chapter pictures are background work, a seek for every chapter of every film
// in a library, so they have slots of their own: with more jobs queued than
// slots, no more than chapterSlots run at once, and a viewer's subtitle is
// still extracted at once and not after them.
func TestChapterImageJobsHaveSlotsOfTheirOwn(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	const films = 5
	for i := range films {
		id := uuid.NewString()
		r.exec(t, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto)
			SELECT $1, library_id, 'Movie', $2, $2, $2, '{}' FROM items LIMIT 1`, id, fmt.Sprint("Chaptered ", i))
		r.exec(t, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, chapters)
			VALUES ($1, $1, 'film.mkv', 1, $2, 'mkv', '[{"Type":"Video"}]', '[{"StartPositionTicks":0,"Name":"a"}]')`, id, fmt.Sprint("etag-", i))
	}
	r.exec(t, "UPDATE libraries SET chapter_images = true")

	// Every frame takes three seconds, so the jobs are still running when the
	// subtitle is asked for.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *image2pipe*) sleep 3 >/dev/null 2>&1;; esac\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.worker(t, nil)
	j := r.jobs(t)

	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	// The rig's own item has no chapters.
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'queued'"); n != films {
		t.Fatalf("%d chapter jobs queued, want %d", n, films)
	}
	running := func() int {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'running'")
	}
	waitFor(t, "two chapter jobs running", 20*time.Second, func() bool { return running() == chapterSlots })

	began := time.Now()
	if _, err := FetchSubtitle(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the subtitle waited %s behind chapter pictures", took)
	}
	if n := running(); n != chapterSlots {
		t.Errorf("%d chapter jobs running when the subtitle was done, want %d", n, chapterSlots)
	}

	// The rest of them, never more than the slots at once.
	most := 0
	waitFor(t, "every chapter job to end", 60*time.Second, func() bool {
		most = max(most, running())
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state IN ('queued', 'running')") == 0
	})
	if most > chapterSlots {
		t.Errorf("%d chapter jobs ran at once, the slots are %d", most, chapterSlots)
	}
	if n := r.count(t, "SELECT count(*) FROM chapter_images WHERE state = 'done' AND keys[1] <> ''"); n != films {
		t.Errorf("%d films have a picture, want %d", n, films)
	}
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'failed'"); n != 0 {
		t.Errorf("%d chapter jobs failed", n)
	}
}

func (r *rig) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) libraryID(t *testing.T) string {
	t.Helper()
	var id string
	if err := r.pool.QueryRow(context.Background(), "SELECT id::text FROM libraries").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A chapter job that fails because a store does is logged as a chapter-image
// failure with the item's id, not as a subtitle extraction, and ends failed.
func TestChapterImageFailureIsLoggedAsOne(t *testing.T) {
	r := newRig(t, nil)
	var logged bytes.Buffer
	r.log = slog.New(slog.NewTextHandler(&logged, nil))
	item := r.spec.Item
	r.exec(t, `UPDATE media_sources SET streams = '[{"Type":"Video"}]', chapters = '[{"StartPositionTicks":0,"Name":"a"}]'`)
	r.exec(t, "UPDATE libraries SET chapter_images = true")
	// A file where the pictures' directory should be: the asset store can't write.
	assets := strings.TrimPrefix(r.stringOf(t, "SELECT asset_store_uri FROM libraries"), "file://")
	// The directory is shared with other tests: put it back as it was.
	blocker := filepath.Join(assets, "chapters")
	os.RemoveAll(blocker)
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(blocker) })
	r.worker(t, nil)
	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the chapter job to fail", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'failed'") == 1
	})
	text := logged.String()
	if !strings.Contains(text, "chapter image job failed") || !strings.Contains(text, "item="+r.stringOf(t, "SELECT id::text FROM items")) {
		t.Errorf("the failure was not logged as a chapter-image failure with the item id (%s):\n%s", item, text)
	}
	if strings.Contains(text, "subtitle extraction failed") {
		t.Errorf("logged as a subtitle failure:\n%s", text)
	}
}

func (r *rig) stringOf(t *testing.T, sql string) string {
	t.Helper()
	var value string
	if err := r.pool.QueryRow(context.Background(), sql).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// A worker that is stopping does not give back the attempt of the chapter job
// it cut off, or restarts would make starts unbounded; the job is queued again.
func TestStoppingAWorkerKeepsTheChapterAttempt(t *testing.T) {
	r := newRig(t, nil)
	r.exec(t, `UPDATE media_sources SET streams = '[{"Type":"Video"}]', chapters = '[{"StartPositionTicks":0,"Name":"a"}]'`)
	r.exec(t, "UPDATE libraries SET chapter_images = true")
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *image2pipe*) exec sleep 60;; esac\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stop := r.worker(t, nil)
	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start making", 20*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM chapter_images WHERE state = 'making' AND attempts = 1") == 1
	})
	stop()
	if n := r.count(t, "SELECT attempts FROM chapter_images"); n != 1 {
		t.Errorf("%d attempts after the worker stopped, want 1", n)
	}
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'queued'"); n != 1 {
		t.Errorf("%d chapter jobs queued after the worker stopped, want 1", n)
	}
}

// The pictures of a source that is gone are deleted by a worker, which can't
// look the source up: the job reads the library from the row.
func TestAWorkerDeletesThePicturesOfARemovedSource(t *testing.T) {
	r := newRig(t, nil)
	ghost := uuid.NewString()
	assets, err := store.Open(r.stringOf(t, "SELECT asset_store_uri FROM libraries"))
	if err != nil {
		t.Fatal(err)
	}
	key := "chapters/" + ghost + "/dir/0.jpg"
	if err := assets.Put(context.Background(), key, strings.NewReader("jpeg")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assets.Delete(context.Background(), key) })
	r.exec(t, `INSERT INTO chapter_images (source_id, library_id, etag, state, attempts, keys) SELECT $1, id, 'e', 'done', 1, ARRAY[$2] FROM libraries`, ghost, key)
	r.worker(t, nil)
	if err := library.EnqueueChapterImages(context.Background(), r.pool, r.libraryID(t)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to be done", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'done'") == 1
	})
	if found, _ := assets.List(context.Background(), "chapters/"+ghost+"/"); len(found) != 0 {
		t.Errorf("%d pictures left", len(found))
	}
	if n := r.count(t, "SELECT count(*) FROM chapter_images"); n != 0 {
		t.Errorf("%d rows left", n)
	}
}
