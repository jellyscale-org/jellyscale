package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// JS-144 (D-016): any origin may call the API and media routes, never with credentials. These checks need nothing
// outside Go: a handler with one role and no database answers every preflight, which is the point (a preflight reads
// no token and sends Postgres nothing). cors_integration_test.go asks the same of real answers, with Postgres.

var corsOrigins = []string{"app://localhost", "http://app.localhost", "https://app.localhost", "https://app.example.test"}

// The six headers a page's script sets on a request to the API.
var corsSix = []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "Content-Type", "Range"}

var corsWildcard = regexp.MustCompile(`\{\w+\}`)

// corsURL is a route's path with every wildcard filled: a preflight is a function of the request line, so any id will do.
func corsURL(route Route) string {
	return corsWildcard.ReplaceAllString(route.Path, "0123456789abcdef0123456789abcdef")
}

func corsHandler(roles ...string) http.Handler {
	return (&Server{Roles: roles, Log: slog.New(slog.DiscardHandler), Name: "one", ServerID: "id-one"}).Handler()
}

// corsRoles are the roles a process has to serve route, and no other.
func corsRoles(route Route) []string { return []string{route.Role} }

type corsAnswer struct {
	*httptest.ResponseRecorder
}

func corsAsk(h http.Handler, method, path string, headers ...string) corsAnswer {
	request := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	return corsAnswer{recorder}
}

func corsPreflight(h http.Handler, path, origin, method, headers string) corsAnswer {
	args := []string{"Origin", origin, "Access-Control-Request-Method", method}
	if headers != "" {
		args = append(args, "Access-Control-Request-Headers", headers)
	}
	return corsAsk(h, "OPTIONS", path, args...)
}

// corsList is a comma-separated header as a set of lower-case words.
func corsList(a corsAnswer, name string) []string {
	var out []string
	for _, value := range a.Header().Values(name) {
		for _, word := range strings.Split(value, ",") {
			if word = strings.ToLower(strings.TrimSpace(word)); word != "" {
				out = append(out, word)
			}
		}
	}
	return out
}

func (a corsAnswer) allowsOrigin(origin string) bool {
	got := a.Header().Values("Access-Control-Allow-Origin")
	return len(got) == 1 && (got[0] == "*" || got[0] == origin)
}

// allowsMethod: by name, or `*`, which a request with no credentials takes for any method.
func (a corsAnswer) allowsMethod(method string) bool {
	list := corsList(a, "Access-Control-Allow-Methods")
	return slices.Contains(list, strings.ToLower(method)) || slices.Contains(list, "*")
}

func (a corsAnswer) allowsMethodByName(method string) bool {
	return slices.Contains(corsList(a, "Access-Control-Allow-Methods"), strings.ToLower(method))
}

// allowsHeader: Authorization only by name, because a browser does not take `*` for it.
func (a corsAnswer) allowsHeader(name string) bool {
	list := corsList(a, "Access-Control-Allow-Headers")
	if slices.Contains(list, strings.ToLower(name)) {
		return true
	}
	return slices.Contains(list, "*") && !strings.EqualFold(name, "Authorization")
}

func (a corsAnswer) is2xx() bool { return a.Code/100 == 2 }

