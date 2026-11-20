//go:build integration

// These checks name the package the build adds, jellyscale/internal/signin, and two functions of it that the sign-in
// handler calls; none exists yet, so this file does not compile today (the one failure of the check: `no non-test Go files`, from go test, or "no required module
// provides package" / "package jellyscale/internal/signin is not in std"). The signatures are the behaviour spec's:
//
//	// FailName counts one failed sign-in for the name (any letter case), real account or not, and stores it before it
//	// returns. It does no HTTP and no bcrypt.
//	func FailName(ctx context.Context, pool *pgxpool.Pool, name string) error
//
//	// NameRefusal is how long attempts for the name are refused from now (rounded up to whole seconds, 1 to 900), or 0.
//	func NameRefusal(ctx context.Context, pool *pgxpool.Pool, name string) (time.Duration, error)
package signin_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
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

func rows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	tables, err := pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'signin\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n) //nolint:errcheck
		names = append(names, n)
	}
	tables.Close()
	total := 0
	for _, n := range names {
		var c int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM "`+n+`"`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		total += c
	}
	return total
}

func clear(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	tables, _ := pool.Query(context.Background(), `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_name LIKE 'signin\_%' ESCAPE '\'`)
	var names []string
	for tables.Next() {
		var n string
		tables.Scan(&n) //nolint:errcheck
		names = append(names, n)
	}
	tables.Close()
	for _, n := range names {
		pool.Exec(context.Background(), `DELETE FROM "`+n+`"`) //nolint:errcheck
	}
}

// statedBound is the number of unknown names docs/access.md says are remembered ("100,000 unknown names").
func statedBound(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("../../docs/access.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?i)\b(\d[\d,]*)\s+unknown\s+names?`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("docs/access.md states no number of unknown names that are remembered (such as `100,000 unknown names`)")
	}
	n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	return n
}

// flood fails n invented names once each, from 16 goroutines.
func flood(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	next, failed := 0, error(nil)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= n {
					return
				}
				if err := signin.FailName(context.Background(), pool, fmt.Sprintf("flood-%s-%d", uuid.NewString()[:8], i)); err != nil {
					mu.Lock()
					failed = err
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if failed != nil {
		t.Fatal(failed)
	}
}

// A flood of invented names (100,000, each failing once) neither changes nor drops a real account's count, nor makes an
// unknown name answer differently from a real one at the same count; past the stated bound what is stored for unknown names
// stops growing and real accounts' counts stay exact.
func TestAFloodOfInventedNamesChangesNoRealAccountsCountAndIsStoredWithinABound(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
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

	flood(t, pool, 100_000)

	// 19 failures plus one more: the first two answer 401 once more (not refused until the 20th is counted), then refuse.
	for _, name := range []string{real19, fake19} {
		if d := refusal(name); d != 0 {
			t.Errorf("%s was at 19 failures and is refused (%v) after the flood", name, d)
		}
		fail(name, 1)
		if d := refusal(name); d < time.Second || d > 900*time.Second {
			t.Errorf("%s after its 20th failure: refused for %v, want 1 to 900 s", name, d)
		}
	}
	for _, name := range []string{real20, fake20} {
		if d := refusal(name); d < time.Second || d > 900*time.Second {
			t.Errorf("%s was refused before the flood and is refused for %v after it, want 1 to 900 s", name, d)
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

	// Past the stated bound the storage stops growing.
	bound := statedBound(t)
	if bound < 100_000 || bound > 150_000 {
		t.Fatalf("docs/access.md remembers %d unknown names: this check floods %d past it in a minute, and the bound must hold at least 100,000; ask the lead if the bound is meant to be larger than 150,000", bound, 20_000)
	}
	flood(t, pool, bound-100_000+2_000)
	before := rows(t, pool)
	flood(t, pool, 20_000)
	if after := rows(t, pool); after > before {
		t.Errorf("the signin_* tables hold %d rows after %d more unknown names past the bound of %d, were %d: what is stored for unknown names must stop growing", after, 20_000, bound, before)
	}
	// Real accounts' counts stay exact: 19 failures made before the flood, then one more.
	if d := refusal(kept); d != 0 {
		t.Errorf("a real account at 19 failures is refused (%v) after the flood past the bound", d)
	}
	fail(kept, 1)
	if refusal(kept) == 0 {
		t.Errorf("a real account whose 20th failure came after the flood past the bound is not refused: its count was dropped")
	}
	if refusal(real20) == 0 {
		t.Errorf("a real account's refusal was dropped by the flood past the bound")
	}
}
