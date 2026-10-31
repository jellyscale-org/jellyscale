package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playstate"
	"jellyscale/internal/store"
)

// These need a disposable database; test/integration.sh provides one.

const filmTicks = int64(100_000_000_000) // Ten thousand seconds in ticks.

type env struct {
	pool       *pgxpool.Pool
	servers    []*httptest.Server // Two "replicas", each with its own pool.
	apis       []*Server
	user       string
	token      string
	other      string // Another user's ID.
	otherToken string
	item       string
	body       []byte
	uri        string
}

func newEnv(t *testing.T) *env { return newEnvOn(t, "") }

// newEnvOn serves the film from the named S3 target in JELLYSCALE_TEST_S3, or a
// directory when it is empty.
func newEnvOn(t *testing.T, s3Target string) *env {
	t.Helper()
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JELLYSCALE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	e := &env{}
	var err error
	if e.pool, err = db.Open(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.pool.Close)
	if _, err := e.pool.Exec(ctx, "TRUNCATE libraries, users CASCADE"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	e.body = bytes.Repeat([]byte("0123456789abcdef"), 4096)
	if err := os.WriteFile(filepath.Join(dir, "film.mkv"), e.body, 0o644); err != nil {
		t.Fatal(err)
	}
	e.uri = "file://" + dir
	if s3Target != "" {
		e.uri = putOnS3(t, s3Target, e.body)
	}
	library := uuid.NewString()
	e.item = uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'L', 'movies', $2, $2)`, library, e.uri)
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', 'Film', 'film', 'film', $3, $4)`, e.item, library, filmTicks, fmt.Sprintf(`{"Name":"Film","MediaSources":[{
		"Id":%q,"Protocol":"File","Container":"mkv","SupportsTranscoding":true,
		"MediaStreams":[{"Index":0,"Type":"Video"},{"Index":2,"Type":"Audio","IsDefault":true}]}]}`,
		db.CompactID(e.item)))
	mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
		VALUES ($1, $1, 'film.mkv', $2, 'abc123', 'mkv', '[]')`, e.item, len(e.body))

	e.user, e.token = e.addUser(t, "viewer")
	e.other, e.otherToken = e.addUser(t, "other")

	for range 2 {
		pool, err := db.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleAPI, RoleStream},
			Log: slog.New(slog.DiscardHandler), SocketIdle: 700 * time.Millisecond}
		server := httptest.NewServer(s.Handler())
		t.Cleanup(server.Close)
		e.apis = append(e.apis, s)
		e.servers = append(e.servers, server)
	}
	return e
}

// putOnS3 stores the film on a target and returns its store URI. Credentials go
// in the environment, as they do for the server, never in the URI.
func putOnS3(t *testing.T, target string, body []byte) string {
	t.Helper()
	var raw string
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		if name, uri, _ := strings.Cut(entry, "="); name == target {
			raw = uri
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || raw == "" {
		t.Fatalf("no S3 target %q", target)
	}
	query := parsed.Query()
	t.Setenv("JELLYSCALE_S3_ACCESS_KEY", query.Get("key"))
	t.Setenv("JELLYSCALE_S3_SECRET_KEY", query.Get("secret"))
	query.Del("key")
	query.Del("secret")
	parsed.RawQuery = query.Encode()
	parsed.Path = "/api-" + uuid.NewString()
	media, err := store.Open(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := media.Put(context.Background(), "film.mkv", bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return parsed.String()
}

func s3Targets() []string {
	var names []string
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		if name, _, found := strings.Cut(entry, "="); found {
			names = append(names, name)
		}
	}
	return names
}

func (e *env) addUser(t *testing.T, name string) (id, token string) {
	t.Helper()
	id, token = uuid.NewString(), uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO users (id, name, password_hash) VALUES ($1, $2, 'x')`, id, name)
	mustExec(t, e.pool, `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version) VALUES ($1, $2, $3, 'dev', 'cli', '1')`,
		token, id, "device-"+name)
	return id, token
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// scalar reads the one value a query returns.
func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var value T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return value
}

// eventually polls until cond holds, for state that handlers' deferred
// cleanups settle after the client has moved on. It reports whether cond held.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func (e *env) do(t *testing.T, replica int, method, path, token string, body any, headers ...string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, e.servers[replica].URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("X-Emby-Token", token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response, data
}

func (e *env) report(t *testing.T, replica int, kind string, item string, ticks int64) int {
	t.Helper()
	response, _ := e.do(t, replica, "POST", "/Sessions/Playing"+kind, e.token,
		map[string]any{"ItemId": item, "PositionTicks": ticks, "PlayMethod": "DirectPlay", "PlaySessionId": "", "IsPaused": false})
	return response.StatusCode
}

type userRow struct {
	position   int64
	played     bool
	count      int
	favorite   bool
	progressAt *time.Time
}

func (e *env) row(t *testing.T, user string) userRow {
	t.Helper()
	var r userRow
	err := e.pool.QueryRow(context.Background(), `SELECT position_ticks, played, play_count, favorite, progress_at
		FROM user_item_data WHERE user_id = $1 AND item_id = $2`, user, e.item).Scan(&r.position, &r.played, &r.count, &r.favorite, &r.progressAt)
	if err != nil {
		t.Fatalf("no user data row: %v", err)
	}
	return r
}

