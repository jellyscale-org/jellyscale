package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// JS-107: how a job ends comes from where the error came from, in one place.
//
// These checks use three names the task implies and the build must add, so the
// package does not compile until it has:
//
//	fromPool(err error) error             marks err as the answer of a call on the Postgres pool
//	fromStore(uri string, err error) error marks err as the failure of the store at uri, and names uri in its text
//	endState(ctx context.Context, err error) (state, reason string)
//	                                       the end state of a job (done is not decided here) and the text recorded in
//	                                       transcode_jobs.error; ctx is the job's context, which says whether the job was
//	                                       given up (its lease lost, or the worker stopping)
//
// An error that no one marked came from ffmpeg, the spec or a local file, and fails the job.

// theFive are the errors the old IsAway took for "Postgres is away" whatever made them.
var theFive = map[string]error{
	"*net.OpError":            &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
	"io.ErrUnexpectedEOF":     io.ErrUnexpectedEOF,
	"io.EOF":                  io.EOF,
	"net.ErrClosed":           net.ErrClosed,
	"a text with conn closed": errors.New("write failed: conn closed"),
}

// bareAndWrapped is each error as it is and with context added.
func bareAndWrapped(err error) map[string]error {
	return map[string]error{"bare": err, "wrapped": fmt.Errorf("storing segment 3: %w", err)}
}

const (
	transcodeStore = "file:///srv/transcode-store"
	mediaStore     = "s3://media-bucket/films?endpoint=http://media.invalid"
)

func TestEndStateOfAStoresErrorIsFailedWhateverItsTypeAndNamesTheStore(t *testing.T) {
	for _, uri := range []string{transcodeStore, mediaStore} {
		for name, base := range theFive {
			for form, err := range bareAndWrapped(base) {
				t.Run(name+" "+form+" "+uri, func(t *testing.T) {
					state, reason := endState(context.Background(), fromStore(uri, err))
					if state != "failed" {
						t.Errorf("a store's %s ended the job %s, want failed", name, state)
					}
					if !strings.Contains(reason, uri) {
						t.Errorf("the recorded error %q does not hold the store's URI %s", reason, uri)
					}
					// The other store is not blamed.
					other := mediaStore
					if uri == mediaStore {
						other = transcodeStore
					}
					if strings.Contains(reason, other) {
						t.Errorf("the recorded error %q names %s, which did not fail", reason, other)
					}
				})
			}
		}
	}
}

func TestEndStateOfAPoolCallsErrorIsCancelledWhenPostgresIsAway(t *testing.T) {
	away := map[string]error{
		"57P01 admin shutdown":     &pgconn.PgError{Code: "57P01"},
		"57P03 cannot connect now": &pgconn.PgError{Code: "57P03"},
		"08006 connection failure": &pgconn.PgError{Code: "08006"},
	}
	for name, base := range theFive {
		away[name] = base
	}
	for name, base := range away {
		for form, err := range bareAndWrapped(base) {
			t.Run(name+" "+form, func(t *testing.T) {
				if state, _ := endState(context.Background(), fromPool(err)); state != "cancelled" {
					t.Errorf("%s from the pool ended the job %s, want cancelled", name, state)
				}
			})
		}
	}
}

func TestEndStateOfAnAnswerOfPostgresThatIsNoOutageIsFailed(t *testing.T) {
	for form, err := range bareAndWrapped(&pgconn.PgError{Code: "23505", Message: "duplicate key value"}) {
		t.Run(form, func(t *testing.T) {
			if state, _ := endState(context.Background(), fromPool(err)); state != "failed" {
				t.Errorf("a unique violation ended the job %s, want failed", state)
			}
		})
	}
}

