//go:build integration

package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-029, the stream and api roles. A Postgres switchover or restart is the
// most ordinary event in Kubernetes; jellyfin-web treats any answer of 400 or
// more as the end of playback (DOMAINS.md), so a replica must not answer one
// because its database is briefly away. Each replica here reaches Postgres
// through a testenv.DBProxy of its own, so the outage is real: every connection
// ended and new ones refused, while the test reads Postgres directly.
//
// The workers' side of the same event is in internal/transcode/outage_*.

type replica struct {
	role   string
	proxy  *testenv.DBProxy
	server *httptest.Server
	log    *syncBuffer
}

// outage is the transcode env with two stream replicas and two api replicas of
// the outage kind, and the URLs a player uses.
type outage struct {
	*env
	segments store.Store
	streams  []*replica
	apis     []*replica
	hash     string

	master, main, init string // Paths with the query, for the first viewer.
	segmentURL         func(n int) string
	direct             string
	psid               string
}

func newReplica(t *testing.T, e *env, segments store.Store, role string) *replica {
	t.Helper()
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	pool, err := db.Open(context.Background(), proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rp := &replica{role: role, proxy: proxy, log: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(rp.log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// A role is "api", "stream", or "api,stream" for a process that runs both (JS-085).
	roles := strings.Split(role, ",")
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: roles, Log: log, SocketIdle: time.Minute}
	if slices.Contains(roles, RoleStream) {
		// A queued job older than two seconds is one nobody took: the outage below lasts far longer.
		s.Transcode = &transcode.Jobs{Pool: pool, Segments: segments, QueueTimeout: 2 * time.Second, Log: log}
	}
	rp.server = httptest.NewServer(s.Handler())
	t.Cleanup(rp.server.Close)
	return rp
}

func newOutage(t *testing.T) *outage {
	t.Helper()
	e, segments, source := transcodeEnv(t, "hevc", "aac")
	o := &outage{env: e, segments: segments}
	for range 2 {
		o.streams = append(o.streams, newReplica(t, e, segments, RoleStream))
		o.apis = append(o.apis, newReplica(t, e, segments, RoleAPI))
	}
	reply := e.playbackInfo(t, 0, e.item, map[string]any{"DeviceProfile": capturedProfile(t), "MediaSourceId": source})
	transcodingURL := firstSource(reply)["TranscodingUrl"].(string)
	values := mustQuery(t, transcodingURL)
	o.psid = values.Get("PlaySessionId")
	path := "/videos/" + db.DashedID(e.item)
	o.master = transcodingURL
	_, master := e.do(t, 0, "GET", transcodingURL, "", nil)
	o.main = path + "/main.m3u8?" + strings.TrimSpace(strings.SplitN(string(master), "main.m3u8?", 2)[1])
	_, main := e.do(t, 1, "GET", o.main, "", nil)
	lines := map[int]string{}
	for _, line := range strings.Split(string(main), "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "hls1/main/%d.mp4?", &n); err == nil {
			lines[n] = path + "/" + line
		}
	}
	o.segmentURL = func(n int) string { return lines[n] }
	o.init = strings.Replace(lines[2], "hls1/main/2.mp4", "hls1/main/-1.mp4", 1)
	o.hash = specHash(t, e, lowerKeys(mustQuery(t, lines[2])))
	o.direct = "/Videos/" + db.CompactID(e.item) + "/stream.mkv?Static=true&mediaSourceId=" + db.CompactID(e.item) + "&ApiKey=" + e.token
	// What a worker would have stored: the init and segment 2. Segment 1 is the one a running job
	// has yet to make, and 4 the one nobody has started.
	put := func(n int, body string) {
		if err := segments.Put(context.Background(), transcode.SegmentKey(o.hash, n), strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	put(2, "segment two")
	put(-1, "the init")
	mustExec(t, e.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at)
		VALUES ($1, '{}', 'someone else', 0, 0, 'running', 'w1', now() + interval '1 hour')`, o.hash)
	return o
}

func (o *outage) put(t *testing.T, n int, body string) {
	t.Helper()
	if err := o.segments.Put(context.Background(), transcode.SegmentKey(o.hash, n), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

// as is the URL with another viewer's play session, which is what the player
// of another device sends.
func (o *outage) as(path string, viewer int) string {
	return strings.Replace(path, "PlaySessionId="+o.psid, fmt.Sprintf("PlaySessionId=%032x", viewer+100), 1)
}

type answer struct {
	status int
	body   []byte
	err    error
	at     time.Time
}

func get(ctx context.Context, base, path string, headers ...string) answer {
	request, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return answer{err: err, at: time.Now()}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
	if err != nil {
		return answer{err: err, at: time.Now()}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return answer{status: response.StatusCode, body: body, err: err, at: time.Now()}
}

// request is one thing a player asks the stream role for.
type request struct {
	name    string
	path    func(viewer int) string
	headers []string
	status  int // The usual status.
}

func (o *outage) requests() []request {
	token := `MediaBrowser Token="` + o.token + `"`
	return []request{
		{"master playlist", func(v int) string { return o.as(o.master, v) }, nil, 200},
		{"main playlist", func(v int) string { return o.as(o.main, v) }, nil, 200},
		{"init segment", func(v int) string { return o.as(o.init, v) }, nil, 200},
		{"a stored segment", func(v int) string { return o.as(o.segmentURL(2), v) }, nil, 200},
		{"a segment its job has yet to make", func(v int) string { return o.as(o.segmentURL(1), v) }, nil, 200},
		{"a segment whose job was queued just before the outage", func(v int) string { return o.as(o.segmentURL(4), v) }, nil, 200},
		{"a direct play range", func(int) string { return o.direct }, []string{"Range", "bytes=100-299", "Authorization", token}, 206},
	}
}

// fakeWorker does what a worker does for the jobs the replicas queue, from the
// moment Postgres is back: it takes a queued job, stores its first segment,
// and finishes it. A job a replica cancels and queues again is taken too.
func (o *outage) fakeWorker(ctx context.Context) {
	for ctx.Err() == nil {
		rows, err := o.pool.Query(ctx, "SELECT id, start_segment FROM transcode_jobs WHERE spec_hash = $1 AND state = 'queued'", o.hash)
		if err == nil {
			type queued struct{ id, start int }
			var found []queued
			for rows.Next() {
				var q queued
				if rows.Scan(&q.id, &q.start) == nil {
					found = append(found, q)
				}
			}
			rows.Close()
			for _, q := range found {
				words := map[int]string{4: "four"}
				o.segments.Put(ctx, transcode.SegmentKey(o.hash, q.start), strings.NewReader("segment "+words[q.start]))                          //nolint:errcheck
				o.pool.Exec(ctx, `UPDATE transcode_jobs SET state = 'done', worker = 'w1', next_segment = start_segment + 1 WHERE id = $1`, q.id) //nolint:errcheck
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var (
	wentAway = regexp.MustCompile(`(?i)((postgres|database).*(away|unreachable|unavailable|is down|went|gone|lost|cannot be reached|not reachable))|((away|unreachable|unavailable|lost|cannot reach).*(postgres|database))`)
	cameBack = regexp.MustCompile(`(?i)((postgres|database).*(is back|came back|back again|reachable again|recovered|restored|returned|available again|answers again))|((back|again|recovered|restored).*(postgres|database))`)
)

// loggedTheOutage fails the test unless the log says Postgres went away and,
// after that, that it was back, and never calls a request that waited a failed one.
func loggedTheOutage(t *testing.T, who, log string) {
	t.Helper()
	lines := strings.Split(log, "\n")
	away := -1
	for i, line := range lines {
		if wentAway.MatchString(line) {
			away = i
			break
		}
	}
	back := false
	if away >= 0 {
		for _, line := range lines[away+1:] {
			back = back || cameBack.MatchString(line)
		}
	}
	if away < 0 || !back {
		t.Errorf("the log of %s must say when Postgres went away and when it was back (away line %d, back %v):\n%s", who, away, back, log)
	}
	for _, line := range lines {
		if strings.Contains(line, "request failed") {
			t.Errorf("the log of %s calls a request that waited a failed one: %s", who, line)
		}
	}
}

// For the whole of the longest wait a segment request may already have
// (WaitTimeout, 90 seconds), no stream-role request is answered 400 or more
// because Postgres is away, whichever of the player's requests it is, from two
// stream replicas and several viewers at once, and one the player abandoned
// after 10 seconds and asked again is no different. Each is answered as usual,
// at once or when Postgres is back. The api replicas answer too, and the
// health probes of all four replicas succeed throughout without waiting for
// Postgres: a probe that failed would take every replica out of service at once.
func TestNoStreamRequestIsAnsweredWithAnErrorWhilePostgresIsAway(t *testing.T) {
	o := newOutage(t)
	const viewers = 3
	// What each request answers with Postgres up, read from a replica that is not in the test.
	usual := map[string][]byte{}
	for _, rq := range o.requests() {
		if rq.name == "a segment its job has yet to make" || strings.Contains(rq.name, "queued") {
			continue
		}
		for v := range viewers {
			a := get(context.Background(), o.servers[0].URL, rq.path(v), rq.headers...)
			if a.err != nil || a.status != rq.status {
				t.Fatalf("%s before the outage: %d %v %s", rq.name, a.status, a.err, a.body)
			}
			usual[fmt.Sprint(rq.name, v)] = a.body
		}
	}
	wantBody := func(rq request, v int) []byte {
		switch {
		case strings.Contains(rq.name, "yet to make"):
			return []byte("segment one")
		case strings.Contains(rq.name, "queued"):
			return []byte("segment four")
		}
		return usual[fmt.Sprint(rq.name, v)]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var (
		mu       sync.Mutex
		backAt   time.Time
		wrong    []string
		finished sync.WaitGroup
	)
	check := func(who string, rq request, v int, a answer) {
		mu.Lock()
		defer mu.Unlock()
		during := backAt.IsZero()
		switch {
		case a.err != nil:
			wrong = append(wrong, fmt.Sprintf("%s: %s: %v", who, rq.name, a.err))
		case a.status >= 400:
			wrong = append(wrong, fmt.Sprintf("%s: %s was answered %d (%s), Postgres away %v", who, rq.name, a.status, bytes.TrimSpace(a.body), during))
		case a.status != rq.status || !bytes.Equal(a.body, wantBody(rq, v)):
			wrong = append(wrong, fmt.Sprintf("%s: %s was answered %d with %d bytes, want %d with %q", who, rq.name, a.status, len(a.body), rq.status, wantBody(rq, v)))
		}
	}
	ask := func(rp *replica, rq request, v int, abandon bool) {
		finished.Add(1)
		go func() {
			defer finished.Done()
			who := fmt.Sprintf("%s viewer %d", rp.server.URL, v)
			if abandon {
				// The player gives up after 10 seconds and asks again, three times; what it hears
				// in between is nothing, or an answer that must not be an error.
				for range 3 {
					attempt, stop := context.WithTimeout(ctx, 10*time.Second)
					a := get(attempt, rp.server.URL, rq.path(v), rq.headers...)
					stop()
					if a.err == nil {
						check(who+" (asking again)", rq, v, a)
						return
					}
				}
			}
			check(who, rq, v, get(ctx, rp.server.URL, rq.path(v), rq.headers...))
		}()
	}

	// The queue wait: viewers ask for a segment nobody has started, a job is queued for it,
	// and Postgres goes away before any worker takes it.
	requests := o.requests()
	queued := requests[5]
	for _, rp := range o.streams {
		for v := range viewers {
			ask(rp, queued, v, false)
		}
	}
	waitLong(t, "a job to be queued", func() bool {
		return scalar[int](t, o.pool, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'queued'", o.hash) >= 1
	})
	for _, rp := range append(append([]*replica{}, o.streams...), o.apis...) {
		rp.away(t, o.pool)
	}
	away := time.Now()
	for _, rp := range o.streams {
		for _, rq := range requests {
			if rq.name == queued.name {
				continue
			}
			for v := range viewers {
				ask(rp, rq, v, v == 0)
			}
		}
	}
	for _, rp := range o.apis {
		for v := range 2 {
			finished.Add(1)
			go func() {
				defer finished.Done()
				a := get(ctx, rp.server.URL, "/Users/Me", "Authorization", `MediaBrowser Token="`+o.token+`"`)
				if a.err != nil || a.status != 200 {
					mu.Lock()
					wrong = append(wrong, fmt.Sprintf("api replica %s request %d: %d %v", rp.server.URL, v, a.status, a.err))
					mu.Unlock()
				}
			}()
		}
	}

	// Health probes, for the whole of the outage and the 90 seconds a segment request may wait, and 5 more.
	var probes sync.WaitGroup
	for _, rp := range append(append([]*replica{}, o.streams...), o.apis...) {
		probes.Add(1)
		go func() {
			defer probes.Done()
			for time.Since(away) < 95*time.Second {
				// A probe that waits for Postgres is no probe: ten seconds is a hang, not a timing bar.
				attempt, stop := context.WithTimeout(ctx, 10*time.Second)
				a := get(attempt, rp.server.URL, "/healthz")
				stop()
				if a.err != nil || a.status < 200 || a.status > 299 {
					mu.Lock()
					wrong = append(wrong, fmt.Sprintf("GET /healthz on %s %v into the outage: %d %v", rp.server.URL, time.Since(away).Round(time.Second), a.status, a.err))
					mu.Unlock()
				}
				time.Sleep(time.Second)
			}
		}()
	}
	probes.Wait()
	mu.Lock()
	early := append([]string{}, wrong...)
	backAt = time.Now()
	mu.Unlock()
	if len(early) > 0 {
		t.Errorf("during the %v outage, %d answers were wrong; the first:\n  %s", time.Since(away).Round(time.Second), len(early), strings.Join(early[:min(len(early), 12)], "\n  "))
	}

	for _, rp := range append(append([]*replica{}, o.streams...), o.apis...) {
		rp.proxy.Back()
	}
	// The worker is back too: the job that was running makes segment 1, and a queued one is taken.
	o.put(t, 1, "segment one")
	mustExec(t, o.pool, "UPDATE transcode_jobs SET next_segment = 2 WHERE spec_hash = $1 AND state = 'running'", o.hash)
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	go o.fakeWorker(workerCtx)

	finished.Wait()
	mu.Lock()
	late := wrong[len(early):]
	mu.Unlock()
	if len(late) > 0 {
		t.Errorf("once Postgres was back, %d answers were wrong; the first:\n  %s", len(late), strings.Join(late[:min(len(late), 12)], "\n  "))
	}
	for _, rp := range append(append([]*replica{}, o.streams...), o.apis...) {
		loggedTheOutage(t, rp.role+" replica "+rp.server.URL, rp.log.String())
	}
}

// away ends the replica's sessions the way a switchover does, with Postgres's own
// FATAL 57P01 to the statement running or the next one (a closed socket is EOF
// to the client and no PgError, so a build that knows only that fails here), and
// then refuses new connections.
func (rp *replica) away(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := rp.proxy.Terminate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // The FATAL message reaches the client before the socket is closed.
	rp.proxy.Away()
}

func waitLong(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Postgres being away never lets a request in. A token that belongs to no
// signed-in device (one that never did, one signed out and refused before, one
// signed out a moment before the outage and asked about after any cache is old)
// gets no playlist, no segment and no film byte while Postgres is away, also from
// a replica that served other viewers a moment before, and is answered 401 at the
// latest when it is back. A viewer who is signed in is answered as usual.
func TestPostgresBeingAwayNeverLetsARequestInWithoutASignedInDevice(t *testing.T) {
	o := newOutage(t)
	_, refused := o.addUser(t, "refused")
	_, lateLeaver := o.addUser(t, "late")
	// A play session belongs to the user who started it, so each token plays one of its own.
	sessions := map[string]int{o.token: 0, refused: 1, lateLeaver: 2, "not-a-token-at-all": 3}
	asToken := func(path, token string) string {
		return o.as(strings.Replace(path, o.token, token, 1), sessions[token])
	}
	kinds := map[string]string{"master": o.master, "main": o.main, "init": o.init, "a stored segment": o.segmentURL(2), "direct play": o.direct}
	// Every replica has served the real token and both leavers' tokens a moment ago.
	for _, rp := range o.streams {
		for name, path := range kinds {
			for _, token := range []string{o.token, refused, lateLeaver} {
				if a := get(context.Background(), rp.server.URL, asToken(path, token)); a.status != 200 && a.status != 206 {
					t.Fatalf("%s with a signed-in token before the outage: %d %s", name, a.status, a.body)
				}
			}
		}
	}
	mustExec(t, o.pool, "DELETE FROM devices WHERE token = $1", refused)
	// Signed out, and refused by every replica.
	for _, rp := range o.streams {
		waitLong(t, "the signed-out token to be refused by "+rp.server.URL, func() bool {
			return get(context.Background(), rp.server.URL, asToken(kinds["main"], refused)).status == 401
		})
	}
	// Every replica has an entry for the late leaver that is fresh this very moment, whatever a
	// cache keeps, so a build that lets an entry that was fresh when Postgres went away stand
	// for the whole outage serves it a playlist, segments and film bytes at the 12 second mark.
	for _, rp := range o.streams {
		for name, path := range kinds {
			if a := get(context.Background(), rp.server.URL, asToken(path, lateLeaver)); a.status != 200 && a.status != 206 {
				t.Fatalf("%s with the late leaver's token just before it is signed out: %d %s", name, a.status, a.body)
			}
		}
	}
	mustExec(t, o.pool, "DELETE FROM devices WHERE token = $1", lateLeaver)
	for _, rp := range o.streams {
		rp.away(t, o.pool)
	}
	away := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var (
		mu       sync.Mutex
		problems []string
		wg       sync.WaitGroup
	)
	// A leaver's request may be answered 401 at any time, and never with anything else; a signed-in
	// one is answered with its usual status once Postgres is back, or sooner.
	ask := func(rp *replica, name, token, path string, delay time.Duration, want int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(delay)
			a := get(ctx, rp.server.URL, asToken(path, token))
			mu.Lock()
			defer mu.Unlock()
			if a.err != nil || a.status != want {
				problems = append(problems, fmt.Sprintf("%s %s with token %.8s: want %d, got %d (%d bytes) %v", rp.server.URL, name, token, want, a.status, len(a.body), a.err))
			}
		}()
	}
	for _, rp := range o.streams {
		for name, path := range kinds {
			ask(rp, name, "not-a-token-at-all", path, 0, 401)
			ask(rp, name, refused, path, 0, 401)
			// The one signed out a moment before the outage is asked about once any cache is old.
			ask(rp, name, lateLeaver, path, 12*time.Second, 401)
			ask(rp, name, o.token, path, 0, 200)
		}
	}
	time.Sleep(max(0, 16*time.Second-time.Since(away)))
	for _, rp := range o.streams {
		rp.proxy.Back()
	}
	wg.Wait()
	if len(problems) > 0 {
		t.Errorf("%s", strings.Join(problems, "\n"))
	}
}

// With Postgres up, whatever a stream replica keeps in memory is short-lived: a
// signed-out token is refused, and a film whose source row the scan changed
// (a replaced file) is served from the new row, by every stream replica. The
// criterion says within 10 seconds; the check waits for it, bounded at ten
// times that to catch a hang, and logs how long each took.
func TestAStreamReplicaForgetsASignedOutTokenAndAReplacedFile(t *testing.T) {
	o := newOutage(t)
	_, leaver := o.addUser(t, "leaver")
	asToken := func(path, token string) string { return strings.Replace(path, o.token, token, 1) }
	for _, rp := range o.streams {
		for _, token := range []string{o.token, leaver} {
			if a := get(context.Background(), rp.server.URL, asToken(o.direct, token), "Range", "bytes=0-9"); a.status != 206 || !bytes.Equal(a.body, o.body[:10]) {
				t.Fatalf("before: %d %q", a.status, a.body)
			}
		}
	}
	mustExec(t, o.pool, "DELETE FROM devices WHERE token = $1", leaver)
	changed := time.Now()
	dir := strings.TrimPrefix(o.uri, "file://")
	replacement := bytes.Repeat([]byte("replaced-file!!!"), 4096)
	if err := os.WriteFile(filepath.Join(dir, "film2.mkv"), replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	mustExec(t, o.pool, "UPDATE media_sources SET store_key = 'film2.mkv', object_etag = 'new', size = $1", len(replacement))
	for _, rp := range o.streams {
		for _, check := range []struct {
			name string
			done func() bool
		}{
			{"the signed-out token to be refused", func() bool {
				return get(context.Background(), rp.server.URL, asToken(o.direct, leaver), "Range", "bytes=0-9").status == 401
			}},
			{"the replaced file to be served", func() bool {
				a := get(context.Background(), rp.server.URL, o.direct, "Range", "bytes=0-9")
				return a.status == 206 && bytes.Equal(a.body, replacement[:10])
			}},
		} {
			deadline := time.Now().Add(100 * time.Second)
			for !check.done() {
				if time.Now().After(deadline) {
					t.Fatalf("%s never happened on %s", check.name, rp.server.URL)
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Logf("%s on %s: %v after the change", check.name, rp.server.URL, time.Since(changed).Round(100*time.Millisecond))
		}
	}
}

// The probe of a replica never waits for Postgres: with it away, GET /healthz on
// two stream and two api replicas answers 2xx throughout, also while their other
// requests wait, so no probe takes them all out of service together.
func TestHealthzAnswersWhilePostgresIsAwayAndOtherRequestsWait(t *testing.T) {
	o := newOutage(t)
	all := append(append([]*replica{}, o.streams...), o.apis...)
	for _, rp := range all {
		rp.proxy.Away()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var waiting sync.WaitGroup
	for _, rp := range o.streams {
		waiting.Add(1)
		go func() { defer waiting.Done(); get(ctx, rp.server.URL, o.segmentURL(2)) }()
	}
	for _, rp := range o.apis {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			get(ctx, rp.server.URL, "/Users/Me", "Authorization", `MediaBrowser Token="`+o.token+`"`)
		}()
	}
	failed := map[string]bool{} // Said once per replica.
	for round := range 20 {
		for _, rp := range all {
			attempt, stop := context.WithTimeout(ctx, 10*time.Second) // A hang, not a timing bar.
			a := get(attempt, rp.server.URL, "/healthz")
			stop()
			if (a.err != nil || a.status < 200 || a.status > 299) && !failed[rp.server.URL] {
				failed[rp.server.URL] = true
				t.Errorf("round %d: GET /healthz on the %s replica %s: %d %v", round, rp.role, rp.server.URL, a.status, a.err)
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	for _, rp := range all {
		rp.proxy.Back()
	}
	waiting.Wait()
}
