//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-031. A replica is killed, rolled or cut off from Postgres while viewers
// are on it. Every replica here is its own process (the SIGKILL and the SIGTERM
// are real), behind a testenv.DBProxy of its own where Postgres has to go away,
// and the checks read Postgres directly. Names the task implies, which do not
// exist yet: the route GET /readyz, and the config key shutdownGrace.
//
// The timings are the criteria's own and are run at that length: a replica
// that is cut off keeps readiness up for 20 s and has lost it by 40 s, and a
// killed replica's claim on a device is gone within two minutes.

// readinessLead is the least time after its first 503 that a replica still
// accepts new requests. The task says the time is documented; a router that
// polls readiness every second or two needs this much.
const readinessLead = 3 * time.Second

const adminPassword = "failover-pass"

var buildOnce sync.Once
var builtDir string
var buildErr error

// binary builds jellyscale once for every test in the package; TestMain removes it.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		builtDir, buildErr = os.MkdirTemp("", "jellyscale-failover-")
		if buildErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o", filepath.Join(builtDir, "jellyscale"), ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(builtDir, "jellyscale")
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		os.RemoveAll(builtDir) //nolint:errcheck
	}
	os.Exit(code)
}

// replica is one jellyscale process of one role.
type replica struct {
	t      *testing.T
	cmd    *exec.Cmd
	addr   string
	log    *bytes.Buffer
	exited chan struct{}
	status error
	id     string // What it calls itself in session_sockets.replica, once a socket on it has been seen.
}

// start runs a replica of the role against dsn, with the extra config keys, and
// waits for its liveness route. Its PATH holds no ffmpeg, as in production.
func start(t *testing.T, role, dsn string, extra map[string]any) *replica {
	t.Helper()
	config := map[string]any{"name": "failover", "adminUser": "admin"}
	for k, v := range extra {
		config[k] = v
	}
	raw, _ := json.Marshal(config)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &replica{t: t, addr: freeAddr(t), log: &bytes.Buffer{}, exited: make(chan struct{})}
	r.cmd = exec.Command(binary(t), "--roles="+role, "--listen", r.addr, "--config", path)
	r.cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+dsn, "JELLYSCALE_ADMIN_PASSWORD="+adminPassword, "PATH="+dir)
	r.cmd.Stderr, r.cmd.Stdout = &lockedWriter{w: r.log}, &lockedWriter{w: r.log}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { r.status = r.cmd.Wait(); close(r.exited) }()
	t.Cleanup(func() { r.cmd.Process.Kill(); <-r.exited }) //nolint:errcheck
	waitFor(t, 60*time.Second, "the "+role+" replica to answer /healthz", func() bool {
		select {
		case <-r.exited:
			t.Fatalf("the %s replica exited: %v\n%s", role, r.status, r.log)
		default:
		}
		return r.status2xx("/healthz")
	})
	return r
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (r *replica) url(path string) string { return "http://" + r.addr + path }

// code is the status of a GET on a connection of its own, or 0.
func (r *replica) code(path string) int {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	response, err := client.Get(r.url(path))
	if err != nil {
		return 0
	}
	response.Body.Close()
	return response.StatusCode
}

func (r *replica) status2xx(path string) bool { c := r.code(path); return c >= 200 && c < 300 }

func (r *replica) alive() bool {
	select {
	case <-r.exited:
		return false
	default:
		return true
	}
}

func (r *replica) sigterm() { r.cmd.Process.Signal(syscall.SIGTERM) } //nolint:errcheck
func (r *replica) kill()    { r.cmd.Process.Kill(); <-r.exited }      //nolint:errcheck

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", limit, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// tokenFor signs a device in on a throwaway api replica of its own, for the
// tests of a stream replica, which has no sign-in route.
func tokenFor(t *testing.T, dsn string) string { return start(t, "api", dsn, nil).signIn(t) }

