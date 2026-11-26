//go:build integration

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-127: what a refused request does not do, and what a request that names the caller still does. The sweep of
// TestAccess (access_scope_integration_test.go) holds the statuses and the rows; these hold the statements Postgres
// is sent, the jobs a viewer has running, the body PlaybackInfo reads, and the outage.

// serveTraced is serve with a pool whose every statement is kept, as traceReplicas does for the env's replicas. The
// api role alone sends Postgres nothing of its own, so the statements it keeps are the requests'.
func (a *accessSite) serveTraced(t *testing.T, roles ...string) (accHandler, *statementLog) {
	t.Helper()
	base, err := db.Open(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(base.Close)
	cfg := base.Config()
	log := &statementLog{}
	log.inner, _ = cfg.ConnConfig.Tracer.(pgx.QueryTracer)
	cfg.ConnConfig.Tracer = log
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: roles, Log: failureLog(t), SocketIdle: 700 * time.Millisecond}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	return accHandler{name: strings.Join(roles, "+"), base: server.URL, roles: roles}, log
}

// A request that names anyone but the caller is refused before any handler runs, so Postgres is sent the one
// statement that reads the token and nothing else: not a look at the user it names (which would tell a non-administrator
// whether an id is a user), not a write. This holds for the 'own' routes in every place, and for the admin-any routes
// when a non-administrator names B, nobody or the zero id. The wrong build the check fails: a guard that looks the
// named user up before it refuses (as saveConfiguration does today), or a handler that goes on to read.
func TestARefusedRequestSendsPostgresNothingBeyondTheTokenLookup(t *testing.T) {
	a := newAccessSite(t)
	f := a.scopeFixture(t)
	h, log := a.serveTraced(t, RoleAPI)
	if r := accSend("GET", h.base, "/Users/Me", f.a.token, 3, ""); r.status != 200 {
		t.Fatalf("GET /Users/Me: %d %s", r.status, r.body)
	}
	asks := a.scopeRefusals(t, h, f)
	for _, k := range a.scopeAdminAnyAsks(t, h, f) {
		if k.want == 403 {
			asks = append(asks, k.scopeAsk)
		}
	}
	wrong := map[string][]string{}
	var order []string
	for _, k := range asks {
		log.take()
		r := a.scopeSend(h, k)
		sent := log.take()
		if len(sent) == 1 && strings.Contains(strings.ToLower(sent[0]), "from devices") {
			continue
		}
		key := routeName(k.route) + " as " + k.who
		if _, ok := wrong[key]; !ok {
			order = append(order, key)
		}
		text := make([]string, len(sent))
		for i, sql := range sent {
			text[i] = fmt.Sprintf("%.50q", strings.Join(strings.Fields(sql), " "))
		}
		wrong[key] = append(wrong[key], fmt.Sprintf("%s: answered %d and sent %d statements %v", k.what, r.status, len(sent), text))
	}
	if len(order) > 0 {
		var lines []string
		for _, key := range order[:min(len(order), 12)] {
			lines = append(lines, fmt.Sprintf("%s: %d requests; %s", key, len(wrong[key]), wrong[key][0]))
		}
		t.Errorf("%d routes sent Postgres more than the token lookup for a request that named another user; the first %d:\n  %s", len(order), len(lines), strings.Join(lines, "\n  "))
	}
}