func TestEveryRoutePreflightIsAnsweredOnAProcessWithOnlyItsRole(t *testing.T) {
	routes := (&Server{}).Routes()
	var byRole = map[string]int{}
	byMethod := map[string]int{}
	paths := map[string]bool{}
	for _, route := range routes {
		byRole[route.Role]++
		byMethod[route.Method]++
		paths[route.Path] = true
	}
	if len(routes) != 109 || len(paths) != 99 || byRole[RoleAPI] != 96 || byRole[RoleStream] != 13 ||
		byMethod["GET"] != 74 || byMethod["POST"] != 27 || byMethod["DELETE"] != 6 || byMethod["PATCH"] != 2 {
		t.Fatalf("the route table is %d routes on %d paths, by role %v, by method %v; the test pins 109 on 99, 96 api and 13 stream, 74 GET, 27 POST, 6 DELETE, 2 PATCH", len(routes), len(paths), byRole, byMethod)
	}
	asked := map[string]int{}
	var bad []string
	all := strings.Join(corsSix, ",")
	for _, route := range routes {
		h := corsHandler(corsRoles(route)...) // no database, and only this route's role
		for _, origin := range corsOrigins {
			asked[route.Role]++
			a := corsPreflight(h, corsURL(route), origin, route.Method, strings.ToLower(all))
			switch {
			case !a.is2xx():
				bad = append(bad, fmt.Sprintf("%s %s from %s: status %d", route.Method, route.Path, origin, a.Code))
			case !a.allowsOrigin(origin):
				bad = append(bad, fmt.Sprintf("%s %s from %s: Access-Control-Allow-Origin %q", route.Method, route.Path, origin, a.Header().Values("Access-Control-Allow-Origin")))
			case !a.allowsMethod(route.Method):
				bad = append(bad, fmt.Sprintf("%s %s from %s: Access-Control-Allow-Methods %q", route.Method, route.Path, origin, a.Header().Get("Access-Control-Allow-Methods")))
			case a.Body.Len() != 0:
				bad = append(bad, fmt.Sprintf("%s %s from %s: a body of %d bytes", route.Method, route.Path, origin, a.Body.Len()))
			}
			for _, name := range corsSix {
				if a.is2xx() && !a.allowsHeader(name) {
					bad = append(bad, fmt.Sprintf("%s %s from %s: %s is not allowed (%q)", route.Method, route.Path, origin, name, a.Header().Get("Access-Control-Allow-Headers")))
				}
			}
		}
	}
	if asked[RoleAPI] != 384 || asked[RoleStream] != 52 {
		t.Fatalf("asked %v preflights, want 384 on api-only processes and 52 on stream-only ones", asked)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("%d of 436 preflights were refused or incomplete; the first 12:\n  %s", len(bad), strings.Join(bad[:min(12, len(bad))], "\n  "))
	}
}

func TestAPreflightAllowsEachOfTheSixHeadersAloneTogetherInAnyOrderAndAnyCase(t *testing.T) {
	h := corsHandler(RoleAPI)
	var bad []string
	try := func(what, asked string) {
		a := corsPreflight(h, "/Users/AuthenticateByName", "app://localhost", "POST", asked)
		if !a.is2xx() || !a.allowsOrigin("app://localhost") {
			bad = append(bad, fmt.Sprintf("%s (%q): status %d, origin %q", what, asked, a.Code, a.Header().Get("Access-Control-Allow-Origin")))
			return
		}
		for _, name := range strings.Split(asked, ",") {
			if !a.allowsHeader(strings.TrimSpace(name)) {
				bad = append(bad, fmt.Sprintf("%s (%q): %s is not allowed (%q)", what, asked, name, a.Header().Get("Access-Control-Allow-Headers")))
			}
		}
	}
	for _, name := range corsSix {
		try("alone", name)
		try("alone, lower case", strings.ToLower(name))
		try("alone, upper case", strings.ToUpper(name))
	}
	try("together", strings.Join(corsSix, ", "))
	reversed := slices.Clone(corsSix)
	slices.Reverse(reversed)
	try("reversed", strings.Join(reversed, ","))
	try("mixed case, spaced", "aUtHoRiZaTiOn , x-EMBY-token,RANGE ,content-TYPE, X-emby-AUTHORIZATION,x-mediabrowser-TOKEN")
	// Authorization must be in the list by its name, where a browser looks for it.
	a := corsPreflight(h, "/Users/AuthenticateByName", "app://localhost", "POST", "authorization")
	if !slices.Contains(corsList(a, "Access-Control-Allow-Headers"), "authorization") {
		bad = append(bad, fmt.Sprintf("Authorization is not allowed by name (%q), and a browser does not take * for it", a.Header().Get("Access-Control-Allow-Headers")))
	}
	if len(bad) > 0 {
		t.Errorf("%d wrong:\n  %s", len(bad), strings.Join(bad[:min(12, len(bad))], "\n  "))
	}
}