// signIn is one device's sign-in on a replica: a token of its own.
func (r *replica) signIn(t *testing.T) string {
	t.Helper()
	device := uuid.NewString()
	request, _ := http.NewRequest("POST", r.url("/Users/AuthenticateByName"), strings.NewReader(`{"Username":"admin","Pw":"`+adminPassword+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", `MediaBrowser Client="failover", Device="d", DeviceId="`+device+`", Version="1"`)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var reply struct{ AccessToken string }
	if response.StatusCode != 200 || json.Unmarshal(body, &reply) != nil || reply.AccessToken == "" {
		t.Fatalf("sign-in: %d %s", response.StatusCode, body)
	}
	return reply.AccessToken
}

// socket is a device's open WebSockets, kept alive as a browser does.
type socket struct {
	conn *websocket.Conn
	stop context.CancelFunc
	read chan error // Gets the error that ended the reading.
}

// dial opens /socket on a replica for a device, reads ForceKeepAlive, and from
// then on answers like a live client: it sends KeepAlive every 5 s, so the
// replica's own idle limit (60 s) never closes it. Six times as often as
// jellyfin-web does, which a check on how soon a row goes stale must not rely on:
// those use dialEvery with jellyfin-web's 30 s.
func (r *replica) dial(t *testing.T, token string) *socket {
	t.Helper()
	return r.dialEvery(t, token, 5*time.Second)
}

// dialEvery is dial with the client's keepalive period. jellyfin-web answers
// ForceKeepAlive (Data 60) by sending KeepAlive every 30 s, so a row is
// refreshed that rarely.
func (r *replica) dialEvery(t *testing.T, token string, period time.Duration) *socket {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, "ws://"+r.addr+"/socket?ApiKey="+token, nil)
	if err != nil {
		cancel()
		t.Fatalf("dial /socket on %s: %v", r.addr, err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(data), "ForceKeepAlive") {
		cancel()
		t.Fatalf("the first message on /socket: %q %v", data, err)
	}
	s := &socket{conn: conn, stop: cancel, read: make(chan error, 1)}
	t.Cleanup(func() { cancel(); conn.CloseNow() }) //nolint:errcheck
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				s.read <- err
				return
			}
		}
	}()
	go func() {
		for ctx.Err() == nil {
			conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`)) //nolint:errcheck
			select {
			case <-ctx.Done():
			case <-time.After(period):
			}
		}
	}()
	return s
}

// owner is sessions.ws_replica of a device, "" for NULL.
func owner(t *testing.T, pool *pgxpool.Pool, token string) string {
	t.Helper()
	var replica *string
	if err := pool.QueryRow(context.Background(), "SELECT ws_replica FROM sessions WHERE token = $1", token).Scan(&replica); err != nil {
		return ""
	}
	if replica == nil {
		return ""
	}
	return *replica
}

// idOf learns what a replica calls itself from the row of a socket just opened on it.
func idOf(t *testing.T, pool *pgxpool.Pool, token string) string {
	t.Helper()
	var id string
	waitFor(t, 10*time.Second, "the socket's row", func() bool {
		return pool.QueryRow(context.Background(), "SELECT replica FROM session_sockets WHERE token = $1 ORDER BY connected_at DESC LIMIT 1", token).Scan(&id) == nil
	})
	return id
}

// --- ownership of a device's socket when a replica is killed -------------------

// A device whose browser reconnects to the survivor: the survivor owns it from
// the moment its socket is open, the killed replica never again, also after the
// new socket closes.
func TestAKilledReplicasDeviceReconnectsAndTheSurvivorOwnsIt(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	first, second := start(t, "api", dsn, nil), start(t, "api", dsn, nil)
	token := first.signIn(t)
	old := first.dial(t, token)
	dead := idOf(t, pool, token)
	if got := owner(t, pool, token); got != dead {
		t.Fatalf("ws_replica %q before the kill, want %q", got, dead)
	}
	first.kill()
	if err := <-old.read; websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		t.Fatalf("a killed replica sent a normal close: %v", err)
	}

	reconnected := second.dial(t, token)
	var live string
	waitFor(t, 10*time.Second, "ws_replica to name the replica that holds the new socket", func() bool {
		live = owner(t, pool, token)
		return live != "" && live != dead
	})
	// Never the killed one again while the new socket is open.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if got := owner(t, pool, token); got != live {
			t.Fatalf("ws_replica went from %q to %q with the new socket still open (killed replica %q)", live, got, dead)
		}
	}
	// And not after it closes: then NULL, or the newest socket on another live replica.
	reconnected.conn.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if got := owner(t, pool, token); got == dead {
			t.Fatalf("after the new socket closed ws_replica named the killed replica %q", dead)
		}
	}
	waitFor(t, 10*time.Second, "ws_replica to clear once the only live socket closed", func() bool { return owner(t, pool, token) == "" })
}

// A device that does not reconnect (its browser is closed, or asleep): within two
// minutes of the kill ws_replica names no killed replica, with no socket of the
// device closing first. This one runs the full two minutes, as its criterion says.
func TestADeviceThatDoesNotReconnectIsNotOwnedByTheKilledReplica(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	first, _ := start(t, "api", dsn, nil), start(t, "api", dsn, nil)
	token := first.signIn(t)
	first.dial(t, token)
	dead := idOf(t, pool, token)
	first.kill()
	killedAt := time.Now()
	waitFor(t, 2*time.Minute+3*time.Second, "ws_replica to stop naming the killed replica", func() bool { return owner(t, pool, token) != dead })
	t.Logf("ws_replica stopped naming the killed replica %v after the kill", time.Since(killedAt).Round(time.Second))
	if got := owner(t, pool, token); got != "" {
		t.Errorf("no socket of the device is open anywhere, yet ws_replica is %q", got)
	}
}

// A live device is not swept with the dead one. jellyfin-web sends KeepAlive every
// 30 s, so a replica that treats a row quiet for less than that (or for a little
// more, 20 s say) as dead deletes every live device's row between keepalives, and
// the per-frame refresh matches no row afterwards: ws_replica stays NULL for a
// device with an open socket. This device keeps its socket alive every 30 s on the
// survivor while the other replica is killed, and must be named by the survivor at
// every look from the kill until the killed replica's device has been cleared (at
// least 70 s, so the sweep has run over rows that are 30 s quiet many times).
func TestASocketKeptAliveEveryThirtySecondsStaysOwnedWhileAnotherReplicaIsKilledAndSwept(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	first, second := start(t, "api", dsn, nil), start(t, "api", dsn, nil)
	doomed, live := first.signIn(t), second.signIn(t)
	first.dial(t, doomed)
	second.dialEvery(t, live, 30*time.Second)
	dead, survivor := idOf(t, pool, doomed), idOf(t, pool, live)
	first.kill()
	killedAt := time.Now()
	for {
		if got := owner(t, pool, live); got != survivor {
			t.Fatalf("%v after the kill ws_replica of the device with an open socket on a live replica is %q, want %q", time.Since(killedAt).Round(time.Second), got, survivor)
		}
		if time.Since(killedAt) > 70*time.Second && owner(t, pool, doomed) != dead {
			return
		}
		if time.Since(killedAt) > 2*time.Minute+3*time.Second {
			t.Fatalf("the killed replica still owns its device after %v", time.Since(killedAt).Round(time.Second))
		}
		time.Sleep(time.Second)
	}
}

// A device with sockets on two replicas, the newer on the one that was killed:
// within two minutes the live replica owns it.
func TestADeviceWithASocketOnTheSurvivorIsOwnedByItWhenTheNewerOneDies(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	first, second := start(t, "api", dsn, nil), start(t, "api", dsn, nil)
	token := first.signIn(t)
	second.dial(t, token)
	live := idOf(t, pool, token)
	first.dial(t, token)
	dead := idOf(t, pool, token)
	if dead == live || owner(t, pool, token) != dead {
		t.Fatalf("setup: killed replica %q, live %q, ws_replica %q", dead, live, owner(t, pool, token))
	}
	first.kill()
	waitFor(t, 2*time.Minute+3*time.Second, "ws_replica to name the live replica", func() bool { return owner(t, pool, token) == live })
	// It stays there.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if got := owner(t, pool, token); got != live {
			t.Fatalf("ws_replica went from the live replica %q to %q", live, got)
		}
	}
}

// SIGTERM: the replica closes its WebSockets and removes their rows before it
// exits, so from its exit ws_replica names it for no device, and no row is left.
func TestAReplicaStoppedWithSIGTERMClosesItsSocketsAndRemovesTheirRows(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	first := start(t, "api", dsn, map[string]any{"shutdownGrace": "20s"})
	var tokens []string
	var sockets []*socket
	for range 3 {
		token := first.signIn(t)
		tokens = append(tokens, token)
		sockets = append(sockets, first.dial(t, token))
	}
	id := idOf(t, pool, tokens[0])
	first.sigterm()
	for i, s := range sockets {
		select {
		case err := <-s.read:
			if websocket.CloseStatus(err) == -1 {
				t.Errorf("socket %d ended without a close frame: %v", i, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("socket %d was not closed by a replica that got SIGTERM", i)
		}
	}
	select {
	case <-first.exited:
	case <-time.After(40 * time.Second):
		t.Fatal("the replica did not exit")
	}
	// From its exit, nothing names it.
	var rows, claims int
	pool.QueryRow(context.Background(), "SELECT count(*) FROM session_sockets WHERE replica = $1", id).Scan(&rows) //nolint:errcheck
	pool.QueryRow(context.Background(), "SELECT count(*) FROM sessions WHERE ws_replica = $1", id).Scan(&claims)   //nolint:errcheck
	if rows != 0 || claims != 0 {
		t.Errorf("at its exit the replica left %d socket rows and %d devices claimed", rows, claims)
	}
}

// --- readiness and the drain ------------------------------------------------------

// film seeds a direct-play film too big for the socket buffers, in a library
// the replicas were not told of (they read the rows), and returns its stream URL path.
func film(t *testing.T, pool *pgxpool.Pool, token string, size int64) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil { // Sparse: the read gets zeros.
		t.Fatal(err)
	}
	f.Close()
	ctx := context.Background()
	library, item := uuid.NewString(), uuid.NewString()
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, $2, 'movies', $3, $3)`, library, "drain "+library, "file://"+dir)
	exec(`INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', 'Drain Film', 'drain film', $4, 1000000000, $3)`, item, library, fmt.Sprintf(`{"Name":"Drain Film","MediaSources":[{
		"Id":%q,"Protocol":"File","Container":"mkv","SupportsTranscoding":true,
		"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Audio","IsDefault":true}]}]}`, db.CompactID(item)), "drain "+item)
	exec(`INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		VALUES ($1, $1, 'film.mkv', $2, 'abc123', 'mkv', '[]')`, item, size)
	return "/Videos/" + db.CompactID(item) + "/stream.mkv?Static=true&mediaSourceId=" + db.CompactID(item) + "&ApiKey=" + token
}