// DELETE /Videos/ActiveEncodings acts on the caller's own viewers (the user is the token's, the viewer the
// playSessionId and deviceId it names). With another user's id in the query it is refused, and the caller's job goes on:
// its viewer is not stopped and the job is not cancelled. With the caller's own id in any spelling it stops them, as it
// does with none. The wrong build: the guard comes after the handler, or does not look at this route's query.
func TestARefusedDeleteActiveEncodingsLeavesTheCallersJobRunning(t *testing.T) {
	e, _, _ := transcodeEnv(t, "hevc", "aac")
	compactItem := db.CompactID(e.item)
	other := db.CompactID(e.other)
	seed := func() (job int64) {
		t.Helper()
		mustExec(t, e.pool, "DELETE FROM transcode_viewers; DELETE FROM transcode_jobs")
		job = scalar[int64](t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at, output_version)
			VALUES ('scope-spec', '{}', 'viewer', 0, 0, 'running', 'w1', now() + interval '1 hour', $1) RETURNING id`, playback.OutputVersion)
		mustExec(t, e.pool, `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, item_id, job_id) VALUES ('scope-play', 'scope-spec', $1, 'scope-device', $2, $3)`,
			e.user, compactItem, job)
		return job
	}
	state := func(job int64) (jobState string, stopped bool) {
		return scalar[string](t, e.pool, "SELECT state FROM transcode_jobs WHERE id = $1", job), scalar[bool](t, e.pool, "SELECT stopped FROM transcode_viewers WHERE play_session_id = 'scope-play'")
	}
	base := "/Videos/ActiveEncodings?deviceId=scope-device&playSessionId=scope-play"
	for _, id := range []string{other, strings.ToUpper(e.other), scopeNobody, scopeMalformed, scopeZero} {
		for _, key := range []string{"userId", "controllableByUserId"} {
			job := seed()
			for replica := range e.servers {
				response, body := e.do(t, replica, "DELETE", base+"&"+key+"="+id, e.token, nil)
				if response.StatusCode != 403 {
					t.Errorf("replica %d: DELETE ActiveEncodings with %s=%s answered %d %.40q, want 403", replica, key, id, response.StatusCode, body)
				}
			}
			if jobState, stopped := state(job); jobState != "running" || stopped {
				t.Errorf("after DELETE ActiveEncodings with %s=%s the caller's job is %q and its viewer stopped=%v, want running and not stopped", key, id, jobState, stopped)
			}
		}
	}
	for _, id := range []string{db.CompactID(e.user), e.user, strings.ToUpper(e.user), strings.ToUpper(db.CompactID(e.user))} {
		job := seed()
		if response, body := e.do(t, 0, "DELETE", base+"&userId="+id, e.token, nil); response.StatusCode != 204 {
			t.Errorf("DELETE ActiveEncodings naming the caller (%s) answered %d %.40q, want 204", id, response.StatusCode, body)
		}
		if jobState, stopped := state(job); jobState != "cancelled" || !stopped {
			t.Errorf("DELETE ActiveEncodings naming the caller (%s) left the job %q and stopped=%v, want cancelled and stopped", id, jobState, stopped)
		}
	}
}

// A PlaybackInfo whose body names the caller reads the same body as one that names nobody: the guard that looks at the
// body's UserId must not use the body up before the handler reads it. With the capture's profile, which makes an
// hevc film a transcode, the answer carries the same transcode in every spelling of the caller's id. The wrong build:
// a guard that reads the body with io.ReadAll and does not put it back, so the handler decodes an empty body and offers a direct
// play of the film as it is. (Passes before the guard exists: it holds the behaviour the guard must keep.)
func TestAPlaybackInfoBodyThatNamesTheCallerIsReadWhole(t *testing.T) {
	e, _, source := transcodeEnv(t, "hevc", "aac")
	profile := capturedProfile(t)
	body := func(userID string) map[string]any {
		b := map[string]any{"DeviceProfile": profile, "MaxStreamingBitrate": 2147483647, "AudioStreamIndex": "1", "SubtitleStreamIndex": "-1", "MediaSourceId": source}
		if userID != "" {
			b["UserId"] = userID
		}
		return b
	}
	shape := func(reply map[string]any) string {
		raw, err := json.Marshal(firstSource(reply))
		if err != nil {
			t.Fatal(err)
		}
		return string(scopeVolatile.ReplaceAll(raw, []byte("#")))
	}
	want := firstSource(e.playbackInfo(t, 0, e.item, body("")))
	if url, _ := want["TranscodingUrl"].(string); url == "" {
		t.Fatalf("the capture's profile does not make this film a transcode: %v", want)
	}
	wantShape := shape(map[string]any{"MediaSources": []any{want}})
	for _, id := range []string{db.CompactID(e.user), e.user, strings.ToUpper(db.CompactID(e.user)), strings.ToUpper(e.user)} {
		for replica := range e.servers {
			got := e.playbackInfo(t, replica, e.item, body(id))
			if have := shape(got); have != wantShape {
				t.Errorf("replica %d: PlaybackInfo with UserId %s in the body:\n%s\nwant (the answer with no UserId)\n%s", replica, id, have, wantShape)
			}
		}
	}
}

// While Postgres is away a stream replica goes on serving, from the snapshot of active sessions, what a token that
// is in it asks for. A request that names another user is not served from the snapshot: the snapshot cannot say who the
// token's user is, so it cannot tell the caller's own id from another's, and the guard needs Postgres. Such a request is
// sent nothing while Postgres is away, no status line and no header, never a 4xx (invariant 37: no request is answered with an
// error because Postgres is away), and is answered 403 once it is back. The same requests with no user id are served
// from the snapshot as today. The wrong build: the guard runs after the snapshot has decided, or runs from the snapshot
// and answers 403 at once. Direct play, the stored segments and the encoder's configuration are the Snapshot routes asked
// here; the subtitle and font routes share the guard and the snapshot with them.
func TestInAnOutageARequestNamingAnotherUserIsNotServedFromTheSnapshot(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	both := newReplica(t, p.env, p.segments, "api,stream")
	p.playing(t, 0)
	refreshed(refresh)
	token := p.tokens[0]
	other := db.CompactID(p.other)

	type snapshotRequest struct {
		what string
		path string
	}
	var requests []snapshotRequest
	for _, rq := range p.requests(0) {
		requests = append(requests, snapshotRequest{rq.what, rq.path})
	}
	requests = append(requests, snapshotRequest{"the encoder's configuration", "/System/Configuration/encoding"})
	both.away(t, p.pool)

	// The controls: with no user id the snapshot serves them, as it did before.
	answeredWithin(t, "with no user id, during the outage", both, p.requests(0))
	if a := get(context.Background(), both.server.URL, "/System/Configuration/encoding", "X-Emby-Token", token); a.err != nil || a.status != 200 {
		t.Errorf("the encoder's configuration with no user id during the outage: %d %v, want 200 from the snapshot", a.status, a.err)
	}

	conns := map[string]net.Conn{}
	for _, rq := range requests {
		for _, key := range []string{"userId", "controllableByUserId"} {
			conns[rq.what+" with "+key+"=B"] = rawRequest(t, both.server.URL, scopeAppend(rq.path, key+"="+other), token)
		}
	}
	time.Sleep(3200 * time.Millisecond) // They have waited more than three seconds, and the outage has lasted longer.
	for name, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) //nolint:errcheck
		buf := make([]byte, 1)
		n, err := conn.Read(buf)
		var timeout net.Error
		if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Errorf("%s was sent %d bytes (%v) while Postgres was away: it must be sent nothing, no status line and no header", name, n, err)
		}
	}
	both.proxy.Back()
	for name, conn := range conns {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second)) //nolint:errcheck
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Errorf("%s was not answered once Postgres was back: %v", name, err)
			continue
		}
		response.Body.Close()
		if response.StatusCode != 403 {
			t.Errorf("%s was answered %d once Postgres was back, want 403", name, response.StatusCode)
		}
	}
}