func TestAPreflightAllowsEachMethodOfARouteByNameAndHeadWhereGetIsServed(t *testing.T) {
	routes := (&Server{}).Routes()
	byPath := map[string][]string{}
	for _, route := range routes {
		byPath[route.Path] = append(byPath[route.Path], route.Method)
	}
	var bad []string
	for _, route := range routes {
		h := corsHandler(route.Role)
		methods := byPath[route.Path]
		ask := []string{route.Method}
		if route.Method == "GET" {
			ask = append(ask, "HEAD")
		}
		for _, method := range ask {
			a := corsPreflight(h, corsURL(route), "https://app.example.test", method, "authorization,range")
			// DELETE and PATCH are by name; so is every method a path serves (a path with two methods allows both).
			if !a.is2xx() || !a.allowsOrigin("https://app.example.test") || !a.allowsMethodByName(method) {
				bad = append(bad, fmt.Sprintf("%s %s asking %s: status %d, methods %q", route.Method, route.Path, method, a.Code, a.Header().Get("Access-Control-Allow-Methods")))
			}
		}
		for _, other := range methods {
			a := corsPreflight(h, corsURL(route), "https://app.example.test", other, "authorization")
			if !a.is2xx() || !a.allowsMethodByName(other) {
				bad = append(bad, fmt.Sprintf("%s: the preflight asking the path's other method %s: status %d, methods %q", route.Path, other, a.Code, a.Header().Get("Access-Control-Allow-Methods")))
			}
		}
	}
	paths := 0
	for _, methods := range byPath {
		if len(methods) == 2 {
			paths++
		}
	}
	if paths != 10 {
		t.Fatalf("%d paths have two methods, the test pins 10", paths)
	}
	if len(bad) > 0 {
		t.Errorf("%d wrong; the first 12:\n  %s", len(bad), strings.Join(bad[:min(12, len(bad))], "\n  "))
	}
}

func TestAPathInAnotherLetterCaseOrWithATrailingSlashIsAnsweredAsTheRouteItselfIs(t *testing.T) {
	h := corsHandler(RoleAPI)
	for _, path := range []string{"/Users/AuthenticateByName", "/users/authenticatebyname", "/USERS/AUTHENTICATEBYNAME", "/Users/AuthenticateByName/"} {
		a := corsPreflight(h, path, "app://localhost", "POST", "authorization,content-type")
		if !a.is2xx() || !a.allowsOrigin("app://localhost") || !a.allowsMethod("POST") || !a.allowsHeader("Content-Type") {
			t.Errorf("preflight of %s: status %d, headers %v", path, a.Code, a.Header())
		}
	}
	// Every route of the table, not only the sign-in (product owner, D-019).
	for _, route := range (&Server{}).Routes() {
		h := corsHandler(route.Role)
		url := corsURL(route)
		spellings := []string{strings.ToLower(url), strings.ToUpper(url)}
		if url != "/" {
			spellings = append(spellings, url+"/")
		}
		for _, path := range spellings {
			a := corsPreflight(h, path, "app://localhost", route.Method, "authorization,content-type")
			if !a.is2xx() || !a.allowsOrigin("app://localhost") || !a.allowsMethod(route.Method) {
				t.Errorf("%s %s: preflight of %s: status %d, headers %v", route.Method, route.Path, path, a.Code, a.Header())
			}
		}
	}
	// A route the process does not have is not answered as one: the ingress sends it elsewhere.
	if a := corsPreflight(corsHandler(RoleStream), "/Users/AuthenticateByName", "app://localhost", "POST", "authorization"); a.Code != http.StatusNotFound && a.allowsOrigin("app://localhost") {
		t.Logf("a stream process answers a preflight of an api route with %d (open question 4: the ingress never sends one)", a.Code)
	}
}

