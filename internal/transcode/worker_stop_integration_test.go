//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-158: a worker an integration test started is stopped, with every write it had in flight ended, before the
// test's cleanups finish, so the next test's kit setup (a TRUNCATE that needs an ACCESS EXCLUSIVE lock on the tables
// the worker writes) never meets it. A write the worker made is held here in Postgres itself: a trigger on the table
// sleeps for heldFor in that one statement, so the statement shows in pg_stat_activity as active, carries its own text
// and holds its row lock, until the test or the worker ends it. The first "test" is a subtest, so that its cleanups
// have run when the parent looks, as they have when the next test starts.
//
// What the timings could hide: heldFor is shorter than production's longest wait for a write (a lease, a store), so
// a build that waits for a write instead of ending it passes here as it should; the heartbeat is 5 s so that the
// lease keeper's own bound (half a heartbeat) is far longer than the teardown the check races, which is what keeps
// the check failing on every run on a build that leaves the keeper's renewal behind, not by chance.

const heldFor = 8 * time.Second

// holdWrites makes every statement that fires the trigger sleep for heldFor. The function and the trigger are made
// before the first rig, which records the database's triggers and functions and fails the test that leaves one, and
// are removed in a cleanup registered before they are made.
func holdWrites(t *testing.T, admin *pgxpool.Pool, table, events, when string) {
	t.Helper()
	remove := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*heldFor)
		defer cancel()
		admin.Exec(ctx, "DROP TRIGGER IF EXISTS js158_hold ON "+table) //nolint:errcheck
		admin.Exec(ctx, "DROP FUNCTION IF EXISTS js158_hold()")        //nolint:errcheck
	}
	t.Cleanup(remove)
	for _, sql := range []string{
		fmt.Sprintf("CREATE OR REPLACE FUNCTION js158_hold() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(%d); RETURN NEW; END $$", int(heldFor.Seconds())),
		fmt.Sprintf("CREATE TRIGGER js158_hold BEFORE %s ON %s FOR EACH ROW %s EXECUTE FUNCTION js158_hold()", events, table, when),
	} {
		if _, err := admin.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

// inFlight says whether a session of that application_name is running a statement with that text now.
func inFlight(admin *pgxpool.Pool, app, statement string) bool {
	var n int
	admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND state = 'active' AND pid <> pg_backend_pid() AND query ILIKE '%' || $2 || '%'`, app, statement).Scan(&n) //nolint:errcheck
	return n > 0
}

// leftRunning lists what the sessions of that application_name are doing: a statement, or a transaction held open. A
// session that is idle holds nothing and is not listed.
func leftRunning(admin *pgxpool.Pool, app string) []string {
	rows, err := admin.Query(context.Background(), `SELECT pid || ' ' || state || ': ' || left(query, 120) FROM pg_stat_activity
		WHERE application_name = $1 AND pid <> pg_backend_pid() AND state IN ('active', 'idle in transaction', 'idle in transaction (aborted)')`, app)
	if err != nil {
		return []string{err.Error()}
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			found = append(found, line)
		}
	}
	return found
}

func slowWorker(w *Worker) { w.Heartbeat, w.Lease = 5*time.Second, time.Minute }

// heldWriter is one of the writes a worker makes beside its jobs.
type heldWriter struct {
	name      string
	table     string
	events    string
	when      string // The trigger's condition, so that only this write is held.
	statement string // Text of the held statement in pg_stat_activity.
	job       string // "" none, "playing" a viewer plays the film, "past the end" a job with nothing to make.
}

var heldWriters = []heldWriter{
	{name: "the segment record", table: "transcode_jobs", events: "UPDATE", job: "playing", statement: "SET next_segment",
		when: "WHEN (OLD.state = 'running' AND NEW.state = 'running' AND NEW.next_segment IS DISTINCT FROM OLD.next_segment)"},
	{name: "the lease renewal on the keeper's own connection", table: "transcode_jobs", events: "UPDATE", job: "playing", statement: "SET lease_expires_at = now()",
		when: "WHEN (OLD.state = 'running' AND NEW.state = 'running' AND NEW.next_segment = OLD.next_segment AND NEW.lease_expires_at IS DISTINCT FROM OLD.lease_expires_at)"},
	{name: "the job end record", table: "transcode_jobs", events: "UPDATE", job: "past the end", statement: "ended_at = now()",
		when: "WHEN (OLD.state = 'running' AND NEW.state IN ('done', 'failed', 'cancelled'))"},
	{name: "the sweep loop", table: "sweeps", events: "INSERT OR UPDATE", statement: "INSERT INTO sweeps"},
	{name: "the heartbeat", table: "transcode_nodes", events: "UPDATE", statement: "UPDATE transcode_nodes SET seen_at",
		when: "WHEN (NEW.seen_at IS DISTINCT FROM OLD.seen_at)"},
}

// startJob makes the worker have a job whose writes the trigger can hold.
func (w heldWriter) startJob(t *testing.T, r *rig, j *Jobs) {
	t.Helper()
	switch w.job {
	case "playing":
		r.playInBackground(t, j, "js158")
	case "past the end":
		raw, err := json.Marshal(r.spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version)
			VALUES ($1, $2, 'viewer', 100000, 100000, $3)`, r.hash, raw, playback.OutputVersion); err != nil {
			t.Fatal(err)
		}
		Notify(context.Background(), r.pool, jobHint)
	}
}

