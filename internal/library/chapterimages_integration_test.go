//go:build integration

package library_test

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// steppedClip makes a 60 s film whose picture is dark until 19 s of its own
// time and bright after, with its streams starting 2 s late, so that a frame
// taken at the wrong clock shows. Chapters start at the given seconds.
func steppedClip(t *testing.T, file string, starts ...int) {
	t.Helper()
	encodedClip(t, file, nil, starts...)
}

// encodedClip is steppedClip with more arguments for the encoder.
func encodedClip(t *testing.T, file string, encoder []string, starts ...int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := ";FFMETADATA1\n"
	for i, start := range starts {
		end := start + 10
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		meta += fmt.Sprintf("[CHAPTER]\nTIMEBASE=1/1\nSTART=%d\nEND=%d\ntitle=Part %d\n", start, end, i+1)
	}
	metaFile := filepath.Join(t.TempDir(), "meta.txt")
	if err := os.WriteFile(metaFile, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-nostdin", "-v", "error", "-y", "-itsoffset", "2", "-f", "lavfi",
		"-i", "color=c=black:size=320x180:rate=10:duration=60,geq=lum='if(lt(T,19),50,200)':cb=128:cr=128", "-i", metaFile,
		"-map", "0:v", "-map_metadata", "1", "-map_chapters", "1", "-c:v", "libx264", "-preset", "ultrafast", "-g", "10"}, encoder...)
	out, err := exec.Command("ffmpeg", append(args, file)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// chapterFixture is a library with the option on or off and a scan of it, its
// asset store a directory.
func chapterFixture(t *testing.T, on bool) (fixture, store.Store) {
	t.Helper()
	return chapterFixtureAt(t, on, "file://"+t.TempDir())
}

func chapterFixtureAt(t *testing.T, on bool, assetsURI string) (fixture, store.Store) {
	t.Helper()
	pool := openDB(t)
	if _, err := pool.Exec(context.Background(), "TRUNCATE subtitle_jobs, chapter_images"); err != nil {
		t.Fatal(err)
	}
	media := t.TempDir()
	// A config may not carry credentials, so an S3 store is put in the row, as
	// the environment's credentials would be in a deployment.
	configured := assetsURI
	if !strings.HasPrefix(assetsURI, "file://") {
		configured = "file://" + t.TempDir()
	}
	libs, err := library.Sync(context.Background(), pool, []library.Config{
		{Name: "test", Kind: "movies", Media: "file://" + media, Assets: configured, ChapterImages: on},
	})
	if err != nil {
		t.Fatal(err)
	}
	if configured != assetsURI {
		if _, err := pool.Exec(context.Background(), "UPDATE libraries SET asset_store_uri = $1", assetsURI); err != nil {
			t.Fatal(err)
		}
	}
	assets, err := store.Open(assetsURI)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool, &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler)}, libs[0], media}, assets
}

// setOption turns the library's option on or off, as a changed config would.
func setOption(t *testing.T, f fixture, on bool) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), "UPDATE libraries SET chapter_images = $1", on); err != nil {
		t.Fatal(err)
	}
}

// brightness is the middle pixel's luma of a stored picture.
func brightness(t *testing.T, assets store.Store, key string) int {
	t.Helper()
	body, err := assets.Open(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	defer body.Close()
	img, _, err := image.Decode(body)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	if width := img.Bounds().Dx(); width > 400 {
		t.Errorf("%s is %d wide, more than the 400 kept", key, width)
	}
	c := img.Bounds().Min.Add(image.Pt(img.Bounds().Dx()/2, img.Bounds().Dy()/2))
	r, _, _, _ := img.At(c.X, c.Y).RGBA()
	return int(r >> 8)
}

// filmID is the id of the one source with chapters, which is its item's too.
func filmID(t *testing.T, f fixture) string {
	t.Helper()
	return scalar[string](t, f.pool, "SELECT id::text FROM media_sources WHERE jsonb_array_length(chapters) > 0")
}

// syncImages runs the job as a worker would, with the media store as given.
func syncImagesWith(ctx context.Context, f fixture, id string, open func(string) (store.Store, error)) error {
	return syncImagesLogged(ctx, f, id, open, slog.New(slog.DiscardHandler))
}

func syncImagesLogged(ctx context.Context, f fixture, id string, open func(string) (store.Store, error), log *slog.Logger) error {
	return library.SyncChapterImages(ctx, f.pool, id, open, log)
}

func syncImages(t *testing.T, f fixture, id string) error {
	t.Helper()
	return syncImagesWith(context.Background(), f, id, store.Open)
}

func mustSync(t *testing.T, f fixture, id string) {
	t.Helper()
	if err := syncImages(t, f, id); err != nil {
		t.Fatal(err)
	}
}

func chapterKeys(t *testing.T, f fixture) []string {
	t.Helper()
	return scalar[[]string](t, f.pool, "SELECT keys FROM chapter_images")
}

// attempts and state are of the one chapter_images row.
func attempts(t *testing.T, f fixture) int {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT attempts FROM chapter_images")
}

func rowState(t *testing.T, f fixture) string {
	t.Helper()
	return scalar[string](t, f.pool, "SELECT state FROM chapter_images")
}

func queued(t *testing.T, f fixture) int {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'chapters' AND state = 'queued'")
}

// finishJobs ends every job as a worker would, so that the next scan may queue it again.
func finishJobs(t *testing.T, f fixture) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET state = 'done', ended_at = now()"); err != nil {
		t.Fatal(err)
	}
}

