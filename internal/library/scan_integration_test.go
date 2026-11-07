//go:build integration

package library_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/library"

	"jellyscale/internal/testenv"
)

// These need a disposable database and ffmpeg; test/integration.sh provides both.
func openDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testenv.DatabaseURL(t)
	ctx := context.Background()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "TRUNCATE libraries, users CASCADE"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func clip(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

type fixture struct {
	pool    *pgxpool.Pool
	scanner *library.Scanner
	lib     library.Library
	media   string
}

func setup(t *testing.T, kind string) fixture {
	t.Helper()
	pool := openDB(t)
	media, assets := t.TempDir(), t.TempDir()
	libs, err := library.Sync(context.Background(), pool, []library.Config{
		{Name: "test", Kind: kind, Media: "file://" + media, Assets: "file://" + assets},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool, &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler)}, libs[0], media}
}

func (f fixture) scan(t *testing.T) {
	t.Helper()
	if err := f.scanner.Scan(context.Background(), f.lib); err != nil {
		t.Fatal(err)
	}
}

// scalar reads the one value a query returns.
func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var value T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return value
}

func (f fixture) count(t *testing.T, kind string) int {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT count(*) FROM items WHERE type = $1", kind)
}

func TestRenameKeepsItemAndWatchState(t *testing.T) {
	f := setup(t, "movies")
	ctx := context.Background()
	old := filepath.Join(f.media, "Old Name (2020)", "Old Name (2020).mkv")
	clip(t, old)
	f.scan(t)

	id := scalar[string](t, f.pool, "SELECT id::text FROM items")
	user := scalar[string](t, f.pool, `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), 'u', 'x') RETURNING id::text`)
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played) VALUES ($1, $2, true)`, user, id); err != nil {
		t.Fatal(err)
	}

	renamed := filepath.Join(f.media, "New Name (2021)", "New Name (2021).mkv")
	if err := os.MkdirAll(filepath.Dir(renamed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(old)); err != nil {
		t.Fatal(err)
	}
	f.scan(t)

	if n := f.count(t, "Movie"); n != 1 {
		t.Fatalf("expected one movie, got %d", n)
	}
	var name string
	var year int
	var played bool
	if err := f.pool.QueryRow(ctx, `SELECT i.name, i.production_year, d.played FROM items i
		JOIN user_item_data d ON d.item_id = i.id WHERE i.id = $1`, id).Scan(&name, &year, &played); err != nil {
		t.Fatalf("item or watch state lost: %v", err)
	}
	if name != "New Name" || year != 2021 || !played {
		t.Fatalf("got %q %d played=%v", name, year, played)
	}
	if keys := scalar[int](t, f.pool, "SELECT count(*) FROM scan_objects"); keys != 1 {
		t.Fatalf("expected the old scan object to be gone, got %d", keys)
	}
}

func TestChangedFileKeepsItem(t *testing.T) {
	f := setup(t, "movies")
	file := filepath.Join(f.media, "Film (2020).mkv")
	clip(t, file)
	f.scan(t)
	before := scalar[string](t, f.pool, "SELECT id::text FROM items")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	clip(t, file)
	f.scan(t)
	if after := scalar[string](t, f.pool, "SELECT id::text FROM items"); before != after {
		t.Fatal("replacing the file's content changed its item")
	}
}

func TestRepeatScanChangesNothing(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020).mkv"))
	f.scan(t)
	before := scalar[string](t, f.pool, "SELECT dto::text FROM items")
	f.scan(t)
	if after := scalar[string](t, f.pool, "SELECT dto::text FROM items"); before != after {
		t.Fatal("repeat scan rewrote an unchanged item")
	}
}

func TestDeletionPrunesEmptyParents(t *testing.T) {
	f := setup(t, "tvshows")
	first := filepath.Join(f.media, "Show", "Season 01", "Show S01E01.mkv")
	second := filepath.Join(f.media, "Show", "Season 02", "Show S02E01.mkv")
	clip(t, first)
	clip(t, second)
	// Another show stays, as a scan refuses to take an empty library for one
	// whose films were all removed.
	clip(t, filepath.Join(f.media, "Other", "Season 01", "Other S01E01.mkv"))
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 2 || se != 3 || e != 3 {
		t.Fatalf("got series=%d seasons=%d episodes=%d", s, se, e)
	}

	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 2 || se != 2 || e != 2 {
		t.Fatalf("after one removal: series=%d seasons=%d episodes=%d", s, se, e)
	}

	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 1 || se != 1 || e != 1 {
		t.Fatalf("after all removals: series=%d seasons=%d episodes=%d", s, se, e)
	}
}

func TestConcurrentScansDoNotDuplicate(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020).mkv"))
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The scans that lose the lock say so.
			if err := f.scanner.Scan(context.Background(), f.lib); err != nil && !errors.Is(err, library.ErrElsewhere) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count(t, "Movie"); n != 1 {
		t.Fatalf("expected one movie, got %d", n)
	}
}

func TestReplicasCanSyncLibrariesTogether(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	configs := []library.Config{{Name: "race", Kind: "movies", Media: "file://" + dir, Assets: "file://" + dir}}
	// Only the first insert of a row can race, so start from empty every round.
	for range 40 {
		if _, err := pool.Exec(ctx, "DELETE FROM libraries"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := library.Sync(ctx, pool, configs); err != nil {
					t.Error(err)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
}

func TestReplicasCanMigrateTogether(t *testing.T) {
	dsn := testenv.DatabaseURL(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS migrate_race"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE migrate_race"); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = "migrate_race"

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := db.Open(ctx, config.ConnString())
			if err != nil {
				t.Error(err)
				return
			}
			pool.Close()
		}()
	}
	wg.Wait()
}

// A source stored before the probe recorded level, field order and range is
// probed again by the next scan, though the file hasn't changed.
func TestScanProbesAgainWhatWasStoredWithoutRange(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Old Film (2020)", "Old Film (2020).mkv"))
	f.scan(t)
	const probed = "SELECT streams @> '[{\"VideoRangeType\": \"SDR\"}]' FROM media_sources"
	if !scalar[bool](t, f.pool, probed) {
		t.Fatal("a fresh scan didn't record the range")
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE media_sources SET streams = '[{"Index":0,"Type":"Video","Codec":"h264"}]'`); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if !scalar[bool](t, f.pool, probed) {
		t.Fatal("the scan left the old streams alone")
	}
	if n := f.count(t, "Movie"); n != 1 {
		t.Fatalf("%d movies after probing again", n)
	}
	// One that has the range but not the transfer function, which tone mapping
	// follows, is probed again too.
	const transfer = "SELECT streams @> '[{\"ColorTransfer\": \"\"}]' FROM media_sources"
	if !scalar[bool](t, f.pool, transfer) {
		t.Fatal("a fresh scan didn't record the transfer function")
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE media_sources SET streams = '[{"Index":0,"Type":"Video","Codec":"h264","VideoRangeType":"SDR"}]'`); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if !scalar[bool](t, f.pool, transfer) {
		t.Fatal("the scan left streams without a transfer function alone")
	}
}

func keyframeCount(t *testing.T, pool *pgxpool.Pool) *int {
	t.Helper()
	return scalar[*int](t, pool, "SELECT cardinality(keyframes) FROM media_sources")
}

// A scan reads the keyframe index of an H.264, HEVC or AV1 file in Matroska or
// MP4; a file with none to read is marked, so it isn't looked at again; one in
// another codec is never looked at.
func TestScanReadsTheKeyframeIndex(t *testing.T) {
	f := setup(t, "movies")
	var logged bytes.Buffer
	f.scanner.Log = slog.New(slog.NewTextHandler(&logged, nil))
	file := filepath.Join(f.media, "Indexed Film (2020)", "Indexed Film (2020).mkv")
	clip(t, file)
	f.scan(t)
	if n := keyframeCount(t, f.pool); n == nil || *n == 0 {
		t.Fatalf("an H.264 file in Matroska: keyframes %v", n)
	}
	// The 2 s clip's last frame ends at 2 s.
	if end := scalar[int64](t, f.pool, "SELECT video_end FROM media_sources"); end < 19_000_000 || end > 21_000_000 {
		t.Fatalf("the video ends at %d ticks, want about 20000000", end)
	}

	// A file that has no Cues is stored with an empty index, not NULL, which
	// would have every scan look again.
	live := filepath.Join(f.media, "Live Film (2021)", "Live Film (2021).mkv")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-live", "1", live).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	f.scan(t)
	if n := scalar[*int](t, f.pool, "SELECT cardinality(keyframes) FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Live Film'"); n == nil || *n != 0 {
		t.Fatalf("a file with no Cues: keyframes %v, want an empty array", n)
	}
	// An operator can see why the film is re-encoded.
	if !strings.Contains(logged.String(), "level=INFO") || !strings.Contains(logged.String(), "Live Film") || !strings.Contains(logged.String(), "SeekHead names no Cues") {
		t.Errorf("the scan did not say why the film has no index:\n%s", logged.String())
	}

	// Stored before keyframes were read: the next scan fills them in.
	if _, err := f.pool.Exec(context.Background(), "UPDATE media_sources SET keyframes = NULL"); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources WHERE keyframes IS NULL"); n != 0 {
		t.Fatalf("%d sources still without an index after a scan", n)
	}

	// HEVC and AV1 are copied too, in either container, so their index is read.
	// The films are made with the encoders' defaults, which cut open GOPs.
	for _, c := range []struct{ name, container, encoder string }{
		{"Hevc Mkv Film (2022)", "mkv", "libx265"}, {"Hevc Mp4 Film (2023)", "mp4", "libx265"},
		{"Av1 Mkv Film (2024)", "mkv", "libsvtav1"}, {"Av1 Mp4 Film (2025)", "mp4", "libsvtav1"},
	} {
		file := filepath.Join(f.media, c.name, c.name+"."+c.container)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
			"-c:v", c.encoder, "-preset", map[string]string{"libx265": "ultrafast", "libsvtav1": "10"}[c.encoder], file).CombinedOutput(); err != nil {
			t.Fatalf("this test needs ffmpeg with %s: %v: %s", c.encoder, err, out)
		}
	}
	// A codec that is never copied is never looked at.
	mpeg4 := filepath.Join(f.media, "Mpeg4 Film (2026)", "Mpeg4 Film (2026).mkv")
	if err := os.MkdirAll(filepath.Dir(mpeg4), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
		"-c:v", "mpeg4", mpeg4).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	f.scan(t)
	for _, name := range []string{"Hevc Mkv Film", "Hevc Mp4 Film", "Av1 Mkv Film", "Av1 Mp4 Film"} {
		n := scalar[*int](t, f.pool, "SELECT cardinality(keyframes) FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = '"+name+"'")
		if n == nil || *n == 0 {
			t.Errorf("%s: keyframes %v, want an index", name, n)
		}
	}
	if n := scalar[*int](t, f.pool, "SELECT cardinality(keyframes) FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Mpeg4 Film'"); n != nil {
		t.Fatalf("an MPEG-4 file has keyframes %v, want NULL", *n)
	}
	// A library scanned before these were copied has no index, and no bit depth
	// in its streams; the next scan reads both without the file changing.
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_sources SET keyframes = NULL,
		streams = (SELECT jsonb_agg(s - 'BitDepth') FROM jsonb_array_elements(streams) s) WHERE container IN ('mkv', 'mp4')`); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := scalar[int](t, f.pool, `SELECT count(*) FROM media_sources m JOIN items i ON i.id = m.item_id
		WHERE i.name IN ('Hevc Mkv Film', 'Hevc Mp4 Film', 'Av1 Mkv Film', 'Av1 Mp4 Film') AND (m.keyframes IS NULL OR NOT m.streams @> '[{"BitDepth": 8}]')`); n != 0 {
		t.Fatalf("%d HEVC and AV1 films still without an index or bit depth after a scan", n)
	}
}

