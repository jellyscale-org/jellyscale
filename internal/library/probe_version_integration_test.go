//go:build integration

package library_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/library"
)

// JS-086: each media source stores the integer version of the probe that last
// wrote it, and the scan probes an unchanged file again for its age from that
// integer alone. These checks name media_sources.probe_version and
// library.ProbeVersion, which the task implies; a source older than the
// binary's has a lower number.

// tools puts ffprobe and ffmpeg in front of the real ones, noting each start
// with its arguments. While the file "fail" exists a call that names a film
// with "Doomed" in its path exits 1 with a reason; while "hold" exists one that
// names "Slow" waits. Real processes, so what it counts is what really ran.
type tools struct {
	dir string
}

func newTools(t *testing.T) tools {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf(`#!/bin/sh
echo "%s $*" >>%q
case "$*" in *Doomed*) if [ -e %q ]; then echo "disk gone" >&2; exit 1; fi;; esac
case "$*" in *Slow*) while [ -e %q ]; do sleep 0.1; done;; esac
exec %q "$@"
`, name, filepath.Join(dir, "runs"), filepath.Join(dir, "fail"), filepath.Join(dir, "hold"), real)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tools{dir}
}

func (x tools) set(t *testing.T, marker string, on bool) {
	t.Helper()
	path := filepath.Join(x.dir, marker)
	if on {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	os.Remove(path)
}

// runs lists the tools started so far.
func (x tools) runs() []string {
	raw, _ := os.ReadFile(filepath.Join(x.dir, "runs"))
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// opened counts the runs that name text, from run number since on.
func (x tools) opened(text string, since int) int {
	n := 0
	for _, run := range x.runs()[since:] {
		if strings.Contains(run, text) {
			n++
		}
	}
	return n
}

func (x tools) count() int { return len(x.runs()) }

func (f fixture) setVersion(t *testing.T, name string, version int) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_sources SET probe_version = $2 WHERE item_id = (SELECT id FROM items WHERE name = $1)`, name, version); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) version(t *testing.T, name string) int {
	t.Helper()
	return scalar[int](t, f.pool, `SELECT m.probe_version FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = $1`, name)
}

// row is the whole of what a scan stores for a film, to compare before and after.
func (f fixture) row(t *testing.T, name string) string {
	t.Helper()
	return scalar[string](t, f.pool, `SELECT row_to_json(m)::text || (SELECT dto::text FROM items WHERE id = m.item_id)
		FROM media_sources m WHERE m.item_id = (SELECT id FROM items WHERE name = $1)`, name)
}

func TestTheVersionOfTheProbeIsAPositiveInteger(t *testing.T) {
	if library.ProbeVersion < 1 {
		t.Fatalf("ProbeVersion is %d: a source stored before it must have a number below it", library.ProbeVersion)
	}
}

// A scan probes an unchanged file again when its source was written by an older
// probe, whatever its stored JSON holds, and not otherwise: of three films, two
// older and one current, exactly the two are probed and the current one is not
// opened. The probe keeps what is not its own, and happens once.
func TestAnUnchangedFileWhoseSourceIsOlderThanTheBinaryIsProbedAgainOnce(t *testing.T) {
	f := setup(t, "movies")
	x := newTools(t)
	for _, name := range []string{"Old A (2020)", "Old B (2021)", "Current C (2022)"} {
		clip(t, filepath.Join(f.media, name, name+".mkv"))
	}
	if err := os.WriteFile(filepath.Join(f.media, "Old A (2020)", "Old A (2020).en.srt"), []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	ctx := context.Background()
	for _, name := range []string{"Old A", "Old B", "Current C"} {
		if got := f.version(t, name); got != library.ProbeVersion {
			t.Fatalf("%s is stored at probe version %d, the binary's is %d", name, got, library.ProbeVersion)
		}
	}
	// What the scan keeps through a probe again: the item and its watch state, the index of the subtitle file
	// beside the film, a keyframe index that was read (a value a read would not write) and its video end.
	idA := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Old A'")
	user := scalar[string](t, f.pool, `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), 'u', 'x') RETURNING id::text`)
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played) VALUES ($1, $2, true)`, user, idA); err != nil {
		t.Fatal(err)
	}
	external := scalar[string](t, f.pool, `SELECT s::text FROM media_sources m, jsonb_array_elements(m.streams) s WHERE m.item_id = $1 AND s->>'IsExternal' = 'true'`, idA)
	// Stale in a way no predicate looks for: every key the old predicates ask for is there. Only the version says so.
	for _, name := range []string{"Old A", "Old B", "Current C"} {
		if _, err := f.pool.Exec(ctx, `UPDATE media_sources SET chapters = '[{"Name":"stale","StartPositionTicks":0}]', streams = jsonb_set(streams, '{0,Sentinel}', 'true')
			WHERE item_id = (SELECT id FROM items WHERE name = $1)`, name); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE items SET dto = jsonb_set(dto, '{MediaSources,0,MediaStreams,0,Sentinel}', 'true') WHERE name = $1`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, `UPDATE media_sources SET keyframes = ARRAY[12345]::bigint[], video_end = 777 WHERE item_id = $1`, idA); err != nil {
		t.Fatal(err)
	}
	f.setVersion(t, "Old A", library.ProbeVersion-1)
	f.setVersion(t, "Old B", 0)
	if _, err := f.pool.Exec(ctx, `UPDATE media_sources SET keyframes = NULL WHERE item_id = (SELECT id FROM items WHERE name = 'Old B')`); err != nil {
		t.Fatal(err)
	}
	currentRow := f.row(t, "Current C")

	before := x.count()
	f.scan(t)
	if n := x.opened("Current C", before); n != 0 {
		t.Errorf("the current film was opened %d times", n)
	}
	for _, name := range []string{"Old A", "Old B"} {
		if n := x.opened(name, before); n == 0 {
			t.Errorf("%s is older than the binary's probe and was not probed again", name)
		}
		if got := f.version(t, name); got != library.ProbeVersion {
			t.Errorf("%s is at probe version %d after its probe, want %d", name, got, library.ProbeVersion)
		}
		if scalar[bool](t, f.pool, `SELECT streams->0 ? 'Sentinel' OR chapters = '[{"Name":"stale","StartPositionTicks":0}]'::jsonb
			OR (SELECT dto::text LIKE '%Sentinel%' FROM items WHERE id = m.item_id) FROM media_sources m WHERE m.item_id = (SELECT id FROM items WHERE name = $1)`, name) {
			t.Errorf("%s: its media_sources row, or its item's stored sources, streams or chapters, were not rewritten", name)
		}
	}
	if got := f.row(t, "Current C"); got != currentRow {
		t.Errorf("the current film's rows changed:\n%s\n%s", currentRow, got)
	}
	if got := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Old A'"); got != idA {
		t.Errorf("Old A has another id after its probe: %s", got)
	}
	if !scalar[bool](t, f.pool, "SELECT played FROM user_item_data WHERE item_id = $1", idA) {
		t.Error("Old A lost its watch state")
	}
	if got := scalar[string](t, f.pool, `SELECT s::text FROM media_sources m, jsonb_array_elements(m.streams) s WHERE m.item_id = $1 AND s->>'IsExternal' = 'true'`, idA); got != external {
		t.Errorf("the subtitle file beside Old A changed through the probe:\n%s\n%s", external, got)
	}
	if got := scalar[string](t, f.pool, "SELECT keyframes::text || ' ' || video_end FROM media_sources WHERE item_id = $1", idA); got != "{12345} 777" {
		t.Errorf("Old A's stored keyframe index and video end were read again: %s", got)
	}
	if first := scalar[*int64](t, f.pool, "SELECT keyframes[1] FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Old B'"); first == nil || *first == 12345 {
		t.Errorf("Old B's keyframe index was not read (it was NULL): %v", first)
	}

	// Probed once: a further scan runs no tool at all.
	before = x.count()
	f.scan(t)
	if after := x.count(); after != before {
		t.Errorf("a further scan ran %d tools:\n%s", after-before, strings.Join(x.runs()[before:], "\n"))
	}
}

// A probe that fails, and a scan cut short, leave the source, its item and its
// old version as they were, the failure is logged with the key and the reason,
// the next scan tries again, and the films already probed stay done.
func TestAFailedOrCutShortProbeLeavesTheOlderSourceAsItWasAndIsTriedAgain(t *testing.T) {
	f := setup(t, "movies")
	var logged syncBuffer
	f.scanner.Log = slog.New(slog.NewTextHandler(&logged, nil))
	x := newTools(t)
	names := []string{"Fine One (2020)", "Doomed Two (2021)", "Slow Three (2022)"}
	for _, name := range names {
		clip(t, filepath.Join(f.media, name, name+".mkv"))
	}
	f.scan(t)
	// A subtitle file arrives beside the failing film: the scan must not list it into a source it can't probe.
	if err := os.WriteFile(filepath.Join(f.media, "Doomed Two (2021)", "Doomed Two (2021).fr.srt"), []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Fine One", "Doomed Two", "Slow Three"} {
		f.setVersion(t, name, 0)
	}
	rows := map[string]string{}
	for _, name := range []string{"Doomed Two", "Slow Three"} {
		rows[name] = f.row(t, name)
	}
	x.set(t, "fail", true)
	x.set(t, "hold", true)

	cut, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.scanner.Scan(cut, f.lib) //nolint:errcheck // Cut short on purpose.
	}()
	for began := time.Now(); f.version(t, "Fine One") != library.ProbeVersion; time.Sleep(50 * time.Millisecond) {
		if time.Since(began) > 30*time.Second {
			t.Fatal("Fine One was not stored while Slow Three was held")
		}
	}
	cancel()
	x.set(t, "hold", false)
	<-finished
	for name, want := range rows {
		if got := f.row(t, name); got != want {
			t.Errorf("%s changed though its probe failed or was cut short:\n%s\n%s", name, want, got)
		}
		if got := f.version(t, name); got != 0 {
			t.Errorf("%s is at probe version %d, want its old 0", name, got)
		}
	}
	if !strings.Contains(logged.String(), "Doomed Two") || !strings.Contains(logged.String(), "disk gone") {
		t.Errorf("the failure was not logged with the key and the reason:\n%s", logged.String())
	}

	// The next scan tries the two again, and leaves the film that is done.
	x.set(t, "fail", false)
	before := x.count()
	f.scan(t)
	if n := x.opened("Fine One", before); n != 0 {
		t.Errorf("Fine One was probed again: %d runs", n)
	}
	for _, name := range []string{"Doomed Two", "Slow Three"} {
		if got := f.version(t, name); got != library.ProbeVersion {
			t.Errorf("%s is at probe version %d after the next scan, want %d", name, got, library.ProbeVersion)
		}
	}
	if n := scalar[int](t, f.pool, `SELECT count(*) FROM media_sources m JOIN items i ON i.id = m.item_id, jsonb_array_elements(m.streams) s
		WHERE i.name = 'Doomed Two' AND s->>'IsExternal' = 'true'`); n != 1 {
		t.Errorf("Doomed Two lists %d subtitle files after its probe, want 1", n)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Only a probe raises a source's version: listing its subtitle files again, or
// a worker marking a track unreadable, rewrites its streams and leaves the
// version as it was, so an older source whose subtitle files also changed is
// still probed.
func TestOnlyAProbeRaisesTheVersion(t *testing.T) {
	f := setup(t, "movies")
	x := newTools(t)
	for _, name := range []string{"Newer (2020)", "Older (2021)"} {
		clip(t, filepath.Join(f.media, name, name+".mkv"))
		if err := os.WriteFile(filepath.Join(f.media, name, name+".en.srt"), []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(t)
	f.setVersion(t, "Older", library.ProbeVersion-1)
	if library.ProbeVersion == 1 {
		f.setVersion(t, "Older", 0)
	}
	older := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Older'")
	fingerprint, index := scalar[string](t, f.pool, `SELECT s->>'Fingerprint' FROM media_sources m, jsonb_array_elements(m.streams) s WHERE m.item_id = $1 AND s->>'IsExternal' = 'true'`, older),
		scalar[int](t, f.pool, `SELECT (s->>'Index')::int FROM media_sources m, jsonb_array_elements(m.streams) s WHERE m.item_id = $1 AND s->>'IsExternal' = 'true'`, older)

	// A worker marks the subtitle file unreadable: the streams change, the version does not.
	if err := library.MarkUnreadable(context.Background(), f.pool, older, index, fingerprint, ""); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, f.pool, `SELECT streams @> '[{"Unreadable": true}]' FROM media_sources WHERE item_id = $1`, older) {
		t.Fatal("the track was not marked unreadable")
	}
	if got := f.version(t, "Older"); got == library.ProbeVersion {
		t.Fatalf("marking a track unreadable raised the version to %d", got)
	}

	// Subtitle files change beside both films. The current source is listed again without a probe and keeps
	// its version; the older one is probed, and only then is at the binary's version.
	for _, name := range []string{"Newer (2020)", "Older (2021)"} {
		if err := os.WriteFile(filepath.Join(f.media, name, name+".fr.srt"), []byte("1\n00:00:00,000 --> 00:00:01,000\nbonjour\n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := x.count()
	f.scan(t)
	if n := x.opened("Newer", before); n != 0 {
		t.Errorf("listing the new subtitle file beside the current film ran %d tools on it", n)
	}
	if n := scalar[int](t, f.pool, `SELECT count(*) FROM media_sources m JOIN items i ON i.id = m.item_id, jsonb_array_elements(m.streams) s
		WHERE i.name = 'Newer' AND s->>'IsExternal' = 'true'`); n != 2 {
		t.Errorf("the current film lists %d subtitle files, want 2", n)
	}
	if got := f.version(t, "Newer"); got != library.ProbeVersion {
		t.Errorf("listing subtitle files changed the current film's version to %d", got)
	}
	if n := x.opened("Older", before); n == 0 {
		t.Error("the older source, whose subtitle files also changed, was not probed")
	}
	if got := f.version(t, "Older"); got != library.ProbeVersion {
		t.Errorf("the older source is at version %d after its probe", got)
	}
}

// What a failed read left unset is retried apart from the version: a keyframe
// index left NULL for a copied codec in mkv is read at the next scan, and the
// image cue points of a Matroska film are read from its head and Cues without a
// probe.
func TestWhatAFailedReadLeftUnsetIsReadAgainWithoutAProbe(t *testing.T) {
	f := setup(t, "movies")
	x := newTools(t)
	clip(t, filepath.Join(f.media, "Plain (2020)", "Plain (2020).mkv"))
	pgsClip(t, filepath.Join(f.media, "Disc (2021)", "Disc (2021).mkv"))
	f.scan(t)
	if scalar[bool](t, f.pool, `SELECT image_cues IS NULL FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Disc'`) {
		t.Fatal("setup: a scan left the image cues unread")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE media_sources SET image_cues = NULL WHERE item_id = (SELECT id FROM items WHERE name = 'Disc');
		UPDATE media_sources SET keyframes = NULL WHERE item_id = (SELECT id FROM items WHERE name = 'Plain');
		UPDATE media_sources SET streams = jsonb_set(streams, '{0,Sentinel}', 'true')`); err != nil {
		t.Fatal(err)
	}
	before := x.count()
	f.scan(t)
	if scalar[bool](t, f.pool, `SELECT image_cues IS NULL FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Disc'`) {
		t.Error("the image cue points were not read")
	}
	if n := x.opened("Disc", before); n != 0 {
		t.Errorf("reading the cue points ran %d tools on the film; they come from its head and Cues", n)
	}
	if scalar[bool](t, f.pool, `SELECT keyframes IS NULL FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Plain'`) {
		t.Error("the keyframe index left NULL was not read")
	}
	if !scalar[bool](t, f.pool, `SELECT streams->0 ? 'Sentinel' FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.name = 'Disc'`) {
		t.Error("the film whose cue points were read was probed again though its version is current")
	}
	for _, name := range []string{"Plain", "Disc"} {
		if got := f.version(t, name); got != library.ProbeVersion {
			t.Errorf("%s is at version %d", name, got)
		}
	}
}

