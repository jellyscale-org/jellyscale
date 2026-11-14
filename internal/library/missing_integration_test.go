//go:build integration

package library_test

// JS-076: a file a scan no longer lists is hidden, not deleted; it comes back
// as the same item inside the grace; a scan that would lose too much of a
// library fails. The names these tests rely on that the task implies and the
// code lacks: Scanner.Grace and Scanner.MaxLostShare.
//
// Every test seeds watch state for two users (position, played, favourite, a
// favourite on each series) and compares the rows, by count and content, after.
// Where a test moves "time" on, it sets Scanner.Grace to a nanosecond after a
// short sleep: the grace is then over for certain, whatever the machine. A
// test of the opposite (a grace that has not passed) uses an hour, and
// TestGoneAgainStartsANewGrace, which has to tell two moments apart, uses 2
// seconds against scans that take milliseconds, a bound far above what the
// scan needs.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// scanAfterGrace runs a scan once the grace of every hidden item is over.
func (f fixture) scanAfterGrace(t *testing.T) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	was := f.scanner.Grace
	f.scanner.Grace = time.Nanosecond
	defer func() { f.scanner.Grace = was }()
	f.scan(t)
}

// users makes two viewers.
func (f fixture) users(t *testing.T) [2]string {
	t.Helper()
	var users [2]string
	for i := range users {
		users[i] = scalar[string](t, f.pool, `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), $1, 'x') RETURNING id::text`, fmt.Sprint("viewer", i))
	}
	return users
}

// seed gives both users a row on every item, different in every column that a
// scan could damage: the first has a position and plays and favourites only
// series, the second has played everything and favourites everything.
func (f fixture) seed(t *testing.T, users [2]string) {
	t.Helper()
	for i, sql := range []string{
		`INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks, last_played_at)
			SELECT $1, id, false, type = 'Series', 2, 1000 + length(name), now() - interval '2 hours' FROM items ON CONFLICT DO NOTHING`,
		`INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks, last_played_at)
			SELECT $1, id, true, true, 3, 0, now() - interval '1 day' FROM items ON CONFLICT DO NOTHING`,
	} {
		if _, err := f.pool.Exec(context.Background(), sql, users[i]); err != nil {
			t.Fatal(err)
		}
	}
}

// rows is every user_item_data row as text, in a fixed order. With ids, only
// those items'.
func (f fixture) rows(t *testing.T, ids ...string) []string {
	t.Helper()
	var filter any
	if len(ids) > 0 {
		filter = ids
	}
	return textRows(t, f.pool, `SELECT user_id::text || '|' || item_id::text || '|' || played || '|' || favorite || '|' || play_count || '|' ||
			position_ticks || '|' || COALESCE(last_played_at::text, '') || '|' || COALESCE(progress_at::text, '') || '|' || COALESCE(data_at::text, '')
		FROM user_item_data WHERE $1::uuid[] IS NULL OR item_id = ANY($1::uuid[]) ORDER BY 1`, filter)
}

// itemRows and sourceRows are what a hidden item must keep exactly.
func (f fixture) itemRows(t *testing.T, ids ...string) []string {
	t.Helper()
	var filter any
	if len(ids) > 0 {
		filter = ids
	}
	return textRows(t, f.pool, `SELECT id::text || '|' || library_id::text || '|' || COALESCE(parent_id::text, '') || '|' || COALESCE(series_id::text, '') || '|' ||
			COALESCE(season_id::text, '') || '|' || type || '|' || name || '|' || natural_key || '|' || date_created::text || '|' || dto::text
		FROM items WHERE $1::uuid[] IS NULL OR id = ANY($1::uuid[]) ORDER BY 1`, filter)
}

func (f fixture) sourceRows(t *testing.T, ids ...string) []string {
	t.Helper()
	var filter any
	if len(ids) > 0 {
		filter = ids
	}
	return textRows(t, f.pool, `SELECT id::text || '|' || item_id::text || '|' || store_key || '|' || size || '|' || object_etag || '|' || container || '|' || streams::text
		FROM media_sources WHERE $1::uuid[] IS NULL OR item_id = ANY($1::uuid[]) ORDER BY 1`, filter)
}

func textRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s changed: %d rows, was %d\n got: %q\nwant: %q", what, len(got), len(want), got, want)
	}
}

func (f fixture) repo() *items.Repo { return &items.Repo{Pool: f.pool, ServerID: "srv"} }

