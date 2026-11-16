//go:build integration

package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
)

// JS-125, the forced checks: the interleavings the access criteria name, made by holding Postgres where it matters
// rather than by waiting for a race. The mutants that each must fail are run by TestTheAccessChecksFailOnTheirMutants.

// A stream replica holds a snapshot of the tokens of playing sessions (invariant 37), and the snapshot decides only
// while Postgres is away. Here Postgres answers: the replica's Postgres is held (nothing is refused and nothing ends,
// so the replica does not think it is away, and its next refresh cannot drop the token either), the token is signed
// out through an api replica, and the first request for a byte range is sent once. A build that asks the snapshot
// before Postgres answers 206 at once, with the token signed out; the right one waits for Postgres, which says 401.
func TestASnapshotNeverDecidesWhilePostgresAnswers(t *testing.T) {
	p := newPlayAway(t, 2)
	_, refresh := timings(t)
	p.playing(t, 0)
	p.playing(t, 1)
	refreshed(refresh) // The replica's snapshot holds both tokens.
	rp := p.streams[0]
	rng := func(token string) served {
		return served{what: "a range", path: p.direct(token), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])}
	}
	answeredWithin(t, "with both sessions signed in", rp, []served{rng(p.tokens[0]), rng(p.tokens[1])})

	rp.proxy.Hold()
	released := false
	release := func() {
		if !released {
			released = true
			rp.proxy.Release()
		}
	}
	t.Cleanup(release)
	if response, body := p.env.do(t, 0, "POST", "/Sessions/Logout", p.tokens[0], nil); response.StatusCode != 204 {
		t.Fatalf("signing out: %d %s", response.StatusCode, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	answer := ask(ctx, rp, rng(p.tokens[0])) // Sent once.
	select {
	case a := <-answer:
		t.Fatalf("the token signed out through the api was answered %d (%d bytes) with Postgres answering and before it was asked: the snapshot decided", a.status, len(a.body))
	case <-time.After(2 * time.Second):
	}
	release()
	select {
	case a := <-answer:
		if a.err != nil || a.status != 401 {
			t.Fatalf("the first request of the signed-out token was answered %d %v, want 401", a.status, a.err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the request was still not answered 90 seconds after Postgres was released")
	}
	// The other session, in the same snapshot, is served.
	answeredWithin(t, "after Postgres was released", rp, []served{rng(p.tokens[1])})
}

// rawRequest is a request as bytes on a connection of its own, so that what is sent back, and when, is the test's to read.
func rawRequest(t *testing.T, base, path, token string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: replica\r\nX-Emby-Token: %s\r\nConnection: close\r\n\r\n", path, token)
	return conn
}

// With Postgres away on a replica with both roles whose snapshot holds a playing session: every viewer route asked with
// no token answers the same 401 within a second, and each Snapshot route asked with an unknown token, for the film the
// held session plays, is sent nothing, no status line and no header, for the three seconds or more the outage lasts, and
// answers that 401 once Postgres is back. The held session's own token is served meanwhile, so what waits is the unknown
// token and not the replica.
func TestInAnOutageEveryViewerRouteRefusesAtOnceAndAnUnknownTokenIsSentNothing(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	both := newReplica(t, p.env, p.segments, "api,stream")
	p.playing(t, 0)
	refreshed(refresh)
	a := &accessSite{env: p.env, asked: map[string]int{}}
	film := accessFilm{store: "posix", item: p.item, source: db.CompactID(p.item), body: p.body}
	both.away(t, p.pool)

	quick := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DisableKeepAlives: true}}
	u := &accUniform{seen: map[string][]string{}}
	var snapshotRoutes []Route
	viewers := 0
	var refusal accReply
	for _, route := range (&Server{}).Routes() {
		if route.Snapshot {
			snapshotRoutes = append(snapshotRoutes, route)
		}
		if accOpen(route) {
			continue
		}
		path, err := a.path(route, film, p.user)
		if err != nil {
			t.Error(err)
			continue
		}
		viewers++
		r := accSendWith(quick, route.Method, both.server.URL, path, "", -1, accRequestBody(route))
		a.judge(u, route, "with no token while Postgres is away, within a second", r, false)
		refusal = r
	}
	u.check(a, "the outage")
	a.report(t)
	if viewers < 77 {
		t.Errorf("%d viewer routes asked, want at least the 77 there were", viewers)
	}
	if len(snapshotRoutes) < 6 {
		t.Fatalf("%d Snapshot routes, want at least the 6 there were", len(snapshotRoutes))
	}

	// The held session's own token is served during the outage.
	answeredWithin(t, "during the outage", both, []served{{what: "direct play", path: p.direct(p.tokens[0]), headers: []string{"Range", "bytes=0-9"}, status: 206, body: string(p.body[:10])}})

	// An unknown token on each Snapshot route, which Postgres alone can refuse.
	conns := map[string]net.Conn{}
	for _, route := range snapshotRoutes {
		path, err := a.path(route, film, p.user)
		if err != nil {
			t.Error(err)
			continue
		}
		conns[routeName(route)] = rawRequest(t, both.server.URL, path, "unknown-"+uuid.NewString())
	}
	time.Sleep(3200 * time.Millisecond) // The requests have waited at least 3 seconds, and the outage has lasted longer.
	for name, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) //nolint:errcheck
		buf := make([]byte, 1)
		n, err := conn.Read(buf)
		var timeout net.Error
		if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("%s with an unknown token was sent %d bytes (%v) while Postgres was away: it must be sent nothing, no status line and no header", name, n, err)
		}
	}
	both.proxy.Back()
	for name, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second)) //nolint:errcheck
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Errorf("%s with an unknown token was not answered once Postgres was back: %v", name, err)
			continue
		}
		body := make([]byte, 128)
		n, _ := response.Body.Read(body)
		response.Body.Close()
		if response.StatusCode != 401 || strings.TrimSpace(string(body[:n])) != "unauthorized" {
			t.Errorf("%s with an unknown token was answered %d %q once Postgres was back, want the 401", name, response.StatusCode, body[:n])
		}
		if response.Header.Get("Content-Type") != refusal.header.Get("Content-Type") {
			t.Errorf("%s: the 401 of an unknown token has Content-Type %q, the other 401s %q", name, response.Header.Get("Content-Type"), refusal.header.Get("Content-Type"))
		}
	}
}