// One source still without an index (a read that keeps failing) must not
// make every other file be probed again on each scan.
func TestScanLooksAgainOnlyAtTheFileWithoutAnIndex(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Stuck Film (2020)", "Stuck Film (2020).mkv"))
	clip(t, filepath.Join(f.media, "Done Film (2021)", "Done Film (2021).mkv"))
	f.scan(t)
	// A value a probe would never write, so a second probe shows.
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_sources m SET keyframes = CASE WHEN i.name = 'Stuck Film' THEN NULL ELSE ARRAY[12345]::bigint[] END
		FROM items i WHERE i.id = m.item_id`); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := scalar[*int](t, f.pool, "SELECT cardinality(keyframes) FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Stuck Film'"); n == nil {
		t.Error("the file without an index was not read again")
	}
	if first := scalar[*int64](t, f.pool, "SELECT keyframes[1] FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Done Film'"); first == nil || *first != 12345 {
		t.Errorf("a file with an index was probed again: first keyframe %v", first)
	}
}

// jellyfin-web offers Download only for an item that says CanDownload.
func TestScannedVideosCanBeDownloaded(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020)", "Film (2020).mkv"))
	f.scan(t)
	if !scalar[bool](t, f.pool, "SELECT (dto->>'CanDownload')::boolean FROM items WHERE type = 'Movie'") {
		t.Fatal("a scanned film says CanDownload false")
	}
	if scalar[bool](t, f.pool, "SELECT coalesce(bool_or((dto->>'CanDownload')::boolean), false) FROM items WHERE type <> 'Movie'") {
		t.Fatal("a folder says CanDownload")
	}
}

// Items scanned before CanDownload was true are fixed by the migration.
func TestMigrationMakesStoredVideosDownloadable(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020)", "Film (2020).mkv"))
	f.scan(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE items SET dto = jsonb_set(dto, '{CanDownload}', 'false')`); err != nil {
		t.Fatal(err)
	}
	sql, err := os.ReadFile("../db/migrations/0009_can_download.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, f.pool, "SELECT (dto->>'CanDownload')::boolean FROM items WHERE type = 'Movie'") {
		t.Fatal("the stored film is still not downloadable")
	}
}