func TestStreamRangesAndHead(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets()...) {
		t.Run("backend="+target, func(t *testing.T) { checkStream(t, newEnvOn(t, target)) })
	}
}

func checkStream(t *testing.T, e *env) {
	opened, err := store.Open(e.uri)
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingStore{Store: opened}
	e.apis[0].stores.Put(e.uri, counting)
	base := "/Videos/" + db.CompactID(e.item) + "/stream.mkv"
	size := len(e.body)

	response, body := e.do(t, 0, "GET", base+"?Static=true&mediaSourceId="+db.CompactID(e.item)+"&ApiKey="+e.token, "", nil, "Range", "bytes=0-")
	if response.StatusCode != 206 || len(body) != size || response.Header.Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("open range: %d %d %q", response.StatusCode, len(body), response.Header.Get("Content-Type"))
	}
	etag := response.Header.Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") || response.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("etag %q, accept-ranges %q", etag, response.Header.Get("Accept-Ranges"))
	}

	// The tail, as a player reading a Matroska cues block asks for it.
	response, body = e.do(t, 0, "GET", base, e.token, nil, "Range", fmt.Sprintf("bytes=%d-%d", size-100, size-1), "If-Range", etag)
	if response.StatusCode != 206 || !bytes.Equal(body, e.body[size-100:]) {
		t.Fatalf("tail with If-Range: %d", response.StatusCode)
	}
	// A stale validator means the whole thing again.
	response, body = e.do(t, 0, "GET", base, e.token, nil, "Range", "bytes=10-20", "If-Range", `"stale"`)
	if response.StatusCode != 200 || len(body) != size {
		t.Fatalf("stale If-Range: %d %d", response.StatusCode, len(body))
	}
	response, body = e.do(t, 0, "GET", base, e.token, nil, "Range", "bytes=-50")
	if response.StatusCode != 206 || !bytes.Equal(body, e.body[size-50:]) {
		t.Fatalf("suffix range: %d %d", response.StatusCode, len(body))
	}
	response, _ = e.do(t, 0, "GET", base, e.token, nil, "Range", fmt.Sprintf("bytes=%d-", size+5))
	if response.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d", response.StatusCode)
	}

	before := counting.opens.Load()
	response, body = e.do(t, 0, "HEAD", base, e.token, nil)
	if response.StatusCode != 200 || len(body) != 0 || response.ContentLength != int64(size) {
		t.Fatalf("head: %d %d %d", response.StatusCode, len(body), response.ContentLength)
	}
	if counting.opens.Load() != before {
		t.Fatal("HEAD opened the store")
	}

	response, _ = e.do(t, 0, "GET", "/Videos/"+db.CompactID(e.item)+"/stream", e.token, nil, "Range", "bytes=0-3")
	if response.StatusCode != 206 {
		t.Fatalf("bare stream: %d", response.StatusCode)
	}
	for _, path := range []string{"/Videos/" + db.CompactID(e.item) + "/streamer", "/Videos/" + db.CompactID(e.item) + "/other.mkv",
		"/Videos/" + uuid.NewString() + "/stream.mkv", base + "?mediaSourceId=" + uuid.NewString()} {
		if response, _ := e.do(t, 0, "GET", path, e.token, nil); response.StatusCode != 404 {
			t.Errorf("%s: %d", path, response.StatusCode)
		}
	}
}

func TestMissingObjectIs404BeforeAnyBody(t *testing.T) {
	for _, target := range append([]string{""}, s3Targets()...) {
		t.Run("backend="+target, func(t *testing.T) {
			e := newEnvOn(t, target)
			mustExec(t, e.pool, "UPDATE media_sources SET store_key = 'gone-' || store_key")
			base := "/Videos/" + db.CompactID(e.item) + "/stream.mkv"
			for _, rng := range []string{"", "bytes=0-", "bytes=100-200"} {
				var headers []string
				if rng != "" {
					headers = []string{"Range", rng}
				}
				response, _ := e.do(t, 0, "GET", base, e.token, nil, headers...)
				if response.StatusCode != 404 || response.Header.Get("Content-Range") != "" {
					t.Errorf("range %q: %d, Content-Range %q", rng, response.StatusCode, response.Header.Get("Content-Range"))
				}
			}
			// HEAD reads nothing, so it still reports the size the scan recorded.
			if response, _ := e.do(t, 0, "HEAD", base, e.token, nil); response.StatusCode != 200 {
				t.Errorf("head: %d", response.StatusCode)
			}
		})
	}
}