// Each mutant is a one-line wrong build of the guard or of a route's declaration (test/mutant.py), and the check that
// is named must fail on it, naming the route or the request, every time it is run. A mutant that no longer applies to
// the code fails here, and is then to be brought up to date: it never counts as a failure of the check it was for.
func TestTheAccessChecksFailOnTheirMutants(t *testing.T) {
	run := func(t *testing.T, mutant string, args ...string) string {
		t.Helper()
		cmd := exec.Command("bash", append([]string{"../../test/mutant.sh", mutant, "go", "test", "-count=1"}, args...)...)
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "the mutant no longer applies") {
			t.Fatalf("the mutant %s is out of date: %s", mutant, out)
		}
		if err == nil {
			t.Errorf("mutant %s: `go test %s` passed", mutant, strings.Join(args, " "))
		}
		return string(out)
	}
	named := func(t *testing.T, out string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("the output of the failing run does not name %q:\n%.3000s", w, out)
			}
		}
	}
	t.Run("GET /Videos/{itemId}/{file} declared anyone", func(t *testing.T) {
		out := run(t, "videos-file-anyone", "-tags", "integration", "-run", "^TestAccess$", "./")
		named(t, out, "GET /Videos/{itemId}/{file}")
		out = run(t, "videos-file-anyone", "-run", "^TestTheOpenRoutesAreExactlyTheOpenList$", "./")
		named(t, out, "GET /Videos/{itemId}/{file}")
	})
	t.Run("the guard accepts any non-empty token", func(t *testing.T) {
		out := run(t, "any-token", "-tags", "integration", "-run", "^TestAccess$", "./")
		named(t, out, "with an unknown token", "want 401")
	})
	t.Run("a token is remembered for 5 seconds", func(t *testing.T) {
		out := run(t, "token-memory", "-tags", "integration", "-run", "^TestAccess$", "./")
		named(t, out, "was accepted until", "want 401")
	})
	t.Run("a Snapshot route is served from the snapshot before Postgres is asked", func(t *testing.T) {
		out := run(t, "snapshot-first", "-tags", "integration", "-run", "^TestASnapshotNeverDecidesWhilePostgresAnswers$", "./")
		named(t, out, "the snapshot decided")
	})
}