// A preflight is a function of the request line, Origin, the two Access-Control-Request-* headers and the route table.
func TestAPreflightIsTheSameWithAnyCredentialAnyIdAndAnyServer(t *testing.T) {
	routes := (&Server{}).Routes()
	shape := func(a corsAnswer) string {
		var lines []string
		for name, values := range a.Header() {
			if name != "Date" {
				lines = append(lines, name+": "+strings.Join(values, ","))
			}
		}
		sort.Strings(lines)
		return fmt.Sprintf("%d\n%s\n%q", a.Code, strings.Join(lines, "\n"), a.Body.String())
	}
	other := (&Server{Roles: []string{RoleAPI, RoleStream}, Log: slog.New(slog.DiscardHandler), Name: "another server", ServerID: "id-two"}).Handler()
	for _, route := range routes {
		h := corsHandler(route.Role)
		first := corsPreflight(h, corsURL(route), "app://localhost", route.Method, "authorization,range")
		if !first.is2xx() || !first.allowsOrigin("app://localhost") {
			t.Errorf("%s %s: the preflight is %d and allows %q: the sameness below would hold of any refusal", route.Method, route.Path, first.Code, first.Header().Get("Access-Control-Allow-Origin"))
		}
		want := shape(first)
		credentials := [][]string{
			nil,
			{"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="x", Version="1", Token="0123456789abcdef"`},
			{"X-Emby-Token", "no-such-token"}, {"X-MediaBrowser-Token", "no-such-token"}, {"Cookie", "X-Emby-Token=abc; ApiKey=abc"},
		}
		for _, credential := range credentials {
			args := append([]string{"Origin", "app://localhost", "Access-Control-Request-Method", route.Method, "Access-Control-Request-Headers", "authorization,range"}, credential...)
			if got := shape(corsAsk(h, "OPTIONS", corsURL(route), args...)); got != want {
				t.Errorf("%s %s: with %v the preflight answers\n%s\nand without a credential\n%s", route.Method, route.Path, credential, got, want)
			}
		}
		// Another id, and a server of another name and id (which also has both roles).
		changed := strings.ReplaceAll(corsURL(route), "0123456789abcdef0123456789abcdef", "ffffffffffffffffffffffffffffffff")
		if got := shape(corsPreflight(h, changed, "app://localhost", route.Method, "authorization,range")); got != want {
			t.Errorf("%s %s: another id changes the preflight:\n%s\nvs\n%s", route.Method, route.Path, got, want)
		}
		if got := shape(corsPreflight(other, corsURL(route), "app://localhost", route.Method, "authorization,range")); got != want {
			t.Errorf("%s %s: another server's preflight differs:\n%s\nvs\n%s", route.Method, route.Path, got, want)
		}
		for name, values := range corsPreflight(h, corsURL(route), "app://localhost", route.Method, "authorization").Header() {
			for _, value := range values {
				for _, secret := range []string{"one", "id-one", Version} {
					if value == secret {
						t.Errorf("%s %s: the preflight's %s is %q, which names this server", route.Method, route.Path, name, value)
					}
				}
			}
		}
	}
}

type corsLog struct{ buf *bytes.Buffer }

func (l corsLog) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (l corsLog) Handle(_ context.Context, r slog.Record) error {
	fmt.Fprintln(l.buf, r.Message)
	return nil
}
func (l corsLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l corsLog) WithGroup(string) slog.Handler      { return l }

func TestAPreflightForAServedRouteIsNotLoggedAsAnUnknownRoute(t *testing.T) {
	buf := &bytes.Buffer{}
	h := (&Server{Roles: []string{RoleAPI, RoleStream}, Log: slog.New(corsLog{buf})}).Handler()
	a := corsPreflight(h, "/Users/AuthenticateByName", "app://localhost", "POST", "authorization")
	if !a.is2xx() {
		t.Fatalf("status %d", a.Code)
	}
	if strings.Contains(buf.String(), "unknown route") {
		t.Errorf("a preflight of a served route was logged: %q", buf.String())
	}
}

// What an operator owns is not opened: nothing of D-016 reaches /healthz, /readyz or (the listener of) /metrics.
func TestHealthAndReadinessCarryNoCrossOriginHeaderAndAnswerAsBefore(t *testing.T) {
	s := &Server{Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler)}
	h := s.Handler()
	check := func(what string, a corsAnswer, code int, body string) {
		t.Helper()
		for name := range a.Header() {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("%s carries %s: %s", what, name, a.Header().Get(name))
			}
		}
		if a.Code != code || a.Body.String() != body {
			t.Errorf("%s answers %d %q, want %d %q (what it answers today)", what, a.Code, a.Body.String(), code, body)
		}
	}
	for _, origin := range corsOrigins {
		check("GET /healthz from "+origin, corsAsk(h, "GET", "/healthz", "Origin", origin), 204, "")
		check("GET /readyz from "+origin, corsAsk(h, "GET", "/readyz", "Origin", origin), 200, "ok")
		for _, path := range []string{"/healthz", "/readyz"} {
			check("preflight of "+path+" from "+origin, corsPreflight(h, path, origin, "GET", "authorization"), 404, "404 page not found\n")
		}
	}
	s.BeginDrain()
	check("GET /readyz when draining", corsAsk(h, "GET", "/readyz", "Origin", "app://localhost"), 503, "not ready\n")
	check("preflight of /readyz when draining", corsPreflight(h, "/readyz", "app://localhost", "GET", "authorization"), 404, "404 page not found\n")
}