func TestBitrateTestIsOnTheStreamRole(t *testing.T) {
	e := newEnv(t)
	apiOnly := &Server{Pool: e.pool, Items: &items.Repo{Pool: e.pool, ServerID: "srv"}, Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(apiOnly.Handler())
	defer server.Close()
	e.servers = append(e.servers, server)
	response, _ := e.do(t, len(e.servers)-1, "GET", "/Playback/BitrateTest?Size=10", e.token, nil)
	if response.StatusCode != 404 {
		t.Fatalf("api role served the bitrate test: %d", response.StatusCode)
	}
}

func TestMediaRoutesNeedAToken(t *testing.T) {
	e := newEnv(t)
	base := "/Videos/" + db.CompactID(e.item) + "/stream.mkv"
	for name, path := range map[string]string{"none": base, "bad header": base, "bad query": base + "?ApiKey=nope"} {
		token := ""
		if name == "bad header" {
			token = "nope"
		}
		if response, _ := e.do(t, 0, "GET", path, token, nil); response.StatusCode != 401 {
			t.Errorf("%s: %d", name, response.StatusCode)
		}
	}
	if response, _ := e.do(t, 0, "GET", "/Playback/BitrateTest?Size=10", "", nil); response.StatusCode != 401 {
		t.Errorf("bitrate test without a token: %d", response.StatusCode)
	}
}

func TestClientDisconnectStopsTheStoreRead(t *testing.T) {
	e := newEnv(t)
	blocking := &blockingStore{opened: make(chan struct{}), canceled: make(chan struct{})}
	e.apis[0].stores.Put(e.uri, blocking)
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, "GET", e.servers[0].URL+"/Videos/"+db.CompactID(e.item)+"/stream.mkv", nil)
	request.Header.Set("X-Emby-Token", e.token)
	go http.DefaultClient.Do(request) //nolint:errcheck
	select {
	case <-blocking.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the store was never read")
	}
	cancel()
	select {
	case <-blocking.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the store read outlived the client")
	}
}

func TestPlaybackInfo(t *testing.T) {
	e := newEnv(t)
	for _, method := range []string{"GET", "POST"} {
		response, body := e.do(t, 0, method, "/Items/"+db.CompactID(e.item)+"/PlaybackInfo", e.token, map[string]any{"UserId": e.user, "DeviceProfile": map[string]any{}})
		var info struct {
			MediaSources  []map[string]any
			PlaySessionId string
		}
		if err := json.Unmarshal(body, &info); err != nil || response.StatusCode != 200 || len(info.MediaSources) != 1 || len(info.PlaySessionId) != 32 {
			t.Fatalf("%s: %d %s", method, response.StatusCode, body)
		}
		source := info.MediaSources[0]
		if source["SupportsTranscoding"] != false || source["DefaultAudioStreamIndex"] != float64(2) ||
			source["DefaultSubtitleStreamIndex"] != float64(-1) || source["Protocol"] != "File" ||
			source["TranscodingSubProtocol"] != "http" || source["HasSegments"] != false || source["IsInfiniteStream"] != false {
			t.Fatalf("%s: %v", method, source)
		}
		for _, key := range []string{"RequiredHttpHeaders", "MediaAttachments", "Formats"} {
			if source[key] == nil {
				t.Errorf("%s: missing %s", method, key)
			}
		}
		var offered []string
		for _, entry := range source["MediaStreams"].([]any) {
			stream := entry.(map[string]any)
			offered = append(offered, fmt.Sprint(stream["Type"], stream["Index"]))
		}
		if fmt.Sprint(offered) != "[Video0 Audio2]" {
			t.Errorf("%s: streams offered: %v", method, offered)
		}
		if strings.Contains(string(body), "http://") {
			t.Errorf("%s: response carries a URL: %s", method, body)
		}
	}
	if response, _ := e.do(t, 0, "GET", "/Items/"+uuid.NewString()+"/PlaybackInfo", e.token, nil); response.StatusCode != 404 {
		t.Errorf("missing item: %d", response.StatusCode)
	}
}

func TestBitrateTest(t *testing.T) {
	e := newEnv(t)
	if _, body := e.do(t, 0, "GET", "/Playback/BitrateTest?Size=500000", e.token, nil); len(body) != 500000 {
		t.Fatalf("got %d bytes", len(body))
	}
	if _, body := e.do(t, 0, "GET", "/Playback/BitrateTest?Size=999999999", e.token, nil); len(body) != maxBitrateTest {
		t.Fatalf("uncapped: %d bytes", len(body))
	}
	if response, _ := e.do(t, 0, "GET", "/Playback/BitrateTest", e.token, nil); response.StatusCode != 400 {
		t.Fatalf("no size: %d", response.StatusCode)
	}
}