// objects lists what the asset store holds for a source.
func objects(t *testing.T, assets store.Store, id string) []string {
	t.Helper()
	found, err := assets.List(context.Background(), "chapters/"+id+"/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, object := range found {
		keys = append(keys, object.Key)
	}
	return keys
}

// fakeFfmpeg puts an ffmpeg first on PATH that runs script before the real one,
// for the runs that take a chapter's frame only (the others make the test's
// files). $* is the arguments; the script may exit, kill itself or sleep.
func fakeFfmpeg(t *testing.T, script string) {
	t.Helper()
	real := testenv.FFmpeg(t)
	bin := t.TempDir()
	text := "#!/bin/sh\ncase \"$*\" in *-seek_timestamp*)\n" + script + "\n;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The scan only queues the job; the job takes a frame at each chapter on the
// file's own clock (a file whose streams start late), records them together,
// leaves no picture for a chapter past the end, and for a changed file makes new
// pictures and deletes the old ones. A source whose pictures are done costs the
// next scan nothing.
func TestChapterImagesAreMadeByTheJobNotTheScan(t *testing.T) {
	f, assets := chapterFixture(t, true)
	film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
	steppedClip(t, film, 0, 20, 45, 100)
	clip(t, filepath.Join(f.media, "Plain Film (2021)", "Plain Film (2021).mkv"))
	f.scan(t)

	id := filmID(t, f)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d chapter jobs after the scan, want 1 (the film with chapters)", n)
	}
	if job := scalar[string](t, f.pool, "SELECT key FROM subtitle_jobs WHERE kind = 'chapters'"); job != "chapters/"+id {
		t.Errorf("the job's key is %q, want chapters/%s", job, id)
	}
	if found := objects(t, assets, id); len(found) != 0 {
		t.Fatalf("the scan made %d chapter pictures", len(found))
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 {
		t.Fatalf("%d rows before the job ran", n)
	}

	mustSync(t, f, id)
	etag := scalar[string](t, f.pool, "SELECT object_etag FROM media_sources WHERE id = $1", id)
	dir := library.ChapterDir(id, etag)
	keys := chapterKeys(t, f)
	want := []string{library.ChapterKey(dir, 0), library.ChapterKey(dir, 1), library.ChapterKey(dir, 2), ""}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("keys %q, want %q (the last chapter starts at 100 s of a 62 s film)", keys, want)
	}
	// 0 s and 20 s of the file's clock are before the step at 19 s of the
	// picture's own time, which is 21 s on the clock; 45 s is after. A frame at
	// 20 s asked for as an offset from the file's start would be bright.
	for n, bright := range []bool{false, false, true} {
		if got := brightness(t, assets, keys[n]); (got > 125) != bright {
			t.Errorf("chapter %d: brightness %d, want bright=%v", n, got, bright)
		}
	}
	if got := rowState(t, f); got != "done" || attempts(t, f) != 1 {
		t.Errorf("row is %s after %d attempts, want done after 1", got, attempts(t, f))
	}

	finishJobs(t, f)
	f.scan(t)
	if n := queued(t, f); n != 0 {
		t.Fatalf("%d jobs queued for a source whose pictures are done", n)
	}

	// The file changes: the scan queues it again, and the job leaves only the
	// new pictures.
	time.Sleep(10 * time.Millisecond)
	steppedClip(t, film, 0, 30)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued for a changed file, want 1", n)
	}
	mustSync(t, f, id)
	newEtag := scalar[string](t, f.pool, "SELECT object_etag FROM media_sources WHERE id = $1", id)
	if newEtag == etag {
		t.Fatal("the file's ETag did not change")
	}
	newDir := library.ChapterDir(id, newEtag)
	if found := objects(t, assets, id); fmt.Sprint(found) != fmt.Sprint([]string{library.ChapterKey(newDir, 0), library.ChapterKey(newDir, 1)}) {
		t.Errorf("objects after the change: %v, want the two of the new file only", found)
	}
	if got := scalar[string](t, f.pool, "SELECT etag FROM chapter_images"); got != newEtag || rowState(t, f) != "done" || attempts(t, f) != 1 {
		t.Errorf("row for %q, %s, %d attempts: want the new file, done, 1 (a new version starts again)", got, rowState(t, f), attempts(t, f))
	}
}

// With the option off nothing is queued, and a job that runs anyway (the
// option was turned off after it was queued) makes nothing and records nothing.
// Turned on later, the next scan queues the film.
func TestChapterImagesOffMakesNothing(t *testing.T) {
	f, assets := chapterFixture(t, false)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM subtitle_jobs"); n != 0 {
		t.Fatalf("%d jobs queued with the option off", n)
	}
	id := filmID(t, f)
	mustSync(t, f, id)
	if found := objects(t, assets, id); len(found) != 0 {
		t.Errorf("%d pictures made with the option off", len(found))
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 {
		t.Errorf("%d rows recorded with the option off", n)
	}
	setOption(t, f, true)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued after turning it on, want 1", n)
	}
}

