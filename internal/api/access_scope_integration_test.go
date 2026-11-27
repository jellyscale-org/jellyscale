//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
)

// JS-127. A user id in a request names the caller, or the request is refused. TestAccess asks every viewer route as a
// non-administrator A and as an administrator, with another user B's id (and nobody's, a malformed one and the
// all-zero one) in every place the route can take one, on each of the three handlers (the api alone, the stream alone,
// both), and holds the answers to compat/captures/user-scope-12.4.md where the criteria keep Jellyfin's behaviour and
// to the criteria where they improve on it. A viewer route that declares Users 'admin-any' lets an administrator who
// names a user act on that user; every other viewer route is 'own'. The routes of Access 'admin' (JS-033) refuse a
// viewer whoever they name. The lists below are which routes the access model puts where, and a route declared
// otherwise fails.

// scopeAdminAny are the admin-any routes whose success for an administrator the sweep judges.
var scopeAdminAny = []string{"GET /Users/{userId}", "POST /Users/Configuration", "POST /Users/{userId}/Configuration", "POST /UserItems/{itemId}/UserData"}

// scopeUserAccounts are JS-033's admin-any viewer routes. The sweep asks them only what changes nothing; what an
// administrator naming another user does there is cmd/jellyscale's users checks', on users of their own.
var scopeUserAccounts = []string{"GET /Users", "POST /Users", "POST /Users/Password", "POST /Users/{userId}/Password"}

// scopeAdministrators are JS-033's routes of Access 'admin'. scopeAdministratorsToo are the others that may be there,
// if a page needs them: also for administrators only.
var scopeAdministrators = []string{"POST /Users/New", "DELETE /Users/{userId}", "POST /Users/{userId}/Policy", "GET /Auth/Providers", "GET /Auth/PasswordResetProviders"}

var scopeAdministratorsToo = []string{"GET /Library/MediaFolders", "GET /Devices"}

// scopeAccessAdmin is Route.Access of a route for administrators.
const scopeAccessAdmin Access = "admin"

const (
	scopeNobody    = "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
	scopeMalformed = "not-a-guid"
	scopeZero      = "00000000-0000-0000-0000-000000000000"
)

type scopeUser struct{ id, token string }

// scopeFixture is the three users of the sweep. A is the site's viewer and B its other user; each holds a row in every
// table a user owns, so that a request that touched either changes something that can be read.
type scopeFixture struct {
	a, b, admin scopeUser
	film        accessFilm
}

