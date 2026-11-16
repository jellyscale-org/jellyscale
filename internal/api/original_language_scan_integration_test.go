//go:build integration

package api

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-117: the original language is not stored. A scan runs the media tools it ran before and writes
// the rows it wrote before: no OriginalLanguage in the item, IsOriginal false on every stored stream,
// the same probe version (so an upgrade makes no scan probe an unchanged file again).
func TestAScanStoresNoOriginalLanguageAndRunsNoExtraTool(t *testing.T) {
	testenv.FFmpeg(t)
	e := newEnv(t)
	tools := traceTools(t)
	dir := t.TempDir()
	film := filepath.Join(dir, "film.mkv")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=2", "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2", "-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast",
		"-c:a", "flac", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=deu", "-disposition:a:0", "default", "-disposition:a:1", "0", film)
	data, err := os.ReadFile(film)
	if err != nil {
		t.Fatal(err)
	}
	mediaDir, assetDir := t.TempDir(), t.TempDir()
	media, assets := "file://"+mediaDir, "file://"+assetDir
	files, err := store.Open(media)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Put(context.Background(), "Scanned Film (2020).mkv", strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	libs, err := library.Sync(context.Background(), e.pool, []library.Config{{Name: "Scan", Kind: "movies", Media: media, Assets: assets}})
	if err != nil {
		t.Fatal(err)
	}
	made := len(tools()) // The ffmpeg that made the film is not the scan's.
	var log syncBuffer
	scanner := &library.Scanner{Pool: e.pool, Log: slog.New(slog.NewTextHandler(&log, nil))}
	if err := scanner.Scan(context.Background(), libs[0]); err != nil {
		t.Fatalf("%v\n%s", err, &log)
	}
	// What a scan ran before this task, on this film: two ffprobe (the probe, the keyframe index) and one
	// ffmpeg (the poster frame).
	ran := tools()[made:]
	want := map[string]int{"ffprobe": 2, "ffmpeg": 1}
	got := map[string]int{}
	for _, line := range ran {
		got[strings.Fields(line)[0]]++
	}
	for tool, n := range want {
		if got[tool] != n {
			t.Errorf("a scan ran %s %d times, want %d (as before the task); runs: %v", tool, got[tool], n, ran)
		}
	}

	item := scalar[string](t, e.pool, "SELECT id::text FROM items WHERE library_id = $1 AND type = 'Movie'", libs[0].ID)
	if _, present := scalar[map[string]any](t, e.pool, "SELECT dto FROM items WHERE id = $1", item)["OriginalLanguage"]; present {
		t.Error("the stored item holds OriginalLanguage")
	}
	for _, query := range []string{
		"SELECT count(*) FROM items, jsonb_array_elements(dto->'MediaSources') m, jsonb_array_elements(m->'MediaStreams') s WHERE items.id = $1 AND s->>'IsOriginal' = 'true'",
		"SELECT count(*) FROM media_sources, jsonb_array_elements(streams) s WHERE item_id = $1 AND s->>'IsOriginal' = 'true'",
	} {
		if n := scalar[int](t, e.pool, query, item); n != 0 {
			t.Errorf("%d stored streams are IsOriginal true: %s", n, query)
		}
	}
	if n := scalar[int](t, e.pool, "SELECT count(*) FROM media_sources, jsonb_array_elements(streams) s WHERE item_id = $1 AND s->>'Type' = 'Audio'", item); n != 2 {
		t.Fatalf("the film's stored audio streams: %d, want 2", n)
	}
	if version := scalar[int](t, e.pool, "SELECT probe_version FROM media_sources WHERE item_id = $1", item); version != 1 || library.ProbeVersion != 1 {
		t.Errorf("probe_version = %d and library.ProbeVersion = %d, want 1 and 1: the task must not raise it", version, library.ProbeVersion)
	}

	// The film scanned by this release answers it with no rescan, and a second scan reads nothing.
	response, body := e.do(t, 0, "GET", "/Items/"+db.CompactID(item)+"?userId="+db.CompactID(e.user), e.token, nil)
	if response.StatusCode != 200 || !strings.Contains(string(body), `"OriginalLanguage":"eng"`) {
		t.Errorf("the scanned film's details: %d, want OriginalLanguage eng: %.300s", response.StatusCode, body)
	}
	before := len(tools())
	if err := scanner.Scan(context.Background(), libs[0]); err != nil {
		t.Fatal(err)
	}
	if after := len(tools()); after != before {
		t.Errorf("a second scan of an unchanged film ran %d more media tools", after-before)
	}
}