// Every source stored before this change is older, and so is a row a worker of
// the previous release inserts during a rolling upgrade without naming the
// column.
func TestSourcesStoredBeforeTheMigrationAndByThePreviousReleaseAreOlder(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020)", "Film (2020).mkv"))
	f.scan(t)
	ctx := context.Background()
	// A row as the previous release's scan writes it: no probe_version in the column list.
	id := scalar[string](t, f.pool, "SELECT id::text FROM items")
	if _, err := f.pool.Exec(ctx, `WITH old AS (DELETE FROM media_sources RETURNING *)
		INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end)
		SELECT id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end FROM old`); err != nil {
		t.Fatal(err)
	}
	if got := scalar[int](t, f.pool, "SELECT probe_version FROM media_sources WHERE item_id = $1", id); got >= library.ProbeVersion {
		t.Errorf("a row inserted without naming probe_version is at version %d, the binary's is %d", got, library.ProbeVersion)
	}
	// The migration run on a table that has rows: they get a version below the binary's.
	entries, err := os.ReadDir("../db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	var migration string
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join("../db/migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "probe_version") {
			migration = string(raw)
		}
	}
	if migration == "" {
		t.Fatal("no migration in internal/db/migrations adds media_sources.probe_version")
	}
	if _, err := f.pool.Exec(ctx, "ALTER TABLE media_sources DROP COLUMN probe_version"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, migration); err != nil {
		t.Fatalf("the migration on a table that has rows: %v", err)
	}
	if got := scalar[int](t, f.pool, "SELECT probe_version FROM media_sources WHERE item_id = $1", id); got >= library.ProbeVersion {
		t.Errorf("a source stored before the migration is at version %d, the binary's is %d", got, library.ProbeVersion)
	}
}