func (a *accessSite) scopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	if a.scoped == nil {
		admin := uuid.NewString()
		mustExec(t, a.pool, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, 'scope-admin', 'x', true)`, admin)
		a.scoped = &scopeFixture{a: scopeUser{a.user, a.token}, b: scopeUser{a.other, a.otherToken}, admin: scopeUser{admin, a.device(t, admin, "scope-admin-device")}, film: a.films[0]}
		mustExec(t, a.pool, `UPDATE users SET configuration = '{"SubtitleMode":"Always","SubtitleLanguagePreference":"eng"}' WHERE id = $1`, a.user)
		mustExec(t, a.pool, `UPDATE users SET configuration = '{"SubtitleMode":"None","SubtitleLanguagePreference":"spa"}' WHERE id = $1`, a.other)
	}
	f := a.scoped
	// Again for each handler: the heartbeat of an api replica that held a socket may have swept the socket rows.
	for n, user := range []scopeUser{f.a, f.b} {
		mustExec(t, a.pool, `INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks) VALUES ($1, $2, true, true, $3, $4)
			ON CONFLICT (user_id, item_id) DO NOTHING`, user.id, f.film.item, n+2, int64(n+2)*1000)
		mustExec(t, a.pool, `INSERT INTO display_preferences (user_id, preference_id, client, data) VALUES ($1, 'home', 'c', $2) ON CONFLICT DO NOTHING`,
			user.id, fmt.Sprintf(`{"Id":"home","SortBy":"seed%d"}`, n))
		mustExec(t, a.pool, `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version, item_id, position_ticks, play_method, play_session_id)
			VALUES ($1, $2, 'c', 'seeded', 'dev', '1', $3, 4200, 'DirectPlay', $4) ON CONFLICT (token) DO NOTHING`, user.token, user.id, f.film.item, "seeded-"+user.id)
		mustExec(t, a.pool, `INSERT INTO session_sockets (connection_id, token, replica) VALUES ($1, $2, 'ghost') ON CONFLICT (connection_id) DO NOTHING`, "seeded-"+user.id, user.token)
	}
	return f
}

// scopeRows is what a user holds, one text per table, so that "unchanged" is byte for byte.
func (a *accessSite) scopeRows(t *testing.T, user string) map[string]string {
	t.Helper()
	queries := map[string]string{
		"users":               `SELECT x::text FROM users x WHERE x.id = $1::uuid`,
		"user_item_data":      `SELECT x::text FROM user_item_data x WHERE x.user_id = $1::uuid`,
		"display_preferences": `SELECT x::text FROM display_preferences x WHERE x.user_id = $1::uuid`,
		"devices":             `SELECT x::text FROM devices x WHERE x.user_id = $1::uuid`,
		"sessions":            `SELECT x::text FROM sessions x WHERE x.user_id = $1::uuid`,
		"session_sockets":     `SELECT x::text FROM session_sockets x WHERE x.token IN (SELECT token FROM sessions WHERE user_id = $1::uuid)`,
	}
	out := map[string]string{}
	for table, query := range queries {
		rows, err := a.pool.Query(context.Background(), query, user)
		if err != nil {
			t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(texts)
		out[table] = strings.Join(texts, "\n")
	}
	return out
}

func (a *accessSite) scopeAllRows(t *testing.T, f *scopeFixture) map[string]map[string]string {
	t.Helper()
	return map[string]map[string]string{"A": a.scopeRows(t, f.a.id), "B": a.scopeRows(t, f.b.id), "admin": a.scopeRows(t, f.admin.id)}
}

// scopeChanged names the rows that differ: "B's user_item_data".
func scopeChanged(before, after map[string]map[string]string) []string {
	var changed []string
	for who, tables := range before {
		for table, text := range tables {
			if after[who][table] != text {
				changed = append(changed, who+"'s "+table)
			}
		}
	}
	sort.Strings(changed)
	return changed
}

// scopeRequest is what a request names: a user in the path, query pairs, a user in the body.
type scopeRequest struct {
	pathUser string
	query    []string
	bodyUser string
}

func scopeAppend(path, pair string) string {
	if strings.Contains(path, "?") {
		return path + "&" + pair
	}
	return path + "?" + pair
}

func scopeIsPlaybackInfo(route Route) bool {
	return route.Method == "POST" && (route.Path == "/Items/{itemId}/PlaybackInfo" || route.Path == "/jellyscale/v1/items/{itemId}/playback-info")
}

func scopeHasPath(route Route) bool { return strings.Contains(route.Path, "{userId}") }

// scopeAsk is one request of the sweep and the status it must be answered with.
type scopeAsk struct {
	route        Route
	who, what    string
	group        string // Requests of one group are answered with one status and one body.
	method, path string
	body, token  string
	want         int
}

func (k scopeAsk) name() string {
	return fmt.Sprintf("%s as %s, %s", routeName(k.route), k.who, k.what)
}

func (a *accessSite) scopeBuild(route Route, film accessFilm, q scopeRequest, body string) (path, requestBody string, err error) {
	path, err = a.path(route, film, q.pathUser)
	if err != nil {
		return "", "", err
	}
	for _, pair := range q.query {
		path = scopeAppend(path, pair)
	}
	requestBody = body
	if requestBody == "" {
		requestBody = accRequestBody(route)
	}
	if q.bodyUser != "" {
		requestBody = fmt.Sprintf(`{"UserId":%q}`, q.bodyUser)
	}
	return path, requestBody, nil
}

type scopePlace struct {
	name string
	req  scopeRequest
}

// scopePlaces is every place the route can take a user's id, with id in it and the caller's own where the place leaves
// a name to fill.
func scopePlaces(route Route, self, id string) []scopePlace {
	places := []scopePlace{
		{"query userId", scopeRequest{pathUser: self, query: []string{"userId=" + url.QueryEscape(id)}}},
		{"query controllableByUserId", scopeRequest{pathUser: self, query: []string{"controllableByUserId=" + url.QueryEscape(id)}}},
	}
	if scopeHasPath(route) {
		places = append(places, scopePlace{"path", scopeRequest{pathUser: id}})
	}
	if scopeIsPlaybackInfo(route) {
		places = append(places, scopePlace{"body UserId", scopeRequest{pathUser: self, bodyUser: id}})
	}
	return places
}

// scopeMixed is the requests that name two users, or one twice, where the caller's own id comes with another's: every
// value counts, whichever comes first.
func scopeMixed(route Route, self, other string) []scopePlace {
	places := []scopePlace{
		{"userId twice, own then other", scopeRequest{pathUser: self, query: []string{"userId=" + self, "userId=" + other}}},
		{"userId twice, other then own", scopeRequest{pathUser: self, query: []string{"userId=" + other, "userId=" + self}}},
		{"userId and UserId, own and other", scopeRequest{pathUser: self, query: []string{"userId=" + self, "UserId=" + other}}},
		{"controllableByUserId twice, own then other", scopeRequest{pathUser: self, query: []string{"controllableByUserId=" + self, "controllableByUserId=" + other}}},
		{"userId own, controllableByUserId other", scopeRequest{pathUser: self, query: []string{"userId=" + self, "controllableByUserId=" + other}}},
	}
	if scopeHasPath(route) {
		places = append(places,
			scopePlace{"path own, query other", scopeRequest{pathUser: self, query: []string{"userId=" + other}}},
			scopePlace{"path other, query own", scopeRequest{pathUser: other, query: []string{"userId=" + self}}})
	}
	if scopeIsPlaybackInfo(route) {
		places = append(places,
			scopePlace{"query own, body other", scopeRequest{pathUser: self, query: []string{"userId=" + self}, bodyUser: other}},
			scopePlace{"query other, body own", scopeRequest{pathUser: self, query: []string{"userId=" + other}, bodyUser: self}})
	}
	return places
}

// scopeRoutes are the viewer routes of the handler, by the user scope each declares. The routes of Access 'admin' are
// in neither: no viewer may call one, whoever it names.
func scopeRoutes(h accHandler) (own, adminAny []Route) {
	for _, route := range (&Server{}).Routes() {
		if accOpen(route) || !h.serves(route) || route.Access == scopeAccessAdmin {
			continue
		}
		if route.Users == UsersAdminAny {
			adminAny = append(adminAny, route)
		} else {
			own = append(own, route)
		}
	}
	return own, adminAny
}

// scopeDeclarations holds what the routes of a handler with the api role declare to the access model's lists: a route
// that is not declared as listed would be swept as something it is not.
func (a *accessSite) scopeDeclarations(h accHandler) {
	if !slices.Contains(h.roles, RoleAPI) {
		return
	}
	declared := map[string]string{}
	for _, route := range (&Server{}).Routes() {
		switch {
		case route.Access == scopeAccessAdmin:
			declared[routeName(route)] = "admin"
		case route.Access == AccessViewer && route.Users == UsersAdminAny:
			declared[routeName(route)] = "admin-any"
		case !accOpen(route):
			declared[routeName(route)] = "own"
		}
	}
	for _, name := range append(slices.Clone(scopeAdminAny), scopeUserAccounts...) {
		if declared[name] != "admin-any" {
			a.fail("%s: %s is declared %q, want a viewer route of scope admin-any", h.name, name, declared[name])
		}
	}
	for _, name := range scopeAdministrators {
		if declared[name] != "admin" {
			a.fail("%s: %s is declared %q, want Access admin", h.name, name, declared[name])
		}
	}
	for name, scope := range declared {
		switch {
		case slices.Contains(scopeAdministratorsToo, name) && scope != "admin":
			a.fail("%s: %s is declared %q, want Access admin", h.name, name, scope)
		case scope == "admin-any" && !slices.Contains(scopeAdminAny, name) && !slices.Contains(scopeUserAccounts, name):
			a.fail("%s: %s is declared admin-any, which the access model does not give it", h.name, name)
		case scope == "admin" && !slices.Contains(scopeAdministrators, name) && !slices.Contains(scopeAdministratorsToo, name):
			a.fail("%s: %s is declared Access admin, which the access model does not give it", h.name, name)
		}
	}
}

// scopeWho is a caller of the sweep.
type scopeWho struct {
	name string
	user scopeUser
}

func (f *scopeFixture) callers() []scopeWho {
	return []scopeWho{{"A", f.a}, {"admin", f.admin}}
}

// scopeToken is the caller's token, or a new device's where the route signs the caller out, so that a refusal that was
// not one leaves the later requests of the sweep signed in.
func (a *accessSite) scopeToken(t *testing.T, route Route, who scopeWho, n *int) string {
	t.Helper()
	if route.Path != "/Sessions/Logout" {
		return who.user.token
	}
	*n++
	return a.device(t, who.user.id, fmt.Sprintf("scope-logout-%s-%d", who.name, *n))
}

// scopeShapes are the routes that stand for every route of their shape (a list, a read of one item, a write, a route that
// signs the caller out, one that stops a job, the socket, a path alias, the two PlaybackInfo POSTs that take a body, the
// streams, the snapshot routes). The ids that need only be tried once a place are tried on these; every route is asked
// with B in each place it has, and with nobody's, a malformed and the zero id in the query.
var scopeShapes = []string{
	"GET /Items", "GET /Items/{itemId}", "GET /Sessions", "GET /DisplayPreferences/{preferenceId}", "POST /DisplayPreferences/{preferenceId}",
	"POST /UserPlayedItems/{itemId}", "POST /Sessions/Logout", "DELETE /Videos/ActiveEncodings", "GET /socket",
	"GET /Users/{userId}/Items", "POST /Users/{userId}/PlayedItems/{itemId}",
	"POST /Items/{itemId}/PlaybackInfo", "POST /jellyscale/v1/items/{itemId}/playback-info", "PATCH /jellyscale/v1/users/configuration",
	"GET /Videos/{itemId}/{file}", "GET /Items/{itemId}/Download", "GET /System/Configuration/encoding",
	"GET /Videos/{itemId}/hls1/main/{segmentId}", "GET /Videos/{itemId}/{sourceId}/Subtitles/{index}/{file}",
}

// scopeRefusals are the requests an 'own' route must refuse with 403. Every route is asked with B's id in each place it
// has (the query's userId and controllableByUserId, the path, the body) and, in the query's userId, with nobody's, a
// malformed one and the zero one; the answers for one place are one status and one body. The shape routes are also asked
// with B's id dashed in upper case, with the other ids in every place, and with the mixed and repeated values.
func (a *accessSite) scopeRefusals(t *testing.T, h accHandler, f *scopeFixture) []scopeAsk {
	t.Helper()
	var asks []scopeAsk
	logouts := 0
	own, _ := scopeRoutes(h)
	ids := []struct{ name, value string }{{"B", db.CompactID(f.b.id)}, {"B dashed upper", strings.ToUpper(f.b.id)}, {"nobody", scopeNobody}, {"malformed", scopeMalformed}, {"zero", scopeZero}}
	for _, route := range own {
		shape := slices.Contains(scopeShapes, routeName(route))
		for _, who := range f.callers() {
			self := db.CompactID(who.user.id)
			add := func(place scopePlace, what string) {
				path, body, err := a.scopeBuild(route, f.film, place.req, "")
				if err != nil {
					a.fail("%v", err)
					return
				}
				group := ""
				if place.name == "query userId" || place.name == "path" {
					group = routeName(route) + "|" + who.name + "|" + place.name
				}
				asks = append(asks, scopeAsk{route: route, who: who.name, what: what, group: group, method: route.Method, path: path, body: body,
					token: a.scopeToken(t, route, who, &logouts), want: 403})
			}
			for n, id := range ids {
				for _, place := range scopePlaces(route, self, id.value) {
					everyRoute := n == 0 || n >= 2 && (place.name == "query userId" || place.name == "path")
					if everyRoute || shape {
						add(place, place.name+": "+id.name)
					}
				}
			}
			if shape {
				for _, place := range scopeMixed(route, self, db.CompactID(f.b.id)) {
					add(place, place.name)
				}
			}
		}
	}
	return asks
}

var scopeHandshake = []string{"Connection", "Upgrade", "Upgrade", "websocket", "Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}

func (a *accessSite) scopeSend(h accHandler, k scopeAsk) accReply {
	var headers []string
	if k.route.Path == "/socket" {
		headers = scopeHandshake
	}
	return accSend(k.method, h.base, k.path, k.token, 3, k.body, headers...)
}

// scopeSendAll sends the asks on a bounded pool of workers, and returns the answers in the order of the asks. The asks of
// one sweep do not depend on each other: each refusal changes nothing, a route that signs out has a device of its own.
func (a *accessSite) scopeSendAll(h accHandler, asks []scopeAsk) []accReply {
	replies := make([]accReply, len(asks))
	work := make(chan int)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				replies[i] = a.scopeSend(h, asks[i])
			}
		}()
	}
	for i := range asks {
		work <- i
	}
	close(work)
	wg.Wait()
	return replies
}

// scopeJudge holds each answer to the status its ask wants, and the answers of one group to one status and one body,
// and says one thing for each route and caller, not one for each request.
func (a *accessSite) scopeJudge(h accHandler, asks []scopeAsk, replies []accReply) {
	bodies := map[string]string{}
	wrong := map[string][]string{}
	var order []string
	for i, k := range asks {
		r := replies[i]
		key := routeName(k.route) + " as " + k.who
		var problem string
		switch {
		case r.err != nil:
			problem = fmt.Sprintf("%s: %v", k.what, r.err)
		case r.status != k.want:
			problem = fmt.Sprintf("%s: %d %.40q, want %d", k.what, r.status, r.body, k.want)
		case k.group != "":
			shape := fmt.Sprintf("%d\n%s", r.status, r.body)
			if before, ok := bodies[k.group]; ok && before != shape {
				problem = fmt.Sprintf("%s: the answer %q differs from another id's %q in the same place", k.what, r.body, before)
			}
			bodies[k.group] = shape
		}
		if problem == "" {
			continue
		}
		if _, ok := wrong[key]; !ok {
			order = append(order, key)
		}
		wrong[key] = append(wrong[key], problem)
	}
	for _, key := range order {
		problems := wrong[key]
		shown := problems[:min(len(problems), 3)]
		a.fail("%s: %s: %d wrong; %s", h.name, key, len(problems), strings.Join(shown, "; "))
	}
}

// scopeVolatile is what differs between two answers to one request: ids that are made, and times.
var scopeVolatile = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}|\d{4}-\d\d-\d\dT[0-9:.]+(?:Z|[+-]\d\d:\d\d)?`)

