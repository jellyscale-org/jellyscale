package transcode

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
)

// Reap is reap as a process holding no lock would run it, for tests that need a
// reaper whose view of the cluster is not a worker's. No role runs it
// (TestNoReplicaCodePathForgetsAWorker reads the source for that).
func Reap(ctx context.Context, pool *pgxpool.Pool, self string) (workers []string, jobs []int64, here bool, err error) {
	return reap(ctx, pool, self, !db.Of(pool).Recent(liveTiming("hold")), nil)
}