func TestProgressIsCoalescedAcrossReplicas(t *testing.T) {
	e := newEnv(t)
	if code := e.report(t, 0, "", e.item, 0); code != 204 {
		t.Fatalf("started: %d", code)
	}
	if row := e.row(t, e.user); row.position != 0 {
		t.Fatalf("started wrote a position: %+v", row)
	}
	// Playing doesn't shield the row: the first Progress after it is stored.
	e.report(t, 1, "/Progress", e.item, 5_000_000_000)
	if first := e.row(t, e.user); first.position != 5_000_000_000 || first.progressAt == nil {
		t.Fatalf("first progress after Playing wasn't stored: %+v", first)
	}
	e.report(t, 0, "/Progress", e.item, 5_500_000_000)
	if second := e.row(t, e.user); second.position != 5_000_000_000 {
		t.Fatalf("progress inside the window wrote: %+v", second)
	}

	// Age the row past the window, then write through the other replica.
	mustExec(t, e.pool, "UPDATE user_item_data SET progress_at = now() - interval '31 seconds'")
	e.report(t, 1, "/Progress", e.item, 6_000_000_000)
	written := e.row(t, e.user)
	if written.position != 6_000_000_000 {
		t.Fatalf("progress outside the window didn't write: %+v", written)
	}
	// A report through the first replica right after must leave the row alone.
	e.report(t, 0, "/Progress", e.item, 7_000_000_000)
	untouched := e.row(t, e.user)
	if untouched.position != 6_000_000_000 || !untouched.progressAt.Equal(*written.progressAt) {
		t.Fatalf("progress within 30s touched the row: %+v then %+v", written, untouched)
	}
	// Progress written through one replica is visible through the other.
	_, body := e.do(t, 0, "GET", "/Items/"+db.CompactID(e.item), e.token, nil)
	var dto struct{ UserData map[string]any }
	json.Unmarshal(body, &dto) //nolint:errcheck
	if dto.UserData["PlaybackPositionTicks"] != float64(6_000_000_000) || dto.UserData["PlayedPercentage"] != 6.0 {
		t.Fatalf("user data: %v", dto.UserData)
	}
}

func TestConcurrentProgressLeavesOneWinner(t *testing.T) {
	e := newEnv(t)
	mustExec(t, e.pool, `INSERT INTO user_item_data (user_id, item_id, progress_at) VALUES ($1, $2, now() - interval '1 minute')`, e.user, e.item)
	done := make(chan int, 40)
	for i := range 40 {
		go func() { done <- e.report(t, i%2, "/Progress", e.item, int64(i+1)*1_000_000_000) }()
	}
	for range 40 {
		<-done
	}
	row := e.row(t, e.user)
	if row.position == 0 || row.progressAt == nil || time.Since(*row.progressAt) > 10*time.Second {
		t.Fatalf("no write won: %+v", row)
	}
	// Whoever won shielded the row, so a later report changes nothing.
	again := row.position
	e.report(t, 0, "/Progress", e.item, 99_000_000_000)
	if e.row(t, e.user).position != again {
		t.Fatal("a write landed inside the window")
	}
}

func TestStoppedRules(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name     string
		ticks    int64
		position int64
		played   bool
		count    int
	}{
		{"early stop resets", filmTicks / 100 * 4, 0, false, 0},
		{"middle is kept", filmTicks / 2, filmTicks / 2, false, 0},
		{"just under 90% is kept", filmTicks/10*9 - 1, filmTicks/10*9 - 1, false, 0},
		{"90% is finished", filmTicks / 10 * 9, 0, true, 1},
		{"the end finishes again", filmTicks, 0, true, 2},
		{"early stop keeps played", filmTicks / 100, 0, true, 2},
	}
	for _, c := range cases {
		if code := e.report(t, 0, "/Stopped", e.item, c.ticks); code != 204 {
			t.Fatalf("%s: %d", c.name, code)
		}
		row := e.row(t, e.user)
		if row.position != c.position || row.played != c.played || row.count != c.count {
			t.Errorf("%s: got %+v", c.name, row)
		}
	}
	// Stopped writes even inside the coalescing window.
	e.report(t, 0, "/Stopped", e.item, filmTicks/2)
	if e.row(t, e.user).position != filmTicks/2 {
		t.Error("stopped was coalesced")
	}
}

func TestReportsForMissingItemsAreAccepted(t *testing.T) {
	e := newEnv(t)
	gone := uuid.NewString()
	for _, kind := range []string{"", "/Progress", "/Stopped"} {
		if code := e.report(t, 0, kind, gone, 123); code != 204 {
			t.Errorf("%q: %d", kind, code)
		}
	}
	if rows := scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data"); rows != 0 {
		t.Errorf("%d rows for a missing item", rows)
	}
	if response, _ := e.do(t, 0, "POST", "/Sessions/Playing/Ping", e.token, nil); response.StatusCode != 204 {
		t.Errorf("ping: %d", response.StatusCode)
	}
	if response, _ := e.do(t, 0, "POST", "/Sessions/Playing/Progress", e.token, map[string]any{"ItemId": "nonsense"}); response.StatusCode != 400 {
		t.Errorf("malformed id: %d", response.StatusCode)
	}
}

func TestSessionsReflectPlayback(t *testing.T) {
	e := newEnv(t)
	e.report(t, 0, "", e.item, 0)
	e.report(t, 1, "/Progress", e.item, 42)
	_, body := e.do(t, 1, "GET", "/Sessions", e.token, nil)
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("%v %s", err, body)
	}
	if sessions[0]["NowPlayingItem"] == nil || sessions[0]["PlayState"].(map[string]any)["PositionTicks"] != float64(42) {
		t.Fatalf("%v", sessions[0])
	}
	if strings.Contains(string(body), e.token) {
		t.Fatal("sessions list leaks the device token")
	}
	e.report(t, 0, "/Stopped", e.item, 0)
	_, body = e.do(t, 0, "GET", "/Sessions", e.token, nil)
	sessions = nil                  // Unmarshal would merge into the old maps.
	json.Unmarshal(body, &sessions) //nolint:errcheck
	if sessions[0]["NowPlayingItem"] != nil {
		t.Fatalf("still playing after stop: %v", sessions[0])
	}
}

