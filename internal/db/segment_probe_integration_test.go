//go:build integration

package db

import (
	"context"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// A waiting segment request asks on every poll whether any job of its spec
// recorded the segment. Done jobs stay a day and are not in the partial index
// of queued and running ones, so the probe needs an index of its own; with
// sequential scans off, a plan without one still shows as a Seq Scan.
func TestSegmentStoredProbeUsesAnIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for name, where := range map[string]string{
		"media segment": "start_segment <= 7 AND 7 < next_segment",
		"init segment":  "next_segment > start_segment",
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx, "EXPLAIN SELECT 1 FROM transcode_jobs WHERE spec_hash = 'abc' AND "+where)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		tx.Rollback(ctx) //nolint:errcheck
		if strings.Contains(plan.String(), "Seq Scan") || strings.Contains(plan.String(), "transcode_jobs_active_idx") {
			t.Errorf("%s probe scans the table or uses the partial index:\n%s", name, plan.String())
		}
	}
}
