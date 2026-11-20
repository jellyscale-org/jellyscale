//go:build integration

package signin_test

import (
	"context"
	"testing"

	"jellyscale/internal/db"
	"jellyscale/internal/signin"
	"jellyscale/internal/testenv"
)

// The sign-in handler counts through Begin, so what it stores for a name that is no account is checked there: the name
// is entered in signin_unknown (the table the stated bound holds), a real account's is not.
func TestBeginRemembersAnUnknownNameAndNoRealAccount(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	clear(t, pool)
	defer clear(t, pool)

	if _, _, err := signin.Begin(ctx, pool, signin.Attempt{Name: "Nobody-Here"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := signin.Begin(ctx, pool, signin.Attempt{Name: "nobody-here"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := signin.Begin(ctx, pool, signin.Attempt{Name: "real", UserID: "00000000-0000-0000-0000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	var names, failures int
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(failures), 0) FROM signin_unknown`).Scan(&names, &failures); err != nil {
		t.Fatal(err)
	}
	if names != 1 || failures != 2 {
		t.Errorf("signin_unknown holds %d names with %d failures, want the one invented name (any letter case) with 2", names, failures)
	}
}