func TestPlayedAndFavoriteToggles(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	data := func(method, path string) map[string]any {
		t.Helper()
		response, body := e.do(t, 0, method, path, e.token, nil)
		var dto map[string]any
		if response.StatusCode != 200 || json.Unmarshal(body, &dto) != nil {
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, body)
		}
		return dto
	}
	if dto := data("POST", "/UserPlayedItems/"+id); dto["Played"] != true || dto["PlayCount"] != float64(1) || dto["ItemId"] != id {
		t.Fatalf("played: %v", dto)
	}
	if dto := data("POST", "/UserFavoriteItems/"+id); dto["IsFavorite"] != true || dto["Played"] != true {
		t.Fatalf("favorite: %v", dto)
	}
	if dto := data("DELETE", "/UserPlayedItems/"+id); dto["Played"] != false || dto["IsFavorite"] != true {
		t.Fatalf("unplayed: %v", dto)
	}
	if dto := data("DELETE", "/UserFavoriteItems/"+id); dto["IsFavorite"] != false {
		t.Fatalf("unfavorited: %v", dto)
	}
	if r := e.row(t, e.user); r.played || r.favorite {
		t.Fatalf("not persisted: %+v", r)
	}

	// The web aliases act on the caller, whatever userId says.
	if dto := data("POST", "/Users/"+e.other+"/PlayedItems/"+id); dto["Played"] != true {
		t.Fatalf("alias: %v", dto)
	}
	data("POST", "/Users/"+e.other+"/FavoriteItems/"+id)
	if r := e.row(t, e.user); !r.played || !r.favorite {
		t.Fatalf("alias missed the caller: %+v", r)
	}
	if scalar[int](t, e.pool, "SELECT count(*) FROM user_item_data WHERE user_id = $1", e.other) != 0 {
		t.Fatal("an alias wrote another user's data")
	}
	data("DELETE", "/Users/"+e.other+"/PlayedItems/"+id)
	if r := e.row(t, e.user); r.played {
		t.Fatal("alias delete missed the caller")
	}
	if response, _ := e.do(t, 0, "POST", "/UserPlayedItems/"+uuid.NewString(), e.token, nil); response.StatusCode != 404 {
		t.Fatalf("missing item: %d", response.StatusCode)
	}
}

func TestPlayedCascadesToEpisodes(t *testing.T) {
	e := newEnv(t)
	lib := scalar[string](t, e.pool, "SELECT library_id FROM items WHERE id = $1", e.item)
	series, season, ep1, ep2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto) VALUES ($1, $2, 'Series', 'S', 's', 's', '{}')`, series, lib)
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, parent_id, series_id, type, name, sort_name, natural_key, dto) VALUES ($1, $2, $3, $3, 'Season', 'Se', 'se', 'se', '{}')`, season, lib, series)
	for _, ep := range []string{ep1, ep2} {
		mustExec(t, e.pool, `INSERT INTO items (id, library_id, parent_id, series_id, season_id, type, name, sort_name, natural_key, dto) VALUES ($1::uuid, $2, $3, $4, $3, 'Episode', $5, $5, $5, '{}')`, ep, lib, season, series, ep)
	}
	played := func(id string) bool {
		return scalar[bool](t, e.pool, "SELECT COALESCE((SELECT played FROM user_item_data WHERE user_id = $1 AND item_id = $2), false)", e.user, id)
	}
	if r, _ := e.do(t, 0, "POST", "/UserPlayedItems/"+db.CompactID(series), e.token, nil); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, id := range []string{series, season, ep1, ep2} {
		if !played(id) {
			t.Fatalf("%s not played after marking the series", id)
		}
	}
	if played(e.item) {
		t.Fatal("an unrelated film was marked")
	}
	e.do(t, 0, "DELETE", "/Users/"+e.other+"/PlayedItems/"+db.CompactID(season), e.token, nil)
	if played(season) || played(ep1) || played(ep2) {
		t.Fatal("season unplayed left episodes watched")
	}
	if !played(series) {
		t.Fatal("unplaying a season changed its series")
	}
}

func (e *env) socketURL(replica int, token string) string {
	return "ws" + strings.TrimPrefix(e.servers[replica].URL, "http") + "/socket?ApiKey=" + token
}