func scopeShape(r accReply) string {
	return fmt.Sprintf("%d\n%s", r.status, scopeVolatile.ReplaceAll(r.body, []byte("#")))
}

// scope asks every viewer route what the criteria of JS-127 name, on one handler. Each phase says what it holds.
func (a *accessSite) scope(t *testing.T, h accHandler) {
	t.Helper()
	f := a.scopeFixture(t)

	// Criteria 7 to 10, 13, 21, 22 and 28: a request naming anyone but the caller is refused with 403 before the
	// handler, in every place, for a non-administrator and an administrator alike; with the same answer whether the id
	// is another user's, nobody's or malformed; and it does nothing: no row of A's, B's or the administrator's changes.
	refusals := a.scopeRefusals(t, h, f)
	before := a.scopeAllRows(t, f)
	replies := a.scopeSendAll(h, refusals)
	a.scopeJudge(h, refusals, replies)
	for i, k := range refusals {
		// After POST /Sessions/Logout naming B the caller's token still works.
		if k.route.Path == "/Sessions/Logout" && !a.accepted(h, k.token, 3) && replies[i].status != 401 {
			a.fail("%s: %s signed the caller's own token out", h.name, k.name())
		}
	}
	if changed := scopeChanged(before, a.scopeAllRows(t, f)); len(changed) > 0 {
		a.fail("%s: requests that named another user changed rows: %s", h.name, strings.Join(changed, ", "))
	}

	a.scopeDeclarations(h)

	// Criteria 15 to 20: the admin-any routes.
	a.scopeAdminAny(t, h, f)

	// Criterion 11: the caller's own id, in any of its spellings and any place, answers as the request without it.
	a.scopeOwnID(t, h, f)

	// Criterion 12: authentication comes first.
	a.scopeAuthenticationFirst(t, h, f)

	// Criteria 23 and 24: an open route answers as it does without the id, and signing in is the name and password's.
	a.scopeOpenRoutes(t, h, f)

	// The WebSocket: the caller's own id in the query upgrades, another's is 403 with no upgraded connection.
	a.scopeSocket(t, h, f)
}

