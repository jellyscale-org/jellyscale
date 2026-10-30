package library_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
)

// These need a disposable database and ffmpeg; test/integration.sh provides both.
func openDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JELLYSCALE_TEST_DATABASE_URL not set")
	}
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

func (f fixture) count(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), "SELECT count(*) FROM items WHERE type = $1", kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRenameKeepsItemAndWatchState(t *testing.T) {
	f := setup(t, "movies")
	ctx := context.Background()
	old := filepath.Join(f.media, "Old Name (2020)", "Old Name (2020).mkv")
	clip(t, old)
	f.scan(t)

	var id string
	if err := f.pool.QueryRow(ctx, "SELECT id::text FROM items").Scan(&id); err != nil {
		t.Fatal(err)
	}
	var user string
	if err := f.pool.QueryRow(ctx, `INSERT INTO users (id, name, password_hash) VALUES (gen_random_uuid(), 'u', 'x') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
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
	var keys int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM scan_objects").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if keys != 1 {
		t.Fatalf("expected the old scan object to be gone, got %d", keys)
	}
}

func TestChangedFileKeepsItem(t *testing.T) {
	f := setup(t, "movies")
	ctx := context.Background()
	file := filepath.Join(f.media, "Film (2020).mkv")
	clip(t, file)
	f.scan(t)
	var before string
	if err := f.pool.QueryRow(ctx, "SELECT id::text FROM items").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	clip(t, file)
	f.scan(t)
	var after string
	if err := f.pool.QueryRow(ctx, "SELECT id::text FROM items").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("replacing the file's content changed its item")
	}
}

func TestRepeatScanChangesNothing(t *testing.T) {
	f := setup(t, "movies")
	ctx := context.Background()
	clip(t, filepath.Join(f.media, "Film (2020).mkv"))
	f.scan(t)
	var before string
	if err := f.pool.QueryRow(ctx, "SELECT dto::text FROM items").Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	var after string
	if err := f.pool.QueryRow(ctx, "SELECT dto::text FROM items").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("repeat scan rewrote an unchanged item")
	}
}

func TestDeletionPrunesEmptyParents(t *testing.T) {
	f := setup(t, "tvshows")
	first := filepath.Join(f.media, "Show", "Season 01", "Show S01E01.mkv")
	second := filepath.Join(f.media, "Show", "Season 02", "Show S02E01.mkv")
	clip(t, first)
	clip(t, second)
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 1 || se != 2 || e != 2 {
		t.Fatalf("got series=%d seasons=%d episodes=%d", s, se, e)
	}

	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 1 || se != 1 || e != 1 {
		t.Fatalf("after one removal: series=%d seasons=%d episodes=%d", s, se, e)
	}

	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if s, se, e := f.count(t, "Series"), f.count(t, "Season"), f.count(t, "Episode"); s != 0 || se != 0 || e != 0 {
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
			if err := f.scanner.Scan(context.Background(), f.lib); err != nil {
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
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JELLYSCALE_TEST_DATABASE_URL not set")
	}
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