func TestSocket(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, response, err := websocket.Dial(ctx, e.socketURL(0, "nope"), nil); err == nil || response == nil || response.StatusCode != 401 {
		t.Fatalf("bad token: %v %v", err, response)
	}

	conn := e.dialSocket(t, 0)
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	json.Unmarshal(data, &message) //nolint:errcheck
	if message["MessageType"] != "ForceKeepAlive" || message["Data"] != float64(60) || len(fmt.Sprint(message["MessageId"])) != 32 {
		t.Fatalf("first message: %s", data)
	}
	replica := func() *string {
		return scalar[*string](t, e.pool, "SELECT ws_replica FROM sessions WHERE token = $1", e.token)
	}
	if got := replica(); got == nil || *got != e.apis[0].replica() {
		t.Fatalf("ws_replica = %v", got)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"KeepAlive"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err = conn.Read(ctx)
	message = nil
	json.Unmarshal(data, &message) //nolint:errcheck
	if err != nil || message["MessageType"] != "KeepAlive" || len(fmt.Sprint(message["MessageId"])) != 32 || len(message) != 2 {
		t.Fatalf("keepalive reply: %v %s", err, data)
	}

	// Silence past the idle limit closes the socket, and the claim clears.
	if _, _, err := conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("idle close: %v", err)
	}
	if !eventually(func() bool { return replica() == nil }) {
		t.Fatal("ws_replica not cleared after close")
	}
}

func TestOldSocketCloseKeepsTheNewSocketsClaim(t *testing.T) {
	e := newEnv(t)
	first := e.dialSocket(t, 0)
	second := e.dialSocket(t, 0) // The reconnect, same replica.
	defer second.CloseNow()      //nolint:errcheck
	first.Close(websocket.StatusNormalClosure, "")
	time.Sleep(300 * time.Millisecond) // Under the 700ms idle limit; lets the old handler finish.
	if scalar[*string](t, e.pool, "SELECT ws_replica FROM sessions WHERE token = $1", e.token) == nil {
		t.Fatal("closing the old socket cleared the live socket's ws_replica")
	}
}

// socketClaim reads what the database says about a device's sockets.
func (e *env) socketClaim(t *testing.T) (replica *string, open int) {
	t.Helper()
	// No session row yet reads as no claim.
	if err := e.pool.QueryRow(context.Background(), "SELECT ws_replica FROM sessions WHERE token = $1", e.token).Scan(&replica); err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return replica, scalar[int](t, e.pool, "SELECT count(*) FROM session_sockets WHERE token = $1", e.token)
}

// waitClaim waits for the handlers' deferred cleanups, which run after the
// close handshake, to land.
func (e *env) waitClaim(t *testing.T, open int, replica string) {
	t.Helper()
	var got *string
	var n int
	if !eventually(func() bool {
		got, n = e.socketClaim(t)
		return n == open && ((replica == "" && got == nil) || (got != nil && *got == replica))
	}) {
		t.Fatalf("want %d sockets on %q, have %d, ws_replica %v", open, replica, n, got)
	}
}

// tryDial opens a socket on a replica. It's safe to call from any goroutine.
func (e *env) tryDial(replica int) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, e.socketURL(replica, e.token), nil)
	return conn, err
}

// dialSocket fails the test if the dial fails, so call it from the test's own
// goroutine.
func (e *env) dialSocket(t *testing.T, replica int) *websocket.Conn {
	t.Helper()
	conn, err := e.tryDial(replica)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestCrashedReplicaCannotBecomeTheClaim(t *testing.T) {
	e := newEnv(t)
	e.apis[0].SocketIdle = time.Minute
	socket := e.dialSocket(t, 0)
	e.waitClaim(t, 1, e.apis[0].replica())
	// A row a replica left behind when it crashed.
	mustExec(t, e.pool, `INSERT INTO session_sockets (connection_id, token, replica, connected_at, last_seen_at)
		VALUES ('crashed', $1, 'dead', now() + interval '1 hour', now() - interval '1 hour')`, e.token)
	socket.Close(websocket.StatusNormalClosure, "")
	e.waitClaim(t, 0, "")
}

// Any frame proves a socket is alive, not only KeepAlive. A closing socket
// drops the device's quiet rows, so a live socket whose row wasn't refreshed
// would be dropped by another socket's close.
func TestAnyFrameKeepsTheSocketsRowFresh(t *testing.T) {
	e := newEnv(t)
	e.apis[0].SocketIdle = time.Minute
	e.apis[1].SocketIdle = time.Minute
	live := e.dialSocket(t, 0)
	defer live.CloseNow() //nolint:errcheck
	e.waitClaim(t, 1, e.apis[0].replica())
	mustExec(t, e.pool, "UPDATE session_sockets SET last_seen_at = now() - interval '10 minutes' WHERE token = $1", e.token)
	if err := live.Write(context.Background(), websocket.MessageText, []byte(`{"MessageType":"SessionsStart","Data":"0,1500"}`)); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool {
		return scalar[bool](t, e.pool, "SELECT last_seen_at > now() - interval '1 minute' FROM session_sockets WHERE token = $1", e.token)
	}) {
		t.Fatal("a received frame didn't refresh last_seen_at")
	}

	other := e.dialSocket(t, 1)
	e.waitClaim(t, 2, e.apis[1].replica())
	other.Close(websocket.StatusNormalClosure, "")
	e.waitClaim(t, 1, e.apis[0].replica())
}

