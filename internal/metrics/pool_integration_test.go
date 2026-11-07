//go:build integration

package metrics

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/testenv"
)

func seriesValue(t *testing.T, text, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			n, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no %s in:\n%s", series, text)
	return 0
}

// The pool's numbers are the pool's: a connection held shows as acquired, and
// the limit and the acquires are the ones it has.
func TestPoolReportsThePool(t *testing.T) {
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scrape := func() string {
		recorder := httptest.NewRecorder()
		Handler(Pool{Pool: pool}).ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
		return recorder.Body.String()
	}
	text := scrape()
	for series, want := range map[string]float64{
		`jellyscale_db_pool_connections{state="acquired"}`: 1,
		`jellyscale_db_pool_connections{state="idle"}`:     0,
		`jellyscale_db_pool_connections{state="total"}`:    1,
		`jellyscale_db_pool_max_connections`:               7,
		`jellyscale_db_pool_acquires_total`:                1,
	} {
		if got := seriesValue(t, text, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if got := seriesValue(t, text, `jellyscale_db_pool_acquire_wait_seconds_total`); got < 0 {
		t.Errorf("wait %v", got)
	}
	conn.Release()
	text = scrape()
	if got := seriesValue(t, text, `jellyscale_db_pool_connections{state="acquired"}`); got != 0 {
		t.Errorf("acquired %v after release", got)
	}
	if got := seriesValue(t, text, `jellyscale_db_pool_connections{state="idle"}`); got != 1 {
		t.Errorf("idle %v after release", got)
	}
}