// scopeAdminAnyAsks are the requests the admin-any routes answer by who asks.
type scopeAdminAnyAsk struct {
	scopeAsk
	success bool // The administrator acts on B.
}

func (a *accessSite) scopeAdminAnyAsks(t *testing.T, h accHandler, f *scopeFixture) []scopeAdminAnyAsk {
	t.Helper()
	_, adminAny := scopeRoutes(h)
	bodies := map[string]string{
		"POST /Users/Configuration":          `{"SubtitleMode":"Smart","SubtitleLanguagePreference":"deu"}`,
		"POST /Users/{userId}/Configuration": `{"SubtitleMode":"Smart","SubtitleLanguagePreference":"deu"}`,
		"POST /UserItems/{itemId}/UserData":  `{"IsFavorite":false,"Played":false,"PlaybackPositionTicks":777}`,
		"GET /Users/{userId}":                "",
		// Never accepted by the sweep, and each a change that would show in the rows if a wrong build made it.
		"POST /Users":                   `{"Name":"scope-sweep-rename"}`,
		"POST /Users/Password":          `{"CurrentPw":"scope-wrong","NewPw":"Qx7!scope-sweep-password"}`,
		"POST /Users/{userId}/Password": `{"CurrentPw":"scope-wrong","NewPw":"Qx7!scope-sweep-password"}`,
	}
	var out []scopeAdminAnyAsk
	bCompact := db.CompactID(f.b.id)
	for _, route := range adminAny {
		for _, who := range f.callers() {
			self := db.CompactID(who.user.id)
			add := func(what, group string, req scopeRequest, want int, success bool) {
				if success && !slices.Contains(scopeAdminAny, routeName(route)) {
					return
				}
				path, body, err := a.scopeBuild(route, f.film, req, bodies[routeName(route)])
				if err != nil {
					a.fail("%v", err)
					return
				}
				out = append(out, scopeAdminAnyAsk{scopeAsk{route: route, who: who.name, what: what, group: group, method: route.Method, path: path, body: body, token: who.user.token, want: want}, success})
			}
			named := func(id string) scopeRequest {
				if scopeHasPath(route) {
					return scopeRequest{pathUser: id}
				}
				return scopeRequest{pathUser: self, query: []string{"userId=" + url.QueryEscape(id)}}
			}
			place := map[bool]string{true: "path", false: "query"}[scopeHasPath(route)]
			ok := 204
			switch route.Method + " " + route.Path {
			case "GET /Users/{userId}", "POST /UserItems/{itemId}/UserData":
				ok = 200
			}
			if who.name == "A" {
				for _, id := range []struct{ name, value string }{{"B", bCompact}, {"B dashed upper", strings.ToUpper(f.b.id)}, {"nobody", scopeNobody}, {"zero", scopeZero}} {
					add(place+": "+id.name, routeName(route)+"|A|"+place, named(id.value), 403, false)
				}
				add(place+": malformed", "", named(scopeMalformed), 400, false)
				if scopeHasPath(route) {
					add("path own, query other", "", scopeRequest{pathUser: self, query: []string{"userId=" + bCompact}}, 403, false)
					add("path other, query own", "", scopeRequest{pathUser: bCompact, query: []string{"userId=" + self}}, 403, false)
					add("path own, query nobody", "", scopeRequest{pathUser: self, query: []string{"userId=" + scopeNobody}}, 403, false)
				}
				if slices.Contains(scopeUserAccounts, routeName(route)) {
					// Every value counts: the caller's own id beside another's, whichever comes first.
					for _, mixed := range scopeMixed(route, self, bCompact) {
						add(mixed.name, "", mixed.req, 403, false)
					}
				}
				continue
			}
			add(place+": B", "", named(bCompact), ok, true)
			add(place+": nobody", "", named(scopeNobody), 404, false)
			add(place+": zero", "", named(scopeZero), 404, false)
			add(place+": malformed", "", named(scopeMalformed), 400, false)
			if scopeHasPath(route) {
				add("path own, query other", "", scopeRequest{pathUser: self, query: []string{"userId=" + bCompact}}, 400, false)
				add("path other, query own", "", scopeRequest{pathUser: bCompact, query: []string{"userId=" + self}}, 400, false)
				add("path B dashed upper, query B compact", "", scopeRequest{pathUser: strings.ToUpper(f.b.id), query: []string{"userId=" + bCompact}}, ok, true)
			}
		}
	}
	return out
}