// holdOpen starts a direct play that the client does not read: the replica
// fills its socket buffers and then waits for a response that does not end.
func holdOpen(t *testing.T, r *replica, path string) (headers chan *http.Response) {
	t.Helper()
	headers = make(chan *http.Response, 1)
	go func() {
		response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get(r.url(path))
		if err != nil {
			close(headers)
			return
		}
		headers <- response
	}()
	select {
	case response, ok := <-headers:
		if !ok || response.StatusCode != 200 {
			t.Fatalf("the direct play did not start: %v", response)
		}
		t.Cleanup(func() { response.Body.Close() })
	case <-time.After(20 * time.Second):
		t.Fatal("the direct play did not start")
	}
	return headers
}

// Readiness is its own route, answered without a token by api and stream alike,
// and liveness keeps answering 2xx.
func TestReadinessIsOpenToEveryRoleAndLivenessIsSeparate(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	for _, role := range []string{"api", "stream"} {
		r := start(t, role, dsn, nil)
		if got := r.code("/readyz"); got != 200 {
			t.Errorf("%s: GET /readyz with no token answered %d, want 200", role, got)
		}
		if got := r.code("/healthz"); got < 200 || got > 299 {
			t.Errorf("%s: GET /healthz answered %d, want 2xx", role, got)
		}
	}
}

