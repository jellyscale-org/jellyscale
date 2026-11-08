//go:build integration

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// GET /Sessions is polled by the dashboard, so looking up a job's media source
// must use media_sources' primary key. With sequential scans disallowed, a
// lookup by an expression of the id (replace(m.id::text, ...)) still plans one;
// a lookup by the key does not.
func TestSessionsLookUpTheJobsSourceByKey(t *testing.T) {
	pool, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(context.Background(), "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(context.Background(), "EXPLAIN "+listSessionsSQL, 600.0, true, uuid.NewString(), "")
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if text := strings.Join(plan, "\n"); strings.Contains(text, "Seq Scan on media_sources") {
		t.Fatalf("the sessions query scans media_sources:\n%s", text)
	}
}