func (a *accessSite) scopeAdminAny(t *testing.T, h accHandler, f *scopeFixture) {
	t.Helper()
	asks := a.scopeAdminAnyAsks(t, h, f)
	var refused []scopeAsk
	for _, k := range asks {
		if !k.success {
			refused = append(refused, k.scopeAsk)
		}
	}
	// What is refused, not found or malformed changes nothing, and a non-administrator's 403 is one answer for B, nobody and zero.
	before := a.scopeAllRows(t, f)
	replies := a.scopeSendAll(h, refused)
	a.scopeJudge(h, refused, replies)
	if changed := scopeChanged(before, a.scopeAllRows(t, f)); len(changed) > 0 {
		a.fail("%s: admin-any requests that were refused, not found or malformed changed rows: %s", h.name, strings.Join(changed, ", "))
	}
	// A malformed id is 400 whoever asks, also GET /Users/Configuration, which the {userId} pattern matches.
	if slices.ContainsFunc(refused, func(k scopeAsk) bool { return k.route.Path == "/Users/{userId}" }) {
		for _, who := range f.callers() {
			if r := accSend("GET", h.base, "/Users/Configuration", who.user.token, 3, ""); r.status != 400 {
				a.fail("%s: GET /Users/Configuration as %s answered %d %.40q, want 400 (the {userId} pattern matches it)", h.name, who.name, r.status, r.body)
			}
		}
	}
	// The administrator acts on B and on B only.
	for _, k := range asks {
		if !k.success {
			continue
		}
		// B holds what it was seeded with, so that a write that lands is a change whatever an earlier request did.
		mustExec(t, a.pool, `UPDATE users SET configuration = '{"SubtitleMode":"None","SubtitleLanguagePreference":"spa"}' WHERE id = $1`, f.b.id)
		mustExec(t, a.pool, `UPDATE user_item_data SET favorite = true, played = true, position_ticks = 3000, play_count = 3 WHERE user_id = $1`, f.b.id)
		before := a.scopeAllRows(t, f)
		r := a.scopeSend(h, k.scopeAsk)
		if r.err != nil || r.status != k.want {
			a.fail("%s: %s: %d %.60q, want %d", h.name, k.name(), r.status, r.body, k.want)
			continue
		}
		changed := scopeChanged(before, a.scopeAllRows(t, f))
		switch routeName(k.route) {
		case "GET /Users/{userId}":
			var record struct{ Id, Name string }
			if json.Unmarshal(r.body, &record) != nil || !strings.EqualFold(record.Id, db.CompactID(f.b.id)) {
				a.fail("%s: %s returned %.80q, want B's record", h.name, k.name(), r.body)
			}
			if len(changed) > 0 {
				a.fail("%s: %s changed rows: %s", h.name, k.name(), strings.Join(changed, ", "))
			}
		case "POST /Users/Configuration", "POST /Users/{userId}/Configuration":
			if want := []string{"B's users"}; !slices.Equal(changed, want) {
				a.fail("%s: %s changed %v, want %v", h.name, k.name(), changed, want)
			}
			if language := scalar[string](t, a.pool, `SELECT coalesce(configuration->>'SubtitleLanguagePreference', '') FROM users WHERE id = $1`, f.b.id); language != "deu" {
				a.fail("%s: %s left B's SubtitleLanguagePreference %q, want the body's deu", h.name, k.name(), language)
			}
		case "POST /UserItems/{itemId}/UserData":
			if want := []string{"B's user_item_data"}; !slices.Equal(changed, want) {
				a.fail("%s: %s changed %v, want %v", h.name, k.name(), changed, want)
			}
		}
	}
}

