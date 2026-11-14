//go:build integration

package transcode

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// JS-084's checks against a real Postgres. They hold the rewrite to the
// mechanisms of today's liveness: one connection and one relock loop, the same
// log lines, several dead workers forgotten at once.

// socketCount counts the sockets a worker's liveness config has open, and the
// most it ever had open at once, and every dial it made.
type socketCount struct {
	open, most, dials atomic.Int32
	refuse            atomic.Bool
}

type countedConn struct {
	net.Conn
	c    *socketCount
	once sync.Once
}

func (c *countedConn) Close() error {
	c.once.Do(func() { c.c.open.Add(-1) })
	return c.Conn.Close()
}

func (s *socketCount) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	s.dials.Add(1)
	if s.refuse.Load() {
		return nil, errors.New("refused by the test")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	n := s.open.Add(1)
	for {
		most := s.most.Load()
		if n <= most || s.most.CompareAndSwap(most, n) {
			break
		}
	}
	return &countedConn{Conn: conn, c: s}, nil
}

func (r *rig) lockPID(id string) int {
	var pid int
	r.pool.QueryRow(context.Background(), `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND classid = `+livenessClass+`
		AND objsubid = 2 AND objid::bigint = hashtext($1)::bigint & 4294967295`, id).Scan(&pid) //nolint:errcheck
	return pid
}

// However often the lock's connection is ended, a worker has at most one
// liveness socket open and at most one relock loop (two would each open one
// while the other holds it), and when Present returns, even in the middle of a
// relock, its row is deleted and no lock or connection of it is left.
func TestAWorkerHoldsOneLivenessConnectionHoweverOftenItsLockEndsAndLeavesNoneBehind(t *testing.T) {
	r := newRig(t, nil)
	var sockets socketCount
	cfg := r.pool.Config().ConnConfig.Copy()
	cfg.Tracer = nil // A lock that cannot be taken is not Postgres being away, as in Present.
	cfg.DialFunc = sockets.dial
	var id string
	stop := r.worker(t, func(w *Worker) { w.LivenessConfig = cfg; id = w.ID })
	for i := 0; i < 12; i++ {
		old := r.lockPID(id)
		if old == 0 {
			t.Fatalf("round %d: the worker holds no lock", i)
		}
		r.terminate(t, id)
		waitFor(t, "the worker to hold its lock again", 30*time.Second, func() bool {
			pid := r.lockPID(id)
			return pid != 0 && pid != old && r.locked(id)
		})
	}
	if most := sockets.most.Load(); most != 1 {
		t.Errorf("the worker had %d liveness sockets open at once, want 1", most)
	}
	// Now relock for good: ended, and every new connection refused, so the relock loop is
	// retrying when the worker is stopped.
	sockets.refuse.Store(true)
	before := sockets.dials.Load()
	r.terminate(t, id)
	waitFor(t, "the relock loop to try several times", 30*time.Second, func() bool { return sockets.dials.Load() >= before+3 })
	if most := sockets.most.Load(); most != 1 {
		t.Errorf("while relocking the worker had %d sockets open at once, want 1", most)
	}
	stop()
	if n := r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id); n != 0 {
		t.Error("the worker's row is still there after Present returned from relocking")
	}
	if pid := r.lockPID(id); pid != 0 {
		t.Errorf("a liveness lock of the stopped worker is still held by backend %d", pid)
	}
	if n := sockets.open.Load(); n != 0 {
		t.Errorf("%d liveness sockets of the stopped worker are still open", n)
	}
	after := sockets.dials.Load()
	time.Sleep(300 * time.Millisecond) // Longer than six retries, so a loop left running dials again.
	if sockets.dials.Load() != after {
		t.Error("a relock loop went on dialling after Present returned")
	}
}

