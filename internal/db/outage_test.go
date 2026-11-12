package db

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// What counts as Postgres being away: a connection that ended or could not be
// made, and Postgres's own shutdown (57P01 is what a switchover sends); a query
// the database answered, or a caller who gave up, is not.
func TestWhichErrorsMeanPostgresIsAway(t *testing.T) {
	for _, c := range []struct {
		err  error
		away bool
	}{
		{io.EOF, true},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{&pgconn.PgError{Severity: "FATAL", Code: "57P01"}, true},
		{&pgconn.PgError{Code: "57P03"}, true},
		{&pgconn.PgError{Code: "08006"}, true},
		{errors.New("conn closed"), true},
		{&pgconn.PgError{Code: "57014"}, false}, // A cancelled query.
		{&pgconn.PgError{Code: "23505"}, false}, // A unique violation.
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := IsAway(c.err); got != c.away {
			t.Errorf("IsAway(%v) = %v, want %v", c.err, got, c.away)
		}
	}
}

// A call that keeps failing for another reason is returned, not waited for, and
// one that fails once because Postgres is away is run again.
func TestRetryRunsAgainOnlyForAnOutage(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), nil, func() error {
		if calls++; calls == 1 {
			return io.EOF
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Errorf("after an outage: %d calls, %v", calls, err)
	}
	other := errors.New("syntax error")
	calls = 0
	if err := Retry(context.Background(), nil, func() error { calls++; return other }); err != other || calls != 1 {
		t.Errorf("another error: %d calls, %v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Retry(ctx, nil, func() error { return io.EOF }); !errors.Is(err, io.EOF) {
		t.Errorf("a request that was given up on: %v", err)
	}
}