// scopeOwnID: the caller's own id in each of its four spellings, in every place, and an empty userId=, answers as the
// request that names nobody does: the same status, and the same body where the route's body is the same twice.
func (a *accessSite) scopeOwnID(t *testing.T, h accHandler, f *scopeFixture) {
	t.Helper()
	own, adminAny := scopeRoutes(h)
	logouts := 100000
	for _, route := range append(own, adminAny...) {
		if route.Path == "/socket" {
			continue
		}
		for _, who := range f.callers() {
			compact := db.CompactID(who.user.id)
			own := ""
			if routeName(route) == "POST /Users" {
				// The profile page's save with nothing changed: the caller's own record, with their own name.
				own = string(accSend("GET", h.base, "/Users/"+compact, who.user.token, 3, "").body)
			}
			send := func(req scopeRequest) (accReply, error) {
				path, body, err := a.scopeBuild(route, f.film, req, own)
				if err != nil {
					return accReply{}, err
				}
				return accSend(route.Method, h.base, path, a.scopeToken(t, route, who, &logouts), 3, body), nil
			}
			baseline, err := send(scopeRequest{pathUser: compact})
			if err != nil {
				a.fail("%v", err)
				continue
			}
			again, _ := send(scopeRequest{pathUser: compact})
			// Two refusals alike would prove nothing about the caller's own id on these two.
			if want := map[string]int{"GET /Users": 200, "POST /Users": 204}[routeName(route)]; want != 0 && again.status != want {
				a.fail("%s: %s as %s with no id answered %d %.40q, want %d", h.name, routeName(route), who.name, again.status, again.body, want)
			}
			// Suggestions come in a random order, so two answers to one request may differ: its body is not compared.
			stable := route.Method == "GET" && route.Path != "/Items/Suggestions" && scopeShape(baseline) == scopeShape(again)
			spellings := []string{compact, who.user.id, strings.ToUpper(compact), strings.ToUpper(who.user.id)}
			var wrong []string
			judge := func(what string, r accReply) {
				switch {
				case r.err != nil:
					wrong = append(wrong, fmt.Sprintf("%s: %v", what, r.err))
				case r.status != again.status:
					wrong = append(wrong, fmt.Sprintf("%s: %d %.40q, want the %d the request without it answers", what, r.status, r.body, again.status))
				case stable && scopeShape(r) != scopeShape(again):
					wrong = append(wrong, fmt.Sprintf("%s: the body differs from the request without it", what))
				}
			}
			for _, spelling := range spellings {
				for _, place := range scopePlaces(route, compact, spelling) {
					r, _ := send(place.req)
					judge(place.name+" "+spelling, r)
				}
			}
			r, _ := send(scopeRequest{pathUser: compact, query: []string{"userId="}})
			judge("query userId empty", r)
			if len(wrong) > 0 {
				a.fail("%s: %s as %s, own id: %d wrong; %s", h.name, routeName(route), who.name, len(wrong), strings.Join(wrong[:min(len(wrong), 3)], "; "))
			}
		}
	}
}