// On SIGTERM readiness answers 503 from then on, and before anything is refused:
// for the documented lead after the first 503 the replica takes new requests and
// answers them normally, so a router that polls readiness loses none. /healthz
// goes on answering 2xx through the drain. A held-open response keeps the process
// in its drain, so what is looked at is the drain, not the exit.
func TestOnSIGTERMReadinessFailsFirstAndRequestsAreStillAnsweredForTheLead(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	for _, role := range []string{"api", "stream"} {
		t.Run(role, func(t *testing.T) {
			r := start(t, role, dsn, map[string]any{"shutdownGrace": "40s"})
			path := "/System/Info/Public"
			if role == "stream" {
				path = film(t, pool, tokenFor(t, dsn), 512<<20)
				holdOpen(t, r, path) // The held response keeps the process in its drain.
			}
			r.sigterm()
			var first time.Time
			waitFor(t, 5*time.Second, "readiness to answer 503 after SIGTERM", func() bool {
				if r.code("/readyz") == 503 {
					first = time.Now()
					return true
				}
				return false
			})
			for time.Since(first) < readinessLead {
				if got := r.code("/readyz"); got != 503 {
					t.Fatalf("readiness answered %d after it had answered 503", got)
				}
				if got := r.code("/healthz"); got < 200 || got > 299 {
					t.Fatalf("/healthz answered %d during the drain", got)
				}
				if got := r.getCode(path, "Range", "bytes=0-99"); got != 200 && got != 206 {
					t.Fatalf("%v after the first 503, a new request for %s was answered %d", time.Since(first).Round(time.Millisecond), path, got)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

func (r *replica) getCode(path string, headers ...string) int {
	request, _ := http.NewRequest("GET", r.url(path), nil)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)) //nolint:errcheck
	return response.StatusCode
}

// The grace is one config setting: a replica with a response that never ends
// exits when it is over, and the exit follows the value. Two replicas get SIGTERM
// together, one with 5 s and one with 40 s: the first is gone while the second
// still serves its response, and the second goes by itself at its own time.
func TestTheExitOfAReplicaWithAnEndlessResponseFollowsTheGrace(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	short := start(t, "stream", dsn, map[string]any{"shutdownGrace": "5s"})
	long := start(t, "stream", dsn, map[string]any{"shutdownGrace": "40s"})
	path := film(t, pool, tokenFor(t, dsn), 512<<20)
	holdOpen(t, short, path)
	holdOpen(t, long, path)
	began := time.Now()
	short.sigterm()
	long.sigterm()
	select {
	case <-short.exited:
	case <-time.After(5*time.Second + 15*time.Second):
		t.Fatal("a replica with a grace of 5 s and a response that never ends was still running 20 s after SIGTERM")
	}
	t.Logf("the replica with a grace of 5 s exited %v after SIGTERM", time.Since(began).Round(100*time.Millisecond))
	if !long.alive() {
		t.Fatal("the replica with a grace of 40 s was gone when the one with 5 s went: the grace is not what ended it")
	}
	// Cut at its end: the held response is not a whole film.
	select {
	case <-long.exited:
	case <-time.After(40*time.Second + 15*time.Second):
		t.Fatal("a replica with a grace of 40 s and a response that never ends was still running 55 s after SIGTERM")
	}
	if elapsed := time.Since(began); elapsed < 30*time.Second {
		t.Errorf("the replica with a grace of 40 s exited after %v, with its response still unsent", elapsed.Round(time.Second))
	}
}

// A response that ends within the grace is sent whole, and the process then
// exits by itself, without waiting out the grace.
func TestAResponseInFlightIsSentWholeAndTheProcessThenExitsByItself(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	r := start(t, "stream", dsn, map[string]any{"shutdownGrace": "60s"})
	const size = 160 << 20
	path := film(t, pool, tokenFor(t, dsn), size)
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get(r.url(path))
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("direct play: %v %v", response, err)
	}
	defer response.Body.Close()
	// Read slowly (about 40 MiB/s), so that most of the film is still to come when SIGTERM arrives.
	buf := make([]byte, 1<<20)
	var got int64
	for got < 8<<20 {
		n, err := response.Body.Read(buf)
		got += int64(n)
		if err != nil {
			t.Fatalf("read %d bytes: %v", got, err)
		}
	}
	r.sigterm()
	for {
		n, err := response.Body.Read(buf)
		got += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the response was cut after %d of %d bytes: %v", got, int64(size), err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got != size {
		t.Fatalf("the response had %d bytes, want %d", got, int64(size))
	}
	select {
	case <-r.exited:
		if r.status != nil {
			t.Errorf("the replica exited with %v", r.status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the replica was still running 15 s after its last response ended (its grace is 60 s)")
	}
}

// A grace that is not a duration, or is negative, stops the start naming the key;
// the unit test in main_test.go holds the same for the parsing. Here the process
// itself refuses.
func TestAReplicaWithAnUnreadableGraceDoesNotStart(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	for _, value := range []string{`"soon"`, `"-5s"`, `30`, `"2 minutes"`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		os.WriteFile(path, []byte(`{"shutdownGrace": `+value+`}`), 0o644) //nolint:errcheck
		cmd := exec.Command(binary(t), "--roles=api", "--listen", freeAddr(t), "--config", path)
		cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+dsn, "PATH="+dir)
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			cmd.Process.Kill() //nolint:errcheck
			t.Errorf("shutdownGrace %s: the replica started", value)
			continue
		}
		if err == nil || !strings.Contains(string(out), "shutdownGrace") || strings.Contains(string(out), "unknown field") {
			t.Errorf("shutdownGrace %s: %v, output %q", value, err, out)
		}
	}
}

// --- Postgres away ---------------------------------------------------------------

// proxied starts a replica of the role whose every connection goes through a
// proxy of its own.
func proxied(t *testing.T, role string, extra map[string]any) (*replica, *testenv.DBProxy) {
	t.Helper()
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	return start(t, role, proxy.DSN(), extra), proxy
}

// readiness is polled every 500 ms for as long as the test runs, and every
// answer is kept with its time.
type readiness struct {
	mu      sync.Mutex
	answers []poll
	stop    chan struct{}
}

type poll struct {
	at            time.Time
	ready, health int
}

func watch(r *replica) *readiness {
	w := &readiness{stop: make(chan struct{})}
	go func() {
		for {
			select {
			case <-w.stop:
				return
			case <-time.After(500 * time.Millisecond):
			}
			p := poll{at: time.Now(), ready: r.code("/readyz"), health: r.code("/healthz")}
			w.mu.Lock()
			w.answers = append(w.answers, p)
			w.mu.Unlock()
		}
	}()
	return w
}

func (w *readiness) end() []poll {
	close(w.stop)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.answers
}

// first is the first time readiness answered other than 200 after from, or zero.
func firstNot200(polls []poll, from time.Time) (time.Time, bool) {
	for _, p := range polls {
		if p.at.After(from) && p.ready != 200 {
			return p.at, true
		}
	}
	return time.Time{}, false
}

func healthStayedUp(t *testing.T, polls []poll) {
	t.Helper()
	for _, p := range polls {
		if p.health < 200 || p.health > 299 {
			t.Errorf("/healthz answered %d at %v", p.health, p.at.Format("15:04:05.000"))
		}
	}
}

// authenticated is a request that needs the database, started in the background.
func (r *replica) ask(token, path string) chan int {
	answered := make(chan int, 1)
	go func() {
		request, _ := http.NewRequest("GET", r.url(path), nil)
		request.Header.Set("Authorization", `MediaBrowser Token="`+token+`"`)
		response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
		if err != nil {
			answered <- 0
			return
		}
		response.Body.Close()
		answered <- response.StatusCode
	}()
	return answered
}

// Postgres away for less than a switchover (20 s): readiness stays 200 throughout,
// and a request made meanwhile waits and is then answered without an error.
func TestReadinessStaysUpWhilePostgresIsAwayForTwentySeconds(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"api", "stream"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			r, proxy := proxied(t, role, nil)
			token := tokenFor(t, testenv.DatabaseURL(t)) // Signing in is an api route, and goes straight to Postgres.
			w := watch(r)
			proxy.Away()
			began := time.Now()
			path := "/Users/Me"
			if role == "stream" {
				path = "/Videos/" + uuid.NewString() + "/stream.mkv?Static=true" // Needs the database for the token first.
			}
			waiting := r.ask(token, path)
			time.Sleep(20 * time.Second)
			if got := r.code("/readyz"); got != 200 {
				t.Errorf("readiness answered %d with Postgres away for 20 s, want 200", got)
			}
			select {
			case got := <-waiting:
				t.Fatalf("a request that needs Postgres was answered %d while it was away", got)
			default:
			}
			proxy.Back()
			select {
			case got := <-waiting:
				if got >= 500 || got == 0 {
					t.Errorf("the request that waited was answered %d, not as usual", got)
				}
			case <-time.After(30 * time.Second):
				t.Error("the request that waited was never answered")
			}
			polls := w.end()
			if when, bad := firstNot200(polls, began); bad {
				t.Errorf("readiness failed at %v, %v after Postgres went away", when.Format("15:04:05.000"), when.Sub(began).Round(time.Second))
			}
			healthStayedUp(t, polls)
		})
	}
}