func TestAWorkerAWriteOfWhichIsInFlightWhenItsTestEndsIsStoppedWithItBeforeTheNextKitTruncates(t *testing.T) {
	admin, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	for _, w := range heldWriters {
		t.Run(w.name, func(t *testing.T) {
			holdWrites(t, admin, w.table, w.events, w.when)
			app := "js158-writer"
			ok := t.Run("the test that starts the worker", func(t *testing.T) {
				if w.job == "playing" {
					slow(t) // So the job is still running when the test ends.
				}
				r := newRig(t, nil)
				j := r.jobs(t)
				r.dsn = withApplicationName(t, r.dsn, app)
				r.worker(t, slowWorker)
				w.startJob(t, r, j)
				waitFor(t, w.name+" of the worker to be in flight in Postgres", 40*time.Second, func() bool { return inFlight(admin, app, w.statement) })
			})
			if !ok {
				return
			}
			// The test has ended, its cleanups with it. What a worker wrote must not be running now.
			if left := leftRunning(admin, app); len(left) > 0 {
				t.Fatalf("the test ended while %s of its worker was still running in Postgres, so the next test's TRUNCATE would meet it:\n%s", w.name, strings.Join(left, "\n"))
			}
			t.Run("the next test's kit setup", func(t *testing.T) { newRig(t, nil) })
		})
	}
}

// A test with several workers has every one stopped, and one stopped early in the test is stopped when its stop returns.
func TestWorkersStartedByOneTestAreAllStoppedWhetherStoppedEarlyOrAtTheEnd(t *testing.T) {
	admin, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	w := heldWriters[1] // The renewal, which only the keeper's own connection makes.
	holdWrites(t, admin, w.table, w.events, w.when)
	ok := t.Run("the test that starts two workers", func(t *testing.T) {
		slow(t)
		r := newRig(t, nil)
		j := r.jobs(t)
		r.dsn = withApplicationName(t, r.dsn, "js158-first")
		stopFirst := r.worker(t, slowWorker)
		w.startJob(t, r, j)
		waitFor(t, "the first worker's renewal to be in flight", 40*time.Second, func() bool { return inFlight(admin, "js158-first", w.statement) })
		r.dsn = withApplicationName(t, r.dsn, "js158-second")
		r.worker(t, slowWorker)
		stopFirst()
		if left := leftRunning(admin, "js158-first"); len(left) > 0 {
			t.Fatalf("the first worker was stopped, and its stop returned, while it still ran in Postgres:\n%s", strings.Join(left, "\n"))
		}
	})
	if !ok {
		return
	}
	for _, app := range []string{"js158-first", "js158-second"} {
		if left := leftRunning(admin, app); len(left) > 0 {
			t.Errorf("the test ended while %s ran in Postgres:\n%s", app, strings.Join(left, "\n"))
		}
	}
}

// A worker process the test killed is gone from Postgres too: the backend of a killed client goes on with the
// statement it was running until it next speaks to the client, which is the same write meeting the next TRUNCATE.
func TestAWorkerProcessKilledWithAWriteInFlightHasNoSessionRunningWhenItsTestEnds(t *testing.T) {
	admin, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	w := heldWriters[4] // The heartbeat: a process makes it from its first moment.
	holdWrites(t, admin, w.table, w.events, w.when)
	app := "js158-process"
	ok := t.Run("the test that starts the process", func(t *testing.T) {
		r := newRig(t, nil)
		r.workerProcess(t, "js158-process-worker", t.TempDir(), "JELLYSCALE_TEST_DATABASE_URL="+withApplicationName(t, r.dsn, app))
		waitFor(t, "the process's heartbeat to be in flight", 40*time.Second, func() bool { return inFlight(admin, app, w.statement) })
	})
	if !ok {
		return
	}
	if left := leftRunning(admin, app); len(left) > 0 {
		t.Fatalf("the test ended and its worker process was killed, but its session still ran in Postgres:\n%s", strings.Join(left, "\n"))
	}
}

// BL-178: run 45049599358 deadlocked at s.fresh(t) of the image-store kit while the test's own worker was alive. fresh
// forgets the nodes, and a worker whose row is forgotten is taken for dead and its job run again. After fresh returns
// the test has no live worker: its sessions are gone, and fresh did not meet them.
func TestTheImageStoreKitsFreshNeverRunsUnderALiveWorkerOfItsTest(t *testing.T) {
	admin, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	w := heldWriters[4]
	holdWrites(t, admin, w.table, w.events, w.when)
	app := "js158-fresh"
	s := newScanned(t, film(t), t.TempDir())
	s.rig.dsn = withApplicationName(t, s.rig.dsn, app)
	s.roleWorker(t, "transcode", newMeters(s, 0), slowWorker)
	waitFor(t, "the worker's heartbeat to be in flight", 40*time.Second, func() bool { return inFlight(admin, app, w.statement) })
	// The sessions the worker had before fresh: those must be gone when fresh returns. A kit may start the test's
	// worker again after the TRUNCATE (the test goes on using it); the new worker's sessions are not these.
	var before []int32
	if err := admin.QueryRow(context.Background(), "SELECT coalesce(array_agg(pid), '{}') FROM pg_stat_activity WHERE application_name = $1 AND pid <> pg_backend_pid()", app).Scan(&before); err != nil {
		t.Fatal(err)
	}
	s.fresh(t)
	var sessions int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1)", before).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions > 0 {
		t.Errorf("fresh forgot the nodes while the test's own worker still had %d of its %d sessions in Postgres: the worker is taken for dead and its job run again", sessions, len(before))
	}
}