// A film copied or uploaded again to a new key, with another ETag, whose item's
// source the previous release stored, is still matched to its item: the content
// fingerprint of a new probe is the one of the row the previous release stored,
// which wrote a key only where the stream has the thing (no Width on an audio
// stream, no Channels on a video).
func TestAFilmCopiedAgainFindsItsItemWhoseSourceThePreviousReleaseStored(t *testing.T) {
	f := setup(t, "movies")
	ctx := context.Background()
	old := filepath.Join(f.media, "Rich (2020)", "Rich (2020).mkv")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-f", "lavfi", "-i", "sine=frequency=660:duration=2",
		"-map", "0", "-map", "1", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "flac", old).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	f.scan(t)
	id := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE name = 'Rich'")
	user := scalar[string](t, f.pool, `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), 'u', 'x') RETURNING id::text`)
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_item_data (user_id, item_id, played) VALUES ($1, $2, true)`, user, id); err != nil {
		t.Fatal(err)
	}
	// The row as the previous release stored it: only the keys that applied to each kind of stream.
	var streams []map[string]any
	if err := json.Unmarshal([]byte(scalar[string](t, f.pool, "SELECT streams::text FROM media_sources WHERE item_id = $1", id)), &streams); err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		switch stream["Type"] {
		case "Video":
			delete(stream, "Channels")
			delete(stream, "SampleRate")
		case "Audio":
			delete(stream, "Width")
			delete(stream, "Height")
		}
	}
	raw, _ := json.Marshal(streams)
	if _, err := f.pool.Exec(ctx, "UPDATE media_sources SET probe_version = 0, streams = $2 WHERE item_id = $1", id, raw); err != nil {
		t.Fatal(err)
	}

	// The same bytes under a new key, with another mtime and so another ETag.
	moved := filepath.Join(f.media, "Rich Again (2020)", "Rich Again (2020).mkv")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(moved, data, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(moved, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if got := scalar[string](t, f.pool, "SELECT id::text FROM items WHERE type = 'Movie' AND natural_key = 'video:Rich Again (2020)/Rich Again (2020).mkv'"); got != id {
		t.Errorf("the film copied again has the id %s, want its item's %s", got, id)
	}
	if !scalar[bool](t, f.pool, "SELECT played FROM user_item_data WHERE item_id = $1", id) {
		t.Error("the film lost its watch state")
	}
	if n := f.count(t, "Movie"); n != 1 {
		t.Errorf("%d movies, want the one", n)
	}
}