// A scan that probes a film again, as it does for an unchanged one whose
// sidecars changed, deletes and writes its media source: the pictures stay.
func TestChapterImagesSurviveAReprobe(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	mustSync(t, f, filmID(t, f))
	finishJobs(t, f)
	// Stored before chapters were read: the scan probes the film again and
	// rewrites its media source row.
	if _, err := f.pool.Exec(context.Background(), "UPDATE media_sources SET chapters = NULL"); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := scalar[int](t, f.pool, "SELECT jsonb_array_length(chapters) FROM media_sources"); n != 2 {
		t.Fatalf("the film was not probed again: %d chapters", n)
	}
	if keys := chapterKeys(t, f); len(keys) != 2 || keys[0] == "" || rowState(t, f) != "done" {
		t.Fatalf("the pictures were lost: %q", keys)
	}
	if n := queued(t, f); n != 0 {
		t.Errorf("%d jobs queued for a film whose pictures are still there", n)
	}
}

// A chapter whose frame ffmpeg can't produce (it exits with an error) has no
// picture and no tag, and the others keep theirs: the job is done, and a later
// scan doesn't queue the film again, since reading it again would not help.
func TestAnUnreadableChapterFrameIsSkippedForGood(t *testing.T) {
	f, assets := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	id := filmID(t, f)
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) echo boom >&2; exit 1;; esac`)
	mustSync(t, f, id)
	keys := chapterKeys(t, f)
	if len(keys) != 3 || keys[0] == "" || keys[1] != "" || keys[2] == "" || rowState(t, f) != "done" {
		t.Fatalf("keys %q: want the second chapter alone without a picture", keys)
	}
	if found := objects(t, assets, id); len(found) != 2 {
		t.Errorf("%d objects, want 2", len(found))
	}
	finishJobs(t, f)
	f.scan(t)
	if n := queued(t, f); n != 0 {
		t.Errorf("%d jobs queued for a film whose chapter can't be read, which would be read again", n)
	}
}

// A frame that takes longer than its own deadline is skipped like an erroring
// one, and the job is done: a stalled store can leave chapters without pictures
// until the file changes.
func TestAHungFrameIsSkipped(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	defer library.SetChapterFrameTimeout(time.Second)()
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) exec sleep 60;; esac`)
	mustSync(t, f, filmID(t, f))
	keys := chapterKeys(t, f)
	if len(keys) != 3 || keys[0] == "" || keys[1] != "" || keys[2] == "" || rowState(t, f) != "done" {
		t.Fatalf("keys %q (%s): want the hung chapter alone without a picture, and done", keys, rowState(t, f))
	}
}

// The job's own context ending is not a skip, even when it ends by a deadline
// as the frame's own does: the job fails and nothing is recorded as done.
func TestAJobCutOffIsNotASkip(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) exec sleep 60;; esac`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var logged strings.Builder
	err := syncImagesLogged(ctx, f, filmID(t, f), store.Open, slog.New(slog.NewTextHandler(&logged, nil)))
	if err == nil {
		t.Fatalf("a job whose context ended was done: %q", chapterKeys(t, f))
	}
	if rowState(t, f) == "done" || strings.Contains(logged.String(), "chapter has no picture") {
		t.Errorf("the row is %s; log: %s", rowState(t, f), logged.String())
	}
}

// An ffmpeg killed by a signal (the OOM killer, say) is the worker's trouble and
// not the file's: the job fails, and the next job resumes where it stopped,
// without making the pictures that exist again.
func TestAKilledFfmpegFailsTheJobAndTheNextOneResumes(t *testing.T) {
	f, assets := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	id := filmID(t, f)
	calls := filepath.Join(t.TempDir(), "calls")
	fakeFfmpeg(t, `echo "$*" >> `+calls+`
