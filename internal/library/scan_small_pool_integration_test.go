//go:build integration

package library_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// A scan holds its library's lock and does all its work meanwhile: on a pool of
// one it must not wait for a second connection.
func TestAScanCompletesOnAPoolOfOneConnection(t *testing.T) {
	f := setup(t, "movies")
	clip(t, filepath.Join(f.media, "Film (2020).mkv"))
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t), db.WithMaxConns(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.scanner.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := f.scanner.Scan(ctx, f.lib); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "Movie"); n != 1 {
		t.Fatalf("expected one movie, got %d", n)
	}
}