// visible is what a viewer asking for the item gets: whether it exists.
func (f fixture) visible(t *testing.T, id string) bool {
	t.Helper()
	dto, err := f.repo().Get(context.Background(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return dto != nil
}

// total is the count of every item a viewer can list in the library.
func (f fixture) total(t *testing.T) int {
	t.Helper()
	result, err := f.repo().Find(context.Background(), items.Query{Recursive: true}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalRecordCount != len(result.Items) {
		t.Fatalf("TotalRecordCount %d but %d items", result.TotalRecordCount, len(result.Items))
	}
	return result.TotalRecordCount
}

// names lists the names a query returns, sorted.
func (f fixture) names(t *testing.T, q items.Query) []string {
	t.Helper()
	result, err := f.repo().Find(context.Background(), q, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, dto := range result.Items {
		names = append(names, dto["Name"].(string))
	}
	slices.Sort(names)
	return names
}

// idOf is the id of the item of the video at key.
func (f fixture) idOf(t *testing.T, key string) string {
	t.Helper()
	return scalar[string](t, f.pool, "SELECT id::text FROM items WHERE natural_key = $1", "video:"+key)
}

func (f fixture) hasItem(t *testing.T, id string) bool {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT count(*) FROM items WHERE id = $1", id) == 1
}

func (f fixture) itemCount(t *testing.T) int {
	t.Helper()
	return scalar[int](t, f.pool, "SELECT count(*) FROM items")
}

// film makes a film that no other call of film with another length can equal.
func film(t *testing.T, file string, seconds int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf("testsrc2=size=160x90:rate=10:duration=%d", seconds), "-c:v", "libx264", "-preset", "ultrafast", file).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
}

// films makes n films at the root of dir, each of its own length and so size.
func films(t *testing.T, dir, prefix string, n int) []string {
	t.Helper()
	var keys []string
	sizes := map[int64]bool{}
	for i := range n {
		key := fmt.Sprintf("%s %c (2020).mkv", prefix, 'A'+i)
		film(t, filepath.Join(dir, key), i+1)
		info, err := os.Stat(filepath.Join(dir, key))
		if err != nil {
			t.Fatal(err)
		}
		if sizes[info.Size()] {
			t.Fatalf("two of the films are %d bytes; the checks need them apart", info.Size())
		}
		sizes[info.Size()] = true
		keys = append(keys, key)
	}
	return keys
}

// sameSize writes two films of the same size and different content: the
// second is longer, and the first carries a title long enough to make up the
// difference (it is remuxed, which does not change what it measures).
func sameSize(t *testing.T, dir string) (shorter, longer string) {
	t.Helper()
	raw := filepath.Join(dir, "raw.mkv")
	shorter, longer = filepath.Join(dir, "Same Size A (2020).mkv"), filepath.Join(dir, "Same Size B (2021).mkv")
	film(t, raw, 2)
	film(t, longer, 3)
	size := func(file string) int {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		return int(info.Size())
	}
	title := "a"
	for range 8 {
		if out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-i", raw, "-c", "copy", "-metadata", "title="+title, shorter).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
		delta := size(longer) - size(shorter)
		if delta == 0 {
			if err := os.Remove(raw); err != nil {
				t.Fatal(err)
			}
			return shorter, longer
		}
		if len(title)+delta < 1 {
			t.Fatalf("the shorter film is already %d bytes longer", -delta)
		}
		if delta > 0 {
			title += strings.Repeat("a", delta)
		} else {
			title = title[:len(title)+delta]
		}
	}
	t.Fatal("could not make two films of one size")
	return
}

func copyFile(t *testing.T, from, to string, mtime time.Time) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(to, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func mtimeOf(t *testing.T, file string) time.Time {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// move renames a file, which keeps its modification time and so its ETag.
func move(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// probeSpy stands in for ffprobe on PATH: it records each run, and fails
// them all while told to. It counts the probes of a film (the run that lists
// its streams), which is how the checks see "probed again".
type probeSpy struct{ log, flag string }

func spyOnProbes(t *testing.T) *probeSpy {
	t.Helper()
	real, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	spy := &probeSpy{log: filepath.Join(dir, "log"), flag: filepath.Join(dir, "fail")}
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nif [ -e %s ]; then echo 'forced failure' >&2; exit 1; fi\nexec %s \"$@\"\n", spy.log, spy.flag, real)
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return spy
}

func (s *probeSpy) failing(t *testing.T, on bool) {
	t.Helper()
	if on {
		if err := os.WriteFile(s.flag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Remove(s.flag); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// probes counts the runs that probed the file called name.
func (s *probeSpy) probes(t *testing.T, name string) int {
	t.Helper()
	data, err := os.ReadFile(s.log)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "-show_streams") && strings.Contains(line, name) {
			n++
		}
	}
	return n
}

// two films with state for two viewers: the fixture most checks start from.
type pair struct {
	f          fixture
	users      [2]string
	a, b       string // Keys.
	idA, idB   string
	all        []string // Every row of user_item_data.
	itemsA     []string
	sourcesA   []string
	rowsOfA    []string
	stashA     string // Where A waits while it is missing.
	pathA      string
	mtimeA     time.Time
	totalFirst int
}

func newPair(t *testing.T) *pair {
	t.Helper()
	p := &pair{f: setup(t, "movies")}
	keys := films(t, p.f.media, "Film", 2)
	p.a, p.b = keys[0], keys[1]
	p.pathA = filepath.Join(p.f.media, p.a)
	p.mtimeA = mtimeOf(t, p.pathA)
	p.f.scan(t)
	p.users = p.f.users(t)
	p.f.seed(t, p.users)
	p.idA, p.idB = p.f.idOf(t, p.a), p.f.idOf(t, p.b)
	p.all = p.f.rows(t)
	p.itemsA = p.f.itemRows(t, p.idA)
	p.sourcesA = p.f.sourceRows(t, p.idA)
	p.rowsOfA = p.f.rows(t, p.idA)
	if len(p.all) != 4 || len(p.rowsOfA) != 2 {
		t.Fatalf("seeded %d rows, %d of the first film", len(p.all), len(p.rowsOfA))
	}
	p.stashA = filepath.Join(t.TempDir(), "stash.mkv")
	p.totalFirst = p.f.total(t)
	return p
}

// lose takes A out of the library and scans.
func (p *pair) lose(t *testing.T) {
	t.Helper()
	move(t, p.pathA, p.stashA)
	p.f.scan(t)
}

// intact: A is hidden, and nothing of it or its rows has changed.
func (p *pair) intact(t *testing.T) {
	t.Helper()
	sameLines(t, "the hidden item", p.f.itemRows(t, p.idA), p.itemsA)
	sameLines(t, "the hidden item's media source", p.f.sourceRows(t, p.idA), p.sourcesA)
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
}

func TestMissingFileIsHiddenNotDeleted(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	p.intact(t)
	if p.f.visible(t, p.idA) {
		t.Fatal("a viewer still gets the missing film")
	}
	if !p.f.visible(t, p.idB) || p.f.total(t) != 1 {
		t.Fatalf("the film that is there is gone, or the count is %d", p.f.total(t))
	}
	if n := p.f.count(t, "Movie"); n != 2 {
		t.Fatalf("%d movies in the database, want both", n)
	}
}

// A file listed again at its old key, unchanged, is the same item, and the
// scan does not read it again.
func TestFileListedAgainAtItsKeyIsTheSameItem(t *testing.T) {
	p := newPair(t)
	spy := spyOnProbes(t)
	p.lose(t)
	probed := spy.probes(t, p.a)
	move(t, p.stashA, p.pathA)
	p.f.scan(t)
	if !p.f.visible(t, p.idA) || p.f.total(t) != p.totalFirst {
		t.Fatal("the film did not come back")
	}
	p.intact(t)
	if n := spy.probes(t, p.a); n != probed {
		t.Fatalf("the returned file was probed %d times", n-probed)
	}
}

// Replaced by new content while it was missing, it is still the same item, with
// the new content.
func TestFileReplacedWhileMissingKeepsItsItem(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	film(t, p.pathA, 5)
	p.f.scan(t)
	if !p.f.visible(t, p.idA) {
		t.Fatal("the film did not come back")
	}
	if got := p.f.idOf(t, p.a); got != p.idA {
		t.Fatalf("a new item %s", got)
	}
	if n := p.f.itemCount(t); n != 2 {
		t.Fatalf("%d items", n)
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
	info, _ := os.Stat(p.pathA)
	if got := scalar[int64](t, p.f.pool, "SELECT size FROM media_sources WHERE item_id = $1", p.idA); got != info.Size() {
		t.Fatalf("the media source still describes the old file: %d bytes, the file is %d", got, info.Size())
	}
	if got := scalar[string](t, p.f.pool, "SELECT id::text FROM media_sources WHERE item_id = $1", p.idA); got != p.idA {
		t.Fatalf("the media source id changed to %s", got)
	}
	was := scalar[string](t, p.f.pool, "SELECT date_created::text FROM items WHERE id = $1", p.idA)
	if !strings.Contains(strings.Join(p.itemsA, "\n"), was) {
		t.Fatalf("the date added changed to %s", was)
	}
}

// No scan removes a hidden item before the grace has passed, whoever scans.
func TestNoScanRemovesAHiddenItemBeforeTheGracePasses(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	for range 3 {
		p.f.scan(t)
	}
	// Restarted workers: scanners with a pool of their own, scanning at once.
	var wg sync.WaitGroup
	for range 6 {
		pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		scanner := &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler), Grace: time.Hour, MaxLostShare: 1}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				if err := scanner.Scan(context.Background(), p.f.lib); err != nil && !errors.Is(err, library.ErrElsewhere) {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	p.intact(t)
	if p.f.visible(t, p.idA) {
		t.Fatal("the hidden film is shown again")
	}
}

// The grace is in the database: a scanner that never saw the file go (another
// worker, a restarted one) removes the item after it, with its watch state, and
// only that item's.
func TestFirstScanAfterTheGraceRemovesTheItemAndItsWatchState(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	p.intact(t) // The scan that missed the file only hid it.
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	time.Sleep(20 * time.Millisecond)
	scanner := &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler), Grace: time.Nanosecond, MaxLostShare: 1}
	if err := scanner.Scan(context.Background(), p.f.lib); err != nil {
		t.Fatal(err)
	}
	if p.f.hasItem(t, p.idA) {
		t.Fatal("the item is still there")
	}
	if n := scalar[int](t, p.f.pool, "SELECT count(*) FROM media_sources WHERE item_id = $1", p.idA); n != 0 {
		t.Fatalf("%d media sources left", n)
	}
	sameLines(t, "the other film's rows", p.f.rows(t), p.f.rows(t, p.idB))
	if got := p.f.rows(t, p.idB); len(got) != 2 {
		t.Fatalf("the other film has %d rows", len(got))
	}
	// A file that returns after that is a new item.
	move(t, p.stashA, p.pathA)
	p.f.scan(t)
	if got := p.f.idOf(t, p.a); got == p.idA {
		t.Fatal("the removed item came back")
	}
	if n := len(p.f.rows(t, p.f.idOf(t, p.a))); n != 0 {
		t.Fatalf("the new item has %d rows of watch state", n)
	}
}

// A file that returns and goes missing again starts a new grace.
func TestGoneAgainStartsANewGrace(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	time.Sleep(2200 * time.Millisecond)
	move(t, p.stashA, p.pathA)
	p.f.scan(t)
	p.lose(t)
	was := p.f.scanner.Grace
	p.f.scanner.Grace = 2 * time.Second // The first miss is over 2 seconds old; this one is a few milliseconds.
	defer func() { p.f.scanner.Grace = was }()
	p.f.scan(t)
	p.intact(t)
}

// The scans that remove an item remove the seasons and series it leaves with no
// episode, with their own watch state, hidden or not; others stay.
func TestRemovalAfterTheGraceLeavesNoEmptySeasonOrSeries(t *testing.T) {
	f := setup(t, "tvshows")
	one1 := filepath.Join(f.media, "Show One", "Season 01", "Show One S01E01.mkv")
	one2 := filepath.Join(f.media, "Show One", "Season 02", "Show One S02E01.mkv")
	other := filepath.Join(f.media, "Other", "Season 01", "Other S01E01.mkv")
	film(t, one1, 1)
	film(t, one2, 2)
	film(t, other, 3)
	f.scan(t)
	f.seed(t, f.users(t))
	series := f.idOfNatural(t, "series:Show One")
	ep1, ep2, epOther := f.idOf(t, "Show One/Season 01/Show One S01E01.mkv"), f.idOf(t, "Show One/Season 02/Show One S02E01.mkv"), f.idOf(t, "Other/Season 01/Other S01E01.mkv")
	season2 := scalar[string](t, f.pool, "SELECT parent_id::text FROM items WHERE id = $1", ep2)
	season1 := scalar[string](t, f.pool, "SELECT parent_id::text FROM items WHERE id = $1", ep1)
	if err := os.Remove(one2); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	for _, id := range []string{ep2, season2} {
		if !f.hasItem(t, id) || len(f.rows(t, id)) != 2 {
			t.Fatalf("%s was removed by the scan that missed its file, or its rows", id)
		}
	}
	f.scanAfterGrace(t)
	for _, id := range []string{ep2, season2} {
		if f.hasItem(t, id) || len(f.rows(t, id)) != 0 {
			t.Fatalf("%s is still there, or its rows", id)
		}
	}
	for _, id := range []string{series, season1, ep1, epOther} {
		if !f.hasItem(t, id) || len(f.rows(t, id)) != 2 {
			t.Fatalf("%s is gone, or its rows", id)
		}
	}

	// A season and a series with no episode at all, listed or hidden, go too.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO items (id, library_id, parent_id, series_id, type, name, sort_name, natural_key, index_number, dto)
		SELECT gen_random_uuid(), library_id, id, id, 'Season', 'Season 9', 'season 9', 'season:empty', 9, '{"Name":"Season 9"}' FROM items WHERE id = $1`, series); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(one1); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	for _, id := range []string{series, season1, ep1} {
		if !f.hasItem(t, id) || len(f.rows(t, id)) != 2 {
			t.Fatalf("%s was removed by the scan that missed its file, or its rows", id)
		}
	}
	f.scanAfterGrace(t)
	for _, id := range []string{series, season1, ep1} {
		if f.hasItem(t, id) || len(f.rows(t, id)) != 0 {
			t.Fatalf("%s is still there, or its rows", id)
		}
	}
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM items WHERE natural_key = 'season:empty'"); n != 0 {
		t.Fatal("a season with no episode is left")
	}
	if !f.hasItem(t, epOther) || len(f.rows(t, epOther)) != 2 {
		t.Fatal("the other show's episode lost its rows")
	}
}

func (f fixture) idOfNatural(t *testing.T, natural string) string {
	t.Helper()
	return scalar[string](t, f.pool, "SELECT id::text FROM items WHERE natural_key = $1", natural)
}

func TestSeriesAndSeasonAreHiddenWithTheirLastEpisode(t *testing.T) {
	f := setup(t, "tvshows")
	one1 := filepath.Join(f.media, "Show One", "Season 01", "Show One S01E01.mkv")
	one2 := filepath.Join(f.media, "Show One", "Season 02", "Show One S02E01.mkv")
	film(t, one1, 1)
	film(t, one2, 2)
	film(t, filepath.Join(f.media, "Other", "Season 01", "Other S01E01.mkv"), 3)
	f.scan(t)
	f.seed(t, f.users(t))
	series := f.idOfNatural(t, "series:Show One")
	before, itemsBefore := f.rows(t), f.itemRows(t)
	if got := scalar[int](t, f.pool, "SELECT count(*) FROM user_item_data WHERE item_id = $1 AND favorite", series); got != 2 {
		t.Fatalf("%d favourites on the series, want 2", got)
	}
	for _, file := range []string{one1, one2} {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(t)
	sameLines(t, "user_item_data (a favourite on the series included)", f.rows(t), before)
	sameLines(t, "the items", f.itemRows(t), itemsBefore)
	if f.visible(t, series) {
		t.Fatal("a viewer still gets the series")
	}
	if got := f.total(t); got != 3 {
		t.Fatalf("a viewer counts %d items, want the other show's 3", got)
	}
	if got := f.names(t, items.Query{Recursive: true, IncludeTypes: []string{"Series"}}); !slices.Equal(got, []string{"Other"}) {
		t.Fatalf("a viewer lists the series %q, want Other", got)
	}
}

func TestSeriesWithAListedEpisodeStaysAndShowsOnlyTheEpisodesThatAreThere(t *testing.T) {
	f := setup(t, "tvshows")
	e1 := filepath.Join(f.media, "Show One", "Season 01", "Show One S01E01.mkv")
	e2 := filepath.Join(f.media, "Show One", "Season 01", "Show One S01E02.mkv")
	e3 := filepath.Join(f.media, "Show One", "Season 02", "Show One S02E01.mkv")
	film(t, e1, 1)
	film(t, e2, 2)
	film(t, e3, 3)
	film(t, filepath.Join(f.media, "Other", "Season 01", "Other S01E01.mkv"), 4)
	f.scan(t)
	f.seed(t, f.users(t))
	series := f.idOfNatural(t, "series:Show One")
	season1 := scalar[string](t, f.pool, "SELECT season_id::text FROM items WHERE natural_key = 'video:Show One/Season 01/Show One S01E01.mkv'")
	season2 := scalar[string](t, f.pool, "SELECT season_id::text FROM items WHERE natural_key = 'video:Show One/Season 02/Show One S02E01.mkv'")
	before := f.rows(t)
	for _, file := range []string{e2, e3} {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	f.scan(t)
	sameLines(t, "user_item_data", f.rows(t), before)
	if !f.visible(t, series) || !f.visible(t, season1) {
		t.Fatal("the series or the season that has an episode listed is hidden")
	}
	if f.visible(t, season2) {
		t.Fatal("the season whose only episode is missing is shown")
	}
	if got := f.names(t, items.Query{ParentID: series, Recursive: true, IncludeTypes: []string{"Episode"}}); len(got) != 1 {
		t.Fatalf("the series lists %d episodes, want 1: %q", len(got), got)
	}
	if got := f.names(t, items.Query{ParentID: series, IncludeTypes: []string{"Season"}}); !slices.Equal(got, []string{"Season 1"}) {
		t.Fatalf("the series lists seasons %q, want Season 1 only", got)
	}
	if got := f.names(t, items.Query{ParentID: season1}); len(got) != 1 {
		t.Fatalf("the season lists %q, want the one episode there", got)
	}
}

// A file under another key of the library, with the size and ETag of a hidden
// item, in a later scan inside the grace, is that item: named by its new key.
func TestMovedFileTakesTheHiddenItemInALaterScan(t *testing.T) {
	f := setup(t, "movies")
	old := filepath.Join(f.media, "Old Name (2020)", "Old Name (2020).mkv")
	film(t, old, 1)
	film(t, filepath.Join(f.media, "Stays (2019)", "Stays (2019).mkv"), 2)
	f.scan(t)
	f.seed(t, f.users(t))
	id := f.idOf(t, "Old Name (2020)/Old Name (2020).mkv")
	all := f.rows(t)
	stash := filepath.Join(t.TempDir(), "stash.mkv")
	move(t, old, stash)
	f.scan(t)
	f.scan(t)
	if f.visible(t, id) {
		t.Fatal("hidden item shown")
	}
	move(t, stash, filepath.Join(f.media, "New Name (2021)", "New Name (2021).mkv"))
	f.scan(t)
	if n := f.count(t, "Movie"); n != 2 {
		t.Fatalf("%d movies, want 2", n)
	}
	var name, key string
	var year int
	if err := f.pool.QueryRow(context.Background(), `SELECT i.name, i.production_year, m.store_key FROM items i JOIN media_sources m ON m.item_id = i.id WHERE i.id = $1`, id).Scan(&name, &year, &key); err != nil {
		t.Fatalf("the moved file did not take the item: %v", err)
	}
	if name != "New Name" || year != 2021 || key != "New Name (2021)/New Name (2021).mkv" {
		t.Fatalf("got %q %d %q", name, year, key)
	}
	if !f.visible(t, id) {
		t.Fatal("the item is still hidden")
	}
	sameLines(t, "user_item_data", f.rows(t), all)
	if n := scalar[int](t, f.pool, "SELECT count(*) FROM scan_objects"); n != 2 {
		t.Fatalf("%d scan objects, want 2", n)
	}
}

func TestMovedEpisodeIsPlacedByItsNewKey(t *testing.T) {
	f := setup(t, "tvshows")
	old := filepath.Join(f.media, "Show One", "Season 01", "Show One S01E02.mkv")
	film(t, filepath.Join(f.media, "Show One", "Season 01", "Show One S01E01.mkv"), 1)
	film(t, old, 2)
	film(t, filepath.Join(f.media, "Other", "Season 01", "Other S01E01.mkv"), 3)
	f.scan(t)
	f.seed(t, f.users(t))
	id := f.idOf(t, "Show One/Season 01/Show One S01E02.mkv")
	all := f.rows(t)
	stash := filepath.Join(t.TempDir(), "stash.mkv")
	move(t, old, stash)
	f.scan(t)
	move(t, stash, filepath.Join(f.media, "Other", "Season 02", "Other S02E05.mkv"))
	f.scan(t)
	var index, parentIndex int
	var seriesID, seasonID, parentID string
	err := f.pool.QueryRow(context.Background(), `SELECT series_id::text, season_id::text, parent_id::text, index_number, parent_index_number FROM items WHERE id = $1`, id).
		Scan(&seriesID, &seasonID, &parentID, &index, &parentIndex)
	if err != nil {
		t.Fatalf("the moved file did not take the episode: %v", err)
	}
	other := f.idOfNatural(t, "series:Other")
	if seriesID != other || seasonID != parentID || index != 5 || parentIndex != 2 {
		t.Fatalf("placed under series %s season %s (%d, %d); want series %s, season 2, episode 5", seriesID, seasonID, parentIndex, index, other)
	}
	if got := scalar[int](t, f.pool, "SELECT index_number FROM items WHERE id = $1", seasonID); got != 2 {
		t.Fatalf("under season %d", got)
	}
	if !f.visible(t, id) {
		t.Fatal("the episode is hidden")
	}
	sameLines(t, "user_item_data", f.rows(t), all)
}

// A move seen within one scan keeps the item (DOMAINS invariant 5), for two viewers.
func TestMoveWithinOneScanKeepsTheItemForTwoViewers(t *testing.T) {
	f := setup(t, "movies")
	old := filepath.Join(f.media, "Old Name (2020)", "Old Name (2020).mkv")
	film(t, old, 1)
	film(t, filepath.Join(f.media, "Stays (2019)", "Stays (2019).mkv"), 2)
	f.scan(t)
	f.seed(t, f.users(t))
	id := f.idOf(t, "Old Name (2020)/Old Name (2020).mkv")
	all := f.rows(t)
	move(t, old, filepath.Join(f.media, "New Name (2021)", "New Name (2021).mkv"))
	f.scan(t)
	if got := f.idOf(t, "New Name (2021)/New Name (2021).mkv"); got != id {
		t.Fatalf("the moved file has a new item %s", got)
	}
	sameLines(t, "user_item_data", f.rows(t), all)
	if f.itemCount(t) != 2 {
		t.Fatalf("%d items", f.itemCount(t))
	}
}

// The same bytes with another modification time (so another ETag) are the same file.
func TestSameBytesWithAnotherETagTakeTheHiddenItem(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	copyFile(t, p.stashA, filepath.Join(p.f.media, "Copied (2021)", "Copied (2021).mkv"), p.mtimeA.Add(time.Hour))
	p.f.scan(t)
	if got := p.f.idOf(t, "Copied (2021)/Copied (2021).mkv"); got != p.idA {
		t.Fatalf("the copy has a new item %s", got)
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
	if p.f.itemCount(t) != 2 || !p.f.visible(t, p.idA) {
		t.Fatal("a second item, or the item is hidden")
	}
}

// Nothing is matched on size alone.
func TestSameSizeOtherContentIsANewItem(t *testing.T) {
	f := setup(t, "movies")
	shorter, longer := sameSize(t, t.TempDir())
	film(t, filepath.Join(f.media, "Stays (2019).mkv"), 5)
	copyFile(t, shorter, filepath.Join(f.media, "Same Size A (2020).mkv"), time.Now().Add(-time.Hour))
	f.scan(t)
	f.seed(t, f.users(t))
	id := f.idOf(t, "Same Size A (2020).mkv")
	all := f.rows(t)
	if err := os.Remove(filepath.Join(f.media, "Same Size A (2020).mkv")); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	copyFile(t, longer, filepath.Join(f.media, "Same Size B (2021).mkv"), time.Now())
	f.scan(t)
	got := f.idOf(t, "Same Size B (2021).mkv")
	if got == id {
		t.Fatal("a file of the same size took the item")
	}
	if n := len(f.rows(t, got)); n != 0 {
		t.Fatalf("the new item has %d rows of watch state", n)
	}
	sameLines(t, "user_item_data", f.rows(t), all)
	if f.visible(t, id) {
		t.Fatal("the hidden item is shown")
	}
}

// Two identical files for one hidden item give one old item and one new one.
func TestOneHiddenItemGoesToOneFile(t *testing.T) {
	for name, shift := range map[string]time.Duration{"same ETag": 0, "other ETag": time.Hour} {
		t.Run(name, func(t *testing.T) {
			p := newPair(t)
			p.lose(t)
			one, two := "Twin One (2021).mkv", "Twin Two (2022).mkv"
			copyFile(t, p.stashA, filepath.Join(p.f.media, one), p.mtimeA)
			copyFile(t, p.stashA, filepath.Join(p.f.media, two), p.mtimeA.Add(shift))
			p.f.scan(t)
			ids := []string{p.f.idOf(t, one), p.f.idOf(t, two)}
			if n := strings.Count(strings.Join(ids, " "), p.idA); n != 1 {
				t.Fatalf("%d of the two files took the hidden item: %v", n, ids)
			}
			if ids[0] == ids[1] {
				t.Fatal("two files share one item")
			}
			sameLines(t, "user_item_data", p.f.rows(t), p.all)
			if p.f.itemCount(t) != 3 {
				t.Fatalf("%d items, want 3", p.f.itemCount(t))
			}
		})
	}
}

// A copy of a file that is still listed is a new item.
func TestCopyOfAListedFileNeverTakesItsItem(t *testing.T) {
	p := newPair(t)
	copyFile(t, p.pathA, filepath.Join(p.f.media, "Copy (2022).mkv"), p.mtimeA)
	p.f.scan(t)
	if got := p.f.idOf(t, p.a); got != p.idA {
		t.Fatalf("the listed file lost its item to %s", got)
	}
	got := p.f.idOf(t, "Copy (2022).mkv")
	if got == p.idA || len(p.f.rows(t, got)) != 0 {
		t.Fatalf("the copy has item %s with %d rows", got, len(p.f.rows(t, got)))
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
}

// With two identical files listed and one missing, a new copy takes the
// missing one's item and the file that is listed keeps its own.
func TestCopyTakesTheHiddenItemAndNotTheListedFilesItem(t *testing.T) {
	p := newPair(t)
	twin := filepath.Join(p.f.media, "Twin (2021).mkv")
	copyFile(t, p.pathA, twin, p.mtimeA)
	p.f.scan(t)
	p.f.seed(t, p.users) // Rows for the twin's item; the others' stay.
	idTwin := p.f.idOf(t, "Twin (2021).mkv")
	all := p.f.rows(t)
	if idTwin == p.idA {
		t.Fatal("the twin took the item")
	}
	p.lose(t)
	copyFile(t, p.stashA, filepath.Join(p.f.media, "Third (2022).mkv"), p.mtimeA)
	p.f.scan(t)
	if got := p.f.idOf(t, "Third (2022).mkv"); got != p.idA {
		t.Fatalf("the new copy has item %s, not the hidden one", got)
	}
	if got := p.f.idOf(t, "Twin (2021).mkv"); got != idTwin {
		t.Fatalf("the listed twin has item %s, not its own", got)
	}
	sameLines(t, "user_item_data", p.f.rows(t), all)
}

// A returning or moved file whose probe fails creates no second item and
// loses no row, even once the grace is over; the next scan matches it.
func TestFailedProbeOfAMovedFileCreatesNoSecondItemAndLosesNoRow(t *testing.T) {
	p := newPair(t)
	spy := spyOnProbes(t)
	p.lose(t)
	moved := "Moved (2021)/Moved (2021).mkv"
	move(t, p.stashA, filepath.Join(p.f.media, moved))
	spy.failing(t, true)
	p.f.scan(t)
	p.f.scanAfterGrace(t)
	if n := p.f.itemCount(t); n != 2 {
		t.Fatalf("%d items, want the 2 there were", n)
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
	if !p.f.hasItem(t, p.idA) {
		t.Fatal("the item was removed while its file was listed and unreadable")
	}
	spy.failing(t, false)
	p.f.scan(t)
	if got := p.f.idOf(t, moved); got != p.idA {
		t.Fatalf("the next scan gave the file item %s", got)
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
}

// An item whose key is listed is never hidden or removed, whatever the file.
func TestListedKeyIsNeverRemovedWhenItsProbeFails(t *testing.T) {
	p := newPair(t)
	p.lose(t)
	if err := os.WriteFile(p.pathA, []byte("this is not a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.f.scanAfterGrace(t)
	if n := p.f.itemCount(t); n != 2 {
		t.Fatalf("%d items, want 2", n)
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
	if got := p.f.idOf(t, p.a); got != p.idA {
		t.Fatalf("the key has item %s", got)
	}
	film(t, p.pathA, 4)
	p.f.scan(t)
	if !p.f.visible(t, p.idA) {
		t.Fatal("the film did not come back once it could be read")
	}
	sameLines(t, "user_item_data", p.f.rows(t), p.all)
}

// tenFilms is a library of ten films with state for two viewers, and a share of one half.
type tenFilms struct {
	f     fixture
	keys  []string
	ids   []string
	items []string
	all   []string
}

func newTenFilms(t *testing.T) *tenFilms {
	t.Helper()
	f := setup(t, "movies")
	f.scanner.MaxLostShare = 0.5
	g := &tenFilms{f: f, keys: films(t, f.media, "Film", 10)}
	f.scan(t)
	f.seed(t, f.users(t))
	for _, key := range g.keys {
		g.ids = append(g.ids, f.idOf(t, key))
	}
	g.items, g.all = f.itemRows(t), f.rows(t)
	return g
}

// hide takes films from..to out of the library, to a stash.
func (g *tenFilms) lose(t *testing.T, from, to int) {
	t.Helper()
	for _, key := range g.keys[from:to] {
		move(t, filepath.Join(g.f.media, key), filepath.Join(g.f.media+".stash", key))
	}
}

func (g *tenFilms) restore(t *testing.T) {
	t.Helper()
	for _, key := range g.keys {
		if _, err := os.Stat(filepath.Join(g.f.media+".stash", key)); err == nil {
			move(t, filepath.Join(g.f.media+".stash", key), filepath.Join(g.f.media, key))
		}
	}
	t.Cleanup(func() { os.RemoveAll(g.f.media + ".stash") })
}

func (g *tenFilms) shown(t *testing.T) int {
	t.Helper()
	n := 0
	for _, id := range g.ids {
		if g.f.visible(t, id) {
			n++
		}
	}
	return n
}

func TestLossOfExactlyTheShareHidesTheFiles(t *testing.T) {
	g := newTenFilms(t)
	g.lose(t, 0, 5)
	g.f.scan(t)
	if n := g.shown(t); n != 5 {
		t.Fatalf("%d films shown, want the 5 that are there", n)
	}
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
}

func TestGuardTripsOnOneFileMoreThanTheShare(t *testing.T) {
	g := newTenFilms(t)
	g.lose(t, 0, 6)
	err := g.f.scanner.Scan(context.Background(), g.f.lib)
	if err == nil {
		t.Fatal("a loss of 6 of 10 films with a share of one half passed")
	}
	for _, want := range []string{`\btest\b`, `\b6\b`, `\b10\b`} {
		if !regexp.MustCompile(want).MatchString(err.Error()) {
			t.Errorf("the error %q does not name %s: the library, the files lost and the files stored", err, want)
		}
	}
	if errors.Is(err, library.ErrElsewhere) {
		t.Fatal("the guard is not another replica")
	}
	if n := g.shown(t); n != 10 {
		t.Fatalf("%d films shown, the guard hides nothing", n)
	}
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
	// The first scan after the files are back succeeds, with everything as it was.
	g.restore(t)
	g.f.scan(t)
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
	if n := g.shown(t); n != 10 {
		t.Fatalf("%d films shown after the files came back", n)
	}
}

// Two scans each under the share but together over it trip on the second;
// the files the first scan hid count.
func TestGuardCountsTheFilesAlreadyHidden(t *testing.T) {
	g := newTenFilms(t)
	g.lose(t, 0, 4)
	g.f.scan(t)
	if n := g.shown(t); n != 6 {
		t.Fatalf("%d films shown after the first loss, want 6", n)
	}
	g.lose(t, 4, 6)
	err := g.f.scanner.Scan(context.Background(), g.f.lib)
	if err == nil || !regexp.MustCompile(`\b6\b`).MatchString(err.Error()) {
		t.Fatalf("the second scan, 6 of 10 gone in all: %v", err)
	}
	if n := g.shown(t); n != 6 {
		t.Fatalf("%d films shown, the guard changes nothing", n)
	}
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
}

// Nothing is removed by a scan the guard stops, a hidden item past its grace included.
func TestGuardRemovesNothingEvenAfterTheGrace(t *testing.T) {
	g := newTenFilms(t)
	g.lose(t, 0, 1)
	g.f.scan(t)
	g.lose(t, 1, 7)
	time.Sleep(20 * time.Millisecond)
	g.f.scanner.Grace = time.Nanosecond
	if err := g.f.scanner.Scan(context.Background(), g.f.lib); err == nil {
		t.Fatal("7 of 10 lost passed")
	}
	if n := g.f.itemCount(t); n != 10 {
		t.Fatalf("%d items, want all 10", n)
	}
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
}

// A library whose every file moved loses none, so the guard lets it pass.
func TestLibraryWhoseEveryFileMovedPassesTheGuardAndKeepsEveryID(t *testing.T) {
	g := newTenFilms(t)
	for _, key := range g.keys {
		move(t, filepath.Join(g.f.media, key), filepath.Join(g.f.media, "moved", key))
	}
	g.f.scan(t)
	for i, key := range g.keys {
		if got := g.f.idOf(t, "moved/"+key); got != g.ids[i] {
			t.Fatalf("%s has item %s, was %s", key, got, g.ids[i])
		}
	}
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
	if g.f.itemCount(t) != 10 || g.shown(t) != 10 {
		t.Fatalf("%d items, %d shown", g.f.itemCount(t), g.shown(t))
	}
}

// A guard that trips in one library changes nothing in another's scan.
func TestGuardInOneLibraryChangesNothingInAnother(t *testing.T) {
	f := setup(t, "movies")
	other := t.TempDir()
	libs, err := library.Sync(context.Background(), f.pool, []library.Config{
		{Name: "test", Kind: "movies", Media: "file://" + f.media, Assets: "file://" + f.media},
		{Name: "other", Kind: "movies", Media: "file://" + other, Assets: "file://" + other},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.scanner.MaxLostShare = 0.5
	mine, theirs := libs[0], libs[1]
	keys := films(t, f.media, "Film", 10)
	otherKeys := films(t, other, "Other", 4)
	for _, lib := range libs {
		if err := f.scanner.Scan(context.Background(), lib); err != nil {
			t.Fatal(err)
		}
	}
	f.seed(t, f.users(t))
	all := f.rows(t)
	for _, key := range keys[:6] {
		move(t, filepath.Join(f.media, key), filepath.Join(t.TempDir(), key))
	}
	move(t, filepath.Join(other, otherKeys[0]), filepath.Join(t.TempDir(), otherKeys[0]))
	if err := f.scanner.Scan(context.Background(), mine); err == nil {
		t.Fatal("6 of 10 lost passed")
	}
	if err := f.scanner.Scan(context.Background(), theirs); err != nil {
		t.Fatalf("the other library's scan: %v", err)
	}
	hidden := f.idOf(t, otherKeys[0])
	if f.visible(t, hidden) {
		t.Fatal("the other library did not hide its missing film")
	}
	for _, key := range otherKeys[1:] {
		if !f.visible(t, f.idOf(t, key)) {
			t.Fatalf("%s is hidden", key)
		}
	}
	for _, key := range keys {
		if !f.visible(t, f.idOf(t, key)) {
			t.Fatalf("%s of the library the guard stopped is hidden", key)
		}
	}
	sameLines(t, "user_item_data", f.rows(t), all)
}

// listing is a media store whose listing a test replaces.
type listing struct {
	store.Store
	list func(ctx context.Context) ([]store.Object, error)
}

func (l listing) List(ctx context.Context, _ string) ([]store.Object, error) { return l.list(ctx) }

// A store that lists no video, and a listing that fails, hide nothing and
// remove nothing: a hidden item whose grace is over stays.
func TestEmptyMountAndFailedListingHideAndRemoveNothing(t *testing.T) {
	cases := map[string]func(cancel context.CancelFunc) func(ctx context.Context) ([]store.Object, error){
		"a store that lists no video": func(context.CancelFunc) func(context.Context) ([]store.Object, error) {
			return func(context.Context) ([]store.Object, error) { return nil, nil }
		},
		"a store error": func(context.CancelFunc) func(context.Context) ([]store.Object, error) {
			return func(context.Context) ([]store.Object, error) { return nil, errors.New("the store is down") }
		},
		"a timeout": func(context.CancelFunc) func(context.Context) ([]store.Object, error) {
			return func(ctx context.Context) ([]store.Object, error) { <-ctx.Done(); return nil, ctx.Err() }
		},
		"a cancelled scan": func(cancel context.CancelFunc) func(context.Context) ([]store.Object, error) {
			return func(ctx context.Context) ([]store.Object, error) { cancel(); return nil, ctx.Err() }
		},
	}
	for name, makeList := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPair(t)
			p.lose(t) // A is hidden, with its grace running.
			want := p.f.itemRows(t)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			lib := p.f.lib
			lib.Media = listing{lib.Media, makeList(cancel)}
			time.Sleep(20 * time.Millisecond)
			p.f.scanner.Grace = time.Nanosecond
			p.f.scanner.MaxLostShare = 1
			if err := p.f.scanner.Scan(ctx, lib); err == nil {
				t.Fatal("the scan reported success")
			}
			sameLines(t, "the items", p.f.itemRows(t), want)
			sameLines(t, "user_item_data", p.f.rows(t), p.all)
			if !p.f.visible(t, p.idB) {
				t.Fatal("the film that is there was hidden")
			}
			if p.f.visible(t, p.idA) {
				t.Fatal("the hidden film was shown")
			}
		})
	}
}