case " $* " in *" -ss 20.000 "*) if [ ! -e `+calls+`.ok ]; then kill -9 $$; fi;; esac`)
	if err := syncImages(t, f, id); err == nil {
		t.Fatal("a killed ffmpeg did not fail the job")
	}
	if rowState(t, f) != "making" || attempts(t, f) != 1 || len(objects(t, assets, id)) != 1 {
		t.Fatalf("row %s after %d attempts with %d pictures, want making, 1, 1", rowState(t, f), attempts(t, f), len(objects(t, assets, id)))
	}
	if err := os.WriteFile(calls+".ok", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(calls, 0); err != nil {
		t.Fatal(err)
	}
	mustSync(t, f, id)
	made, _ := os.ReadFile(calls)
	if got := strings.Count(string(made), "image2pipe"); got != 2 || strings.Contains(string(made), "-ss 0.000") {
		t.Errorf("the retry took %d frames (%s), want the 2 that were missing", got, made)
	}
	if rowState(t, f) != "done" || attempts(t, f) != 2 || len(objects(t, assets, id)) != 3 {
		t.Errorf("row %s after %d attempts with %d pictures, want done, 2, 3", rowState(t, f), attempts(t, f), len(objects(t, assets, id)))
	}
}

// Making stops after three starts for one file version, and the count survives
// the sweep that deletes finished job rows. A new file version starts again.
// Turning the option off and on deletes the row, so the count starts again.
func TestMakingStopsAfterThreeAttemptsPerFileVersion(t *testing.T) {
	f, assets := chapterFixture(t, true)
	film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
	steppedClip(t, film, 0, 20)
	f.scan(t)
	id := filmID(t, f)
	// The first chapter's picture is made, then ffmpeg is killed on the second.
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) kill -9 $$;; esac`)
	for run := 1; run <= 3; run++ {
		if n := queued(t, f); n != 1 {
			t.Fatalf("run %d: %d jobs queued, want 1", run, n)
		}
		if err := syncImages(t, f, id); err == nil {
			t.Fatalf("run %d did not fail", run)
		}
		if attempts(t, f) != run {
			t.Fatalf("run %d: %d attempts", run, attempts(t, f))
		}
		// The failed job is swept away, as the daily sweep does.
		if _, err := f.pool.Exec(context.Background(), "DELETE FROM subtitle_jobs"); err != nil {
			t.Fatal(err)
		}
		f.scan(t)
	}
	if len(objects(t, assets, id)) == 0 {
		t.Fatal("no picture was made before the kill, so the test proves nothing")
	}
	// At the cap the job gives up: it reads nothing, deletes what was made and
	// records the version as done with no pictures, and nothing more is queued.
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued at the cap, want 1 to give up", n)
	}
	mustSync(t, f, id)
	if attempts(t, f) != 3 || rowState(t, f) != "done" || len(objects(t, assets, id)) != 0 {
		t.Fatalf("after giving up: %d attempts, row %s, objects %v", attempts(t, f), rowState(t, f), objects(t, assets, id))
	}
	finishJobs(t, f)
	f.scan(t)
	if n := queued(t, f); n != 0 {
		t.Fatalf("%d jobs queued after giving up, want none", n)
	}
	if tags := scalar[int](t, f.pool, "SELECT cardinality(keys) FROM chapter_images"); tags != 0 {
		t.Fatalf("%d keys recorded after giving up", tags)
	}

	// A replaced file is a new version with a fresh count.
	time.Sleep(10 * time.Millisecond)
	steppedClip(t, film, 0, 20, 30)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued for a new file version, want 1", n)
	}
	if err := syncImages(t, f, id); err == nil || attempts(t, f) != 1 {
		t.Fatalf("new version: error %v, %d attempts, want a failure and 1", err, attempts(t, f))
	}
	for range 2 {
		if _, err := f.pool.Exec(context.Background(), "DELETE FROM subtitle_jobs"); err != nil {
			t.Fatal(err)
		}
		f.scan(t)
		if err := syncImages(t, f, id); err == nil {
			t.Fatal("did not fail")
		}
	}
	if _, err := f.pool.Exec(context.Background(), "DELETE FROM subtitle_jobs"); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	mustSync(t, f, id) // Gives up.
	if rowState(t, f) != "done" {
		t.Fatalf("row %s at the cap, want done", rowState(t, f))
	}

	// Off: deleting has no cap and takes the row; on again: a fresh count.
	finishJobs(t, f)
	setOption(t, f, false)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued to delete, want 1", n)
	}
	mustSync(t, f, id)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 || len(objects(t, assets, id)) != 0 {
		t.Fatalf("%d rows and %d objects after deleting", n, len(objects(t, assets, id)))
	}
	setOption(t, f, true)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued after turning it on again", n)
	}
}

// What the scan queues (EnqueueChapterImages, which it ends with), for each row
// a source may have.
func TestTheScanQueuesJobsForExactlyTheSourcesWithWork(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	id := filmID(t, f)
	etag := scalar[string](t, f.pool, "SELECT object_etag FROM media_sources WHERE id = $1", id)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	row := func(rowEtag, state string, attempts int) {
		exec("DELETE FROM chapter_images")
		exec(`INSERT INTO chapter_images (source_id, library_id, etag, state, attempts) VALUES ($1, $2, $3, $4, $5)`, id, f.lib.ID, rowEtag, state, attempts)
	}
	cases := []struct {
		name  string
		setup func()
		want  int
	}{
		{"no row", func() { exec("DELETE FROM chapter_images") }, 1},
		{"done for this file", func() { row(etag, "done", 1) }, 0},
		{"done for another file", func() { row("other", "done", 1) }, 1},
		{"making for another file, at the cap", func() { row("other", "making", 3) }, 1},
		{"making, attempts left", func() { row(etag, "making", 2) }, 1},
		{"making, at the cap: a job to give up", func() { row(etag, "making", 3) }, 1},
		{"given up: done with no keys", func() { row(etag, "done", 3) }, 0},
		{"option off, a row at the cap", func() { setOption(t, f, false); row(etag, "making", 3) }, 1},
		{"option off, no row", func() { setOption(t, f, false); exec("DELETE FROM chapter_images") }, 0},
		{"no chapters, a row", func() { exec("UPDATE media_sources SET chapters = '[]'"); row(etag, "done", 1) }, 1},
		{"no video stream, no row", func() {
			exec(`UPDATE media_sources SET streams = '[{"Type":"Audio"}]'`)
			exec("DELETE FROM chapter_images")
		}, 0},
		{"a row with no source", func() {
			exec("DELETE FROM chapter_images")
			exec(`INSERT INTO chapter_images (source_id, library_id, etag, state, attempts) VALUES (gen_random_uuid(), $1, 'e', 'done', 1)`, f.lib.ID)
			setOption(t, f, false)
		}, 1},
	}
	for _, c := range cases {
		exec("DELETE FROM subtitle_jobs")
		setOption(t, f, true)
		exec(`UPDATE media_sources SET chapters = '[{"StartPositionTicks":0,"Name":"a"}]', streams = '[{"Type":"Video"}]'`)
		c.setup()
		if err := library.EnqueueChapterImages(ctx, f.pool, f.lib.ID); err != nil {
			t.Fatal(err)
		}
		if got := queued(t, f); got != c.want {
			t.Errorf("%s: %d jobs queued, want %d", c.name, got, c.want)
		}
	}

	// A job already queued or running is left as it is.
	exec("DELETE FROM subtitle_jobs")
	exec("DELETE FROM chapter_images")
	setOption(t, f, true)
	exec("UPDATE media_sources SET chapters = '[{\"StartPositionTicks\":0,\"Name\":\"a\"}]', streams = '[{\"Type\":\"Video\"}]'")
	exec(`INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, lease_expires_at, attempts)
		VALUES ('chapters/' || $1, 'chapters', $1, $1, 0, 'running', 'w1', now() + interval '1 hour', 1)`, id)
	if err := library.EnqueueChapterImages(ctx, f.pool, f.lib.ID); err != nil {
		t.Fatal(err)
	}
	if state, worker := scalar[string](t, f.pool, "SELECT state FROM subtitle_jobs"), scalar[string](t, f.pool, "SELECT worker FROM subtitle_jobs"); state != "running" || worker != "w1" {
		t.Errorf("a running job became %s of %s", state, worker)
	}
}