// pgsClip writes a two second film with one PGS track, as a Blu-ray remux has.
func pgsClip(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := filepath.Join(t.TempDir(), "track.sup")
	if err := os.WriteFile(sup, testenv.PGS(1920, 1080, testenv.Cue{Start: 0.5, End: 1.5, X: 100, Y: 100, W: 200, H: 50}), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
		"-i", sup, "-map", "0:v", "-map", "1", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "copy", "-metadata:s:s:0", "language=eng", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// A PGS track is listed as Jellyfin lists it, and the track of a film stored
// before image tracks were listed is listed by the next scan, without reading
// the film's keyframes again.
func TestScanListsImageSubtitles(t *testing.T) {
	f := setup(t, "movies")
	pgsClip(t, filepath.Join(f.media, "Disc Film (2020)", "Disc Film (2020).mkv"))
	f.scan(t)
	const listed = `SELECT dto->'MediaStreams' @> '[{"Type":"Subtitle","Codec":"PGSSUB","Language":"eng","DisplayTitle":"eng - PGSSUB","Index":1,"Width":1920,"Height":1080,"SupportsExternalStream":true,"IsTextSubtitleStream":false}]' FROM items WHERE type = 'Movie'`
	if !scalar[bool](t, f.pool, listed) {
		t.Fatalf("the track is not listed: %s", scalar[string](t, f.pool, "SELECT dto->'MediaStreams' FROM items WHERE type = 'Movie'"))
	}
	if !scalar[bool](t, f.pool, `SELECT streams @> '[{"Type":"Subtitle","IsImageSubtitleStream":true}]' FROM media_sources`) {
		t.Fatal("the stored track is not marked as an image one")
	}

	// As an earlier version stored it: not listed, and with no word that it is an image track.
	mustExec := func(sql string) {
		t.Helper()
		if _, err := f.pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`UPDATE media_sources SET keyframes = '{7}', streams = (SELECT jsonb_agg(s - 'IsImageSubtitleStream') FROM jsonb_array_elements(streams) s)`)
	mustExec(`UPDATE items SET dto = jsonb_set(jsonb_set(dto, '{MediaStreams}', (SELECT jsonb_agg(s) FROM jsonb_array_elements(dto->'MediaStreams') s WHERE s->>'Type' <> 'Subtitle')), '{MediaSources,0,MediaStreams}', (SELECT jsonb_agg(s) FROM jsonb_array_elements(dto->'MediaStreams') s WHERE s->>'Type' <> 'Subtitle'))`)
	if scalar[bool](t, f.pool, listed) {
		t.Fatal("the test did not hide the track")
	}
	f.scan(t)
	if !scalar[bool](t, f.pool, listed) || !scalar[bool](t, f.pool, `SELECT dto->'MediaSources'->0->'MediaStreams' @> '[{"Codec":"PGSSUB"}]' FROM items WHERE type = 'Movie'`) {
		t.Fatal("the scan left the old track unlisted")
	}
	if got := scalar[string](t, f.pool, "SELECT keyframes::text FROM media_sources"); got != "{7}" {
		t.Fatalf("the film's keyframes were read again: %s", got)
	}
	// Listed, so it is left alone from now on.
	mustExec(`UPDATE media_sources SET keyframes = '{8}'`)
	f.scan(t)
	if got := scalar[string](t, f.pool, "SELECT keyframes::text FROM media_sources"); got != "{8}" {
		t.Fatalf("a film with a listed image track was probed again: %s", got)
	}
}

// chapterClip makes a film with chapters at the given start seconds, titled
// "Part N" (N from 1) and the last untitled.
func chapterClip(t *testing.T, file string, starts ...int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := ";FFMETADATA1\n"
	for i, start := range starts {
		end := 60
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		meta += fmt.Sprintf("[CHAPTER]\nTIMEBASE=1/1\nSTART=%d\nEND=%d\n", start, end)
		if i+1 < len(starts) {
			meta += fmt.Sprintf("title=Part %d\n", i+1)
		}
	}
	metaFile := filepath.Join(t.TempDir(), "meta.txt")
	if err := os.WriteFile(metaFile, []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=60", "-i", metaFile,
		"-map", "0:v", "-map_metadata", "1", "-map_chapters", "1", "-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

const chapterNames = "SELECT dto->'Chapters' FROM items WHERE type = 'Movie' AND name = $1"

// A scan stores a film's chapters in its item and in the media source; a film
// with none is stored with an empty list, so it is not read again; a film
// stored before chapters were read is probed again without its keyframe index;
// and a file that changes gets its new chapters.
func TestScanStoresChapters(t *testing.T) {
	f := setup(t, "movies")
	chaptered := filepath.Join(f.media, "Chaptered Film (2020)", "Chaptered Film (2020).mkv")
	chapterClip(t, chaptered, 0, 20, 45)
	clip(t, filepath.Join(f.media, "Plain Film (2021)", "Plain Film (2021).mkv"))
	f.scan(t)

	var chapters []map[string]any
	if err := json.Unmarshal(scalar[[]byte](t, f.pool, chapterNames, "Chaptered Film"), &chapters); err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 3 || chapters[0]["Name"] != "Part 1" || chapters[1]["StartPositionTicks"] != float64(200_000_000) || chapters[2]["Name"] != "Chapter 3" {
		t.Fatalf("the item's chapters: %v", chapters)
	}
	for _, chapter := range chapters {
		if _, ok := chapter["ImageTag"]; ok {
			t.Errorf("a chapter has an ImageTag: %v", chapter)
		}
	}
	if n := scalar[int](t, f.pool, "SELECT jsonb_array_length(m.chapters) FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Chaptered Film'"); n != 3 {
		t.Errorf("the media source has %d chapters", n)
	}
	if got := scalar[string](t, f.pool, chapterNames, "Plain Film"); got != "[]" {
		t.Errorf("a film without chapters: %s", got)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources WHERE chapters IS NULL"); n != 0 {
		t.Fatalf("%d films stored with chapters unread", n)
	}

	// A film with none is not probed again (a probe would reset its name) and
	// neither is one with chapters.
	if _, err := f.pool.Exec(context.Background(), "UPDATE items SET name = name || ' kept' WHERE type = 'Movie'"); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM items WHERE type = 'Movie' AND name LIKE '% kept'"); n != 2 {
		t.Fatalf("a scan with nothing new probed %d of 2 films again", 2-n)
	}

	// Stored before chapters were read: the dto has the empty list every one
	// had, and the column is NULL. The scan reads the chapters, keeps the
	// keyframe index it has (a value a read would not write) and the item.
	id := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Chaptered Film kept'")
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_sources SET chapters = NULL, keyframes = ARRAY[12345]::bigint[];
		UPDATE items SET dto = jsonb_set(dto, '{Chapters}', '[]')`); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if got := scalar[string](t, f.pool, "SELECT name FROM items WHERE id = $1", id); got != "Chaptered Film" {
		t.Fatalf("the film was not probed again: %q", got)
	}
	if n := scalar[int](t, f.pool, "SELECT jsonb_array_length(dto->'Chapters') FROM items WHERE id = $1", id); n != 3 {
		t.Errorf("%d chapters read for the film stored without", n)
	}
	if first := scalar[*int64](t, f.pool, "SELECT keyframes[1] FROM media_sources WHERE item_id = $1", id); first == nil || *first != 12345 {
		t.Errorf("the keyframe index was read again: %v", first)
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM media_sources WHERE chapters IS NULL"); n != 0 {
		t.Errorf("%d films still without chapters read", n)
	}

	// The file is replaced with other chapters: the next scan notices.
	chapterClip(t, chaptered, 0, 30)
	f.scan(t)
	if err := json.Unmarshal(scalar[[]byte](t, f.pool, chapterNames, "Chaptered Film"), &chapters); err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 || chapters[1]["StartPositionTicks"] != float64(300_000_000) {
		t.Errorf("a changed file's chapters: %v", chapters)
	}
	if got := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Chaptered Film'"); got != id {
		t.Errorf("the item changed its id")
	}
}
