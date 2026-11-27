//go:build integration

package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

// JS-144 (D-016), with Postgres and the fixture films of access_integration_test.go: what the real answers of every
// route carry, with a token and without. The preflights alone are asked in cors_test.go with no database.

const (
	corsA = "app://localhost"
	corsB = "https://app.example.test"
)

func corsAllows(r accReply, origin string) bool {
	got := r.header.Values("Access-Control-Allow-Origin")
	return len(got) == 1 && (got[0] == "*" || got[0] == origin)
}

func corsReplyList(r accReply, name string) []string {
	var out []string
	for _, value := range r.header.Values(name) {
		for _, word := range strings.Split(value, ",") {
			if word = strings.ToLower(strings.TrimSpace(word)); word != "" {
				out = append(out, word)
			}
		}
	}
	return out
}

// corsForbidden is what no answer of jellyscale carries, whatever it is: a credentialed CORS answer, or a cookie.
func corsForbidden(what string, r accReply) string {
	for _, name := range []string{"Access-Control-Allow-Credentials", "Set-Cookie"} {
		if r.header.Get(name) != "" {
			return fmt.Sprintf("%s carries %s: %s", what, name, r.header.Get(name))
		}
	}
	return ""
}

// asked is every (route, handler of its role) pair with a way to ask it for real.
func (a *accessSite) corsRoutes(t *testing.T) (map[string]accHandler, []Route) {
	t.Helper()
	return map[string]accHandler{RoleAPI: a.serve(t, RoleAPI), RoleStream: a.serve(t, RoleStream)}, (&Server{}).Routes()
}

// Every answer of every route to a request that carries an Origin allows that origin, whatever the answer, with a
// token in any of the seven spellings, with an unknown one and with none; and none of them allows credentials or sets
// a cookie. The count is held: 96 routes, so a new route is asked too.
func TestEveryAnswerOfEveryRouteToARequestWithAnOriginAllowsIt(t *testing.T) {
	a := newAccessSite(t)
	handlers, routes := a.corsRoutes(t)
	var bad []string
	asked, statuses := 0, map[int]int{}
	for _, route := range routes {
		h := handlers[route.Role]
		path, err := a.path(route, a.films[0], a.user)
		if err != nil {
			t.Fatal(err)
		}
		body := accRequestBody(route)
		for _, origin := range []string{corsA, corsB} {
			for spelling := -1; spelling < len(accSpellings); spelling++ {
				if spelling >= 5 && route.Method != "GET" {
					continue
				}
				token := a.device(t, a.user, "cors-"+uuid.NewString()) // A fresh one: POST /Sessions/Logout ends the one it is asked with.
				credentials := []string{token, "no-such-token"}
				if spelling < 0 {
					credentials = []string{""}
				}
				for _, credential := range credentials {
					r := accSendWith(noRedirect, route.Method, h.base, path, credential, spelling, body, "Origin", origin, "Cookie", "session=1")
					asked++
					statuses[r.status]++
					what := fmt.Sprintf("%s %s from %s with the credential %q in spelling %d", route.Method, route.Path, origin, credential[:min(len(credential), 6)], spelling)
					switch {
					case r.err != nil:
						bad = append(bad, what+": "+r.err.Error())
					case !corsAllows(r, origin):
						bad = append(bad, fmt.Sprintf("%s: the %d does not allow the origin (Access-Control-Allow-Origin %q)", what, r.status, r.header.Values("Access-Control-Allow-Origin")))
					}
					if problem := corsForbidden(what, r); problem != "" {
						bad = append(bad, problem)
					}
				}
			}
		}
	}
	// HEAD is served wherever GET is, and a page's HEAD of a film, a download or a subtitle fails as a network error
	// when its answer does not allow the origin: a build that finds the route by its exact method misses it.
	heads := 0
	for _, route := range routes {
		if route.Method != "GET" {
			continue
		}
		path, err := a.path(route, a.films[0], a.user)
		if err != nil {
			t.Fatal(err)
		}
		token := a.device(t, a.user, "cors-head-"+uuid.NewString())
		for _, origin := range []string{corsA, corsB} {
			for _, credential := range []string{token, ""} {
				r := accSendWith(noRedirect, "HEAD", handlers[route.Role].base, path, credential, 3, "", "Origin", origin)
				heads++
				if r.err != nil || !corsAllows(r, origin) {
					bad = append(bad, fmt.Sprintf("HEAD %s from %s: the %d does not allow the origin (Access-Control-Allow-Origin %q) %v", route.Path, origin, r.status, r.header.Values("Access-Control-Allow-Origin"), r.err))
				}
				if problem := corsForbidden("HEAD "+route.Path, r); problem != "" {
					bad = append(bad, problem)
				}
			}
		}
	}
	if heads != 67*4 {
		t.Fatalf("%d HEADs asked, want 67 GET routes x 2 origins x 2 credentials", heads)
	}
	if len(routes) != 96 {
		t.Fatalf("%d routes asked, want 96", len(routes))
	}
	t.Logf("%d answers, by status: %v", asked, statuses)
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d of %d answers were wrong; the first 10:\n  %s", len(bad), asked+heads, strings.Join(bad[:min(10, len(bad))], "\n  "))
	}
}

