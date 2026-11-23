package differential

// JS-126: what compat/captures/access-12.4.json must hold, and what docs and compat/differences.yaml may say
// about it. The capture is written by a script kept in test/differential (capture_access*) from a real
// jellyfin/jellyfin:12.4. Its shape, which these tests read:
//
//	{"image": "jellyfin/jellyfin:12.4", "server_version": "12.4.x",
//	 "me_before": 200, "me_after": 200,          GET /Users/Me with the non-administrator's token, before and after the sweep
//	 "rows": [{"key": "GET /Videos/{id}/stream?static=true",   method, a space, what was asked ({id} any item id, {userId} the caller's own)
//	           "method": "GET", "route": "/Videos/{itemId}/{file}",   the template of compat/endpoints.yaml
//	           "asks": "/Videos/{id}/stream?static=true",
//	           "no_token": 200, "unknown_token": 401, "user_token": 200,   statuses; a redirect is its 3xx
//	           "token_spelling": "ApiKey",        the one spelling both tokens were sent in (one of the seven)
//	           "why": "",                          required when user_token is not 2xx: why Jellyfin refuses that user
//	           "media": "static stream",           only on a media route, one of mediaLabels
//	           "bodies": {"no_token": "same-bytes", "unknown_token": "refused", "user_token": "same-bytes"}}],
//	 "open_without_token": ["GET ..."],            the keys whose no_token is 2xx or 3xx
//	 "refused_to_user": ["GET ..."]}               the keys whose user_token is 401 or 403
//
// bodies says whether the body was the content: "same-bytes" (byte for byte the film file or the font),
// "non-empty" (not empty and of the route's kind), "no-bytes" (a 2xx with an empty or error body), "no-body"
// (a HEAD answer), "refused" (a status that is not 2xx). access-12.4.md lists the same two lists, one
// "- `key`" line each, under the headings "## Open with no token" and "## Refused to the non-administrator".

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type accessRow struct {
	Key          string            `json:"key"`
	Method       string            `json:"method"`
	Route        string            `json:"route"`
	Asks         string            `json:"asks"`
	NoToken      int               `json:"no_token"`
	UnknownToken int               `json:"unknown_token"`
	UserToken    int               `json:"user_token"`
	Spelling     string            `json:"token_spelling"`
	Why          string            `json:"why"`
	Media        string            `json:"media"`
	Bodies       map[string]string `json:"bodies"`
}

type accessCapture struct {
	Image            string      `json:"image"`
	ServerVersion    string      `json:"server_version"`
	MeBefore         int         `json:"me_before"`
	MeAfter          int         `json:"me_after"`
	Rows             []accessRow `json:"rows"`
	OpenWithoutToken []string    `json:"open_without_token"`
	RefusedToUser    []string    `json:"refused_to_user"`
}

const accessCapturePath = "../../compat/captures/access-12.4.json"

func loadAccessCapture(t *testing.T) accessCapture {
	t.Helper()
	raw, err := os.ReadFile(accessCapturePath)
	if err != nil {
		t.Fatalf("the capture of what Jellyfin opens is missing: %v", err)
	}
	var c accessCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c accessCapture) row(key string) (accessRow, bool) {
	for _, r := range c.Rows {
		if r.Key == key {
			return r, true
		}
	}
	return accessRow{}, false
}

func is2xx(s int) bool     { return s >= 200 && s <= 299 }
func isOpen(s int) bool    { return s >= 200 && s <= 399 }
func isRefused(s int) bool { return s == 401 || s == 403 }

type endpoint struct {
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
	Access string `yaml:"access"`
}