// Several short outages that add up to more than 30 s never fail readiness: it is
// a continuous outage that does, not the sum.
func TestSeveralShortOutagesNeverFailReadiness(t *testing.T) {
	t.Parallel()
	r, proxy := proxied(t, "api", nil)
	token := r.signIn(t)
	w := watch(r)
	began := time.Now()
	for range 4 {
		proxy.Away()
		time.Sleep(9 * time.Second)
		proxy.Back()
		// Between them Postgres answers and the replica uses it.
		waitFor(t, 20*time.Second, "a request to be answered once Postgres is back", func() bool { return r.getCode("/Users/Me", "Authorization", `MediaBrowser Token="`+token+`"`) == 200 })
		time.Sleep(5 * time.Second)
	}
	polls := w.end()
	if when, bad := firstNot200(polls, began); bad {
		t.Errorf("readiness failed at %v, %v in, with no outage longer than 9 s", when.Format("15:04:05.000"), when.Sub(began).Round(time.Second))
	}
	healthStayedUp(t, polls)
}

// Postgres away continuously: 503 by 40 s on the replica that cannot reach it and
// only on it; the request already waiting goes on waiting, and readiness is back
// within seconds of Postgres answering, with no restart.
func TestReadinessFailsByFortySecondsOnlyOnTheReplicaThatCannotReachPostgres(t *testing.T) {
	t.Parallel()
	cut, proxy := proxied(t, "api", nil)
	other, _ := proxied(t, "api", nil)
	pool := connect(t, testenv.DatabaseURL(t))
	token := cut.signIn(t)
	// A device on the cut replica, kept alive as jellyfin-web does (every 30 s).
	cut.dialEvery(t, token, 30*time.Second)
	cutID := idOf(t, pool, token)
	wc, wo := watch(cut), watch(other)
	began := time.Now()
	proxy.Away()
	waiting := cut.ask(token, "/Users/Me")
	var failedAt time.Time
	waitFor(t, 41*time.Second, "readiness to fail on the replica that is cut off (503 by 40 s)", func() bool {
		if cut.code("/readyz") == 503 {
			failedAt = time.Now()
			return true
		}
		return false
	})
	if failedAt.Sub(began) < 20*time.Second {
		t.Errorf("readiness failed %v after Postgres went away, which is still a switchover's length", failedAt.Sub(began).Round(time.Second))
	}
	select {
	case got := <-waiting:
		t.Fatalf("the waiting request was answered %d once readiness failed: it must go on waiting", got)
	default:
	}
	if got := other.code("/readyz"); got != 200 {
		t.Errorf("the replica that reaches Postgres answered %d", got)
	}
	pid := cut.cmd.Process.Pid
	proxy.Back()
	backAt := time.Now()
	waitFor(t, 20*time.Second, "readiness to be 200 again", func() bool { return cut.code("/readyz") == 200 })
	// The rows could not be refreshed for 40 s and the other replica has swept
	// since: the device's socket is still open, so within one keepalive of Postgres
	// answering the cut replica names it again (a build that deleted the row and
	// only updates by connection never would).
	waitFor(t, 35*time.Second, "ws_replica to name the cut replica again for the device whose socket stayed open", func() bool { return owner(t, pool, token) == cutID })
	t.Logf("readiness was 200 again %v after Postgres answered", time.Since(backAt).Round(100*time.Millisecond))
	select {
	case got := <-waiting:
		if got == 0 || got >= 500 {
			t.Errorf("the request that waited through the outage was answered %d", got)
		}
	case <-time.After(30 * time.Second):
		t.Error("the request that waited was never answered")
	}
	if !cut.alive() || cut.cmd.Process.Pid != pid {
		t.Error("the replica restarted")
	}
	healthStayedUp(t, wc.end())
	polls := wo.end()
	if when, bad := firstNot200(polls, began); bad {
		t.Errorf("readiness failed on the replica that reaches Postgres, at %v", when.Format("15:04:05.000"))
	}
}

