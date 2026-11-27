//go:build integration

package main

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// JS-033: passwords (criteria 15 and 16; D-096), and the configured administrator (21 and 22; D-095).

// passwordRoutes are the two spellings: the 12.4 route and jellyfin-web's (an obsolete alias).
var passwordRoutes = []struct {
	name string
	path func(id string) string
}{
	{"POST /Users/Password", func(id string) string { return "/Users/Password?userId=" + id }},
	{"POST /Users/{userId}/Password", func(id string) string { return "/Users/" + id + "/Password" }},
}

func TestChangingYourOwnPasswordNeedsTheCurrentOneAndEndsYourOtherTokensOnly(t *testing.T) {
	for _, route := range passwordRoutes {
		t.Run(route.name, func(t *testing.T) {
			u := newUsersSite(t, 2, nil)
			admin, viewer := u.member(true), u.member(false)
			film := u.film(4096)
			stream := start(t, "stream", u.dsn, map[string]any{"adminUser": ""})
			other := signedIn(t, "a second device", u.try(0, newDevice(), viewer.name, rightPassword))
			next := strong()
			for what, who := range map[string]qcAccount{"a viewer's own": viewer, "an administrator's own": admin} {
				for how, fields := range map[string]map[string]any{
					"a wrong CurrentPw":  {"CurrentPw": "wrong", "NewPw": next},
					"an empty CurrentPw": {"CurrentPw": "", "NewPw": next},
					"no CurrentPw":       {"NewPw": next},
				} {
					u.check(403, u.as(0, who.token, "POST", route.path(who.id), body(fields)), what+" change with "+how)
				}
				signedIn(t, what+": the old password after the refusals", u.try(0, newDevice(), who.name, rightPassword))
			}
			u.check(200, u.as(1, other, "GET", "/Users/Me", ""), "the other token after the refusals")

			u.check(204, u.as(0, viewer.token, "POST", route.path(viewer.id), body(map[string]any{"CurrentPw": rightPassword, "NewPw": next})), "the change")
			u.check(401, u.as(1, other, "GET", "/Users/Me", ""), "the other token, on the next request through the other api process")
			if got := rangeStatus(stream, film, other); got != 401 {
				t.Errorf("the other token's byte range through stream: %d, want 401", got)
			}
			u.check(200, u.as(1, viewer.token, "GET", "/Users/Me", ""), "the token of the device that changed it")
			if got := rangeStatus(stream, film, viewer.token); got != 206 {
				t.Errorf("its byte range through stream: %d, want 206", got)
			}
			u.check(401, u.try(1, newDevice(), viewer.name, rightPassword), "the old password")
			signedIn(t, "the new password", u.try(1, newDevice(), viewer.name, next))
		})
	}
}

func TestAnAdministratorSetsOrResetsAnothersPasswordWithoutTheCurrentOne(t *testing.T) {
	for _, route := range passwordRoutes {
		t.Run(route.name, func(t *testing.T) {
			u := newUsersSite(t, 2, nil)
			admin, viewer := u.member(true), u.member(false)
			film := u.film(4096)
			stream := start(t, "stream", u.dsn, map[string]any{"adminUser": ""})
			next := strong()
			u.check(204, u.as(0, admin.token, "POST", route.path(viewer.id), body(map[string]any{"CurrentPw": "wrong", "NewPw": next})), "the administrator's change")
			u.check(401, u.as(1, viewer.token, "GET", "/Users/Me", ""), "the user's token afterwards")
			if got := rangeStatus(stream, film, viewer.token); got != 401 {
				t.Errorf("the user's byte range through stream afterwards: %d, want 401", got)
			}
			u.check(200, u.as(1, admin.token, "GET", "/Users/Me", ""), "the administrator's token afterwards")
			token := signedIn(t, "the new password", u.try(1, newDevice(), viewer.name, next))

			// D-096: a reset ends the user's tokens too (Jellyfin 12.4 keeps them).
			u.check(204, u.as(0, admin.token, "POST", route.path(viewer.id), `{"ResetPassword":true}`), "the administrator's reset")
			u.check(401, u.as(1, token, "GET", "/Users/Me", ""), "the user's token after the reset")
			u.check(401, u.try(1, newDevice(), viewer.name, next), "the password from before the reset")
			signedIn(t, "an empty password after the reset", u.try(1, newDevice(), viewer.name, ""))
		})
	}
}

// A user with no password (criterion 15, D-082), as the capture has it: no CurrentPw, or an empty one, will do, and a
// wrong one will not.
func TestAUserWithNoPasswordSetsOneAsCaptured(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin := u.member(true)
	for how, current := range map[string]any{"no CurrentPw": nil, "an empty CurrentPw": ""} {
		name := "bare-" + uuid.NewString()[:8]
		made := u.created(admin, name, "-")
		token := signedIn(t, "with no password", u.try(0, newDevice(), name, ""))
		u.check(403, u.as(0, token, "POST", "/Users/Password?userId="+made.Id, body(map[string]any{"CurrentPw": "wrong", "NewPw": strong()})), "a wrong CurrentPw")
		fields := map[string]any{"NewPw": strong()}
		if current != nil {
			fields["CurrentPw"] = current
		}
		u.check(204, u.as(0, token, "POST", "/Users/Password?userId="+made.Id, body(fields)), "setting one with "+how)
		u.check(401, u.try(0, newDevice(), name, ""), "the empty password once one is set")
		signedIn(t, "the password set", u.try(0, newDevice(), name, fields["NewPw"].(string)))
	}
}