func buffered(buf *logBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// The operator's log: each end of the lock's connection is logged once, as is
// the lock held again.
func TestEachEndOfTheLockConnectionAndEachLockTakenAgainIsLoggedOnce(t *testing.T) {
	r := newRig(t, nil)
	var buf logBuffer
	var id string
	r.worker(t, func(w *Worker) { w.Log = buffered(&buf); id = w.ID })
	const ends = 3
	for i := 0; i < ends; i++ {
		old := r.lockPID(id)
		r.terminate(t, id)
		waitFor(t, "the worker to hold its lock again", 30*time.Second, func() bool {
			pid := r.lockPID(id)
			return pid != 0 && pid != old && r.locked(id)
		})
		// Its row is looked for once the lock is back; the line about it follows.
		waitFor(t, "the worker to say its row is there", 30*time.Second, func() bool {
			return strings.Count(buf.String(), "holds its liveness lock again and its row is there") >= i+1
		})
	}
	out := buf.String()
	if n := strings.Count(out, "liveness connection ended"); n != ends {
		t.Errorf("the end of the lock's connection was logged %d times, want %d:\n%s", n, ends, out)
	}
	if n := strings.Count(out, "holds its liveness lock again and its row is there"); n != ends {
		t.Errorf("the lock held again with its row was logged %d times, want %d", n, ends)
	}
	if strings.Contains(out, "level=ERROR") && strings.Contains(out, "took this worker for dead") {
		t.Errorf("a worker that took its lock again was told it was forgotten:\n%s", out)
	}
}

// A lock Postgres does not show while the connection stands is logged once, naming the
// transaction-mode pooler.
func TestALockPostgresDoesNotShowIsLoggedOnceNamingTheTransactionModePooler(t *testing.T) {
	r := newRig(t, nil)
	p := newProxy(t, r.dsn, false)
	cfg, err := through(r.dsn, p.addr())
	if err != nil {
		t.Fatal(err)
	}
	var buf logBuffer
	r.worker(t, func(w *Worker) { w.Log = buffered(&buf); w.LivenessConfig = cfg })
	p.cut() // The server's end goes, the client's stays open: the lock is gone, the connection stands.
	waitFor(t, "the worker to say Postgres does not show its lock", 30*time.Second, func() bool {
		return strings.Contains(buf.String(), "does not show its lock")
	})
	time.Sleep(time.Second)
	out := buf.String()
	if n := strings.Count(out, "does not show its lock"); n != 1 {
		t.Errorf("the lock Postgres does not show was logged %d times, want 1", n)
	}
	if !strings.Contains(out, "transaction-mode pooler") {
		t.Error("the line does not name the transaction-mode pooler")
	}
}

// Several dead workers at once are forgotten by a locked worker, and a live
// one is not, in the same looks.
func TestALockedWorkerForgetsSeveralDeadWorkersAtOnceAndNoLiveOne(t *testing.T) {
	r := newRig(t, nil)
	var a, b string
	r.worker(t, func(w *Worker) { a = w.ID })
	r.worker(t, func(w *Worker) { b = w.ID })
	var dead []string
	for i := 0; i < 3; i++ {
		id := "dead-" + uuid.NewString()
		dead = append(dead, id)
		// A node that took a lock and no longer holds it: the row says locked, no lock is in pg_locks.
		r.exec(t, `INSERT INTO transcode_nodes (id, roles, store, slots, locked) VALUES ($1, 'worker', '', 2, true)`, id)
		r.exec(t, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
			VALUES ($1, '{}', 'o', 0, 0, 'running', $2, now() + interval '10 seconds')`, "h"+id, id)
	}
	waitFor(t, "every dead worker to be forgotten", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = ANY($1)", dead) == 0
	})
	for _, id := range []string{a, b} {
		if !r.locked(id) {
			t.Errorf("live worker %s was forgotten", id)
		}
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE worker = ANY($1) AND lease_expires_at > now() + interval '5 seconds'", dead); n != 0 {
		t.Errorf("%d jobs of the forgotten workers were left more than their cut lease", n)
	}
}