// SIGTERM while Postgres is away, before readiness has failed for it and after:
// readiness is 503 at once, and the process exits by the end of the grace, though
// its requests wait on the database and its socket row cannot be removed.
func TestSIGTERMWhilePostgresIsAwayFailsReadinessAtOnceAndExitsByTheGrace(t *testing.T) {
	t.Parallel()
	for _, when := range []struct {
		name  string
		after time.Duration
	}{{"before readiness has failed for it", 5 * time.Second}, {"after readiness has failed for it", 45 * time.Second}} {
		t.Run(when.name, func(t *testing.T) {
			t.Parallel()
			r, proxy := proxied(t, "api", map[string]any{"shutdownGrace": "10s"})
			token := r.signIn(t)
			r.dial(t, token)
			proxy.Away()
			waiting := r.ask(token, "/Users/Me")
			time.Sleep(when.after)
			wasReady := r.code("/readyz")
			if (when.after < 20*time.Second) != (wasReady == 200) {
				t.Fatalf("setup: readiness answered %d with Postgres away for %v", wasReady, when.after)
			}
			began := time.Now()
			r.sigterm()
			waitFor(t, 2*time.Second, "readiness to answer 503 at once after SIGTERM", func() bool { return r.code("/readyz") == 503 })
			select {
			case <-r.exited:
			case <-time.After(10*time.Second + 20*time.Second):
				t.Fatalf("the replica was still running %v after SIGTERM, with a grace of 10 s and Postgres away", time.Since(began).Round(time.Second))
			}
			// Whatever the waiting request got, it was not held past the process.
			select {
			case <-waiting:
			case <-time.After(5 * time.Second):
				t.Error("the request that waited was left hanging by a process that exited")
			}
		})
	}
}