// The statuses the task names, one by one, each readable by the page: 200, 204, 206, the 401 of no token, an unknown
// token and a wrong password, a handler's 404, a 416 and a 5xx from a store failure; and the query spellings, which
// need no preflight.
func TestTheAnswersTheTaskNamesAreEachReadableFromAnotherOrigin(t *testing.T) {
	a := newAccessSite(t)
	api, stream := a.serve(t, RoleAPI), a.serve(t, RoleStream)
	film := a.films[0]
	// A film whose store is refusing connections: a 5xx of the store, not of Postgres.
	library, item := uuid.NewString(), uuid.NewString()
	mustExec(t, a.pool, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'Refused', 'movies', 's3://nobucket/media?endpoint=http://127.0.0.1:1&region=us-east-1', 'file:///nonexistent')`, library)
	mustExec(t, a.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto) VALUES ($1, $2, 'Movie', 'Refused', 'refused', 'refused', $3, '{"Name":"Refused"}')`, item, library, filmTicks)
	mustExec(t, a.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams) VALUES ($1, $1, 'film.mkv', 65536, 'abc', 'mkv', '[]')`, item)
	token := a.device(t, a.user, "cors-named")
	bearer := `MediaBrowser Client="c", Device="d", DeviceId="named", Version="1", Token="` + token + `"`
	for _, tc := range []struct {
		name   string
		h      accHandler
		method string
		path   string
		body   string
		spell  int
		token  string
		extra  []string
		want   func(int) bool
	}{
		{"200 of a viewer route", api, "GET", "/Users/Me", "", 0, token, nil, func(s int) bool { return s == 200 }},
		{"204 of a sign-out", api, "POST", "/Sessions/Logout", "", 0, "", nil, func(s int) bool { return s == 204 }},
		{"206 of a range", stream, "GET", a.rangePath(film), "", 0, token, []string{"Range", "bytes=100-199"}, func(s int) bool { return s == 206 }},
		{"416 of a range past the end", stream, "GET", a.rangePath(film), "", 0, token, []string{"Range", "bytes=99999999-"}, func(s int) bool { return s == 416 }},
		{"401 of no token", api, "GET", "/Users/Me", "", -1, "", nil, func(s int) bool { return s == 401 }},
		{"401 of an unknown token", stream, "GET", a.rangePath(film), "", 3, "no-such-token", nil, func(s int) bool { return s == 401 }},
		{"401 of a wrong password", api, "POST", "/Users/AuthenticateByName", `{"Username":"viewer","Pw":"wrong"}`, -1, "", []string{"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="wrong", Version="1"`}, func(s int) bool { return s == 401 }},
		{"200 of the sign-in", api, "POST", "/Users/AuthenticateByName", fmt.Sprintf(`{"Username":%q,"Pw":%q}`, accessUserName, accessUserPassword), -1, "", []string{"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="right", Version="1"`}, func(s int) bool { return s == 200 }},
		{"404 of a handler", api, "GET", "/Items/" + uuid.NewString(), "", 0, token, nil, func(s int) bool { return s == 404 }},
		{"5xx of a store", stream, "GET", "/Videos/" + db.CompactID(item) + "/stream?Static=true&mediaSourceId=" + db.CompactID(item), "", 0, token, []string{"Range", "bytes=0-9"}, func(s int) bool { return s >= 500 }},
		{"206 with ApiKey in the query", stream, "GET", a.rangePath(film), "", 5, token, []string{"Range", "bytes=100-199"}, func(s int) bool { return s == 206 }},
		{"206 with api_key in the query", stream, "GET", a.rangePath(film), "", 6, token, []string{"Range", "bytes=100-199"}, func(s int) bool { return s == 206 }},
		{"200 of the Authorization header", api, "GET", "/UserViews", "", -1, "", []string{"Authorization", bearer}, func(s int) bool { return s == 200 }},
	} {
		for _, origin := range []string{corsA, corsB} {
			if tc.path == "/Sessions/Logout" {
				tc.token = a.device(t, a.user, "cors-out-"+uuid.NewString()) // Signing out ends the token, so each origin signs out one of its own.
			}
			r := accSendWith(noRedirect, tc.method, tc.h.base, tc.path, tc.token, tc.spell, tc.body, append([]string{"Origin", origin}, tc.extra...)...)
			switch {
			case r.err != nil:
				t.Errorf("%s from %s: %v", tc.name, origin, r.err)
			case !tc.want(r.status):
				t.Errorf("%s from %s: answered %d %.80q, which is not the status the case is about", tc.name, origin, r.status, r.body)
			case !corsAllows(r, origin):
				t.Errorf("%s from %s: the %d does not allow the origin (%q), so the page cannot read it", tc.name, origin, r.status, r.header.Values("Access-Control-Allow-Origin"))
			}
		}
	}
}