// No answer of a route allows credentials or sets a cookie, with a token or without: here the answers that need no
// database (no token, a cookie that is no token, a preflight). cors_integration_test.go asks them with a token.
func TestNoAnswerWithoutADatabaseAllowsCredentialsOrSetsACookie(t *testing.T) {
	var bad []string
	look := func(what string, a corsAnswer) {
		// An answer that a browser refuses carries none of these either: it is the preflight's being answered that this asks.
		if strings.HasPrefix(what, "preflight") && !a.is2xx() {
			bad = append(bad, what+": status "+fmt.Sprint(a.Code)+", not a preflight answer")
		}
		for _, name := range []string{"Access-Control-Allow-Credentials", "Set-Cookie"} {
			if a.Header().Get(name) != "" {
				bad = append(bad, what+": "+name+": "+a.Header().Get(name))
			}
		}
	}
	for _, route := range (&Server{}).Routes() {
		h := corsHandler(route.Role)
		for _, origin := range corsOrigins {
			look(fmt.Sprintf("preflight %s %s from %s", route.Method, route.Path, origin), corsPreflight(h, corsURL(route), origin, route.Method, strings.ToLower(strings.Join(corsSix, ","))))
		}
		look(fmt.Sprintf("%s %s with an Origin and a Cookie", route.Method, route.Path),
			corsAsk(h, route.Method, corsURL(route), "Origin", "https://app.example.test", "Cookie", "sid=1"))
		look(fmt.Sprintf("%s %s with no token", route.Method, route.Path), corsAsk(h, route.Method, corsURL(route), "Origin", "https://app.example.test"))
	}
	if len(bad) > 0 {
		t.Errorf("%d answers allow credentials or set a cookie; the first 8:\n  %s", len(bad), strings.Join(bad[:min(8, len(bad))], "\n  "))
	}
}

// Access is as before: each viewer route asked from another origin with no token is 401 `unauthorized`, and the answer
// can be read by the page, which is what lets a client tell a refused request from a lost connection.
func TestEachViewerRouteAskedFromAnotherOriginWithNoTokenIsA401TheOriginMayRead(t *testing.T) {
	viewers := 0
	var bad []string
	for _, route := range (&Server{}).Routes() {
		if route.Access == AccessAnyone {
			continue
		}
		viewers++
		for _, origin := range []string{"app://localhost", "https://app.example.test"} {
			a := corsAsk(corsHandler(route.Role), route.Method, corsURL(route), "Origin", origin, "Cookie", "X-Emby-Token=whatever")
			if a.Code != 401 || strings.TrimSpace(a.Body.String()) != "unauthorized" {
				bad = append(bad, fmt.Sprintf("%s %s from %s: %d %q, want 401 unauthorized", route.Method, route.Path, origin, a.Code, a.Body.String()))
			} else if !a.allowsOrigin(origin) {
				bad = append(bad, fmt.Sprintf("%s %s from %s: the 401 does not allow the origin (%q), so the page sees a failed connection", route.Method, route.Path, origin, a.Header().Get("Access-Control-Allow-Origin")))
			}
		}
	}
	if viewers != 95 {
		t.Fatalf("%d viewer routes asked, want the 95 that need a token (109 less the 14 open ones)", viewers)
	}
	if len(bad) > 0 {
		t.Errorf("%d wrong; the first 8:\n  %s", len(bad), strings.Join(bad[:min(8, len(bad))], "\n  "))
	}
}

