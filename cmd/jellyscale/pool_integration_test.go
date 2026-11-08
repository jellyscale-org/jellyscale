//go:build integration

package main

import (
	"context"
	"testing"

	"jellyscale/internal/testenv"
)

// The configured size reaches the pool that every role shares. openDatabase is
// the name the task implies for the step of run() that opens it; it does not
// exist yet.
func TestDatabaseMaxConnsReachesThePool(t *testing.T) {
	ctx := context.Background()
	pool, err := openDatabase(ctx, testenv.DatabaseURL(t), config{DatabaseMaxConns: 12})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 12 {
		t.Errorf("pool max %d, configured 12", got)
	}
	left, err := openDatabase(ctx, testenv.DatabaseURL(t), config{})
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	if got := left.Config().MaxConns; got < 4 {
		t.Errorf("the default pool max %d is below the 4 that was found to limit", got)
	}
}
