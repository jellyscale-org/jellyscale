package transcode

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const channel = "jellyscale_transcode"

// Hint payloads. A segment hint is segmentHint(hash); a new job is jobHint.
const jobHint = "job"

func segmentHint(hash string) string { return "segment:" + hash }

// Notify sends a hint after the data it announces has committed. It runs in
// its own transaction with synchronous_commit off, because inline with a data
// transaction pg_notify capped commits at about 189 a second (invariant 9).
// Hints are lossy, so a failure only costs the waiter its next poll.
func Notify(ctx context.Context, pool *pgxpool.Pool, payload string) {
	pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { //nolint:errcheck
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
		return err
	})
}

// Hub fans Postgres hints out to waiters in this process. A nil Hub is valid
// and delivers nothing, so callers fall back to polling, which they do anyway
// (invariant 10).
type Hub struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger

	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// Subscribe returns a channel that receives when a hint with this payload
// arrives, and a function to stop listening.
func (h *Hub) Subscribe(payload string) (<-chan struct{}, func()) {
	if h == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan struct{}]struct{}{}
	}
	if h.subs[payload] == nil {
		h.subs[payload] = map[chan struct{}]struct{}{}
	}
	h.subs[payload][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[payload], ch)
		if len(h.subs[payload]) == 0 {
			delete(h.subs, payload)
		}
		h.mu.Unlock()
	}
}

func (h *Hub) fire(payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[payload] {
		select {
		case ch <- struct{}{}:
		default: // One pending wakeup is enough.
		}
	}
}

// Run listens until ctx ends, reconnecting after failures.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := h.listen(ctx); err != nil && ctx.Err() == nil {
			h.Log.Warn("hint listener lost its connection; waiters poll meanwhile", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.fire(notification.Payload)
	}
}
