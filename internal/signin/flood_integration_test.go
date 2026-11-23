//go:build integration

// JS-172 (D-050): what is stored for unknown names has a ceiling no flood raises. Writers that fail invented names at once
// can each see a count that lacks the others' commits, so signin_unknown may hold more than the bound while they run and
// when they stop, by no more than the sign-ins in flight at once; the next name that fails alone forgets the excess. Every
// count below is read once and judged as read: nothing is re-counted or waited for until it fits, except the one sign-in
// made alone that the rule names.
package signin_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/signin"
	"jellyscale/internal/testenv"
)

const (
	writers = 16 // Sign-ins in flight at once: a replica's databaseMaxConns (16 by default) bounds its own.
	// stripeRows is what signin_unknown_count holds: one row for each of the 32 stripes the count is spread over. A build
	// that changes it changes this line and says so in docs/testing.md.
	stripeRows = 32
	// countWindow is how long a failure counts toward a name and a refusal lasts (docs/access.md: 15 minutes).
	countWindow = 15 * time.Minute
)

// TestMain makes this binary a flood writer when a test starts it as one: the two-process check needs a second process
// with a pool of its own, as a second api replica has.
func TestMain(m *testing.M) {
	if spec := os.Getenv("JELLYSCALE_FLOOD_WRITER"); spec != "" {
		var names, goroutines int
		fmt.Sscanf(spec, "%d,%d", &names, &goroutines) //nolint:errcheck
		pool, err := db.Open(context.Background(), os.Getenv("JELLYSCALE_TEST_DATABASE_URL"))
		if err == nil {
			err = writeNames(pool, names, goroutines)
			pool.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "flood writer:", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

var signinTables = []string{"signin_unknown", "signin_failures", "signin_unknown_count", "signin_device_ended"}

// tables is the number of rows of each signin_* table.
func tables(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, name := range signinTables {
		var c int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+name).Scan(&c); err != nil {
			t.Fatal(err)
		}
		got[name] = c
	}
	return got
}

// counted is what the stripes of signin_unknown_count add up to, which is how many names a build believes it remembers.
func counted(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(sum(n), 0)::int FROM signin_unknown_count`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// orphans is the rows of signin_failures for a name that is neither an account nor in signin_unknown: failures of a
// forgotten name, which go with it.
func orphans(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var c int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM signin_failures f WHERE f.kind = 'name'
		AND NOT EXISTS (SELECT 1 FROM signin_unknown u WHERE u.key = f.key)
		AND f.key NOT IN (SELECT encode(sha256(convert_to(lower(name), 'UTF8')), 'hex') FROM users)`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func clear(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, n := range signinTables {
		if _, err := pool.Exec(context.Background(), `DELETE FROM `+n); err != nil {
			t.Fatal(err)
		}
	}
}

// writeNames fails n invented names once each, from the given number of goroutines.
func writeNames(pool *pgxpool.Pool, n, goroutines int) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	next, failed := 0, error(nil)
	prefix := uuid.NewString()[:8]
	for w := 0; w < goroutines; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				stop := failed != nil
				mu.Unlock()
				if i >= n || stop {
					return
				}
				if err := signin.FailName(context.Background(), pool, fmt.Sprintf("flood-%s-%d", prefix, i)); err != nil {
					mu.Lock()
					failed = err
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return failed
}

// floodCounted fails n invented names, with 16 sign-ins in flight at once: from 16 goroutines of this process, or from 8 of it
// and 8 of a second process with a pool of its own. Meanwhile a pool of its own counts signin_unknown as often as it
// can; the counts are returned.
func floodCounted(t *testing.T, pool *pgxpool.Pool, n, processes int) []int {
	t.Helper()
	ctx := context.Background()
	sampler, err := pgxpool.New(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	var counts []int
	var sampleErr error
	stop, sampled := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var c int
			if sampleErr = sampler.QueryRow(ctx, `SELECT count(*) FROM signin_unknown`).Scan(&c); sampleErr != nil {
				return
			}
			counts = append(counts, c)
		}
	}()
	var other *exec.Cmd
	var otherOut strings.Builder
	mine := n
	if processes == 2 {
		mine = n / 2
		other = exec.Command(os.Args[0])
		other.Env = append(os.Environ(), fmt.Sprintf("JELLYSCALE_FLOOD_WRITER=%d,%d", n-mine, writers/2))
		other.Stdout, other.Stderr = &otherOut, &otherOut
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
	}
	mineErr := writeNames(pool, mine, writers/processes)
	var otherErr error
	if other != nil {
		otherErr = other.Wait()
	}
	close(stop)
	<-sampled
	if mineErr != nil || otherErr != nil || sampleErr != nil {
		t.Fatalf("flood: %v, other process: %v %s, counting: %v", mineErr, otherErr, otherOut.String(), sampleErr)
	}
	return counts
}

// flood fails n invented names once each, from 16 goroutines.
func flood(t *testing.T, pool *pgxpool.Pool, n int) { floodCounted(t, pool, n, 1) }

// floods is how many floods of 20,000 names are made past the bound: at least two, which is what a build is judged on;
// JELLYSCALE_FLOODS=10 makes the ten that docs/testing.md records.
func floods(t *testing.T) int {
	n := 2
	if v := os.Getenv("JELLYSCALE_FLOODS"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 2 {
			t.Fatalf("JELLYSCALE_FLOODS=%q: want a number, 2 or more", v)
		}
	}
	return n
}

// tablesHoldTheRule judges what a flood left: the ceiling on signin_unknown while it ran and at rest, and for every table
// what is stored once it has stopped.
func tablesHoldTheRule(t *testing.T, pool *pgxpool.Pool, when string, counts []int, ended int, bound int) {
	t.Helper()
	ceiling := bound + writers
	got := tables(t, pool)
	lo, hi := 0, 0
	if len(counts) > 0 {
		lo, hi = counts[0], counts[0]
	}
	for _, c := range counts {
		lo, hi = min(lo, c), max(hi, c)
	}
	t.Logf("OBSERVED %s: signin_unknown_count sums to %d; signin_unknown %d, signin_failures %d, signin_unknown_count %d, signin_device_ended %d rows at rest; while the writers ran signin_unknown was counted %d times, lowest %d, highest %d; signin_failures rows for forgotten names %d",
		when, counted(t, pool), got["signin_unknown"], got["signin_failures"], got["signin_unknown_count"], got["signin_device_ended"], len(counts), lo, hi, orphans(t, pool))
	if len(counts) < 10 {
		t.Errorf("%s: signin_unknown was counted %d times while the writers ran, want at least 10", when, len(counts))
	}
	if len(counts) > 0 && hi > ceiling {
		t.Errorf("%s: signin_unknown held %d rows while the writers ran, the ceiling is %d (the bound %d and %d sign-ins in flight)", when, hi, ceiling, bound, writers)
	}
	if got["signin_unknown"] > ceiling {
		t.Errorf("%s: signin_unknown holds %d rows at rest, the ceiling is %d (the bound %d and %d sign-ins in flight)", when, got["signin_unknown"], ceiling, bound, writers)
	}
	if c := counted(t, pool); c != got["signin_unknown"] {
		t.Errorf("%s: signin_unknown holds %d rows and signin_unknown_count adds up to %d: names that are remembered and not counted are never forgotten", when, got["signin_unknown"], c)
	}
	if o := orphans(t, pool); o != 0 {
		t.Errorf("%s: signin_failures holds %d rows for names that are neither an account nor in signin_unknown, want 0: a forgotten name's failures go with it", when, o)
	}
	if got["signin_unknown_count"] != stripeRows {
		t.Errorf("%s: signin_unknown_count holds %d rows, want its fixed %d", when, got["signin_unknown_count"], stripeRows)
	}
	if got["signin_device_ended"] != ended {
		t.Errorf("%s: signin_device_ended holds %d rows, held %d before the floods", when, got["signin_device_ended"], ended)
	}
}

// floodsHoldTheRule fills the store to the stated bound and then makes floods of 20,000 invented names, each failing once,
// with 16 sign-ins in flight at once (processes says in how many processes). Each flood's ceiling is judged while the
// writers run and at rest after it; the next invented name to fail alone must then bring signin_unknown to the bound itself,
// so that no excess passes from one flood to the next. A build that leaves one name per flood that is neither counted nor
// forgotten fails at the first flood; one that leaves a forgotten name's failures behind fails at the first; one with no cap
// fails while the fill runs.
func floodsHoldTheRule(t *testing.T, pool *pgxpool.Pool, processes int) {
	t.Helper()
	ctx := context.Background()
	bound := signin.UnknownNames
	if bound < 100_000 || bound > 150_000 {
		t.Fatalf("signin.UnknownNames is %d: this check floods 20,000 past it, and the bound must hold at least 100,000; ask the lead if the bound is meant to be larger than 150,000", bound)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO signin_device_ended (user_id, device_id, ended_at) VALUES ($1, 'flood-device', clock_timestamp())`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	ended := tables(t, pool)["signin_device_ended"]
	tablesHoldTheRule(t, pool, "the fill to the bound", floodCounted(t, pool, bound, processes), ended, bound)
	for k := 1; k <= floods(t); k++ {
		counts := floodCounted(t, pool, 20_000, processes)
		tablesHoldTheRule(t, pool, fmt.Sprintf("flood %d at rest", k), counts, ended, bound)
		// One more invented name fails alone, with nothing else in flight: whatever the flood left over the bound is
		// forgotten by it, so nothing carries over to the next flood.
		if err := signin.FailName(ctx, pool, "alone-"+uuid.NewString()[:8]); err != nil {
			t.Fatal(err)
		}
		got := tables(t, pool)
		t.Logf("OBSERVED flood %d after one name failed alone: signin_unknown_count sums to %d; signin_unknown %d, signin_failures %d", k, counted(t, pool), got["signin_unknown"], got["signin_failures"])
		if c := counted(t, pool); c != got["signin_unknown"] {
			t.Errorf("flood %d: after one more invented name failed alone, signin_unknown holds %d rows and signin_unknown_count adds up to %d: names that are remembered and not counted are never forgotten", k, got["signin_unknown"], c)
		}
		if got["signin_unknown"] > bound {
			t.Errorf("flood %d: after one more invented name failed alone, signin_unknown holds %d rows, want at most the bound %d: the excess is carried over to the next flood", k, got["signin_unknown"], bound)
		}
		if o := orphans(t, pool); o != 0 {
			t.Errorf("flood %d: after one more invented name failed alone, signin_failures holds %d rows for names that are neither an account nor in signin_unknown, want 0", k, o)
		}
	}
}

// A flood of invented names neither changes nor drops a real account's count, nor makes an unknown name answer differently
// from a real one at the same count; past the stated bound what is stored for unknown names holds the rule
// (floodsHoldTheRule) and real accounts' counts stay exact (JS-145 criterion 8, D-050).
func TestAFloodOfInventedNamesChangesNoRealAccountsCountAndIsStoredWithinABound(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // before clear's, which runs first
	clear(t, pool)
	t.Cleanup(func() { clear(t, pool) })

	account := func(prefix string) string {
		name := prefix + "-" + uuid.NewString()[:8]
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, 'x', false)`, uuid.NewString(), name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	fail := func(name string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := signin.FailName(ctx, pool, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	refusal := func(name string) time.Duration {
		t.Helper()
		d, err := signin.NameRefusal(ctx, pool, name)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// Every failure this relies on is made within one 15-minute window: the first here, and the last count read at the end.
	began := time.Now()
	real19, real20, kept := account("real"), account("real"), account("real")
	fake19, fake20 := "invented-"+uuid.NewString()[:8], "invented-"+uuid.NewString()[:8]
	fail(real19, 19)
	fail(fake19, 19)
	fail(kept, 19)
	fail(real20, 20)
	fail(fake20, 20)
	if refusal(real19) != 0 || refusal(fake19) != 0 || refusal(real20) == 0 || refusal(fake20) == 0 {
		t.Fatalf("before the flood: refusals %v %v %v %v, want 0 0 >0 >0", refusal(real19), refusal(fake19), refusal(real20), refusal(fake20))
	}

	floodsHoldTheRule(t, pool, 1)

	// 19 failures plus one more: the first two answer 401 once more (not refused until the 20th is counted), then refuse.
	for _, name := range []string{real19, fake19} {
		if d := refusal(name); d != 0 {
			t.Errorf("%s was at 19 failures and is refused (%v) after the floods", name, d)
		}
		fail(name, 1)
		if d := refusal(name); d < time.Second || d > 900*time.Second {
			t.Errorf("%s after its 20th failure: refused for %v, want 1 to 900 s", name, d)
		}
	}
	for _, name := range []string{real20, fake20} {
		if d := refusal(name); d < time.Second || d > 900*time.Second {
			t.Errorf("%s was refused before the floods and is refused for %v after them, want 1 to 900 s", name, d)
		}
	}
	// A new invented name is a new account: 19 failures do not refuse it, the 20th does.
	fresh := "invented-" + uuid.NewString()[:8]
	if refusal(fresh) != 0 {
		t.Errorf("a new invented name is refused")
	}
	fail(fresh, 19)
	if refusal(fresh) != 0 {
		t.Errorf("a new invented name is refused at 19 failures")
	}
	fail(fresh, 1)
	if refusal(fresh) == 0 {
		t.Errorf("a new invented name is not refused at 20 failures")
	}
	// Real accounts' counts stay exact: 19 failures made before the floods, then one more.
	if d := refusal(kept); d != 0 {
		t.Errorf("a real account at 19 failures is refused (%v) after the floods", d)
	}
	fail(kept, 1)
	if d := refusal(kept); d < time.Second || d > 900*time.Second {
		t.Errorf("a real account whose 20th failure came after the floods is refused for %v, want 1 to 900 s: its count was dropped", d)
	}
	// A count is read as it stands: the failures above age out at countWindow, and this says how far the run had got, so that a
	// failure of the counts after a long run is read for what it is.
	t.Logf("the last count was read %v after the first failure it relies on was made; they age out at %v", time.Since(began).Round(time.Second), countWindow)
}

// The same rule when the 16 sign-ins in flight come from two processes at once, each with a pool of its own (two api
// replicas): 8 writers here and 8 in a second process.
func TestAFloodFromTwoProcessesHoldsTheSameCeilingAndCarriesNothingOver(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // before clear's, which runs first
	clear(t, pool)
	t.Cleanup(func() { clear(t, pool) })
	// Two names that failed before the fill, as the check above has, so that the fill crosses the bound: from a store
	// that holds exactly the bound a flood adds and forgets in step, which cannot show an overshoot.
	for i := 0; i < 2; i++ {
		if err := signin.FailName(ctx, pool, "invented-"+uuid.NewString()[:8]); err != nil {
			t.Fatal(err)
		}
	}
	floodsHoldTheRule(t, pool, 2)
}

// The rule's worst case, set up rather than waited for: the store as 16 sign-ins in flight at once can leave it, each
// having counted without the others' commits, 15 names over the bound (16 entered, 1 forgotten). The next invented name
// to fail alone must bring it back to the bound. A flood leaves an excess only when the race happens to, so the floods
// above may never judge the carry-over; this does every run (D-050). The excess is written directly, as the race
// leaves it (a name, its one failure and its count each), because a race forced through a lock does not leave one every
// run on CI's runners (queue run 45086436037: 99,967 after 16 released together).
func TestAnExcessLeftBySignInsInFlightIsForgottenByTheNextOneAlone(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // before clear's, which runs first
	clear(t, pool)
	t.Cleanup(func() { clear(t, pool) })
	bound := signin.UnknownNames

	// Below the bound nothing is forgotten, so the fill leaves exactly bound-1 names, however its writers interleave.
	if err := writeNames(pool, bound-1, writers); err != nil {
		t.Fatal(err)
	}
	if n := tables(t, pool)["signin_unknown"]; n != bound-1 {
		t.Fatalf("the fill left signin_unknown at %d, want %d", n, bound-1)
	}
	excess := writers - 1
	if _, err := pool.Exec(ctx, `WITH names AS (
			SELECT encode(sha256(convert_to('in-flight-' || g, 'UTF8')), 'hex') AS key FROM generate_series(1, $1::int) g),
		entered AS (INSERT INTO signin_unknown (key, failures, last_at) SELECT key, 1, clock_timestamp() FROM names RETURNING key),
		failed AS (INSERT INTO signin_failures (kind, key) SELECT $2, key FROM entered)
		UPDATE signin_unknown_count SET n = n + $1 WHERE stripe = (SELECT min(stripe) FROM signin_unknown_count)`,
		excess+1, signin.BoundName); err != nil {
		t.Fatal(err)
	}
	got := tables(t, pool)
	if got["signin_unknown"] != bound+excess || counted(t, pool) != bound+excess {
		t.Fatalf("harness: the store holds %d names counted as %d, want %d for both", got["signin_unknown"], counted(t, pool), bound+excess)
	}

	if err := signin.FailName(ctx, pool, "alone-"+uuid.NewString()[:8]); err != nil {
		t.Fatal(err)
	}
	got = tables(t, pool)
	t.Logf("OBSERVED from %d names, after one name failed alone: signin_unknown %d, signin_unknown_count sums to %d", bound+excess, got["signin_unknown"], counted(t, pool))
	if got["signin_unknown"] > bound {
		t.Errorf("after one more invented name failed alone, signin_unknown holds %d rows, want at most the bound %d: the excess is carried over", got["signin_unknown"], bound)
	}
	if c := counted(t, pool); c != got["signin_unknown"] {
		t.Errorf("after one more invented name failed alone, signin_unknown holds %d rows and signin_unknown_count adds up to %d", got["signin_unknown"], c)
	}
	if o := orphans(t, pool); o != 0 {
		t.Errorf("after one more invented name failed alone, signin_failures holds %d rows for names that are neither an account nor in signin_unknown, want 0", o)
	}
}
