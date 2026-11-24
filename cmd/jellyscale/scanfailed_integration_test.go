//go:build integration

package main

// JS-108: a scan counts the films it could not store, says so in its closing
// log line and in jellyscale_scan_last_failed_films, and a library with any of
// them is not a success. Every check drives scanLoop, which is what an operator
// sees: the log and the gauges. The names the task implies and the code lacks:
// the gauge scanFailed, and a "failed" attribute on the "scan finished" line.
//
// A scan here is one scanLoop, stopped once the library's gauges are set. The
// short pause after the timestamp moves only lets the loop's goroutine set the
// gauges it sets after the timestamp; no check waits on it for a value.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// flakyStore fails every read of the keys in failing; with hang set it blocks
// every read until the scan's context ends. A key it fails is read through
// Open, not from the local path, so the failure is the store's.
type flakyStore struct {
	store.Store
	mu      sync.Mutex
	failing map[string]bool
	hang    bool
	reading chan struct{} // Closed at the first read that hangs.
	once    sync.Once
}

func (s *flakyStore) state(key string) (fails, hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hang || s.failing[key], s.hang
}

func (s *flakyStore) fail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = map[string]bool{}
	for _, key := range keys {
		s.failing[key] = true
	}
}

func (s *flakyStore) LocalPath(key string) (string, bool) {
	if fails, _ := s.state(key); fails {
		return "", false
	}
	return s.Store.LocalPath(key)
}

