//go:build integration

package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/testenv"
)

// The stress driver's fake workers hold, as real ones do, a connection for the
// liveness lock and one for the lease keeper (JS-105), so the stress table's
// connection counts stay true (ARCHITECTURE.md, Control-plane ceiling). With the
// fleet up and no load yet, the backends Postgres gained are the pool's and the
// hint listener's (a few) and the workers': 10 workers hold 10 with the lock's
// alone, which is under 20, and 20 with a keeper's as well.
func TestTheStressDriverHoldsTwoConnectionsPerFakeWorker(t *testing.T) {
	ctx := context.Background()
	dsn := testenv.DatabaseURL(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) }) //nolint:errcheck
	backends := func() int {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const workers = 10
	before := backends()
	var gained int
	if _, _, err := runStress(ctx, t, dsn, stressConfig{Viewers: 2, Workers: workers, Specs: 2, Pool: 2, Duration: 2 * time.Second,
		AfterSetup: func(context.Context) { gained = backends() - before }}); err != nil {
		t.Fatal(err)
	}
	if gained < 2*workers {
		t.Errorf("Postgres gained %d connections for %d fake workers beside a pool of 2, want at least %d: the fake workers do not hold a keeper's connection beside the lock's", gained, workers, 2*workers)
	}
}
