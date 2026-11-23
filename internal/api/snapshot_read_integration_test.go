//go:build integration

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-158 (review of the first checks): the deadlock at newEnvOn's TRUNCATE (playback_integration_test.go:69, runs
// 44855263307 and 45041377896) needs no worker. A test that makes more than one env (decisionEnv once per case) has
// the first env's two replicas alive when the second env truncates: each has RoleStream, so Handler starts
// keepSnapshot, which every snapshotEvery reads refreshFilms (media_sources, then items through shown_items, then
// libraries) until its pool closes, and that pool closes only when the whole test ends. The read holds ACCESS SHARE
// on media_sources and items while it asks for libraries; `TRUNCATE libraries, users CASCADE` holds libraries and
// asks for items.
//
// The interleaving is forced with a lock of the test's own on libraries: the first env's read is in flight, holding
// what it was granted and waiting for that lock, and the second env's TRUNCATE is then issued and waits for it too.
// While the TRUNCATE waits, the read must not be running: the kit ends the earlier env's replicas (their reads and
// their pools) before it truncates. Which of the two Postgres would abort, and whether it aborts either, is down to
// the order the lock queue is served in, so the check looks at what is running at the moment the TRUNCATE waits and
// never hopes for a 40P01. The test lets its lock go as soon as it has looked.
//
// What the timings could hide: snapshotEvery is production's 5 s, because the read the check holds is the one the
// loop starts on its own; the wait for the TRUNCATE is for a state only the test's release can end.
//
// A one-line mutant of a right build that this fails every run: newEnvOn without the step that ends the earlier env's
// replicas (today's kit).

// snapshotRead is how refreshFilms' statement starts, anchored so that a statement that merely mentions it (a kit's
// terminate of the reader) is not taken for the read.
const snapshotRead = "SELECT m.id::text, m.item_id::text, m.store_key"

func TestAnEarlierEnvsSnapshotReadIsEndedBeforeTheNextEnvInTheSameTestTruncates(t *testing.T) {
	ctx := context.Background()
	admin, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	count := func(where string) int {
		return scalar[int](t, admin, `SELECT count(*)::int FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND state = 'active' AND `+where)
	}
	reads := func() int { return count(`query LIKE '` + snapshotRead + `%'`) }

	lock, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { lock.Rollback(context.Background()) }) } //nolint:errcheck
	t.Cleanup(release)                                                            // Before the envs, so after theirs.

	newEnv(t) // The first env, whose replicas read the snapshot until the test ends.
	if _, err := lock.Exec(ctx, "LOCK TABLE libraries IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, bound time.Duration, done func() bool) bool {
		for end := time.Now().Add(bound); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if done() {
				return true
			}
		}
		t.Errorf("timed out waiting for %s", what)
		return false
	}
	// The read the loop starts on its own, held by the lock above and holding what it was granted before libraries.
	if !waitFor("a replica's snapshot read to wait for the lock", 40*time.Second, func() bool { return reads() > 0 }) {
		return
	}
	if held := scalar[int](t, admin, `SELECT count(*)::int FROM pg_locks l JOIN pg_stat_activity a USING (pid)
		WHERE a.query LIKE '`+snapshotRead+`%' AND l.granted AND l.relation IN ('items'::regclass, 'media_sources'::regclass)`); held == 0 {
		t.Fatal("the read in flight holds no lock on items or media_sources, so it is not the read the deadlock needs")
	}

	// The second env's TRUNCATE waits for the lock too. When it does, look at what the first env's replicas are doing.
	var mu sync.Mutex
	var seen string
	go func() {
		defer release()
		end := time.Now().Add(60 * time.Second)
		for time.Now().Before(end) {
			var waiting int
			err := admin.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity
				WHERE state = 'active' AND wait_event_type = 'Lock' AND query LIKE 'TRUNCATE libraries, users CASCADE%'`).Scan(&waiting)
			if err == nil && waiting > 0 {
				var n int
				admin.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity WHERE state = 'active' AND query LIKE '`+snapshotRead+`%'`).Scan(&n) //nolint:errcheck
				mu.Lock()
				seen = map[bool]string{true: "ended", false: "running"}[n == 0]
				mu.Unlock()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		seen = "no TRUNCATE waited"
		mu.Unlock()
	}()
	newEnv(t)
	mu.Lock()
	defer mu.Unlock()
	switch seen {
	case "ended":
	case "running":
		t.Error("the second env's TRUNCATE waited for a lock while the first env's replica still ran its snapshot read, " +
			"which holds items and asks for libraries as the TRUNCATE holds libraries and asks for items: a deadlock, or a read that goes on under the TRUNCATE")
	default:
		t.Errorf("the second env's TRUNCATE was never seen waiting: %s", seen)
	}
}