func (s *flakyStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if fails, hang := s.state(key); fails {
		if hang {
			s.once.Do(func() { close(s.reading) })
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return nil, errors.New("the store failed this read")
	}
	return s.Store.Open(ctx, key, offset)
}

type scanEnv struct {
	pool    *pgxpool.Pool
	lib     library.Library
	media   string
	store   *flakyStore
	scanner *library.Scanner
	logs    *logSink
	seen    int // Log records the scans before this one wrote.
}

func clip(t *testing.T, file string, seconds int) {
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

func junk(t *testing.T, file string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("these bytes are not a film, whatever the name says"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newScanEnv opens a library of the kind with a store that fails on demand. The
// scanner has the default guard and a grace of an hour, as a deployment does.
func newScanEnv(t *testing.T, kind string) *scanEnv {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	media := t.TempDir()
	flaky := &flakyStore{Store: store.NewPosix(media), reading: make(chan struct{})}
	lib := library.Library{ID: uuid.NewString(), Name: "lib-" + uuid.NewString()[:8], Kind: kind, Media: flaky, Assets: store.NewPosix(t.TempDir())}
	if _, err := pool.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, $2, $3, 'file:///x', 'file:///x')`,
		lib.ID, lib.Name, kind); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}
	scanner := &library.Scanner{Pool: pool, Log: slog.New(slog.NewJSONHandler(logs, nil)), Grace: time.Hour, MaxLostShare: library.DefaultMaxLostShare}
	return &scanEnv{pool: pool, lib: lib, media: media, store: flaky, scanner: scanner, logs: logs}
}

// second makes a second library in the same process, sharing the database.
func (e *scanEnv) second(t *testing.T) (library.Library, string) {
	t.Helper()
	dir := t.TempDir()
	lib := library.Library{ID: uuid.NewString(), Name: "lib-" + uuid.NewString()[:8], Kind: "movies", Media: store.NewPosix(dir), Assets: store.NewPosix(t.TempDir())}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, $2, 'movies', 'file:///x', 'file:///x')`,
		lib.ID, lib.Name); err != nil {
		t.Fatal(err)
	}
	return lib, dir
}

func (e *scanEnv) films(t *testing.T, names ...string) {
	t.Helper()
	for i, name := range names {
		clip(t, filepath.Join(e.media, name), i+1)
	}
}

func filmNames(n int) []string {
	var names []string
	for i := range n {
		names = append(names, fmt.Sprintf("Film %c (2020).mkv", 'A'+i))
	}
	return names
}

// scan runs one scan of each library by scanLoop and returns once every one has
// reported.
func (e *scanEnv) scan(t *testing.T, libs ...library.Library) {
	t.Helper()
	if len(libs) == 0 {
		libs = []library.Library{e.lib}
	}
	was := map[string]float64{}
	for _, lib := range libs {
		was[lib.ID], _ = gaugeOf(t, scanTime, lib.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		scanLoop(ctx, e.scanner, libs, time.Hour, slog.New(slog.NewJSONHandler(e.logs, nil)))
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Minute)
	for _, lib := range libs {
		for {
			if at, ok := gaugeOf(t, scanTime, lib.ID); ok && at != was[lib.ID] {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("library %s: no scan reported", lib.Name)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
}

// lines are the log records since the scan before this one.
func (e *scanEnv) lines() []map[string]any {
	all := e.logs.records()
	out := all[e.seen:]
	e.seen = len(all)
	return out
}

func (e *scanEnv) gauge(t *testing.T, vec *prometheus.GaugeVec) float64 {
	t.Helper()
	return gaugeFor(t, vec, e.lib.ID)
}

func gaugeFor(t *testing.T, vec *prometheus.GaugeVec, id string) float64 {
	t.Helper()
	value, ok := gaugeOf(t, vec, id)
	if !ok {
		t.Errorf("library %s has no value in the gauge", id)
		return -1
	}
	return value
}

// reported checks the library's gauges after a scan.
func reported(t *testing.T, id string, failed, success float64) {
	t.Helper()
	if got := gaugeFor(t, scanFailed, id); got != failed {
		t.Errorf("jellyscale_scan_last_failed_films %v, want %v", got, failed)
	}
	if got := gaugeFor(t, scanOK, id); got != success {
		t.Errorf("jellyscale_scan_last_success %v, want %v", got, success)
	}
}

// closing finds the scan's closing line for the library and checks what it
// says of the failed films.
func closing(t *testing.T, lines []map[string]any, name string, failed float64) {
	t.Helper()
	for _, line := range lines {
		if line["msg"] == "scan finished" && line["library"] == name {
			if line["failed"] != failed {
				t.Errorf("the closing line says failed=%v, want %v: %v", line["failed"], failed, line)
			}
			return
		}
	}
	t.Errorf("no closing line for %s in %v", name, lines)
}

// failedLine finds the line of one film that failed: its key and an error.
func failedLine(t *testing.T, lines []map[string]any, key string) {
	t.Helper()
	for _, line := range lines {
		if line["key"] == key && line["level"] == "ERROR" && line["error"] != nil && line["error"] != "" {
			return
		}
	}
	t.Errorf("no error line with key %q and an error", key)
}

// shown counts the films a viewer lists in the library: stored and not hidden.
func (e *scanEnv) shown(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM items WHERE library_id = $1 AND type IN ('Movie', 'Episode') AND missing_since IS NULL`, e.lib.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *scanEnv) idOf(t *testing.T, key string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), "SELECT id::text FROM items WHERE library_id = $1 AND natural_key = $2", e.lib.ID, "video:"+key).Scan(&id); err != nil {
		return ""
	}
	return id
}

func TestTwoOfFiveFilmsFailingAreCountedAndTheOthersStored(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.store.fail(names[1], names[3])
	began := time.Now()
	e.scan(t)
	lines := e.lines()

	if n := e.shown(t); n != 3 {
		t.Errorf("%d films listed, want the 3 that could be read", n)
	}
	for _, name := range []string{names[1], names[3]} {
		if id := e.idOf(t, name); id != "" {
			t.Errorf("%s failed and has item %s", name, id)
		}
		failedLine(t, lines, name)
	}
	closing(t, lines, e.lib.Name, 2)
	reported(t, e.lib.ID, 2, 0)
	if at := e.gauge(t, scanTime); at < float64(began.UnixNano())/1e9 {
		t.Errorf("the scan's timestamp %v is older than the scan", at)
	}
	if d, ok := gaugeOf(t, scanDuration, e.lib.ID); !ok || d <= 0 {
		t.Errorf("the scan's duration %v, %v", d, ok)
	}
}

func TestTheCountIsOfOneScanAndTheNextCleanScanStoresThemAndSucceeds(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.store.fail(names[1], names[3])
	e.scan(t)
	e.lines()
	e.scan(t)
	closing(t, e.lines(), e.lib.Name, 2)
	reported(t, e.lib.ID, 2, 0)
	if n := e.shown(t); n != 3 {
		t.Errorf("%d films after a second scan, want 3", n)
	}

	e.store.fail() // No file changed.
	e.scan(t)
	closing(t, e.lines(), e.lib.Name, 0)
	reported(t, e.lib.ID, 0, 1)
	if n := e.shown(t); n != 5 {
		t.Errorf("%d films once the store reads, want 5", n)
	}
}

func TestAScanThatStoresNoneOfItsFilmsSaysSoAndFails(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.store.fail(names...)
	e.scan(t)
	closing(t, e.lines(), e.lib.Name, 5)
	reported(t, e.lib.ID, 5, 0)
	if n := e.shown(t); n != 0 {
		t.Errorf("%d films, want none", n)
	}
}

// The films are probed; their rows are refused by Postgres: a trigger on the
// natural key. It matches only these films' names, which no other test uses.
func TestAFilmPostgresRefusedIsCountedLikeOneThatCouldNotBeProbed(t *testing.T) {
	e := newScanEnv(t, "movies")
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE OR REPLACE FUNCTION js108_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'refused by the test'; END $$`,
		`CREATE TRIGGER js108_refuse BEFORE INSERT OR UPDATE ON items FOR EACH ROW WHEN (NEW.natural_key LIKE 'video:Refused%') EXECUTE FUNCTION js108_refuse()`,
	} {
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { e.pool.Exec(ctx, "DROP TRIGGER IF EXISTS js108_refuse ON items") }) //nolint:errcheck
	e.films(t, "Refused A (2020).mkv", "Refused B (2020).mkv", "Film A (2020).mkv", "Film B (2020).mkv", "Film C (2020).mkv")
	e.scan(t)
	lines := e.lines()
	closing(t, lines, e.lib.Name, 2)
	reported(t, e.lib.ID, 2, 0)
	if n := e.shown(t); n != 3 {
		t.Errorf("%d films, want 3", n)
	}
}

func TestAFileThatIsNoFilmIsOneFailureAtEveryScanUntilItIsReplacedOrRemoved(t *testing.T) {
	for _, how := range []string{"replaced by a film", "removed"} {
		t.Run(how, func(t *testing.T) {
			e := newScanEnv(t, "movies")
			e.films(t, filmNames(2)...)
			bad := filepath.Join(e.media, "Not A Film (2020).mkv")
			junk(t, bad)
			for range 3 {
				e.scan(t)
				lines := e.lines()
				closing(t, lines, e.lib.Name, 1)
				failedLine(t, lines, "Not A Film (2020).mkv")
				reported(t, e.lib.ID, 1, 0)
			}
			want := 2
			if how == "removed" {
				if err := os.Remove(bad); err != nil {
					t.Fatal(err)
				}
			} else {
				clip(t, bad, 3)
				want = 3
			}
			e.scan(t)
			closing(t, e.lines(), e.lib.Name, 0)
			reported(t, e.lib.ID, 0, 1)
			if n := e.shown(t); n != want {
				t.Errorf("%d films, want %d", n, want)
			}
		})
	}
}

func TestAnEpisodeSkippedForHavingNoNumberIsNotAFailure(t *testing.T) {
	e := newScanEnv(t, "tvshows")
	clip(t, filepath.Join(e.media, "Show", "Season 01", "Show S01E01.mkv"), 1)
	clip(t, filepath.Join(e.media, "Show", "Season 01", "Behind the scenes.mkv"), 2)
	e.scan(t)
	lines := e.lines()
	closing(t, lines, e.lib.Name, 0)
	reported(t, e.lib.ID, 0, 1)
	if n := e.shown(t); n != 1 {
		t.Errorf("%d episodes, want the 1 that has a number", n)
	}
	skipped := false
	for _, line := range lines {
		skipped = skipped || strings.Contains(fmt.Sprint(line["msg"]), "skipping")
	}
	if !skipped {
		t.Error("the unnumbered file was not reported as skipped, so the check did not reach the skip")
	}
}

// A stored film whose file becomes one that cannot be probed is failed and
// stays: listed, with its ID and watch state, at every scan and past the grace,
// and the guard on lost files does not see it as lost.
func TestStoredFilmsThatCanNoLongerBeProbedStayShownAndAreCountedAtEveryScan(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.scan(t)
	e.lines()
	ids := map[string]string{}
	for _, name := range names {
		ids[name] = e.idOf(t, name)
	}
	var user string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), $1, 'x') RETURNING id::text`, "viewer-"+e.lib.Name).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks, last_played_at)
		VALUES ($1, $2, true, true, 3, 12345, now())`, user, ids[names[0]]); err != nil {
		t.Fatal(err)
	}
	junk(t, filepath.Join(e.media, names[0]))
	junk(t, filepath.Join(e.media, names[2]))
	for round := range 3 {
		if round == 2 {
			time.Sleep(20 * time.Millisecond)
			e.scanner.Grace = time.Nanosecond // The grace is over for certain.
		}
		e.scan(t)
		lines := e.lines()
		closing(t, lines, e.lib.Name, 2)
		failedLine(t, lines, names[0])
		failedLine(t, lines, names[2])
		reported(t, e.lib.ID, 2, 0)
		for _, line := range lines {
			if line["level"] == "ERROR" && strings.Contains(fmt.Sprint(line["error"]), "stored files are missing") {
				t.Errorf("round %d: the guard on lost files fired: %v", round, line)
			}
		}
		if n := e.shown(t); n != 5 {
			t.Errorf("round %d: %d films shown, want 5", round, n)
		}
		for _, name := range names {
			if got := e.idOf(t, name); got != ids[name] {
				t.Errorf("round %d: %s has item %q, was %q", round, name, got, ids[name])
			}
		}
		var rows int
		if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM user_item_data WHERE user_id = $1 AND item_id = $2 AND played AND favorite AND position_ticks = 12345", user, ids[names[0]]).Scan(&rows); err != nil || rows != 1 {
			t.Errorf("round %d: the watch state is %d rows, %v", round, rows, err)
		}
	}
}

func TestAScanWithFailedFilmsStillHidesTheFileThatWentAway(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.scan(t)
	e.lines()
	gone := e.idOf(t, names[4])
	if err := os.Remove(filepath.Join(e.media, names[4])); err != nil {
		t.Fatal(err)
	}
	junk(t, filepath.Join(e.media, names[0]))
	e.scan(t)
	closing(t, e.lines(), e.lib.Name, 1)
	reported(t, e.lib.ID, 1, 0)
	var hidden int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM items WHERE id = $1 AND missing_since IS NOT NULL", gone).Scan(&hidden); err != nil || hidden != 1 {
		t.Errorf("the film whose file went away is hidden in %d rows, %v", hidden, err)
	}
	if n := e.shown(t); n != 4 {
		t.Errorf("%d films shown, want the 3 untouched and the one whose file cannot be probed", n)
	}
}