// A deadlock or a serialization failure on a worker's own write is Postgres
// answering, and the other transaction's doing: the worker's job never fails for it
// (Fable 5.2, .results/js105-recurring-fable-2026-11-15.md A.1).
func TestADeadlockOrSerializationFailureOnAWorkersWriteNeverFailsTheJob(t *testing.T) {
	for _, code := range []string{"40P01", "40001"} {
		for form, err := range bareAndWrapped(&pgconn.PgError{Code: code, Message: "tuple concurrently updated or deadlock detected"}) {
			t.Run(code+" "+form, func(t *testing.T) {
				state, _ := endState(context.Background(), fromPool(err))
				if state == "failed" {
					t.Errorf("SQLSTATE %s from a write ended the job failed, which refuses the spec for 30 seconds", code)
				}
				if state != "cancelled" {
					t.Errorf("state %q, want cancelled", state)
				}
			})
		}
	}
}

func TestEndStateOfAJobGivenUpIsCancelledWhateverErrorComesBackWithIt(t *testing.T) {
	storeErr := fromStore(transcodeStore, io.ErrUnexpectedEOF)
	ffmpegErr := errors.New("ffmpeg exited with status 1: signal: killed")
	lost := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(fmt.Errorf("%w: another job took over the spec", errInterrupted))
		return ctx
	}
	stopping := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	for _, give := range []struct {
		name string
		ctx  func() context.Context
		errs map[string]error
	}{
		{"a lease taken by a seek's replacement or a reap", lost, map[string]error{
			"alone":        fmt.Errorf("%w: another job took over the spec", errInterrupted),
			"with a store": fmt.Errorf("%w: %w", errInterrupted, storeErr),
			"store first":  errors.Join(storeErr, errInterrupted),
			"with ffmpeg":  fmt.Errorf("%w: %w", errInterrupted, ffmpegErr),
		}},
		{"a lease not renewed in time", lost, map[string]error{
			"alone":        fmt.Errorf("%w: %w", errInterrupted, errUnrenewed),
			"with a store": fmt.Errorf("%w: %w: %w", errInterrupted, errUnrenewed, storeErr),
		}},
		{"a refusal at the fence before a put", context.Background, map[string]error{
			"alone":        fmt.Errorf("%w: the lease ran out before the segment could be stored", errInterrupted),
			"with a store": fmt.Errorf("%w: the lease ran out: %w", errInterrupted, storeErr),
		}},
		{"a stopping worker", stopping, map[string]error{
			"cancelled":    context.Canceled,
			"with a store": storeErr,
			"with ffmpeg":  ffmpegErr,
		}},
	} {
		for name, err := range give.errs {
			t.Run(give.name+" "+name, func(t *testing.T) {
				if state, _ := endState(give.ctx(), err); state != "cancelled" {
					t.Errorf("ended %s, want cancelled", state)
				}
			})
		}
	}
}

func TestEndStateOfWhatStillFailsIsFailed(t *testing.T) {
	for name, err := range map[string]error{
		"ffmpeg exiting non-zero":          errors.New("ffmpeg: exit status 187: Invalid data found when processing input"),
		"ffmpeg making no segment":         fmt.Errorf("ffmpeg made no segment in %s: %s", "30s", "frame= 0"),
		"a source that changed or is gone": errors.New("the source file has changed or is gone"),
		"a full disk":                      &fs.PathError{Op: "write", Path: "/scratch/job-1/3.m4s", Err: syscall.ENOSPC},
		"a missing segment":                &fs.PathError{Op: "open", Path: "/scratch/job-1/3.m4s", Err: syscall.ENOENT},
		"a wrapped missing segment":        fmt.Errorf("uploading: %w", &fs.PathError{Op: "open", Path: "/scratch/job-1/3.m4s", Err: fs.ErrNotExist}),
		// Where an error came from decides, not what it is: a pipe of ffmpeg's that ends early is ffmpeg's.
		"an ffmpeg pipe that ended (io.ErrUnexpectedEOF)": fmt.Errorf("reading ffmpeg's output: %w", io.ErrUnexpectedEOF),
		"an ffmpeg pipe that closed (*net.OpError)":       &net.OpError{Op: "read", Net: "unix", Err: syscall.EPIPE},
	} {
		t.Run(name, func(t *testing.T) {
			state, reason := endState(context.Background(), err)
			if state != "failed" {
				t.Errorf("ended %s, want failed", state)
			}
			if reason == "" {
				t.Error("a failed job records no reason")
			}
		})
	}
}