// Wherever an answer carries Content-Range or Accept-Ranges, a page on another origin can read them and
// Content-Length: the direct play, the download, the subtitle and font files, on 200, 206 and HEAD.
func TestEveryRangeHeaderOfAnAnswerCanBeReadByAPageOnAnotherOrigin(t *testing.T) {
	a := newAccessSite(t)
	h := a.serve(t, RoleAPI, RoleStream)
	token := a.device(t, a.user, "cors-expose")
	film := a.films[0]
	carriers := map[string]bool{}
	for _, p := range []struct{ name, path string }{
		{"direct play", a.rangePath(film)},
		{"download", "/Items/" + film.item + "/Download"},
		{"subtitle", "/Videos/" + film.item + "/" + film.source + "/Subtitles/3/0/Stream.vtt"},
		{"font", "/Videos/" + film.item + "/" + film.source + "/Attachments/6"},
	} {
		for _, ask := range []struct {
			what, method string
			extra        []string
		}{{"200", "GET", nil}, {"206", "GET", []string{"Range", "bytes=1-5"}}, {"HEAD", "HEAD", nil}} {
			r := accSendWith(noRedirect, ask.method, h.base, p.path, token, 3, "", append([]string{"Origin", corsB}, ask.extra...)...)
			what := p.name + " " + ask.what
			if r.err != nil || r.status/100 != 2 {
				t.Errorf("%s: %d %v", what, r.status, r.err)
				continue
			}
			if !corsAllows(r, corsB) {
				t.Errorf("%s: the answer does not allow the origin (Access-Control-Allow-Origin %q)", what, r.header.Values("Access-Control-Allow-Origin"))
			}
			if r.header.Get("Content-Range") == "" && r.header.Get("Accept-Ranges") == "" {
				continue
			}
			carriers[p.name] = true
			exposed := corsReplyList(r, "Access-Control-Expose-Headers")
			for _, name := range []string{"content-range", "accept-ranges", "content-length"} {
				if !slices.Contains(exposed, name) && !slices.Contains(exposed, "*") {
					t.Errorf("%s: the answer carries Content-Range or Accept-Ranges and its Access-Control-Expose-Headers %q lacks %s, so a page reads null", what, exposed, name)
				}
			}
		}
	}
	if !carriers["direct play"] {
		t.Errorf("the direct play carried neither Content-Range nor Accept-Ranges on any answer: the check asked nothing")
	}
}

