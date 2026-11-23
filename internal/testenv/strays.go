package testenv

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// livenessClass is the first key of the advisory lock a worker holds for as long as it lives
// (internal/transcode/liveness.go; testenv cannot import it, the tests of that package import this one).
const livenessClass = "27219"

// strayBound is how long a worker or a session may take to go once the test's own cleanups have run: a backend
// whose client has closed its socket ends when it next reads, not at that instant.
const strayBound = 5 * time.Second

// NoStrays fails the test, as its last cleanup, for a worker of the test that still lives and for a Postgres
// session that the test opened and that still runs a statement or holds a transaction. Each kit that starts
// workers or servers calls it first, and so does NewDBProxy (whose own cleanup must end before the guard looks), so
// that it runs after every cleanup the kit and the test register, whichever code started the worker: the next test's
// TRUNCATE then never meets a write of this one (JS-158). Each failure is one message naming the worker or the
// session, and is reported at the line of the test that called the kit.
//
// A session is the test's when it began after the call, in the same database; a worker is found by its liveness
// lock, which is held by the process that runs it and by nothing else, taken after the call too. A database dsn
// does not reach, or one that is not the integration tier's Postgres (a stand-in process's fake address), has
// nothing to leave behind.
func NoStrays(t *testing.T, dsn string) {
	t.Helper()
	guard(t, dsn, true)
}

// NoWorkersLeft is NoStrays for a package whose tests run beside others that use the same server (cmd/jellyscale:
// tests that make and drop databases of their own, and run in parallel), where a session that runs a statement is as
// likely another test's: it holds the test to its workers only, which a liveness lock, in a database of its own,
// attributes.
func NoWorkersLeft(t *testing.T, dsn string) {
	t.Helper()
	guard(t, dsn, false)
}

func guard(t *testing.T, dsn string, sessions bool) {
	t.Helper()
	if !sameServer(dsn, os.Getenv("JELLYSCALE_TEST_DATABASE_URL")) {
		return
	}
	// Once for each test, at the first kit it calls: whatever the test registers after it ends first.
	if _, again := guarded.LoadOrStore(t, true); again {
		return
	}
	ctx := context.Background()
	// No connection is kept between the two: a session in the database would stop a test that copies it as a template.
	var since time.Time
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		guarded.Delete(t)
		return
	}
	err = conn.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&since)
	conn.Close(ctx)
	if err != nil {
		guarded.Delete(t)
		return
	}
	t.Cleanup(func() {
		t.Helper()
		defer guarded.Delete(t)
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return // A database that is gone has nothing left running.
		}
		defer conn.Close(ctx)
		var left []string
		for end := time.Now().Add(strayBound); ; time.Sleep(50 * time.Millisecond) {
			left = strays(ctx, conn, since, sessions)
			if len(left) == 0 || time.Now().After(end) {
				break
			}
		}
		for _, line := range left {
			t.Error(line)
		}
	})
}

// guarded is the tests that have their guard.
var guarded sync.Map

func sameServer(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host != "" && ua.Host == ub.Host
}

// strays says what of the test's is still alive, one line each.
func strays(ctx context.Context, conn *pgx.Conn, since time.Time, sessions bool) []string {
	var left []string
	// A node row whose liveness lock is held: its worker lives. The table is not there in a database no worker
	// ever opened.
	rows, err := conn.Query(ctx, `SELECT 'worker ' || n.id || ' still lives: its liveness lock is held and its node is registered'
		FROM transcode_nodes n WHERE n.locked AND EXISTS (SELECT 1 FROM pg_locks l JOIN pg_stat_activity a USING (pid)
			WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2 AND l.classid = `+livenessClass+`
				AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
				AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295 AND a.backend_start >= $1)`, since)
	if err == nil {
		left = append(left, collect(rows)...)
	}
	if !sessions {
		return left
	}
	rows, err = conn.Query(ctx, `SELECT 'session ' || pid || ' of this test (application_name "' || application_name || '") still ' ||
			CASE WHEN state = 'active' THEN 'runs: ' ELSE 'holds a transaction open: ' END || left(query, 120)
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend' AND backend_start >= $1
			AND state IN ('active', 'idle in transaction', 'idle in transaction (aborted)')`, since)
	if err != nil {
		return append(left, fmt.Sprintf("the sessions could not be read: %v", err))
	}
	return append(left, collect(rows)...)
}

func collect(rows pgx.Rows) []string {
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			lines = append(lines, line)
		}
	}
	return lines
}
