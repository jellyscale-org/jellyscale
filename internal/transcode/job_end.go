package transcode

import (
	"context"
	"errors"
	"io"

	"github.com/jackc/pgx/v5/pgconn"

	"jellyscale/internal/db"
)

// How a job ends comes from where its error came from, not from what the error is:
// the call sites mark an error as the answer of a call on the Postgres pool (fromPool)
// or the failure of a store (fromStore), and endState reads the mark. An error no one
// marked came from ffmpeg, the spec or a local file, and fails the job.

// poolError is the answer of a call on the Postgres pool. Its text is the error's own.
type poolError struct{ err error }

func (e *poolError) Error() string { return e.err.Error() }
func (e *poolError) Unwrap() error { return e.err }

func fromPool(err error) error {
	if err == nil {
		return nil
	}
	return &poolError{err}
}

// storeError is the failure of the store at uri, which its text names.
type storeError struct {
	uri string
	err error
}

func (e *storeError) Error() string {
	if e.uri == "" {
		return e.err.Error()
	}
	return e.uri + ": " + e.err.Error()
}
func (e *storeError) Unwrap() error { return e.err }

func fromStore(uri string, err error) error {
	if err == nil {
		return nil
	}
	return &storeError{uri, err}
}

// storeBody is the body a store opened at uri, read into a local file: the copy has two
// ends, and only the errors of its reads are the store's.
type storeBody struct {
	uri  string
	body io.Reader
}

func (b storeBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && err != io.EOF {
		err = fromStore(b.uri, err)
	}
	return n, err
}

// endState is how a job that did not finish ends, and the text recorded for it.
// A job given up (its lease lost, or the worker stopping) is cancelled whatever error
// came back with it; Postgres being away, or aborting a write for the other
// transaction's sake (a deadlock or a serialization failure), is no fault of the job's.
// Anything else fails it, which refuses the spec for a while.
func endState(ctx context.Context, err error) (state, reason string) {
	reason = err.Error()
	if ctx.Err() != nil || errors.Is(err, errInterrupted) {
		return "cancelled", reason
	}
	var pool *poolError
	if errors.As(err, &pool) {
		if db.IsAway(pool.err) || abortedForAnother(pool.err) {
			return "cancelled", reason
		}
	}
	return "failed", reason
}

// abortedForAnother says Postgres aborted a statement for the other transaction's
// sake, a deadlock (40P01) or a serialization failure (40001): asking again is right.
func abortedForAnother(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}