func loadEndpoints(t *testing.T) []endpoint {
	t.Helper()
	raw, err := os.ReadFile("../../compat/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Routes []endpoint }
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// jellyscale's own namespace has no Jellyfin route to observe: a real Jellyfin answers 404 there.
	var jellyfin []endpoint
	for _, e := range doc.Routes {
		if !strings.HasPrefix(strings.ToLower(e.Path), "/jellyscale/") {
			jellyfin = append(jellyfin, e)
		}
	}
	return jellyfin
}

// uncovered lists the routes no row of rows is for.
func uncovered(routes []endpoint, rows []accessRow) []string {
	var out []string
	for _, e := range routes {
		found := false
		for _, r := range rows {
			if r.Method == e.Method && r.Route == e.Path {
				found = true
			}
		}
		if !found {
			out = append(out, e.Method+" "+e.Path)
		}
	}
	return out
}

func TestAnAccessCaptureScriptIsKeptInTheRepository(t *testing.T) {
	found, _ := filepath.Glob("capture_access*")
	if len(found) == 0 {
		t.Error("no test/differential/capture_access* script writes compat/captures/access-12.4.json")
	}
}

func TestTheAccessCaptureIsOfJellyfin12_4(t *testing.T) {
	c := loadAccessCapture(t)
	if c.Image != "jellyfin/jellyfin:12.4" || !strings.HasPrefix(c.ServerVersion, "12.4") {
		t.Errorf("image %q, server version %q", c.Image, c.ServerVersion)
	}
}

func TestTheAccessCaptureHasARowForEveryRouteOfEndpointsYaml(t *testing.T) {
	c := loadAccessCapture(t)
	routes := loadEndpoints(t)
	if missing := uncovered(routes, c.Rows); len(missing) > 0 {
		t.Errorf("the capture has no row for %v", missing)
	}
	// The {file} route is asked three ways, so there are at least two rows more than routes.
	if len(c.Rows) < len(routes)+2 {
		t.Errorf("%d rows for %d routes", len(c.Rows), len(routes))
	}
	for _, want := range []string{"GET", "HEAD"} {
		found := false
		for _, r := range c.Rows {
			found = found || (r.Method == want && r.Route == "/Videos/{itemId}/{file}" && strings.Contains(r.Asks, "/stream?") && strings.Contains(r.Asks, "static=true"))
		}
		if !found {
			t.Errorf("no %s row for stream?static=true", want)
		}
	}
	mkv := false
	for _, r := range c.Rows {
		mkv = mkv || (r.Method == "GET" && r.Route == "/Videos/{itemId}/{file}" && strings.Contains(r.Asks, "/stream.mkv"))
	}
	if !mkv {
		t.Error("no row for stream.mkv")
	}
}

// The test above is what fails on a route the capture lacks; this shows it would.
func TestACaptureThatLacksARouteIsReportedAsLackingIt(t *testing.T) {
	routes := []endpoint{{"GET", "/a", "viewer"}, {"POST", "/b", "viewer"}}
	got := uncovered(routes, []accessRow{{Method: "GET", Route: "/a"}})
	if !slices.Equal(got, []string{"POST /b"}) {
		t.Errorf("got %v", got)
	}
}

var seven = []string{"Authorization: MediaBrowser", "Authorization: Emby", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "ApiKey", "api_key"}

func TestEveryRowHasThreeStatusesOneSpellingAndAWhyWhereTheUserWasRefused(t *testing.T) {
	c := loadAccessCapture(t)
	keys := map[string]bool{}
	for _, r := range c.Rows {
		if r.Key != r.Method+" "+r.Asks || keys[r.Key] {
			t.Errorf("row %q: the key is not its method and what it asks, or repeats", r.Key)
		}
		keys[r.Key] = true
		for name, s := range map[string]int{"no_token": r.NoToken, "unknown_token": r.UnknownToken, "user_token": r.UserToken} {
			if s < 100 || s > 599 {
				t.Errorf("%s: %s is %d", r.Key, name, s)
			}
		}
		if !slices.Contains(seven, r.Spelling) {
			t.Errorf("%s: token_spelling %q is not one of the seven", r.Key, r.Spelling)
		}
		if !is2xx(r.UserToken) && strings.TrimSpace(r.Why) == "" {
			t.Errorf("%s: the non-administrator got %d and the row does not say why", r.Key, r.UserToken)
		}
	}
}

func TestTheNonAdministratorsTokenStillWorkedWhenTheSweepEnded(t *testing.T) {
	c := loadAccessCapture(t)
	if c.MeBefore != 200 || c.MeAfter != 200 {
		t.Errorf("GET /Users/Me answered %d before and %d after the sweep; POST /Sessions/Logout may have turned later rows into 401", c.MeBefore, c.MeAfter)
	}
}

var mediaLabels = []string{"static stream", "static stream head", "stream.mkv", "master playlist", "main playlist", "segment",
	"text subtitle", "text subtitle from an offset", "image subtitle", "font", "download", "poster", "chapter picture"}

func TestEveryMediaRouteSaysForEachCredentialWhetherTheBodyWasTheContent(t *testing.T) {
	c := loadAccessCapture(t)
	byLabel := map[string][]accessRow{}
	for _, r := range c.Rows {
		if r.Media != "" {
			byLabel[r.Media] = append(byLabel[r.Media], r)
		}
	}
	for _, label := range mediaLabels {
		rows := byLabel[label]
		if len(rows) != 1 {
			t.Errorf("%d rows are labelled media %q, want 1", len(rows), label)
			continue
		}
		r := rows[0]
		for name, status := range map[string]int{"no_token": r.NoToken, "unknown_token": r.UnknownToken, "user_token": r.UserToken} {
			body := r.Bodies[name]
			var ok []string
			switch {
			case !is2xx(status):
				ok = []string{"refused"}
			case label == "static stream head":
				ok = []string{"no-body"}
			case label == "static stream" || label == "download" || label == "font":
				ok = []string{"same-bytes", "no-bytes"} // a file sent as it is: the same bytes, or nothing worth calling content
			default:
				ok = []string{"non-empty", "no-bytes"}
			}
			if !slices.Contains(ok, body) {
				t.Errorf("%s (%s) answered %d to %s and records the body as %q, want one of %v", label, r.Key, status, name, body, ok)
			}
		}
	}
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

func (c accessCapture) recomputed() (open, refused []string) {
	for _, r := range c.Rows {
		if isOpen(r.NoToken) {
			open = append(open, r.Key)
		}
		if isRefused(r.UserToken) {
			refused = append(refused, r.Key)
		}
	}
	return sorted(open), sorted(refused)
}

func TestTheTwoListsAreExactlyWhatTheRowsGiveInTheJSON(t *testing.T) {
	c := loadAccessCapture(t)
	open, refused := c.recomputed()
	if got := sorted(c.OpenWithoutToken); !slices.Equal(got, open) {
		t.Errorf("open_without_token is %v, the rows give %v", got, open)
	}
	if got := sorted(c.RefusedToUser); !slices.Equal(got, refused) {
		t.Errorf("refused_to_user is %v, the rows give %v", got, refused)
	}
}

// What the first probe of Jellyfin 12.4 found (see notoken_test.go) must still be what the capture shows, or
// the improvement entries built on it would rest on nothing.
func TestTheCaptureShowsWhatTheFirstProbeOfJellyfinFound(t *testing.T) {
	c := loadAccessCapture(t)
	for _, label := range []string{"static stream", "static stream head", "stream.mkv", "text subtitle", "text subtitle from an offset", "font", "poster"} {
		found := false
		for _, r := range c.Rows {
			found = found || (r.Media == label && isOpen(r.NoToken))
		}
		if !found {
			t.Errorf("the capture does not show Jellyfin 12.4 answering the %s with no token (it did on 2026-11-16)", label)
		}
	}
	for _, label := range []string{"master playlist", "download"} {
		for _, r := range c.Rows {
			if r.Media == label && isOpen(r.NoToken) {
				t.Errorf("the capture shows the %s open with no token; the first probe got 401", label)
			}
		}
	}
}

// endpointAccess is the access word of a request's route, a HEAD being served by its GET.
func endpointAccess(t *testing.T, routes []endpoint, row accessRow) string {
	t.Helper()
	for _, e := range routes {
		if e.Path == row.Route && (e.Method == row.Method || (row.Method == "HEAD" && e.Method == "GET")) {
			return e.Access
		}
	}
	return ""
}

var idRef = regexp.MustCompile(`\{\{[^}]+\}\}`)

// rowOf finds the capture row a request of requests.yaml is: its method and its path without the query, ids
// as {id} and the caller as {userId}. Two rows that match are a mistake of the capture.
func rowOf(t *testing.T, c accessCapture, r Request) (accessRow, bool) {
	t.Helper()
	path, _, _ := strings.Cut(r.Path, "?")
	path = strings.ReplaceAll(path, "{{User/admin}}", "{userId}")
	path = idRef.ReplaceAllString(path, "{id}")
	var found []accessRow
	for _, row := range c.Rows {
		asks, _, _ := strings.Cut(row.Asks, "?")
		if row.Method == r.Method && asks == path {
			found = append(found, row)
		}
	}
	if len(found) > 1 {
		t.Fatalf("%q matches %d rows of the capture", r.Name, len(found))
	}
	if len(found) == 0 {
		return accessRow{}, false
	}
	return found[0], true
}

func noTokenRequests(t *testing.T) []Request {
	t.Helper()
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out []Request
	for _, r := range reqs {
		if r.NoToken {
			out = append(out, r)
		}
	}
	return out
}

func TestEveryNoTokenRequestHasARowInTheCapture(t *testing.T) {
	c := loadAccessCapture(t)
	for _, r := range noTokenRequests(t) {
		if _, ok := rowOf(t, c, r); !ok {
			t.Errorf("the capture has no row for %q (%s %s)", r.Name, r.Method, r.Path)
		}
	}
}

func entriesFor(t *testing.T, name string) []Entry {
	t.Helper()
	a, err := LoadAllowlist("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out []Entry
	for _, e := range a.Entries() {
		if slices.Contains(e.Requests, name) {
			out = append(out, e)
		}
	}
	return out
}

// Where Jellyfin answers 2xx with no token and the route needs one here (jellyscale answers 401), the request has
// an entry of its own of kind improvement. Where the capture shows Jellyfin refusing, nothing excuses a difference.
func TestEachOfTheTwelveJellyfinOpensAndJellyscaleRefusesHasAnImprovementEntryOfItsOwn(t *testing.T) {
	c := loadAccessCapture(t)
	routes := loadEndpoints(t)
	opened := 0
	for _, r := range noTokenRequests(t) {
		row, ok := rowOf(t, c, r)
		if !ok {
			continue // the test above says so
		}
		entries := entriesFor(t, r.Name)
		jellyscaleRefuses := endpointAccess(t, routes, row) == "viewer"
		switch {
		case is2xx(row.NoToken) && jellyscaleRefuses:
			opened++
			found := false
			for _, e := range entries {
				found = found || (e.Kind == "improvement" && e.Path == "(status)" && strings.TrimSpace(e.Reason) != "")
			}
			if !found {
				t.Errorf("%q: Jellyfin answers %d with no token and jellyscale 401, and compat/differences.yaml has no improvement entry for it with the path (status)", r.Name, row.NoToken)
			}
		case isRefused(row.NoToken) && len(entries) > 0:
			t.Errorf("%q: Jellyfin refuses it with no token, so no entry can be wanted: %v", r.Name, entries)
		}
	}
	if opened == 0 {
		t.Error("no request of the twelve is one Jellyfin opens and jellyscale refuses; the probe of 2026-11-16 found six")
	}
}

func TestEveryImprovementEntryOnANoTokenRequestIsBorneOutByTheCapture(t *testing.T) {
	c := loadAccessCapture(t)
	a, err := LoadAllowlist("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	noToken := map[string]Request{}
	for _, r := range noTokenRequests(t) {
		noToken[r.Name] = r
	}
	checked := 0
	for _, e := range a.Entries() {
		for _, name := range e.Requests {
			r, ok := noToken[name]
			if !ok || e.Kind != "improvement" {
				continue
			}
			checked++
			row, found := rowOf(t, c, r)
			if !found || !is2xx(row.NoToken) {
				t.Errorf("the improvement entry for %q says jellyscale is stricter, and the capture does not show Jellyfin answering 2xx with no token (row %v, %d)", name, found, row.NoToken)
			}
		}
	}
	if checked == 0 {
		t.Error("no improvement entry names a no-token request")
	}
}

// askMatches says whether what a row asked is its route filled in: segment by segment, a literal segment of
// the template as it is, a segment with a {name} in it as one segment, and in what was asked no placeholder
// but {id} (any item) and {userId} (the caller). A row that asks "/Items/{itemId}" asked for nothing real.
func askMatches(route, asks string) bool {
	path, _, _ := strings.Cut(asks, "?")
	rs, as := strings.Split(route, "/"), strings.Split(path, "/")
	if len(rs) != len(as) {
		return false
	}
	brace := regexp.MustCompile(`\{[^}]*\}`)
	for i := range rs {
		if brace.MatchString(as[i]) && as[i] != "{id}" && as[i] != "{userId}" {
			return false
		}
		if !strings.Contains(rs[i], "{") {
			if rs[i] != as[i] {
				return false
			}
			continue
		}
		// QuoteMeta escaped the braces; a {name} is then one run of non-slash characters.
		re := "^" + regexp.MustCompile(`\\\{[^}]*\\\}`).ReplaceAllString(regexp.QuoteMeta(rs[i]), "[^/]+") + "$"
		if !regexp.MustCompile(re).MatchString(as[i]) {
			return false
		}
	}
	return true
}

func TestAskMatchesTellsARealAskFromATemplateLeftUnfilled(t *testing.T) {
	for _, c := range []struct {
		route, asks string
		ok          bool
	}{
		{"/Items/{itemId}", "/Items/{id}", true},
		{"/Items/{itemId}", "/Items/{itemId}", false},
		{"/Users/{userId}/Items/{itemId}", "/Users/{userId}/Items/{id}", true},
		{"/Users/{userId}/Items/{itemId}", "/Users/{id}/Items/{itemId}", false},
		{"/Videos/{itemId}/{file}", "/Videos/{id}/stream?static=true", true},
		{"/Videos/{itemId}/{file}", "/Videos/{id}/stream.mkv", true},
		{"/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/Stream.{format}", "/Videos/{id}/{id}/Subtitles/2/Stream.vtt", true},
		{"/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/Stream.{format}", "/Videos/{id}/{id}/Subtitles/2/Stream.{format}", false},
		{"/Items/{itemId}/Images/Primary", "/Items/{id}/Images/Backdrop", false},
		{"/Items/{itemId}", "/Items", false},
	} {
		if got := askMatches(c.route, c.asks); got != c.ok {
			t.Errorf("askMatches(%q, %q) = %v", c.route, c.asks, got)
		}
	}
}

// A sweep that asked each route as its template, with no id filled in, would get a 404 or a 401 for every
// row and record what Jellyfin does with nothing, not with something it has.
func TestEveryRowAsksForARealThingOfItsRoute(t *testing.T) {
	for _, r := range loadAccessCapture(t).Rows {
		if !askMatches(r.Route, r.Asks) {
			t.Errorf("%s asks %q, which is not %s filled in (only {id} and {userId} may stand in it)", r.Key, r.Asks, r.Route)
		}
	}
}

// Where the non-administrator did not get 2xx, Jellyfin must have refused that user (401 or 403), and the row
// says why. No other status is a refusal: a 404 or a 400 means the row asked for what the server does not have,
// and those are no part of the list "refused to the non-administrator". The one way past this is a named
// exception here, with the reason, written when a real Jellyfin answer is seen that is neither.
var userTokenExceptions = map[string]string{
	// A real Jellyfin 12.4.0 upgrades a signed-in viewer's socket (101) and refuses one with no token or an unknown one (403).
	"GET /socket": "the WebSocket upgrade answers 101 Switching Protocols to a signed-in viewer, which is being served",
}

func TestANonAdministratorWhoWasNotServedWasRefusedAndNotMistaken(t *testing.T) {
	for _, r := range loadAccessCapture(t).Rows {
		if is2xx(r.UserToken) || isRefused(r.UserToken) {
			continue
		}
		if why, ok := userTokenExceptions[r.Key]; !ok || why == "" {
			t.Errorf("%s: the non-administrator got %d, which is neither 2xx nor a refusal (401, 403); the row asked for something the server lacks", r.Key, r.UserToken)
		}
	}
}

// A token the server never issued gets no more than no token: where no token is refused, so is an unknown one.
func TestAnUnknownTokenIsNeverServedWhereNoTokenIsRefused(t *testing.T) {
	for _, r := range loadAccessCapture(t).Rows {
		if isRefused(r.NoToken) && !isRefused(r.UnknownToken) {
			t.Errorf("%s: no token %d, a token never issued %d", r.Key, r.NoToken, r.UnknownToken)
		}
	}
}

// The probe of 2026-11-16 saw these accept a real token and refuse an invented one. A capture whose
// non-administrator never authenticated (the token in a spelling Jellyfin ignores for that route) shows 401 for
// all of them and fails here, whatever else it says. The count says the rest of the sweep was served too.
func TestTheNonAdministratorWasServedWhereJellyfinAcceptsAToken(t *testing.T) {
	c := loadAccessCapture(t)
	for _, key := range []string{"GET /Users/Me", "GET /Items", "GET /Items/{id}", "GET /Videos/{id}/master.m3u8"} {
		r, ok := c.row(key)
		if !ok {
			// the asks may carry a query; find by the path alone
			for _, row := range c.Rows {
				if p, _, _ := strings.Cut(row.Key, "?"); p == key {
					r, ok = row, true
				}
			}
		}
		if !ok {
			t.Errorf("the capture has no row %q", key)
			continue
		}
		if !is2xx(r.UserToken) {
			t.Errorf("%s: the non-administrator's token got %d", r.Key, r.UserToken)
		}
		if r.UnknownToken != 401 {
			t.Errorf("%s: a token the server never issued got %d, want 401", r.Key, r.UnknownToken)
		}
	}
	served := 0
	for _, r := range c.Rows {
		if isRefused(r.NoToken) && is2xx(r.UserToken) {
			served++
		}
	}
	if served < 30 {
		t.Errorf("only %d rows are refused with no token and served to the non-administrator; 88 routes of which most need a signed-in user", served)
	}
}

// The probe measured the static stream at 3055606 bytes and the font at 2543520, both sent as they are, so a
// capture that recorded either as other than the film's or the font's bytes, for a credential that got 2xx,
// measured nothing.
func TestTheStaticStreamAndTheFontAreRecordedAsTheFilesBytesForEveryCredentialThatWasServed(t *testing.T) {
	c := loadAccessCapture(t)
	for _, label := range []string{"static stream", "font", "download"} {
		var row accessRow
		for _, r := range c.Rows {
			if r.Media == label {
				row = r
			}
		}
		for name, status := range map[string]int{"no_token": row.NoToken, "unknown_token": row.UnknownToken, "user_token": row.UserToken} {
			if is2xx(status) && row.Bodies[name] != "same-bytes" {
				t.Errorf("%s answered %d to %s and its body is recorded as %q, want same-bytes", label, status, name, row.Bodies[name])
			}
		}
	}
	// The font and the static stream are open with no token on Jellyfin 12.4 and the non-administrator's token works.
	for _, label := range []string{"static stream", "font"} {
		for _, r := range c.Rows {
			if r.Media == label && (r.NoToken != 200 || r.UserToken != 200) {
				t.Errorf("%s: no token %d, non-administrator %d; both were 200 on 2026-11-16", label, r.NoToken, r.UserToken)
			}
		}
	}
}
