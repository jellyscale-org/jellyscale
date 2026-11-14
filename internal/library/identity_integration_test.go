//go:build integration

package library_test

// JS-076, library identity: a configured library keeps its id when it is
// renamed, a library left out of config is hidden and later removed. The names
// these use that the code lacks: library.RemoveGone(ctx, pool, grace), which
// removes the libraries that have been out of config for longer than grace (a
// process calls it after Sync, at every start).

import (
	"context"
	"crypto/md5"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/library"
)

func cfg(name, dir string) library.Config {
	return library.Config{Name: name, Kind: "movies", Media: "file://" + dir, Assets: "file://" + dir}
}

func libraryRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return textRows(t, pool, "SELECT id::text || '|' || name FROM libraries ORDER BY name")
}

func idsByName(t *testing.T, libs []library.Library) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, lib := range libs {
		ids[lib.Name] = lib.ID
	}
	return ids
}

// Twelve replicas that start at once with one config agree on each library's
// id and leave one row each.
func TestReplicasStartingTogetherAgreeOnEachLibrary(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	configs := []library.Config{cfg("Movies", t.TempDir()), cfg("Films", t.TempDir())}
	for round := range 20 {
		if _, err := pool.Exec(ctx, "DELETE FROM libraries"); err != nil {
			t.Fatal(err)
		}
		answers := make([]map[string]string, 12)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				libs, err := library.Sync(ctx, pool, configs)
				if err != nil {
					t.Error(err)
					return
				}
				answers[i] = idsByName(t, libs)
			}()
		}
		close(start)
		wg.Wait()
		if n := scalar[int](t, pool, "SELECT count(*) FROM libraries"); n != 2 {
			t.Fatalf("round %d: %d library rows, want 2", round, n)
		}
		for _, got := range answers {
			if !equalMaps(got, answers[0]) {
				t.Fatalf("round %d: replicas disagree on the ids: %v and %v", round, got, answers[0])
			}
		}
		if rows := libraryRows(t, pool); len(rows) != 2 {
			t.Fatalf("rows %v", rows)
		}
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A library stored before JS-076 has the hash of its name as its id, and keeps
// it when the server starts with the same config.
func TestStoredLibraryKeepsTheIDItHasToday(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	id := uuid.UUID(md5.Sum([]byte("library:Movies"))).String()
	if _, err := pool.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'Movies', 'movies', $2, $2)`, id, "file://"+dir); err != nil {
		t.Fatal(err)
	}
	libs, err := library.Sync(ctx, pool, []library.Config{cfg("Movies", dir)})
	if err != nil {
		t.Fatal(err)
	}
	if libs[0].ID != id {
		t.Fatalf("the library has id %s, stored as %s", libs[0].ID, id)
	}
	if rows := libraryRows(t, pool); !slices.Equal(rows, []string{id + "|Movies"}) {
		t.Fatalf("rows %v", rows)
	}
	// The same, with twelve replicas starting at once.
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := library.Sync(ctx, pool, []library.Config{cfg("Movies", dir)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if rows := libraryRows(t, pool); !slices.Equal(rows, []string{id + "|Movies"}) {
		t.Fatalf("rows %v", rows)
	}
}

// libraries with films and state, scanned: two libraries, "Movies" and "Other".
type twoLibraries struct {
	f         fixture
	moviesDir string
	otherDir  string
	libs      []library.Library
	moviesID  string
	otherID   string
	items     []string
	all       []string
}

func newTwoLibraries(t *testing.T) *twoLibraries {
	t.Helper()
	pool := openDB(t)
	g := &twoLibraries{moviesDir: t.TempDir(), otherDir: t.TempDir()}
	libs, err := library.Sync(context.Background(), pool, []library.Config{cfg("Movies", g.moviesDir), cfg("Other", g.otherDir)})
	if err != nil {
		t.Fatal(err)
	}
	g.libs = libs
	g.f = fixture{pool: pool, scanner: nil, lib: libs[0], media: g.moviesDir}
	g.f.scanner = newScanner(pool)
	films(t, g.moviesDir, "Film", 3)
	films(t, g.otherDir, "Other", 2)
	for _, lib := range libs {
		if err := g.f.scanner.Scan(context.Background(), lib); err != nil {
			t.Fatal(err)
		}
	}
	g.f.seed(t, g.f.users(t))
	ids := idsByName(t, libs)
	g.moviesID, g.otherID = ids["Movies"], ids["Other"]
	g.items, g.all = g.f.itemRows(t), g.f.rows(t)
	if len(g.all) != 10 {
		t.Fatalf("%d rows seeded, want 10", len(g.all))
	}
	return g
}

func newScanner(pool *pgxpool.Pool) *library.Scanner {
	return &library.Scanner{Pool: pool, Log: slog.New(slog.DiscardHandler), Grace: time.Hour, MaxLostShare: 1}
}

// A config that changes only a library's name leaves one library, with its id
// and everything in it, and the others as they were.
func TestRenamedLibraryIsTheSameLibrary(t *testing.T) {
	g := newTwoLibraries(t)
	ctx := context.Background()
	libs, err := library.Sync(ctx, g.f.pool, []library.Config{cfg("Films", g.moviesDir), cfg("Other", g.otherDir)})
	if err != nil {
		t.Fatal(err)
	}
	ids := idsByName(t, libs)
	if ids["Films"] != g.moviesID || ids["Other"] != g.otherID {
		t.Fatalf("ids after the rename: %v; were Movies %s, Other %s", ids, g.moviesID, g.otherID)
	}
	if n := scalar[int](t, g.f.pool, "SELECT count(*) FROM libraries"); n != 2 {
		t.Fatalf("%d libraries, want 2", n)
	}
	shown, err := g.f.repo().Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, lib := range shown {
		names = append(names, lib.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"Films", "Other"}) {
		t.Fatalf("/UserViews lists %q", names)
	}
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
	// Nothing goes when the grace later passes.
	time.Sleep(20 * time.Millisecond)
	if err := library.RemoveGone(ctx, g.f.pool, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	sameLines(t, "the items after the grace", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data after the grace", g.f.rows(t), g.all)
	if n := scalar[int](t, g.f.pool, "SELECT count(*) FROM libraries"); n != 2 {
		t.Fatalf("%d libraries after the grace, want 2", n)
	}
	// A scan of the renamed library still finds its items.
	if err := g.f.scanner.Scan(ctx, libs[0]); err != nil {
		t.Fatal(err)
	}
	sameLines(t, "the items after a scan", g.f.itemRows(t), g.items)
}

// Twelve replicas starting at once with the renamed config still leave one row.
func TestTwelveReplicasStartingWithARenamedConfigLeaveOneRow(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	dir := t.TempDir()
	was, err := library.Sync(ctx, pool, []library.Config{cfg("Movies", dir)})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 20 {
		name := fmt.Sprint("Renamed ", round)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				libs, err := library.Sync(ctx, pool, []library.Config{cfg(name, dir)})
				if err != nil {
					t.Error(err)
					return
				}
				if libs[0].ID != was[0].ID {
					t.Errorf("a replica got id %s, want %s", libs[0].ID, was[0].ID)
				}
			}()
		}
		close(start)
		wg.Wait()
		if rows := libraryRows(t, pool); !slices.Equal(rows, []string{was[0].ID + "|" + name}) {
			t.Fatalf("round %d: rows %v", round, rows)
		}
	}
}

// A new library that takes the old name is a second library with an id of its own.
func TestNewLibraryTakingTheOldNameIsASecondLibrary(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	dir, elsewhere := t.TempDir(), t.TempDir()
	first, err := library.Sync(ctx, pool, []library.Config{cfg("Movies", dir)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := library.Sync(ctx, pool, []library.Config{cfg("Films", dir)}); err != nil {
		t.Fatal(err)
	}
	config := []library.Config{cfg("Films", dir), cfg("Movies", elsewhere)}
	answers := make([]map[string]string, 12)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			libs, err := library.Sync(ctx, pool, config)
			if err != nil {
				t.Error(err)
				return
			}
			answers[i] = idsByName(t, libs)
		}()
	}
	close(start)
	wg.Wait()
	if n := scalar[int](t, pool, "SELECT count(*) FROM libraries"); n != 2 {
		t.Fatalf("%d libraries, want 2", n)
	}
	for _, got := range answers {
		if !equalMaps(got, answers[0]) {
			t.Fatalf("replicas disagree: %v and %v", got, answers[0])
		}
	}
	ids := answers[0]
	if ids["Films"] != first[0].ID {
		t.Fatalf("the renamed library has id %s, was %s", ids["Films"], first[0].ID)
	}
	if ids["Movies"] == ids["Films"] || ids["Movies"] == "" {
		t.Fatalf("the new library has id %q; the renamed one has it", ids["Movies"])
	}
	again, err := library.Sync(ctx, pool, config)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsByName(t, again); !equalMaps(got, ids) {
		t.Fatalf("a later start gives %v, was %v", got, ids)
	}
}

// A library left out of config is hidden, with its items; put back inside the
// grace it returns with its id, items and every user's watch state.
func TestLibraryLeftOutOfConfigIsHiddenAndComesBack(t *testing.T) {
	g := newTwoLibraries(t)
	ctx := context.Background()
	movie := g.f.idOf(t, "Film A (2020).mkv")
	if _, err := library.Sync(ctx, g.f.pool, []library.Config{cfg("Other", g.otherDir)}); err != nil {
		t.Fatal(err)
	}
	shown, err := g.f.repo().Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || shown[0].ID != g.otherID {
		t.Fatalf("/UserViews lists %v, want only Other", shown)
	}
	if _, found, err := g.f.repo().IsLibrary(ctx, g.moviesID); err != nil || found {
		t.Fatalf("GET /Items/{library id} finds the library: %v %v", found, err)
	}
	if g.f.visible(t, movie) {
		t.Fatal("an item of the library is still shown")
	}
	result, err := g.f.repo().Find(ctx, items.Query{Recursive: true}, nil, true)
	if err != nil || result.TotalRecordCount != 2 {
		t.Fatalf("a viewer counts %d items, want Other's 2: %v", result.TotalRecordCount, err)
	}
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
	if n := scalar[int](t, g.f.pool, "SELECT count(*) FROM libraries WHERE id = $1", g.moviesID); n != 1 {
		t.Fatal("the library row was deleted")
	}
	// A start inside the grace removes nothing.
	if err := library.RemoveGone(ctx, g.f.pool, time.Hour); err != nil {
		t.Fatal(err)
	}
	libs, err := library.Sync(ctx, g.f.pool, []library.Config{cfg("Movies", g.moviesDir), cfg("Other", g.otherDir)})
	if err != nil {
		t.Fatal(err)
	}
	if ids := idsByName(t, libs); ids["Movies"] != g.moviesID || ids["Other"] != g.otherID {
		t.Fatalf("ids %v", ids)
	}
	if !g.f.visible(t, movie) {
		t.Fatal("the library's items are still hidden")
	}
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)
}

// Removal comes once the grace has passed since the first start without the
// library, whichever replica or restart comes in between, and not earlier.
func TestLibraryLeftOutIsRemovedOnlyAfterTheGraceSinceTheFirstStartWithoutIt(t *testing.T) {
	g := newTwoLibraries(t)
	ctx := context.Background()
	without := []library.Config{cfg("Other", g.otherDir)}
	if _, err := library.Sync(ctx, g.f.pool, without); err != nil {
		t.Fatal(err)
	}
	// Replicas starting and checking at once, within the grace.
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := library.Sync(ctx, g.f.pool, without); err != nil {
				t.Error(err)
			}
			if err := library.RemoveGone(ctx, g.f.pool, time.Hour); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	sameLines(t, "user_item_data", g.f.rows(t), g.all)

	time.Sleep(1200 * time.Millisecond)
	// Another start, later: it must not move the time the library went missing.
	if _, err := library.Sync(ctx, g.f.pool, without); err != nil {
		t.Fatal(err)
	}
	if err := library.RemoveGone(ctx, g.f.pool, time.Hour); err != nil {
		t.Fatal(err)
	}
	sameLines(t, "the items", g.f.itemRows(t), g.items)
	// More than 1 second since the first start without it (the second start, 1.2 s later, is under it).
	if err := library.RemoveGone(ctx, g.f.pool, time.Second); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int](t, g.f.pool, "SELECT count(*) FROM libraries WHERE id = $1", g.moviesID); n != 0 {
		t.Fatal("the library is still there, or its grace was counted from a later start")
	}
	if n := scalar[int](t, g.f.pool, "SELECT count(*) FROM items WHERE library_id = $1", g.moviesID); n != 0 {
		t.Fatalf("%d items of the removed library left", n)
	}
	var others []string
	for _, line := range g.items {
		if strings.Contains(line, "|"+g.otherID+"|") {
			others = append(others, line)
		}
	}
	sameLines(t, "the other library's items", g.f.itemRows(t), others)
	if got := g.f.rows(t); len(got) != 4 {
		t.Fatalf("%d rows of watch state left, want the 4 of the other library's two films", len(got))
	}
}