// scopeCredential is a token that is no token of anybody's, in a spelling, or none.
type scopeCredential struct {
	name, token string
	spelling    int
}

func (a *accessSite) scopeAuthenticationFirst(t *testing.T, h accHandler, f *scopeFixture) {
	t.Helper()
	own, adminAny := scopeRoutes(h)
	routes := slices.DeleteFunc(append(own, adminAny...), func(route Route) bool { return slices.Contains(scopeUserAccounts, routeName(route)) })
	// JS-033's routes are asked by name, those for administrators too, whatever Routes() declares of them.
	users := append(slices.Clone(scopeUserAccounts), scopeAdministrators...)
	if slices.Contains(h.roles, RoleAPI) {
		for _, name := range users {
			method, path, _ := strings.Cut(name, " ")
			routes = append(routes, Route{Method: method, Path: path, Role: RoleAPI})
		}
	}
	other := db.CompactID(f.b.id)
	for _, route := range routes {
		var wrong []string
		var headers []string
		if route.Path == "/socket" {
			headers = scopeHandshake
		}
		credentials := []scopeCredential{{"no token", "", -1}, {"unknown token", "unknown-" + uuid.NewString(), 3}, {"unknown token in ApiKey", "unknown-" + uuid.NewString(), 5}}
		if slices.Contains(users, routeName(route)) {
			credentials = []scopeCredential{{"no token", "", -1}}
			for spelling, name := range accSpellings {
				credentials = append(credentials, scopeCredential{"unknown token in " + name, "unknown-" + uuid.NewString(), spelling})
			}
		}
		for _, place := range append(scopePlaces(route, other, other), scopeMixed(route, other, other)...) {
			path, body, err := a.scopeBuild(route, f.film, place.req, "")
			if err != nil {
				a.fail("%v", err)
				continue
			}
			for _, credential := range credentials {
				r := accSend(route.Method, h.base, path, credential.token, credential.spelling, body, headers...)
				if r.err != nil || r.status != 401 || strings.TrimSpace(string(r.body)) != "unauthorized" {
					wrong = append(wrong, fmt.Sprintf("%s with %s: %d %.40q", place.name, credential.name, r.status, r.body))
				}
			}
		}
		if len(wrong) > 0 {
			a.fail("%s: %s naming B with no valid token: %d answers are not the 401 unauthorized; %s", h.name, routeName(route), len(wrong), strings.Join(wrong[:min(len(wrong), 3)], "; "))
		}
	}
}