func TestFailedFilmsInOneLibraryLeaveTheOthersGaugesAlone(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.store.fail(names[0], names[1])
	other, otherDir := e.second(t)
	for i := range 3 {
		clip(t, filepath.Join(otherDir, fmt.Sprintf("Other %c (2020).mkv", 'A'+i)), i+1)
	}
	e.scan(t, e.lib, other)
	reported(t, e.lib.ID, 2, 0)
	reported(t, other.ID, 0, 1)
	closing(t, e.lines(), other.Name, 0)
}

// Neither a scan another replica holds the lock for nor one cut short by
// shutdown says anything of the library: its gauges stay as the last scan left
// them, and a read the shutdown cut off is not a film that failed.
func TestAScanElsewhereOrCutShortLeavesTheGaugesAsTheyWere(t *testing.T) {
	e := newScanEnv(t, "movies")
	names := filmNames(5)
	e.films(t, names...)
	e.store.fail(names[1], names[3])
	e.scan(t)
	at := e.gauge(t, scanTime)
	same := func(what string) {
		t.Helper()
		reported(t, e.lib.ID, 2, 0)
		if got := e.gauge(t, scanTime); got != at {
			t.Errorf("%s: the scan's timestamp moved from %v to %v", what, at, got)
		}
	}

	// Another replica holds the lock.
	ctx := context.Background()
	other, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release() // A held connection would hang the pool's close if a check fails.
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "scan:"+e.lib.ID); err != nil {
		t.Fatal(err)
	}
	e.store.fail() // A scan that ran would clear the count.
	loop, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		scanLoop(loop, e.scanner, []library.Library{e.lib}, time.Hour, slog.New(slog.NewJSONHandler(e.logs, nil)))
		close(done)
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		found := false
		for _, line := range e.logs.records() {
			found = found || line["msg"] == "scan already running elsewhere"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scan did not say another replica has it")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	same("a scan elsewhere")
	if _, err := other.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "scan:"+e.lib.ID); err != nil {
		t.Fatal(err)
	}

	// Shutdown while the films are being read.
	e.store.mu.Lock()
	e.store.hang = true
	e.store.mu.Unlock()
	loop, cancel = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() {
		scanLoop(loop, e.scanner, []library.Library{e.lib}, time.Hour, slog.New(slog.NewJSONHandler(e.logs, nil)))
		close(done)
	}()
	select {
	case <-e.store.reading:
	case <-time.After(time.Minute):
		t.Fatal("the scan never read a film")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the scan did not stop at shutdown")
	}
	same("a scan cut short")
}

// The gauge has the name an operator alerts on.
func TestTheFailedFilmsGaugeIsNamedAndServed(t *testing.T) {
	descs := make(chan *prometheus.Desc, 1)
	scanFailed.Describe(descs)
	if got := (<-descs).String(); !strings.Contains(got, `"jellyscale_scan_last_failed_films"`) || !strings.Contains(got, "library") {
		t.Errorf("the gauge is %s", got)
	}
}
