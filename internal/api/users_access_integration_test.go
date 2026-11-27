//go:build integration

package api

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
)

// JS-033: who may call the user management routes (criteria 3 and 4).

// administratorsOnly are the five routes only an administrator may call, with a user who is there in the path.
func administratorsOnly(target string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{"POST", "/Users/New", fmt.Sprintf(`{"Name":"made-%s","Password":"Qx7!%s"}`, uuid.NewString()[:8], uuid.NewString())},
		{"DELETE", "/Users/" + target, ""},
		{"POST", "/Users/" + target + "/Policy", `{"IsAdministrator":true}`},
		{"GET", "/Auth/Providers", ""},
		{"GET", "/Auth/PasswordResetProviders", ""},
	}
}

// scopeMalformedAdministratorAsks are requests to the administrators' routes that name a malformed id: a viewer's is
// refused for who asks, before the id is looked at, as Jellyfin 12.4 does.
var scopeMalformedAdministratorAsks = []struct{ method, path, body string }{
	{"DELETE", "/Users/" + scopeMalformed, ""},
	{"POST", "/Users/" + scopeMalformed + "/Policy", `{"IsAdministrator":true}`},
	{"POST", "/Users/New?userId=" + scopeMalformed, `{"Name":"made-malformed"}`},
	{"GET", "/Auth/Providers?userId=" + scopeMalformed, ""},
	{"GET", "/Auth/PasswordResetProviders?userId=" + scopeMalformed, ""},
}

// administrators is TestAccess's part for those five, on a handler with the api role: no token and an unknown one are
// 401, and a viewer is 403, also naming themselves. The wrong build: the guard that treats 'admin' as 'viewer'.
func (a *accessSite) administrators(t *testing.T, h accHandler) {
	t.Helper()
	if !h.serves(Route{Role: RoleAPI}) {
		return
	}
	f := a.scopeFixture(t)
	users := func() int { return scalar[int](t, a.pool, "SELECT count(*)::int FROM users") }
	before, rows := users(), a.scopeAllRows(t, f)
	for _, target := range []string{db.CompactID(f.b.id), db.CompactID(f.a.id)} {
		for _, route := range administratorsOnly(target) {
			for who, ask := range map[string]struct {
				token string
				want  int
			}{"with no token": {"", 401}, "with an unknown token": {"unknown-" + uuid.NewString(), 401}, "as a viewer": {f.a.token, 403}} {
				if r := accSend(route.method, h.base, route.path, ask.token, 3, route.body); r.err != nil || r.status != ask.want {
					a.fail("%s: %s %s %s answered %d %.40q, want %d", h.name, route.method, route.path, who, r.status, r.body, ask.want)
				}
			}
		}
	}
	for _, route := range scopeMalformedAdministratorAsks {
		if r := accSend(route.method, h.base, route.path, f.a.token, 3, route.body); r.err != nil || r.status != 403 {
			a.fail("%s: %s %s as a viewer answered %d %.40q, want 403", h.name, route.method, route.path, r.status, r.body)
		}
	}
	// What a page may also need is the administrators' alone, where it is there at all.
	for _, route := range (&Server{}).Routes() {
		if slices.Contains(scopeAdministratorsToo, routeName(route)) {
			if r := accSend(route.Method, h.base, route.Path, f.a.token, 3, ""); r.err != nil || r.status != 403 {
				a.fail("%s: %s as a viewer answered %d %.40q, want 403", h.name, routeName(route), r.status, r.body)
			}
		}
	}
	// Sessions and their sockets are the heartbeat's to change as well, so they are left out.
	changed := slices.DeleteFunc(scopeChanged(rows, a.scopeAllRows(t, f)), func(row string) bool { return strings.Contains(row, "session") })
	if len(changed) > 0 || users() != before {
		a.fail("%s: refused administrator requests changed rows %v, and there are %d users where there were %d", h.name, changed, users(), before)
	}
	for _, path := range []string{"/Auth/Providers", "/Auth/PasswordResetProviders"} {
		if r := accSend("GET", h.base, path, f.admin.token, 3, ""); r.status != 200 {
			a.fail("%s: GET %s as an administrator answered %d, want 200", h.name, path, r.status)
		}
	}
}

// A request refused for who asks, or for whom it names, is sent to Postgres as the token lookup and nothing else; one
// with no token as nothing. The wrong builds: a guard that looks the named user up first, or a handler that reads
// before it refuses.
func TestARefusedUserManagementRequestSendsPostgresNothingBeyondTheTokenLookup(t *testing.T) {
	a := newAccessSite(t)
	f := a.scopeFixture(t)
	h, log := a.serveTraced(t, RoleAPI)
	if r := accSend("GET", h.base, "/Users/Me", f.a.token, 3, ""); r.status != 200 {
		t.Fatalf("GET /Users/Me: %d %s", r.status, r.body)
	}
	other := db.CompactID(f.b.id)
	check := func(what, token string, want, statements int, method, path, body string) {
		t.Helper()
		log.take()
		r := accSend(method, h.base, path, token, 3, body)
		sent := log.take()
		lookups := 0
		for _, sql := range sent {
			if strings.Contains(strings.ToLower(sql), "from devices") {
				lookups++
			}
		}
		if r.status != want || len(sent) != statements || lookups != statements {
			t.Errorf("%s %s %s: answered %d with %d statements (%d token lookups), want %d with %d", method, path, what, r.status, len(sent), lookups, want, statements)
		}
	}
	for _, target := range []string{other, scopeNobody} {
		for _, route := range administratorsOnly(target) {
			check("with no token", "", 401, 0, route.method, route.path, route.body)
			check("with an unknown token", "unknown-"+uuid.NewString(), 401, 1, route.method, route.path, route.body)
			check("as a viewer", f.a.token, 403, 1, route.method, route.path, route.body)
		}
		for _, route := range []struct{ method, path, body string }{
			{"GET", "/Users?userId=" + target, ""},
			{"POST", "/Users?userId=" + target, `{"Name":"x"}`},
			{"POST", "/Users/Password?userId=" + target, `{"NewPw":"x"}`},
			{"POST", "/Users/" + target + "/Password", `{"NewPw":"x"}`},
		} {
			check("as a viewer", f.a.token, 403, 1, route.method, route.path, route.body)
			check("with no token", "", 401, 0, route.method, route.path, route.body)
		}
	}
	for _, route := range scopeMalformedAdministratorAsks {
		check("as a viewer", f.a.token, 403, 1, route.method, route.path, route.body)
	}
	check("by an administrator, path and query disagreeing", f.admin.token, 400, 1, "POST", "/Users/"+other+"/Password?userId="+db.CompactID(f.a.id), `{"NewPw":"x"}`)
}