// Leak: a job fails midway, leaving a picture, and the option is then turned
// off. The next scan's job leaves nothing under the prefix and no row.
func TestAJobFailingMidwayThenTheOptionOffLeavesNothing(t *testing.T) {
	f, assets := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	id := filmID(t, f)
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) kill -9 $$;; esac`)
	if err := syncImages(t, f, id); err == nil {
		t.Fatal("the job did not fail")
	}
	if n := len(objects(t, assets, id)); n != 1 {
		t.Fatalf("%d pictures left by the failed job, want 1", n)
	}
	setOption(t, f, false)
	finishJobs(t, f)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued, want 1", n)
	}
	mustSync(t, f, id)
	if found := objects(t, assets, id); len(found) != 0 {
		t.Errorf("objects left: %v", found)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 {
		t.Errorf("%d rows left", n)
	}
}

// gatedJob starts the job of a source with an ffmpeg that waits for the gate to
// open, and returns what opens it and waits for the job's result.
func gatedJob(t *testing.T, f fixture, id string) (release func() error) {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	fakeFfmpeg(t, `while [ ! -e `+gate+` ]; do sleep 0.1; done`)
	done := make(chan error, 1)
	go func() { done <- syncImages(t, f, id) }()
	// The job has started when its row says making.
	deadline := time.Now().Add(20 * time.Second)
	for scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the job did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() error {
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return <-done
	}
}

// Leak: the file is replaced while its job runs. The scan queues the new
// version; the old job's pictures are not served (its row is for the old ETag)
// and the new version's job deletes them, leaving only its own.
func TestAFileReplacedWhileItsJobRunsLeavesNothingStale(t *testing.T) {
	f, assets := chapterFixture(t, true)
	film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
	steppedClip(t, film, 0, 20, 45)
	f.scan(t)
	id := filmID(t, f)
	oldEtag := scalar[string](t, f.pool, "SELECT object_etag FROM media_sources WHERE id = $1", id)
	release := gatedJob(t, f, id)
	time.Sleep(10 * time.Millisecond)
	steppedClip(t, film, 0, 30)
	f.scan(t)
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if len(objects(t, assets, id)) == 0 {
		t.Fatal("the old job made nothing, so the test shows nothing")
	}
	if got := scalar[string](t, f.pool, "SELECT etag FROM chapter_images"); got != oldEtag {
		t.Fatalf("row for %q, want the old version %q", got, oldEtag)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images c JOIN media_sources m ON m.id = c.source_id AND m.object_etag = c.etag"); n != 0 {
		t.Fatal("the old version's pictures would be served")
	}
	finishJobs(t, f)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued after the replacement, want 1", n)
	}
	mustSync(t, f, id)
	newDir := library.ChapterDir(id, scalar[string](t, f.pool, "SELECT object_etag FROM media_sources WHERE id = $1", id))
	for _, key := range objects(t, assets, id) {
		if !strings.HasPrefix(key, newDir+"/") {
			t.Errorf("stale object %s", key)
		}
	}
	if n := len(objects(t, assets, id)); n != 2 {
		t.Errorf("%d objects, want the 2 of the new version", n)
	}
	if rowState(t, f) != "done" {
		t.Errorf("row %s", rowState(t, f))
	}
}

// If a newer job moved the row on while an older one ran, the older one writes
// nothing (what it made is removed by the sweep of the job after: see
// TestAFileReplacedWhileItsJobRunsLeavesNothingStale).
func TestAnOldJobWritesNothingOnceTheRowMovedOn(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	id := filmID(t, f)
	release := gatedJob(t, f, id)
	if _, err := f.pool.Exec(context.Background(), "UPDATE chapter_images SET etag = 'newer', state = 'making', attempts = 1"); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, f.pool, "SELECT etag || ' ' || state || ' ' || cardinality(keys)::text FROM chapter_images"); got != "newer making 0" {
		t.Errorf("the old job wrote to the newer row: %q", got)
	}
}

// Leak: the file loses its chapters. The next scan's job deletes the pictures
// and the row.
func TestChaptersGoneLeaveNothing(t *testing.T) {
	f, assets := chapterFixture(t, true)
	film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
	steppedClip(t, film, 0, 20)
	f.scan(t)
	id := filmID(t, f)
	mustSync(t, f, id)
	finishJobs(t, f)
	time.Sleep(10 * time.Millisecond)
	if err := os.Remove(film); err != nil {
		t.Fatal(err)
	}
	clip(t, film)
	f.scan(t)
	if n := queued(t, f); n != 1 {
		t.Fatalf("%d jobs queued after the chapters went, want 1", n)
	}
	mustSync(t, f, id)
	if found := objects(t, assets, id); len(found) != 0 {
		t.Errorf("objects left: %v", found)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 {
		t.Errorf("%d rows left", n)
	}
}

// Leak: a source is removed by a real scan. The row outlives it, the next scan
// queues the job, which finds the asset store through the row's library and
// leaves nothing. On a directory and on every S3 store.
func TestARemovedSourceLeavesNothing(t *testing.T) {
	targets := map[string]string{"posix": ""}
	for _, entry := range testenv.S3(t) {
		if name, _, found := strings.Cut(entry, "="); found {
			targets[name] = entry
		}
	}
	for name, entry := range targets {
		t.Run(name, func(t *testing.T) {
			uri := "file://" + t.TempDir()
			if entry != "" {
				uri = s3Root(t, entry)
			}
			f, assets := chapterFixtureAt(t, true, uri)
			film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
			steppedClip(t, film, 0, 20)
			f.scan(t)
			id := filmID(t, f)
			mustSync(t, f, id)
			if n := len(objects(t, assets, id)); n != 2 {
				t.Fatalf("%d pictures before the removal, want 2", n)
			}
			finishJobs(t, f)
			if err := os.Remove(film); err != nil {
				t.Fatal(err)
			}
			f.scan(t)
			if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources"); n != 0 {
				t.Fatalf("%d sources left by the scan", n)
			}
			if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 1 {
				t.Fatalf("%d rows after the removal, want the one that outlives the source", n)
			}
			if n := queued(t, f); n != 1 {
				t.Fatalf("%d jobs queued after the removal, want 1", n)
			}
			mustSync(t, f, id)
			if found := objects(t, assets, id); len(found) != 0 {
				t.Errorf("objects left: %v", found)
			}
			if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images"); n != 0 {
				t.Errorf("%d rows left", n)
			}
		})
	}
}

// A file with no video stream has no frame to take, so it is not queued.
func TestChapterImagesAreNotQueuedForAFileWithoutVideo(t *testing.T) {
	f, _ := chapterFixture(t, true)
	file := filepath.Join(f.media, "Audio Film (2020)", "Audio Film (2020).mkv")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(t.TempDir(), "meta.txt")
	if err := os.WriteFile(meta, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1\nSTART=0\nEND=5\ntitle=A\n[CHAPTER]\nTIMEBASE=1/1\nSTART=5\nEND=10\ntitle=B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=duration=10", "-i", meta,
		"-map", "0:a", "-map_metadata", "1", "-map_chapters", "1", "-c:a", "aac", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	f.scan(t)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources WHERE jsonb_array_length(chapters) = 2"); n != 1 {
		t.Fatalf("the audio file with chapters was not scanned (%d)", n)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM subtitle_jobs"); n != 0 {
		t.Errorf("%d jobs queued for a file with no video", n)
	}
}

// An HDR film's pictures are tone mapped as playback is: the same pixels
// tagged SDR come out different from what they do tagged PQ.
func TestChapterImagesOfAnHDRFilmAreToneMapped(t *testing.T) {
	tagged := func(transfer string) int {
		f, assets := chapterFixture(t, true)
		encodedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), []string{
			"-pix_fmt", "yuv420p10le", "-x264-params", "colorprim=bt2020:transfer=" + transfer + ":colormatrix=bt2020nc"}, 40)
		f.scan(t)
		mustSync(t, f, filmID(t, f))
		return brightness(t, assets, chapterKeys(t, f)[0])
	}
	sdr, pq := tagged("bt709"), tagged("smpte2084")
	t.Logf("sdr %d, pq %d", sdr, pq)
	if max(sdr-pq, pq-sdr) < 20 {
		t.Errorf("brightness %d tagged PQ against %d tagged SDR: the picture was not tone mapped", pq, sdr)
	}
}

// downMedia is a media store that is on S3 as far as the job can tell (no
// local path) and fails: at Open, or after the first bytes of a read.
type downMedia struct {
	store.Store
	midRead bool
}

func (downMedia) LocalPath(string) (string, bool) { return "", false }

func (d downMedia) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if !d.midRead {
		return nil, errors.New("media store is down")
	}
	return io.NopCloser(io.MultiReader(strings.NewReader("x"), errReader{})), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A failure of the media store reaches ffmpeg only as a failed read, but it is
// the store's doing and not the file's: the job fails so that it is retried,
// and the row is not done.
func TestAMediaStoreFailureFailsTheChapterJob(t *testing.T) {
	for name, midRead := range map[string]bool{"at open": false, "during a read": true} {
		t.Run(name, func(t *testing.T) {
			f, _ := chapterFixture(t, true)
			steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
			f.scan(t)
			open := func(uri string) (store.Store, error) {
				real, err := store.Open(uri)
				if uri == "file://"+f.media {
					return downMedia{real, midRead}, err
				}
				return real, err
			}
			if err := syncImagesWith(context.Background(), f, filmID(t, f), open); err == nil {
				t.Fatalf("a down media store made the job done; keys recorded: %q", chapterKeys(t, f))
			}
			if rowState(t, f) != "making" {
				t.Errorf("row %s after a failed job", rowState(t, f))
			}
		})
	}
}

// ffmpeg missing on the worker is the worker's doing, not the file's.
func TestAMissingFfmpegFailsTheChapterJob(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	t.Setenv("PATH", t.TempDir())
	if err := syncImages(t, f, filmID(t, f)); err == nil {
		t.Fatalf("the job was done without ffmpeg; keys recorded: %q", chapterKeys(t, f))
	}
}

// s3Root returns the URI of a fresh, empty place on a target ("name=URI").
// Credentials go in the environment, as they do for the server, never in the URI.
func s3Root(t *testing.T, entry string) string {
	t.Helper()
	_, raw, _ := strings.Cut(entry, "=")
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" {
		t.Fatalf("no S3 target in %q", entry)
	}
	query := parsed.Query()
	t.Setenv("JELLYSCALE_S3_ACCESS_KEY", query.Get("key"))
	t.Setenv("JELLYSCALE_S3_SECRET_KEY", query.Get("secret"))
	query.Del("key")
	query.Del("secret")
	parsed.RawQuery = query.Encode()
	parsed.Path = "/chapters-" + uuid.NewString()
	return parsed.String()
}

// cancelOnList cancels the job's context in the first List, as a worker stopping
// while the job sweeps would.
type cancelOnList struct {
	store.Store
	cancel context.CancelFunc
}

func (c cancelOnList) List(ctx context.Context, prefix string) ([]store.Object, error) {
	c.cancel()
	return nil, ctx.Err()
}

// A job's attempt is counted before it touches the asset store and is never
// given back, so jobs cut off while listing (a stopping worker, a store that
// is down) use up the 3 starts of a file version and the next job gives up.
func TestJobsCutOffAfterTakingAnAttemptUseUpTheCap(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	id := filmID(t, f)

	for i := 1; i <= 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		err := syncImagesWith(ctx, f, id, func(uri string) (store.Store, error) {
			assets, err := store.Open(uri)
			return cancelOnList{assets, cancel}, err
		})
		cancel()
		if err == nil || attempts(t, f) != i {
			t.Fatalf("job %d cut off while listing returned %v with %d attempts, want an error and %d", i, err, attempts(t, f), i)
		}
	}
	mustSync(t, f, id)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM chapter_images WHERE state = 'done' AND keys = '{}'"); n != 1 {
		t.Fatalf("%d rows given up after 3 cut-off jobs, want 1", n)
	}
}

// A first chapter whose frame errors or hangs is skipped like any other: the
// job is done and the other chapters keep their pictures. (jellyfin-web then
// lists no chapters, as it needs the first one's picture; README says so.)
func TestAFirstChapterWithoutAPictureIsSkippedLikeAnyOther(t *testing.T) {
	for name, script := range map[string]string{
		"error": `case " $* " in *" -ss 0.000 "*) echo boom >&2; exit 1;; esac`,
		"hang":  `case " $* " in *" -ss 0.000 "*) exec sleep 60;; esac`,
	} {
		t.Run(name, func(t *testing.T) {
			f, assets := chapterFixture(t, true)
			steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
			f.scan(t)
			id := filmID(t, f)
			defer library.SetChapterFrameTimeout(time.Second)()
			fakeFfmpeg(t, script)
			mustSync(t, f, id)
			if rowState(t, f) != "done" || len(objects(t, assets, id)) != 2 {
				t.Fatalf("row %s, objects %v: want done with the other two pictures", rowState(t, f), objects(t, assets, id))
			}
		})
	}
}

type unlistableStore struct{ store.Store }

func (unlistableStore) List(context.Context, string) ([]store.Object, error) {
	return nil, errors.New("access denied")
}

// A job that fails before it reads anything, here on listing the asset store,
// has still taken its attempt, so a store that keeps failing reaches the cap.
func TestAFailureBeforeAnyPictureCountsAnAttempt(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	id := filmID(t, f)
	open := func(uri string) (store.Store, error) {
		s, err := store.Open(uri)
		return unlistableStore{s}, err
	}
	for run := 1; run <= 3; run++ {
		if err := syncImagesWith(context.Background(), f, id, open); err == nil {
			t.Fatalf("run %d did not fail", run)
		}
	}
	if attempts(t, f) != 3 {
		t.Fatalf("%d attempts after three failed lists, want 3", attempts(t, f))
	}
}

// From a POSIX store ffmpeg reads the file itself, so a file it can't open
// must fail the job, not be taken for frames that can't be read.
func TestAnUnreadablePosixFileFailsTheChapterJob(t *testing.T) {
	f, _ := chapterFixture(t, true)
	film := filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv")
	steppedClip(t, film, 0, 20, 45)
	f.scan(t)
	if err := os.Chmod(film, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(film, 0o644)
	if err := syncImages(t, f, filmID(t, f)); err == nil {
		t.Fatalf("an unreadable file made the job done; keys recorded: %q", chapterKeys(t, f))
	}
	if rowState(t, f) != "making" {
		t.Errorf("row %s after a failed job, want making", rowState(t, f))
	}
}

// A picture whose write was cut off leaves a .put- file in the version's
// directory; the next job deletes it rather than taking it for a picture.
func TestACutOffPictureWriteIsDeleted(t *testing.T) {
	f, assets := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	id := filmID(t, f)
	mustSync(t, f, id)
	dir := path.Dir(objects(t, assets, id)[0])
	if err := assets.Put(context.Background(), dir+"/.put-123", strings.NewReader("half")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE chapter_images SET state = 'making'"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, f, id)
	for _, key := range objects(t, assets, id) {
		if strings.Contains(key, ".put-") {
			t.Fatalf("%s kept after the next job", key)
		}
	}
}

// A skip is final for the file version even when the attempt that made it
// fails later: the next attempt doesn't read that chapter again.
func TestASkipOutlivesTheAttemptThatMadeIt(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20, 45)
	f.scan(t)
	id := filmID(t, f)
	dir := t.TempDir()
	calls, kill := filepath.Join(dir, "calls"), filepath.Join(dir, "kill")
	if err := os.WriteFile(kill, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Chapter 0 always errors; chapter 20 kills ffmpeg while the kill file is there.
	fakeFfmpeg(t, `case " $* " in
		*" -ss 0.000 "*) echo 0 >> `+calls+`; echo boom >&2; exit 1;;
		*" -ss 20.000 "*) [ -e `+kill+` ] && kill -9 $$;;
	esac`)
	if err := syncImages(t, f, id); err == nil {
		t.Fatal("a killed ffmpeg did not fail the job")
	}
	if err := os.Remove(kill); err != nil {
		t.Fatal(err)
	}
	mustSync(t, f, id)
	read, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(read), "0\n"); n != 1 {
		t.Fatalf("chapter 0 read %d times, want once", n)
	}
	if keys := chapterKeys(t, f); len(keys) != 3 || keys[0] != "" || keys[1] == "" || keys[2] == "" {
		t.Fatalf("keys %q, want a skip and two pictures", keys)
	}
}

// stalledMedia is a POSIX store, so ffmpeg reads the file itself, whose Open
// never answers: a store that stalled the frame stalls the check after it too.
type stalledMedia struct{ store.Store }

func (stalledMedia) Open(ctx context.Context, _ string, _ int64) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// hungMedia opens at once but its read never returns, whatever the context:
// a POSIX file on a mount that hung.
type hungMedia struct{ store.Store }

func (hungMedia) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(hungReader{}), nil
}

type hungReader struct{}

func (hungReader) Read([]byte) (int, error) { select {} }

// The check that a file can still be read, after a frame failed, has its own
// deadline: a store that never answers fails the job, it doesn't hold the slot.
func TestAStalledStoreAfterAHungFrameFailsTheJob(t *testing.T) {
	f, _ := chapterFixture(t, true)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	defer library.SetChapterFrameTimeout(time.Second)()
	defer library.SetReadableTimeout(time.Second)()
	fakeFfmpeg(t, `case " $* " in *" -ss 20.000 "*) exec sleep 60;; esac`)
	for name, wrap := range map[string]func(store.Store) store.Store{
		"open waits":      func(s store.Store) store.Store { return stalledMedia{s} },
		"read never ends": func(s store.Store) store.Store { return hungMedia{s} },
	} {
		t.Run(name, func(t *testing.T) {
			open := func(uri string) (store.Store, error) {
				real, err := store.Open(uri)
				if uri == "file://"+f.media {
					return wrap(real), err
				}
				return real, err
			}
			done := make(chan error, 1)
			go func() { done <- syncImagesWith(context.Background(), f, filmID(t, f), open) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("a stalled store made the job done; keys %q", chapterKeys(t, f))
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the job still runs 20 s after the hung frame's deadline")
			}
			// The store, not the file, failed the frame: no skip is recorded,
			// so the next attempt reads that chapter again.
			if keys := chapterKeys(t, f); len(keys) > 1 {
				t.Fatalf("keys %q: the stalled chapter was recorded", keys)
			}
		})
	}
}

// A scan of a POSIX library whose root is gone (an unmounted volume) fails and
// keeps its items, rather than taking it for a library with nothing in it.
func TestAScanOfAMissingMediaRootKeepsItsItems(t *testing.T) {
	f, _ := chapterFixture(t, false)
	steppedClip(t, filepath.Join(f.media, "Stepped Film (2020)", "Stepped Film (2020).mkv"), 0, 20)
	f.scan(t)
	if err := os.Rename(f.media, f.media+".unmounted"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(f.media+".unmounted", f.media)
	if err := f.scanner.Scan(context.Background(), f.lib); err == nil {
		t.Error("a scan of a missing media root succeeded")
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources"); n != 1 {
		t.Fatalf("%d media sources after the scan, want the 1 kept", n)
	}
}