// A picture asked from one origin, another and none is right for each from a shared cache: the allowed origin is `*`,
// or every answer of the route carries Vary: Origin whatever the request. Asked here of the answers that need no database.
func TestEveryAnswerOfARouteIsRightFromASharedCache(t *testing.T) {
	var bad []string
	for _, route := range (&Server{}).Routes() {
		h := corsHandler(route.Role)
		answers := []corsAnswer{
			corsAsk(h, route.Method, corsURL(route), "Origin", "app://localhost"),
			corsAsk(h, route.Method, corsURL(route), "Origin", "https://app.example.test"),
			corsAsk(h, route.Method, corsURL(route)),
		}
		for i, a := range answers[:2] {
			if !a.allowsOrigin([]string{"app://localhost", "https://app.example.test"}[i]) {
				bad = append(bad, fmt.Sprintf("%s %s, request %d: the answer %d does not allow its origin (%q), so there is no cache to be right or wrong", route.Method, route.Path, i, a.Code, a.Header().Get("Access-Control-Allow-Origin")))
			}
		}
		echoed := false
		for _, a := range answers {
			if v := a.Header().Get("Access-Control-Allow-Origin"); v != "" && v != "*" {
				echoed = true
			}
		}
		if !echoed {
			continue
		}
		for i, a := range answers {
			if !slices.Contains(corsList(a, "Vary"), "origin") {
				bad = append(bad, fmt.Sprintf("%s %s, request %d (%v): origin echoed by another answer, and Vary is %q", route.Method, route.Path, i, []string{"app", "web", "no Origin"}[i], a.Header().Get("Vary")))
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d answers lack Vary: Origin; the first 8:\n  %s", len(bad), strings.Join(bad[:min(8, len(bad))], "\n  "))
	}
}

// A preflight allows only what the path serves: it never lists a method the path does not serve, and one that asks
// for such a method does not allow it. A build that answers every preflight with one blanket list, or that answers
// OPTIONS for /healthz, /readyz or a path no route has, fails here. (What a real Jellyfin answers to such a preflight
// is the capture's; this holds the part that is true of any right build.)
func TestAPreflightNeverAllowsAMethodThePathDoesNotServe(t *testing.T) {
	routes := (&Server{}).Routes()
	// What a path serves is what the server's mux routes there: the same patterns, matched
	// as the mux matches them, so GET /Users/{userId} also serves GET /Users/AuthenticateByName
	// (lead correction: the guard first compared route paths as strings).
	muxes := map[string]*http.ServeMux{}
	for _, route := range routes {
		if muxes[route.Role] == nil {
			muxes[route.Role] = http.NewServeMux()
		}
		muxes[route.Role].HandleFunc(route.Method+" "+lowerLiterals(route.Path), func(http.ResponseWriter, *http.Request) {})
	}
	servedAt := func(role, url, method string) bool {
		_, pattern := muxes[role].Handler(httptest.NewRequest(method, lowerLiterals(url), nil))
		return pattern != ""
	}
	var bad []string
	for _, route := range routes {
		h := corsHandler(route.Role)
		ok := []string{"OPTIONS"}
		for _, method := range []string{"GET", "HEAD", "POST", "DELETE", "PATCH", "PUT"} {
			if servedAt(route.Role, corsURL(route), method) {
				ok = append(ok, method)
			}
		}
		for _, method := range []string{"POST", "DELETE", "PATCH", "PUT", "GET"} {
			if slices.Contains(ok, method) {
				continue
			}
			a := corsPreflight(h, corsURL(route), "https://app.example.test", method, "authorization")
			if a.allowsMethod(method) {
				bad = append(bad, fmt.Sprintf("%s: a preflight asking %s allows it (%q)", route.Path, method, a.Header().Get("Access-Control-Allow-Methods")))
			}
		}
		a := corsPreflight(h, corsURL(route), "https://app.example.test", route.Method, "authorization")
		for _, word := range corsList(a, "Access-Control-Allow-Methods") {
			if word != "*" && !slices.Contains(ok, strings.ToUpper(word)) {
				bad = append(bad, fmt.Sprintf("%s lists %s, which it does not serve", route.Path, word))
			}
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		a := corsPreflight(corsHandler(RoleAPI), path, "https://app.example.test", "GET", "authorization")
		if a.Header().Get("Access-Control-Allow-Origin") != "" || a.Header().Get("Access-Control-Allow-Methods") != "" {
			bad = append(bad, path+" answers a preflight with a cross-origin header")
		}
	}
	if len(bad) > 0 {
		t.Errorf("%d wrong; the first 12:\n  %s", len(bad), strings.Join(bad[:min(12, len(bad))], "\n  "))
	}
}
