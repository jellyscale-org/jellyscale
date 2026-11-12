package testenv

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DBProxy stands between a process and the test Postgres, so a test can make
// the database go away the way a Kubernetes switchover or a restart does: every
// connection ended on both sides, and new ones either accepted at once (a
// switchover seen through a Service) or refused for as long as the test says
// (a restart). Postgres itself keeps running and the test reads it directly,
// so what the outage did to the rows can be looked at while it lasts.
//
// A refused connection is accepted and closed at once, which pgx reports as the
// server closing the connection during startup; a socket that is not listening
// at all would be reported differently by the kernel and the same by the code.
type DBProxy struct {
	ln     net.Listener
	target string
	dsn    string
	name   string // The application_name of every session that came through the proxy.

	mu      sync.Mutex
	refuse  bool
	conns   map[net.Conn]struct{}
	accepts int
	trip    []byte        // When a statement holds this, the connections end instead of passing it on.
	tripped chan struct{} // Closed when they did.
}

// NewDBProxy listens on a port of its own and forwards to the database of dsn.
func NewDBProxy(t *testing.T, dsn string) *DBProxy {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &DBProxy{ln: ln, target: u.Host, conns: map[net.Conn]struct{}{}}
	u.Host = ln.Addr().String()
	// Postgres sees the proxy's own address or a NAT'd port as the client, so the
	// sessions are told apart by the name they give themselves.
	p.name = "dbproxy-" + u.Port()
	q := u.Query()
	q.Set("application_name", p.name)
	u.RawQuery = q.Encode()
	p.dsn = u.String()
	go p.accept()
	t.Cleanup(func() {
		ln.Close()
		p.Sever()
	})
	return p
}

// DSN is the database URL that goes through the proxy.
func (p *DBProxy) DSN() string { return p.dsn }

// Addr is the host:port the proxy listens on.
func (p *DBProxy) Addr() string { return p.ln.Addr().String() }

// Away ends every connection and refuses new ones until Back.
func (p *DBProxy) Away() {
	p.mu.Lock()
	p.refuse = true
	p.mu.Unlock()
	p.Sever()
}

// Back accepts connections again.
func (p *DBProxy) Back() {
	p.mu.Lock()
	p.refuse = false
	p.mu.Unlock()
}

// Sever ends every connection, on both sides, and leaves the proxy accepting
// whatever it accepted before: a switchover, where the old primary's
// connections all end and the Service points at the new one at once.
func (p *DBProxy) Sever() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// Terminate has Postgres itself end every backend that came through the proxy,
// by pg_terminate_backend run on pool (which must not go through the proxy). Sessions are
// found by the application_name the DSN gives them, which a DSN derived from it
// (a worker's liveness connection) keeps. The
// client is told what a switchover or a fast shutdown tells it: a FATAL message,
// SQLSTATE 57P01 (admin_shutdown), to the statement that runs or the next one,
// where Sever only closes the socket and the client sees EOF. Code that
// recognises "Postgres is away" by connection errors alone fails on this one.
// It returns how many backends it ended.
func (p *DBProxy) Terminate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1 AND pid <> pg_backend_pid()", p.name)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// SeverOn arms the proxy once: the first statement a client sends that contains
// text is not passed on, and every connection ends instead, so the statement
// fails as one does when the server goes away while it runs. The channel is
// closed when that has happened.
func (p *DBProxy) SeverOn(text string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trip, p.tripped = []byte(text), make(chan struct{})
	return p.tripped
}

// Accepted counts the connections the proxy let through, for a test that must
// know whether a process tried again.
func (p *DBProxy) Accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepts
}

func (p *DBProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refused := p.refuse
		p.mu.Unlock()
		if refused {
			client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			client.Close()
			continue
		}
		p.mu.Lock()
		p.accepts++
		p.conns[client], p.conns[server] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go p.pipe(server, client, true)
		go p.pipe(client, server, false)
	}
}

// pipe copies until either side ends, then ends both.
func (p *DBProxy) pipe(dst, src net.Conn, fromClient bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && fromClient && p.tripOn(buf[:n]) {
			break
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	dst.Close()
	src.Close()
}

// tripOn ends every connection if the armed text is in what a client sent.
func (p *DBProxy) tripOn(sent []byte) bool {
	p.mu.Lock()
	if p.trip == nil || !bytes.Contains(sent, p.trip) {
		p.mu.Unlock()
		return false
	}
	tripped := p.tripped
	p.trip, p.tripped = nil, nil
	p.mu.Unlock()
	p.Sever()
	close(tripped)
	return true
}