// A picture asked from one origin, from another and with none is right for each from a shared cache.
func TestAPictureIsRightForEachOriginFromASharedCache(t *testing.T) {
	a := newAccessSite(t)
	h := a.serve(t, RoleStream)
	path := "/Items/" + a.films[0].item + "/Images/Primary"
	var replies []accReply
	for _, origin := range []string{corsA, corsB, ""} {
		var headers []string
		if origin != "" {
			headers = []string{"Origin", origin}
		}
		r := accSend("GET", h.base, path, "", -1, "", headers...)
		if r.err != nil || r.status != 200 {
			t.Fatalf("the picture from %q: %d %v", origin, r.status, r.err)
		}
		if origin != "" && !corsAllows(r, origin) {
			t.Errorf("the picture from %s does not allow the origin (%q): an <img> with crossorigin cannot be drawn", origin, r.header.Values("Access-Control-Allow-Origin"))
		}
		replies = append(replies, r)
	}
	echoed := slices.ContainsFunc(replies, func(r accReply) bool { v := r.header.Get("Access-Control-Allow-Origin"); return v != "" && v != "*" })
	for i, r := range replies {
		if echoed && !slices.Contains(corsReplyList(r, "Vary"), "origin") {
			t.Errorf("the allowed origin is echoed and the picture's answer %d has Vary %q: a shared cache would hand one origin's header to another", i, r.header.Values("Vary"))
		}
	}
}

// No route sets a cookie and a cookie is no token (DOMAINS.md names this check). Driven from the route table: a route
// added tomorrow is asked too. A valid token sent only in a Cookie, under each spelling's name, is a 401 `unauthorized`.
func TestNoRouteSetsACookieAndACookieIsNoToken(t *testing.T) {
	a := newAccessSite(t)
	handlers, routes := a.corsRoutes(t)
	cookieNames := []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "ApiKey", "api_key"}
	var bad []string
	look := func(what string, r accReply) {
		if r.err != nil {
			bad = append(bad, what+": "+r.err.Error())
		} else if problem := corsForbidden(what, r); problem != "" {
			bad = append(bad, problem)
		}
	}
	cookied, viewers := 0, 0
	for _, route := range routes {
		h := handlers[route.Role]
		path, err := a.path(route, a.films[0], a.user)
		if err != nil {
			t.Fatal(err)
		}
		token := a.device(t, a.user, "cors-cookie-"+uuid.NewString())
		body := accRequestBody(route)
		look(route.Method+" "+route.Path+" with a token", accSend(route.Method, h.base, path, token, 3, body, "Origin", corsA))
		look(route.Method+" "+route.Path+" with no token", accSend(route.Method, h.base, path, "", -1, body, "Origin", corsA))
		for _, origin := range corsOrigins {
			look("the preflight of "+route.Method+" "+route.Path, corsRawPreflight(h.base, corsURL(route), origin, route.Method))
		}
		if route.Access != AccessViewer {
			continue
		}
		viewers++
		for _, name := range cookieNames {
			value := token
			if strings.Contains(name, "Authorization") {
				value = `MediaBrowser Client="c", Device="d", DeviceId="cookie", Version="1", Token="` + token + `"`
			}
			cookied++
			r := accSend(route.Method, h.base, path, "", -1, body, "Cookie", name+"="+strings.ReplaceAll(value, `"`, "%22"), "Origin", corsA)
			// The cookie's value may be quoted too: a server that reads either spelling would be let in by one of them.
			r2 := accSend(route.Method, h.base, path, "", -1, body, "Cookie", name+`=`+value+`; `+name+`="`+token+`"`, "Origin", corsA)
			for _, got := range []accReply{r, r2} {
				if got.err != nil || got.status != 401 || strings.TrimSpace(string(got.body)) != "unauthorized" {
					bad = append(bad, fmt.Sprintf("%s %s with the token only in a Cookie named %s: %d %.40q, want 401 unauthorized", route.Method, route.Path, name, got.status, got.body))
				}
			}
		}
	}
	if viewers != 82 || cookied != 82*6 {
		t.Errorf("asked %d viewer routes and %d cookies, want 82 and 492", viewers, cookied)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d wrong; the first 8:\n  %s", len(bad), strings.Join(bad[:min(8, len(bad))], "\n  "))
	}
}

