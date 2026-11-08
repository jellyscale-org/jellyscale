//go:build integration

package db

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/testenv"
)

// WithMaxConns and DefaultMaxConns are the names the task implies; Open takes
// options after the DSN, so its callers do not change. They do not compile yet.

// The pool opens as many connections as it is allowed to and no more, as
// Postgres sees them: three times as many callers as the limit each hold a
// connection for a second.
func TestWithMaxConnsLimitsTheConnectionsPostgresSees(t *testing.T) {
	ctx := context.Background()
	watcher, err := pgx.Connect(ctx, testenv.DatabaseURL(t)) // Apart from the pool, which is full.
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close(ctx)
	for _, max := range []int32{3, 12} {
		pool, err := Open(ctx, testenv.DatabaseURL(t), WithMaxConns(max))
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.Config().MaxConns; got != max {
			t.Errorf("pool max %d, asked for %d", got, max)
		}
		var wg sync.WaitGroup
		for i := 0; i < int(max)*3; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); pool.Exec(ctx, "SELECT pg_sleep(1.5)") }() //nolint:errcheck
		}
		busy, peak := 0, 0
		for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if err := watcher.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'SELECT pg_sleep(1.5)%' AND state = 'active'").Scan(&busy); err != nil {
				t.Fatal(err)
			}
			peak = max2(peak, busy)
		}
		wg.Wait()
		pool.Close()
		if peak != int(max) {
			t.Errorf("max %d: Postgres saw at most %d connections busy", max, peak)
		}
	}
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func TestOpenWithoutAnOptionUsesTheDefault(t *testing.T) {
	pool, err := Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != DefaultMaxConns {
		t.Errorf("pool max %d, DefaultMaxConns %d", got, DefaultMaxConns)
	}
}