func TestTwoSocketsOnDifferentReplicas(t *testing.T) {
	for _, order := range []string{"older first", "newer first"} {
		t.Run(order, func(t *testing.T) {
			e := newEnv(t)
			// Idle expiry would close them mid-test.
			for _, api := range e.apis {
				api.SocketIdle = time.Minute
			}
			older := e.dialSocket(t, 0)
			e.waitClaim(t, 1, e.apis[0].replica())
			newer := e.dialSocket(t, 1)
			e.waitClaim(t, 2, e.apis[1].replica())
			first, second, survivor := older, newer, e.apis[1].replica()
			if order == "newer first" {
				first, second, survivor = newer, older, e.apis[0].replica()
			}
			first.Close(websocket.StatusNormalClosure, "")
			e.waitClaim(t, 1, survivor)
			// It stays set while the other is open.
			time.Sleep(200 * time.Millisecond)
			e.waitClaim(t, 1, survivor)
			second.Close(websocket.StatusNormalClosure, "")
			e.waitClaim(t, 0, "")
		})
	}
}

// Connects and closes that all start at the same instant, over several rounds,
// so the session row lock is what keeps ws_replica on the right socket.
func TestConcurrentSocketReconnects(t *testing.T) {
	e := newEnv(t)
	for _, api := range e.apis {
		api.SocketIdle = time.Minute
	}
	const n = 32
	// together runs f(0..n-1) at once, and returns the first error.
	together := func(f func(i int) error) error {
		errs := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = f(i)
			}()
		}
		close(start)
		wg.Wait()
		return errors.Join(errs...)
	}
	for round := range 12 {
		conns := make([]*websocket.Conn, n)
		if err := together(func(i int) (err error) { conns[i], err = e.tryDial(i % 2); return }); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// Handlers register after the upgrade, so wait for all of them.
		if !eventually(func() bool { _, open := e.socketClaim(t); return open == n }) {
			t.Fatalf("round %d: not every socket registered", round)
		}
		// The claim names the replica of the newest row.
		newest := scalar[string](t, e.pool, `SELECT replica FROM session_sockets WHERE token = $1
			ORDER BY connected_at DESC, connection_id DESC LIMIT 1`, e.token)
		if got, _ := e.socketClaim(t); got == nil || *got != newest {
			t.Fatalf("round %d: ws_replica = %v, newest socket is on %q", round, got, newest)
		}

		// Close all but one at once; the claim must end on the survivor's replica.
		_ = together(func(i int) error {
			if i > 0 {
				conns[i].Close(websocket.StatusNormalClosure, "") //nolint:errcheck
			}
			return nil
		})
		e.waitClaim(t, 1, e.apis[0].replica())
		conns[0].Close(websocket.StatusNormalClosure, "")
		e.waitClaim(t, 0, "")
	}
}

func TestToggleDuringPlaybackDoesNotSuppressProgress(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	e.report(t, 0, "", e.item, 0)
	e.do(t, 0, "POST", "/UserFavoriteItems/"+id, e.token, nil)
	e.report(t, 1, "/Progress", e.item, 1_000_000_000)
	if r := e.row(t, e.user); r.position != 1_000_000_000 || !r.favorite {
		t.Fatalf("first progress after Playing and a toggle: %+v", r)
	}
	// Toggles don't move the shield either way.
	mustExec(t, e.pool, "UPDATE user_item_data SET progress_at = now() - interval '31 seconds'")
	e.do(t, 0, "DELETE", "/UserFavoriteItems/"+id, e.token, nil)
	e.do(t, 0, "POST", "/UserPlayedItems/"+id, e.token, nil)
	e.report(t, 0, "/Progress", e.item, 2_000_000_000)
	if r := e.row(t, e.user); r.position != 2_000_000_000 {
		t.Fatalf("toggles suppressed progress: %+v", r)
	}
	// Stopped shields the row too.
	e.report(t, 0, "/Stopped", e.item, filmTicks/2)
	e.report(t, 1, "/Progress", e.item, 3_000_000_000)
	if r := e.row(t, e.user); r.position != filmTicks/2 {
		t.Fatalf("progress right after Stopped wrote: %+v", r)
	}
}