func corsRawPreflight(base, path, origin, method string) accReply {
	return accSendWith(noRedirect, "OPTIONS", base, path, "", -1, "", "Origin", origin, "Access-Control-Request-Method", method,
		"Access-Control-Request-Headers", "authorization,content-type,range,x-emby-authorization,x-emby-token,x-mediabrowser-token")
}

// A preflight reads no token, sends Postgres no statement and runs no handler: asked with a valid token on it, for the
// routes that would change something, on a server whose Postgres is behind a proxy that counts what Postgres sends.
func TestAPreflightSendsPostgresNothingAndRunsNoHandler(t *testing.T) {
	a := newAccessSite(t)
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	pool, err := db.Open(context.Background(), proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// The api role alone starts nothing in the background that asks Postgres (the stream role keeps its snapshot).
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	token := a.device(t, a.user, "cors-inert")
	devices := scalar[int](t, a.pool, "SELECT count(*) FROM devices")
	time.Sleep(500 * time.Millisecond) // Whatever the pool does as it opens.
	before := proxy.Received()
	for _, route := range (&Server{}).Routes() {
		if route.Role != RoleAPI {
			continue
		}
		for _, spelling := range []int{0, 3} {
			headers := []string{"Origin", corsB, "Access-Control-Request-Method", route.Method, "Access-Control-Request-Headers", "authorization,content-type"}
			r := accSend("OPTIONS", server.URL, corsURL(route), token, spelling, "", headers...)
			if r.err != nil || r.status/100 != 2 || len(r.body) != 0 || !corsAllows(r, corsB) {
				t.Errorf("preflight of %s %s: %d %q %v", route.Method, route.Path, r.status, r.body, r.err)
			}
		}
	}
	if after := proxy.Received(); after != before {
		t.Errorf("Postgres sent %d bytes while the server answered preflights, so one of them sent it a statement", after-before)
	}
	if got := scalar[int](t, a.pool, "SELECT count(*) FROM devices"); got != devices {
		t.Errorf("the devices went from %d to %d: a preflight for POST /Sessions/Logout ran its handler", devices, got)
	}
	if r := accSend("GET", server.URL, "/Users/Me", token, 3, ""); r.status != 200 {
		t.Errorf("the token a preflight carried is %d afterwards", r.status)
	}
}

// With Postgres away from two api and two stream replicas, every preflight of every route a replica has is answered, at
// once and by the replica itself: a preflight needs nothing from Postgres.
func TestEveryPreflightIsAnsweredByAnApiAndAStreamReplicaWhilePostgresIsAway(t *testing.T) {
	o := newOutage(t)
	all := append(append([]*replica{}, o.streams...), o.apis...)
	for _, rp := range all {
		rp.proxy.Away()
	}
	var failures []string
	for _, rp := range all {
		asked := 0
		for _, route := range (&Server{}).Routes() {
			if !strings.Contains(rp.role, route.Role) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second) // A hang, not a timing bar.
			r := corsPreflightOn(ctx, rp.server.URL, corsURL(route), corsB, route.Method)
			cancel()
			asked++
			if r.err != nil || r.status/100 != 2 || !corsAllows(r, corsB) {
				failures = append(failures, fmt.Sprintf("%s replica: preflight of %s %s: %d %v", rp.role, route.Method, route.Path, r.status, r.err))
			}
		}
		if asked == 0 {
			failures = append(failures, rp.role+" replica: nothing asked")
		}
	}
	for _, rp := range all {
		rp.proxy.Back()
	}
	if len(failures) > 0 {
		t.Errorf("%d wrong; the first 8:\n  %s", len(failures), strings.Join(failures[:min(8, len(failures))], "\n  "))
	}
}

