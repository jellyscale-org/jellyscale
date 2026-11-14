package db

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outage is a process's knowledge of whether Postgres is away: a switchover or
// a restart ends every connection, and for some seconds new ones fail. The first
// query or connection that fails in that way marks it away, one goroutine
// probes with a connection of its own until Postgres answers as a primary, and
// everything that waits on it wakes at once. One per pool, made by Open.
//
// What waits is a request: the player of a film treats any answer of 400 or
// more as the end of playback, so a stream replica waits for the database
// (Retry) instead of answering 500.
type Outage struct {
	probe *pgx.ConnConfig // Without the tracer, so probing is not itself a failure.
	pool  *pgxpool.Pool

	mu     sync.Mutex
	log    *slog.Logger
	away   bool
	since  time.Time     // When it went away, while it is.
	total  time.Duration // Time spent away before since.
	backAt time.Time
	back   chan struct{} // Closed when Postgres is back; replaced when it goes away.
}

// Of is the outage tracker of a pool made by Open, or nil for any other pool.
// A nil tracker never says Postgres is away.
func Of(pool *pgxpool.Pool) *Outage {
	o, _ := outages.Load(pool)
	tracker, _ := o.(*Outage)
	return tracker
}

// outages holds the tracker of each pool Open made; a pool lives as long as the process.
var outages sync.Map

// SetLog says where the lines about Postgres going away and coming back go.
func (o *Outage) SetLog(log *slog.Logger) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.log = log
	o.mu.Unlock()
}

// Recent says whether Postgres is away, or was within the last d. A process
// that was cut off from it cannot tell, for that long, who else was.
func (o *Outage) Recent(d time.Duration) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.away || time.Since(o.backAt) < d
}

// Away says whether Postgres is away now.
func (o *Outage) Away() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.away
}

// AwayFor is how long Postgres has been away without a break, 0 when it is not.
func (o *Outage) AwayFor() time.Duration {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.away {
		return 0
	}
	return time.Since(o.since)
}

// Total is how long Postgres has been away in all, the present outage included.
func (o *Outage) Total() time.Duration {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.away {
		return o.total + time.Since(o.since)
	}
	return o.total
}

// Wait returns once Postgres is not away, or ctx ends.
func (o *Outage) Wait(ctx context.Context) error {
	if o != nil {
		o.mu.Lock()
		back := o.back
		away := o.away
		o.mu.Unlock()
		if away {
			select {
			case <-back:
			case <-ctx.Done():
			}
		}
	}
	return ctx.Err()
}

// Retry runs f again, once Postgres is back, for as long as it fails because
// Postgres is away. It returns the first other result, or what ends ctx.
func Retry(ctx context.Context, pool *pgxpool.Pool, f func() error) error {
	o := Of(pool)
	for {
		err := f()
		if err == nil || !IsAway(err) {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		// An error that has not been seen to mark Postgres away (it is back already,
		// or the error was a stale connection's) is tried again after a moment.
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return err
		}
		o.Wait(ctx) //nolint:errcheck // ctx.Err() is looked at above.
	}
}

// IsAway says whether err is the database being away rather than a question it
// answered: a connection that ended or could not be made, or Postgres itself
// shutting down or not accepting connections yet (SQLSTATE class 57 and 08).
func IsAway(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") || (strings.HasPrefix(pgErr.Code, "57") && pgErr.Code != "57014") // 57014: a query cancelled.
	}
	// A file's error is never the database's, though syscall.Errno satisfies
	// net.Error: a missing segment or a full disk must fail the job, not cancel
	// it as if Postgres were away (JS-029, found in the merge queue).
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	var connectErr *pgconn.ConnectError
	var opErr *net.OpError
	return errors.As(err, &connectErr) || errors.As(err, &opErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "conn closed")
}

// goneAway is called with every error a query or connection met.
func (o *Outage) goneAway(err error) {
	if !IsAway(err) {
		return
	}
	o.mu.Lock()
	if o.away {
		o.mu.Unlock()
		return
	}
	o.away, o.since, o.back = true, time.Now(), make(chan struct{})
	log := o.log
	o.mu.Unlock()
	if log == nil {
		log = slog.Default()
	}
	log.Warn("Postgres is away; requests that need it wait, and workers go on", "error", err)
	go o.watch()
}

// watch connects until Postgres answers as a primary, then says so.
func (o *Outage) watch() {
	if o.pool != nil {
		// The connections the pool has are the old server's.
		o.pool.Reset()
	}
	for {
		if o.reachable() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	o.mu.Lock()
	o.away = false
	o.backAt = time.Now()
	o.total += time.Since(o.since)
	close(o.back)
	log, away := o.log, time.Since(o.since)
	o.mu.Unlock()
	if log == nil {
		log = slog.Default()
	}
	log.Info("Postgres is back", "was away", away.Round(time.Millisecond))
}

func (o *Outage) reachable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, o.probe)
	if err != nil {
		return false
	}
	defer conn.Close(context.Background()) //nolint:errcheck
	// A demoted primary answers too, and cannot be written to.
	var readOnly string
	return conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly) == nil && readOnly == "off"
}

// The tracer is how the failures of every query and connection reach the tracker
// without each call site reporting its own.

func (o *Outage) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (o *Outage) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	o.goneAway(data.Err)
}

func (o *Outage) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return ctx
}

func (o *Outage) TraceConnectEnd(_ context.Context, data pgx.TraceConnectEndData) {
	o.goneAway(data.Err)
}