func (a *accessSite) scopeOpenRoutes(t *testing.T, h accHandler, f *scopeFixture) {
	t.Helper()
	other := db.CompactID(f.b.id)
	for _, route := range (&Server{}).Routes() {
		if !accOpen(route) || !h.serves(route) {
			continue
		}
		path, err := a.path(route, f.film, db.CompactID(f.a.id))
		if err != nil {
			a.fail("%v", err)
			continue
		}
		body := accRequestBody(route)
		for _, credential := range []struct {
			name, token string
			spelling    int
		}{{"no token", "", -1}, {"A's token", f.a.token, 3}} {
			plain := accSend(route.Method, h.base, path, credential.token, credential.spelling, body)
			again := accSend(route.Method, h.base, path, credential.token, credential.spelling, body)
			for _, key := range []string{"userId", "controllableByUserId"} {
				named := accSend(route.Method, h.base, scopeAppend(path, key+"="+other), credential.token, credential.spelling, body)
				switch {
				case named.err != nil || plain.err != nil:
					a.fail("%s: %s with %s: %v %v", h.name, routeName(route), credential.name, named.err, plain.err)
				case named.status != plain.status:
					a.fail("%s: %s with %s and %s=B answered %d, %d without it", h.name, routeName(route), credential.name, key, named.status, plain.status)
				case scopeShape(plain) == scopeShape(again) && route.Path != "/Users/AuthenticateByName" && scopeShape(named) != scopeShape(plain):
					a.fail("%s: %s with %s and %s=B answered another body than without it", h.name, routeName(route), credential.name, key)
				}
			}
		}
		if route.Path == "/Users/AuthenticateByName" {
			r := accSend("POST", h.base, scopeAppend(path, "userId="+other), "", -1, body, "Authorization", `MediaBrowser Client="c", Device="d", DeviceId="scope-signin", Version="1"`)
			var signedIn struct {
				AccessToken string
				User        struct{ Id string }
			}
			if r.status != 200 || json.Unmarshal(r.body, &signedIn) != nil || !strings.EqualFold(signedIn.User.Id, db.CompactID(f.a.id)) {
				a.fail("%s: POST /Users/AuthenticateByName?userId=B with A's name and password answered %d %.80q, want a sign-in of A", h.name, r.status, r.body)
			}
		}
	}
}

func (a *accessSite) scopeSocket(t *testing.T, h accHandler, f *scopeFixture) {
	t.Helper()
	if !slices.Contains(h.roles, RoleAPI) {
		return
	}
	for _, who := range f.callers() {
		dial := func(id string) (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.base, "http")+"/socket?ApiKey="+who.user.token+"&userId="+id, nil)
			if err == nil {
				conn.CloseNow() //nolint:errcheck
				return 101, nil
			}
			if response != nil {
				return response.StatusCode, nil
			}
			return 0, err
		}
		for _, id := range []string{strings.ToUpper(who.user.id), db.CompactID(who.user.id)} {
			if status, err := dial(id); status != 101 {
				a.fail("%s: GET /socket?userId=<own, %s> as %s: %d %v, want the upgrade", h.name, id, who.name, status, err)
			}
		}
		if status, err := dial(db.CompactID(f.b.id)); status != http.StatusForbidden || err != nil {
			a.fail("%s: GET /socket?userId=B as %s: %d %v, want 403 and no upgraded connection", h.name, who.name, status, err)
		}
	}
}