// A viewer in the snapshot who plays from another origin with a header token and no token in the URL still gets each
// range and segment of the film, and the preflight of each, with the origin allowed, through the outage.
func TestASnapshotViewersRequestsFromAnotherOriginAreAnsweredWhilePostgresIsAway(t *testing.T) {
	p := newPlayAway(t, 1)
	_, refresh := timings(t)
	p.playing(t, 0)
	refreshed(refresh)
	for _, rp := range p.streams {
		rp.away(t, p.pool)
	}
	token := `MediaBrowser Client="c", Device="d", DeviceId="cors-away", Version="1", Token="` + p.tokens[0] + `"`
	var failures []string
	for _, rp := range p.streams {
		for _, rq := range p.requests(0) {
			u, err := url.Parse(rq.path)
			if err != nil {
				t.Fatal(err)
			}
			query := u.Query()
			query.Del("ApiKey")
			u.RawQuery = query.Encode()
			if strings.Contains(strings.ToLower(u.String()), strings.ToLower(p.tokens[0])) {
				t.Fatalf("the token is still in the URL of %s: %s", rq.what, u)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // A hang, not a timing bar.
			pre := corsDo(ctx, "OPTIONS", rp.server.URL, u.String(), "Origin", corsB, "Access-Control-Request-Method", "GET", "Access-Control-Request-Headers", "authorization,range")
			r := corsDo(ctx, "GET", rp.server.URL, u.String(), append([]string{"Origin", corsB, "Authorization", token}, rq.headers...)...)
			cancel()
			if pre.err != nil || pre.status/100 != 2 || !corsAllows(pre, corsB) {
				failures = append(failures, fmt.Sprintf("the preflight of %s: %d %v", rq.what, pre.status, pre.err))
			}
			switch {
			case r.err != nil || r.status != rq.status:
				failures = append(failures, fmt.Sprintf("%s with a header token during the outage: %d %v, want %d", rq.what, r.status, r.err, rq.status))
			case !corsAllows(r, corsB):
				failures = append(failures, fmt.Sprintf("%s during the outage does not allow the origin (%q)", rq.what, r.header.Values("Access-Control-Allow-Origin")))
			}
		}
	}
	for _, rp := range p.streams {
		rp.proxy.Back()
	}
	if len(failures) > 0 {
		t.Errorf("%d wrong; the first 8:\n  %s", len(failures), strings.Join(failures[:min(8, len(failures))], "\n  "))
	}
}

func corsPreflightOn(ctx context.Context, base, path, origin, method string) accReply {
	return corsDo(ctx, "OPTIONS", base, path, "Origin", origin, "Access-Control-Request-Method", method, "Access-Control-Request-Headers", "authorization,range")
}

// The capture of a real Jellyfin 12.4 (compat/captures/cors-12.4.json, test/differential/capture_cors.py) held to what
// jellyscale answers to the same requests: status, every Access-Control-* header and Vary. A difference that is not in
// compat/differences.yaml, under the request "cors: <request>" with the path "(status)" or "header <name>", fails; so
// does an entry that no longer matches a difference.
func TestJellyscaleAnswersTheCapturedCrossOriginRequestsAsJellyfinDoesOrSaysWhy(t *testing.T) {
	capture := readCorsCapture(t)
	a := newAccessSite(t)
	h := a.serve(t, RoleAPI, RoleStream)
	film := a.films[0]
	token := a.device(t, a.user, "cors-capture")
	bearer := `MediaBrowser Client="c", Device="d", DeviceId="capture", Version="1", Token="` + token + `"`
	type difference struct{ request, path string }
	found := map[difference][]string{}
	for _, row := range capture.Rows {
		path := strings.ReplaceAll(row.Path, "{film}", film.source)
		headers := []string{}
		if row.Origin != "" {
			headers = append(headers, "Origin", row.Origin)
		}
		for name, value := range row.Sent {
			headers = append(headers, name, value)
		}
		if row.Token {
			headers = append(headers, "Authorization", bearer)
		}
		got := accSend(row.Method, h.base, path, "", -1, "", headers...)
		if got.err != nil {
			t.Fatalf("%s from %q: %v", row.Request, row.Origin, got.err)
		}
		request := "cors: " + row.Request
		note := func(path, jellyfin, ours string) {
			d := difference{request, path}
			found[d] = append(found[d], fmt.Sprintf("from %q Jellyfin %s, jellyscale %s", row.Origin, jellyfin, ours))
		}
		if got.status != row.Status {
			note("(status)", fmt.Sprint(row.Status), fmt.Sprint(got.status))
		}
		names := map[string]bool{}
		for name := range row.Headers {
			if strings.HasPrefix(name, "access-control-") || name == "vary" { // Allow is recorded and not compared.
				names[name] = true
			}
		}
		for name := range got.header {
			if lower := strings.ToLower(name); strings.HasPrefix(lower, "access-control-") || lower == "vary" {
				names[lower] = true
			}
		}
		for name := range names {
			if jellyfin, ours := corsSet(row.Headers[name]), corsSet(got.header.Get(name)); jellyfin != ours {
				note("header "+name, fmt.Sprintf("%q", row.Headers[name]), fmt.Sprintf("%q", got.header.Get(name)))
			}
		}
	}
	var listed []struct {
		Requests []string `yaml:"requests"`
		Path     string   `yaml:"path"`
		Paths    []string `yaml:"paths"`
		Kind     string   `yaml:"kind"`
		Reason   string   `yaml:"reason"`
	}
	raw, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	allowed := map[difference]bool{}
	used := map[difference]bool{}
	for _, entry := range listed {
		for _, request := range entry.Requests {
			if !strings.HasPrefix(request, "cors: ") {
				continue
			}
			for _, path := range append([]string{entry.Path}, entry.Paths...) {
				if path != "" {
					allowed[difference{request, path}] = true
				}
			}
		}
	}
	for d, what := range found {
		if allowed[d] {
			used[d] = true
			continue
		}
		t.Errorf("%s: %s differs from Jellyfin 12.4 and compat/differences.yaml does not list it (%d times; e.g. %s)", d.request, d.path, len(what), what[0])
	}
	for d := range allowed {
		if !used[d] {
			t.Errorf("compat/differences.yaml lists %s of %q and jellyscale answers as Jellyfin does: remove the entry", d.path, d.request)
		}
	}
}

// corsSet reads a header value as a set of lower-case words, so that the order of a list does not matter.
func corsSet(value string) string {
	var words []string
	for _, word := range strings.Split(value, ",") {
		if word = strings.ToLower(strings.TrimSpace(word)); word != "" {
			words = append(words, word)
		}
	}
	sort.Strings(words)
	return strings.Join(words, ",")
}

func corsDo(ctx context.Context, method, base, path string, headers ...string) accReply {
	request, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return accReply{err: err}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: noRedirect.CheckRedirect}).Do(request)
	if err != nil {
		return accReply{err: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return accReply{status: response.StatusCode, header: response.Header, body: data, err: err}
}

// A preflight for DELETE /Videos/ActiveEncodings runs no handler, even with the viewer's token on it: the viewer's
// transcode keeps going. A real DELETE, as the control, does stop it, so the check could have seen the difference.
func TestAPreflightForDeleteActiveEncodingsStopsNoViewersTranscode(t *testing.T) {
	o := newOutage(t)
	token := `MediaBrowser Client="c", Device="d", DeviceId="dev1", Version="1", Token="` + o.token + `"`
	rp := o.streams[0]
	if a := get(context.Background(), rp.server.URL, o.segmentURL(2)); a.status != 200 {
		t.Fatalf("a segment: %d", a.status)
	}
	live := func() int { return scalar[int](t, o.pool, "SELECT count(*) FROM transcode_viewers WHERE NOT stopped") }
	before := live()
	if before == 0 {
		t.Fatal("the fixture has no viewer of a transcode, so nothing could be stopped")
	}
	path := "/Videos/ActiveEncodings?deviceId=dev1&playSessionId=" + o.psid
	for _, spelling := range []string{"Authorization", "X-Emby-Authorization"} {
		r := corsDo(context.Background(), "OPTIONS", rp.server.URL, path, "Origin", corsB, "Access-Control-Request-Method", "DELETE",
			"Access-Control-Request-Headers", "authorization", spelling, token)
		if r.err != nil || r.status/100 != 2 || !corsAllows(r, corsB) {
			t.Errorf("the preflight (token in %s): %d %v", spelling, r.status, r.err)
		}
	}
	if after := live(); after != before {
		t.Errorf("%d viewers of a transcode were live before the preflights and %d after: a preflight ran the handler", before, after)
	}
	if r := corsDo(context.Background(), "DELETE", rp.server.URL, path, "Authorization", token); r.status != 204 {
		t.Fatalf("the control DELETE answered %d", r.status)
	}
	if after := live(); after >= before {
		t.Errorf("the control DELETE stopped nothing (%d live before, %d after), so this check cannot tell a handler run from none", before, after)
	}
}

// D-016 adds headers and nothing else: each route's answer to a request that carries an Origin differs from its answer
// to the same request with none only in Access-Control-* and Vary: the same status, the same other headers by name, and
// the same body wherever the body is not JSON (JSON holds times, which differ between two asks).
func TestAnOriginChangesNoStatusBodyOrHeaderButAccessControlAndVary(t *testing.T) {
	a := newAccessSite(t)
	handlers, routes := a.corsRoutes(t)
	names := func(r accReply) []string {
		var out []string
		for name := range r.header {
			lower := strings.ToLower(name)
			if lower != "date" && lower != "vary" && !strings.HasPrefix(lower, "access-control-") {
				out = append(out, lower)
			}
		}
		sort.Strings(out)
		return out
	}
	var bad []string
	for _, route := range routes {
		h := handlers[route.Role]
		path, err := a.path(route, a.films[0], a.user)
		if err != nil {
			t.Fatal(err)
		}
		body := accRequestBody(route)
		for _, credential := range []string{"", "token"} {
			ask := func(headers ...string) accReply {
				token, spelling := "", -1
				if credential != "" {
					token, spelling = a.device(t, a.user, "cors-same-"+uuid.NewString()), 3
				}
				return accSend(route.Method, h.base, path, token, spelling, body, headers...)
			}
			plain, withOrigin := ask(), ask("Origin", corsB)
			what := fmt.Sprintf("%s %s (%s)", route.Method, route.Path, map[string]string{"": "no token", "token": "a token"}[credential])
			switch {
			case plain.err != nil || withOrigin.err != nil:
				bad = append(bad, fmt.Sprintf("%s: %v %v", what, plain.err, withOrigin.err))
			case plain.status != withOrigin.status:
				bad = append(bad, fmt.Sprintf("%s: %d without an Origin and %d with one", what, plain.status, withOrigin.status))
			case !slices.Equal(names(plain), names(withOrigin)):
				bad = append(bad, fmt.Sprintf("%s: the headers are %v without an Origin and %v with one", what, names(plain), names(withOrigin)))
			case !strings.Contains(plain.header.Get("Content-Type"), "json") && string(plain.body) != string(withOrigin.body):
				bad = append(bad, fmt.Sprintf("%s: the body changes with an Origin (%d bytes, %d bytes)", what, len(plain.body), len(withOrigin.body)))
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d answers changed; the first 8:\n  %s", len(bad), strings.Join(bad[:min(8, len(bad))], "\n  "))
	}
}
