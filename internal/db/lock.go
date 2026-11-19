package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lock is session advisory locks held on a connection of its own, outside the
// pool. A holder whose work asks the pool for connections would otherwise wait
// for itself when the pool is small, and take the pool from everything else in
// its process while it did.
type Lock struct {
	conn *pgx.Conn
}

// NewLock connects with the pool's own settings, less its tracer: a lock that
// cannot be taken is not Postgres being away for the pool's readers. The caller
// takes locks on Conn and releases them all with Release.
func NewLock(ctx context.Context, pool *pgxpool.Pool) (*Lock, error) {
	cfg := pool.Config().ConnConfig // A copy.
	cfg.Tracer = nil
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Lock{conn}, nil
}

// Conn is the connection the locks belong to.
func (l *Lock) Conn() *pgx.Conn { return l.conn }

// Try takes the session advisory lock of name, and says whether it did: ok is
// false when another session holds it.
func (l *Lock) Try(ctx context.Context, name string) (ok bool, err error) {
	err = l.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", name).Scan(&ok)
	return ok, err
}

// UnlockAll gives back every lock taken on the connection, which stays open.
func (l *Lock) UnlockAll(ctx context.Context) error {
	_, err := l.conn.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	return err
}

// TryLock is NewLock and Try of one name: the lock, or none when it is held.
func TryLock(ctx context.Context, pool *pgxpool.Pool, name string) (lock *Lock, ok bool, err error) {
	lock, err = NewLock(ctx, pool)
	if err != nil {
		return nil, false, err
	}
	if ok, err = lock.Try(ctx, name); err != nil || !ok {
		lock.Release()
		return nil, false, err
	}
	return lock, true, nil
}

// Release gives the locks back and closes the connection. A lock that cannot be
// released goes with the connection, which ends the session.
func (l *Lock) Release() {
	ctx := context.Background()
	l.UnlockAll(ctx)  //nolint:errcheck
	l.conn.Close(ctx) //nolint:errcheck
}
