//go:build integration

package signin_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/signin"
	"jellyscale/internal/testenv"
)

// A name that failed while it was no account, and an account made after, keep their count through a flood past the
// bound: the name's signin_unknown row must not be forgotten, taking the account's failures with it. Two cases: one
// that failed again as an account (19 counted), one that failed only before the account existed (1 counted).
func TestANameThatFailedBeforeItsAccountExistedKeepsItsCountThroughAFlood(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // before clear's, which runs first
	clear(t, pool)
	t.Cleanup(func() { clear(t, pool) })

	fail := func(name string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := signin.FailName(ctx, pool, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	create := func(name string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, 'x', false)`, uuid.NewString(), name); err != nil {
			t.Fatal(err)
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
	a, b := "late-"+uuid.NewString()[:8], "late-"+uuid.NewString()[:8]
	fail(a, 1)
	fail(b, 1)
	create(a)
	create(b)
	fail(a, 18)

	flood(t, pool, signin.UnknownNames+20_000)

	fail(a, 1) // its 20th
	if d := refusal(a); d < time.Second || d > 900*time.Second {
		t.Errorf("a name at 20 failures, one before its account existed, is refused for %v after a flood, want 1 to 900 s", d)
	}
	fail(b, 18) // with the one before the account, 19
	if d := refusal(b); d != 0 {
		t.Errorf("a name at 19 failures is refused (%v)", d)
	}
	fail(b, 1)
	if d := refusal(b); d == 0 {
		t.Errorf("a name whose only failure before the flood was made before its account existed lost it: not refused at 20")
	}
}