// Wrong current passwords are guesses like wrong sign-ins: from one address, five of each make the ten that refuse,
// and a refused change stores nothing.
func TestWrongCurrentPasswordsAndWrongSignInsShareTheAddressBound(t *testing.T) {
	u := newUsersSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	viewer := u.member(false, behindProxy(randomV4())...)
	address := behindProxy(randomV4())
	change := func(replica int, current, next string) signInReply {
		return u.as(replica, viewer.token, "POST", "/Users/Password?userId="+viewer.id, body(map[string]any{"CurrentPw": current, "NewPw": next}), address...)
	}
	for i := range 5 {
		u.check(403, change(i%2, "wrong", strong()), "a wrong current password")
		u.check(401, u.wrong(i%2, invented(), address...), "a wrong sign-in from the same address")
	}
	next := strong()
	refusedWithin(t, "the right current password after ten failures from the address", change(0, rightPassword, next), 1, 900)
	refusedWithin(t, "a sign-in from the address", u.try(1, newDevice(), invented(), "x", address...), 1, 900)
	signedIn(t, "the old password from another address", u.try(0, newDevice(), viewer.name, rightPassword, behindProxy(randomV4())...))
}

// With no proxy declared, the account's own bounds (the known device's 10, or the name's 20) stop the guessing.
func TestGuessingTheCurrentPasswordIsBoundedForTheAccount(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	viewer := u.member(false)
	next := strong()
	change := func(replica int, current string) signInReply {
		return u.as(replica, viewer.token, "POST", "/Users/Password?userId="+viewer.id, body(map[string]any{"CurrentPw": current, "NewPw": next}))
	}
	for i := range 20 {
		got := change(i%2, "wrong")
		if got.status != 403 {
			t.Fatalf("wrong current password %d: %d %q, want 403", i+1, got.status, got.body)
		}
	}
	refusedWithin(t, "the right current password after 20 wrong ones", change(0, rightPassword), 1, 900)
	u.check(401, u.try(0, newDevice(), viewer.name, next), "the password the refused change asked for")
}

// The three are on any list of commonly guessed passwords.
var guessed = []string{"password", "123456", "qwerty"}

func TestACommonlyGuessedPasswordIs400WithAMessageAndNothingIsStored(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	other := signedIn(t, "a second device", u.try(0, newDevice(), viewer.name, rightPassword))
	refused := func(what string, got signInReply) {
		t.Helper()
		u.check(400, got, what)
		if got.status == 400 && len(strings.TrimSpace(string(got.body))) == 0 {
			t.Errorf("%s: the 400 has no message", what)
		}
	}
	for _, password := range guessed {
		name := "weak-" + uuid.NewString()[:8]
		refused("a new user with "+password, u.as(0, admin.token, "POST", "/Users/New", body(map[string]any{"Name": name, "Password": password})))
		if n := u.userCount(name); n != 0 {
			t.Errorf("a refused new user with %q was stored", password)
		}
		for _, route := range passwordRoutes {
			refused(route.name+", own, to "+password, u.as(0, viewer.token, "POST", route.path(viewer.id), body(map[string]any{"CurrentPw": rightPassword, "NewPw": password})))
			refused(route.name+", an administrator's, to "+password, u.as(0, admin.token, "POST", route.path(viewer.id), body(map[string]any{"NewPw": password})))
		}
		u.check(401, u.try(0, newDevice(), viewer.name, password), "signing in with the refused password")
	}
	u.check(200, u.as(0, other, "GET", "/Users/Me", ""), "the other token after the refusals")
	signedIn(t, "the old password after the refusals", u.try(0, newDevice(), viewer.name, rightPassword))
	u.created(admin, "fine-"+uuid.NewString()[:8], strong())
}

// D-095. While the configuration names the account, no client deletes or renames it; it can be demoted and disabled,
// and a restart revives neither. With the key removed it is an ordinary user.
func TestTheConfiguredAdministratorIsNeitherDeletedNorRenamedWhileConfigured(t *testing.T) {
	configured := "configured-" + uuid.NewString()[:8]
	u := newUsersSite(t, 1, map[string]any{"adminUser": configured})
	admin := u.member(true)
	var id string
	for _, user := range u.listed(0, admin.token, "") {
		if user.Name == configured {
			id = user.Id
		}
	}
	if id == "" {
		t.Fatalf("GET /Users does not list the configured administrator %s", configured)
	}
	own := signedIn(t, "the configured administrator", u.try(0, newDevice(), configured, adminPassword))
	for what, token := range map[string]string{"another administrator": admin.token, "themselves": own} {
		for how, got := range map[string]signInReply{
			"DELETE":   u.as(0, token, "DELETE", "/Users/"+id, ""),
			"a rename": u.as(0, token, "POST", "/Users?userId="+id, body(map[string]any{"Name": "other-" + uuid.NewString()[:8]})),
		} {
			u.check(403, got, how+" by "+what)
			if !strings.Contains(string(got.body), "adminUser") {
				t.Errorf("%s by %s: the 403 does not name adminUser: %q", how, what, got.body)
			}
		}
	}
	if len(u.logLines("WARN", "adminUser")) == 0 {
		t.Errorf("no warn line names adminUser")
	}
	u.setPolicy(admin, id, map[string]any{"IsAdministrator": false})
	u.setPolicy(admin, id, map[string]any{"IsDisabled": true})
	u.restart(0)
	u.wantPolicy(admin, id, "after a restart", map[string]any{"IsAdministrator": false, "IsDisabled": true})

	without := u.add(map[string]any{"adminUser": ""})
	renamed := "released-" + uuid.NewString()[:8]
	u.check(204, u.as(without, admin.token, "POST", "/Users?userId="+id, body(map[string]any{"Name": renamed})), "a rename with the key removed")
	u.check(204, u.as(without, admin.token, "DELETE", "/Users/"+id, ""), "a delete with the key removed")
}