func TestExtremeNumbersNeverFail(t *testing.T) {
	e := newEnv(t)
	for _, ticks := range []int64{math.MaxInt64, math.MaxInt64 - 1, math.MinInt64, -1} {
		for _, kind := range []string{"", "/Progress", "/Stopped", "/Progress"} {
			if code := e.report(t, 0, kind, e.item, ticks); code != 204 {
				t.Errorf("%q with %d: %d", kind, ticks, code)
			}
		}
	}
	// An item whose runtime is itself extreme must not overflow the 90% test.
	mustExec(t, e.pool, "UPDATE items SET run_time_ticks = $2 WHERE id = $1", e.item, int64(math.MaxInt64))
	for _, ticks := range []int64{math.MaxInt64, 1 << 50, 1} {
		if code := e.report(t, 0, "/Stopped", e.item, ticks); code != 204 {
			t.Errorf("huge runtime, stopped at %d: %d", ticks, code)
		}
	}
	for _, within := range []string{"9223372036854775807", "9223372036854775808", "99999999999999999999999", "1e400", "-9223372036854775808", "0", "x", "1000000000000"} {
		if response, body := e.do(t, 0, "GET", "/Sessions?ActiveWithinSeconds="+within, e.token, nil); response.StatusCode != 200 {
			t.Errorf("ActiveWithinSeconds=%s: %d %s", within, response.StatusCode, body)
		}
	}
	e.report(t, 0, "", e.item, 1)
	mustExec(t, e.pool, "UPDATE user_item_data SET progress_at = NULL WHERE item_id = $1", e.item)
	e.report(t, 0, "/Progress", e.item, math.MaxInt64)
	session := scalar[int64](t, e.pool, "SELECT position_ticks FROM sessions WHERE token = $1", e.token)
	stored := scalar[int64](t, e.pool, "SELECT position_ticks FROM user_item_data WHERE item_id = $1", e.item)
	if session != playstate.MaxTicks || stored != playstate.MaxTicks {
		t.Errorf("session %d, stored %d, want %d", session, stored, playstate.MaxTicks)
	}
	// A far-out window still finds the recently active device.
	_, body := e.do(t, 0, "GET", "/Sessions?ActiveWithinSeconds=9223372036854775807", e.token, nil)
	var sessions []map[string]any
	if json.Unmarshal(body, &sessions) != nil || len(sessions) != 1 {
		t.Errorf("sessions: %s", body)
	}
}

func TestLateStoppedLeavesTheCurrentItem(t *testing.T) {
	e := newEnv(t)
	lib := scalar[string](t, e.pool, "SELECT library_id FROM items WHERE id = $1", e.item)
	other := uuid.NewString()
	mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, dto) VALUES ($1, $2, 'Movie', 'B', 'b', 'b', '{}')`, other, lib)
	current := func() (item *string, ticks int64) {
		e.pool.QueryRow(context.Background(), "SELECT item_id::text, position_ticks FROM sessions WHERE token = $1", e.token).Scan(&item, &ticks) //nolint:errcheck
		return
	}
	e.report(t, 0, "", e.item, 0)
	e.report(t, 0, "", other, 77)
	e.report(t, 1, "/Stopped", e.item, 5) // A, late.
	if item, ticks := current(); item == nil || *item != other || ticks != 77 {
		t.Fatalf("a late Stopped for A cleared B: %v %d", item, ticks)
	}
	e.report(t, 0, "/Progress", e.item, 9) // A, late, while B plays.
	if item, ticks := current(); item == nil || *item != other || ticks != 77 {
		t.Fatalf("a late Progress for A moved the session: %v %d", item, ticks)
	}
	e.report(t, 1, "/Progress", other, 88)
	if _, ticks := current(); ticks != 88 {
		t.Fatalf("Progress for the current item wasn't recorded: %d", ticks)
	}
	e.report(t, 1, "/Stopped", other, 5)
	if item, _ := current(); item != nil {
		t.Fatalf("Stopped for the current item left it: %v", *item)
	}
	e.report(t, 0, "/Progress", other, 9) // Late, after the stop.
	if item, _ := current(); item != nil {
		t.Fatalf("a late Progress revived a stopped session: %v", *item)
	}
}

func TestContentTypeComesFromTheStoredContainer(t *testing.T) {
	e := newEnv(t)
	// The key and URL say mkv; the probed container says mp4.
	mustExec(t, e.pool, "UPDATE media_sources SET container = 'mp4'")
	for _, path := range []string{"/Videos/" + db.CompactID(e.item) + "/stream.mkv", "/Videos/" + db.CompactID(e.item) + "/stream.webm", "/Videos/" + db.CompactID(e.item) + "/stream"} {
		response, _ := e.do(t, 0, "GET", path, e.token, nil, "Range", "bytes=0-3")
		if got := response.Header.Get("Content-Type"); got != "video/mp4" {
			t.Errorf("%s: %q", path, got)
		}
	}
}

func TestWebMKeepsItsContentType(t *testing.T) {
	// ffprobe's "matroska,webm" is stored as mkv; the .webm key marks it WebM.
	for key, want := range map[string]string{
		"film.webm": "video/webm", "Film.WEBM": "video/webm",
		"film.mkv": "video/x-matroska", "film": "video/x-matroska",
	} {
		if got := contentType(items.Source{Key: key, Container: "mkv"}); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if got := contentType(items.Source{Key: "film.webm", Container: "mp4"}); got != "video/mp4" {
		t.Errorf("a non-Matroska container follows the container: %q", got)
	}
}

type countingStore struct {
	store.Store
	opens atomic.Int64
}

func (c *countingStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	c.opens.Add(1)
	return c.Store.Open(ctx, key, offset)
}

// blockingStore serves a read that never produces data, and notes when the
// request's context ends.
type blockingStore struct {
	store.Store
	opened, canceled chan struct{}
}

func (b *blockingStore) Open(ctx context.Context, _ string, _ int64) (io.ReadCloser, error) {
	close(b.opened)
	<-ctx.Done()
	close(b.canceled)
	return nil, ctx.Err()
}
