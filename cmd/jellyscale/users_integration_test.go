//go:build integration

package main

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// JS-033. The statuses and shapes are compat/captures/users-admin-12.4.json's, except where a criterion or a decision
// (D-093 to D-096) keeps jellyscale different, which is said beside each.

// capturedUserFields are the fields Jellyfin 12.4 gave for a new user (the capture's "the fields of a new user's
// answer"); Configuration and Policy are always there, LastLoginDate only for one who has signed in.
func capturedUserFields(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/users-admin-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		API []struct {
			Case   string          `json:"case"`
			Answer json.RawMessage `json:"answer"`
		} `json:"api"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	for _, row := range capture.API {
		var fields []string
		if row.Case == "the fields of a new user's answer" && json.Unmarshal(row.Answer, &fields) == nil {
			return fields
		}
	}
	t.Fatal("the capture has no row of a new user's fields")
	return nil
}

func wantFields(t *testing.T, raw []byte, what string, fields []string) {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Errorf("%s: %.200q is no object", what, raw)
		return
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			t.Errorf("%s has no field %s", what, field)
		}
	}
}

// Criteria 2 and 6.
func TestNewUserAnswersAsCapturedAndMakesAnEnabledNonAdministrator(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer := u.member(true), u.member(false)
	name, password := "new-"+uuid.NewString()[:8], strong()

	got := u.must(200, 0, admin.token, "POST", "/Users/New", body(map[string]any{"Name": name, "Password": password}), "a new user")
	wantFields(t, got.body, "POST /Users/New", capturedUserFields(t))
	made := decodeUser(t, got, "POST /Users/New")
	if made.Name != name || made.Policy["IsAdministrator"] != false || made.Policy["IsDisabled"] != false || !made.HasPassword {
		t.Errorf("POST /Users/New made %q with IsAdministrator %v IsDisabled %v HasPassword %v, want an enabled non-administrator", made.Name, made.Policy["IsAdministrator"], made.Policy["IsDisabled"], made.HasPassword)
	}
	signedIn(t, "the new user signing in through the other replica", u.try(1, newDevice(), name, password))

	for what, fields := range map[string]map[string]any{
		"the same name":                {"Name": name, "Password": strong()},
		"the name in another case":     {"Name": strings.ToUpper(name), "Password": strong()},
		"an empty name":                {"Name": "", "Password": strong()},
		"a blank name":                 {"Name": "   ", "Password": strong()},
		"no Name":                      {"Password": strong()},
		"a name with a trailing space": {"Name": "trail-" + uuid.NewString()[:8] + " ", "Password": strong()},
		"a name with a slash":          {"Name": "a/" + uuid.NewString()[:8], "Password": strong()},
	} {
		before := len(u.listed(0, admin.token, ""))
		u.check(400, u.as(1, admin.token, "POST", "/Users/New", body(fields)), what)
		if after := len(u.listed(0, admin.token, "")); after != before {
			t.Errorf("%s: %d users before and %d after, want nothing created", what, before, after)
		}
	}
	if n := u.userCount(name); n != 1 {
		t.Errorf("%d users named %q in some case, want 1", n, name)
	}
	u.check(403, u.as(0, viewer.token, "POST", "/Users/New", body(map[string]any{"Name": "by-viewer", "Password": strong()})), "a viewer making a user")

	bare := "bare-" + uuid.NewString()[:8]
	u.created(admin, bare, "-")
	signedIn(t, "a user made without a Password signing in with an empty one", u.try(0, newDevice(), bare, ""))
	u.check(401, u.try(0, newDevice(), bare, "something"), "a user made without a Password signing in with one")
}

// Criterion 6, forced: both processes have read that the name is free before either writes. users is held in EXCLUSIVE
// mode (reads, the token's check among them, go on; writes and row locks wait) until both requests wait in Postgres.
// The wrong build: look the name up, then insert, and answer the loser's unique violation as a server error, or with
// no unique key make two users.
func TestTwoProcessesCreatingOneNameAtOnceMakeExactlyOneUser(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin := u.member(true)
	name := "twin-" + uuid.NewString()[:8]
	replies := u.heldUsers([]int{0, 1},
		func() signInReply {
			return u.as(0, admin.token, "POST", "/Users/New", body(map[string]any{"Name": name, "Password": strong()}))
		},
		func() signInReply {
			return u.as(1, admin.token, "POST", "/Users/New", body(map[string]any{"Name": strings.ToUpper(name), "Password": strong()}))
		})
	got := []int{replies[0].status, replies[1].status}
	sort.Ints(got)
	if !slices.Equal(got, []int{200, 400}) {
		t.Errorf("the two answers are %v (%.100q, %.100q), want one 200 and one 400", got, replies[0].body, replies[1].body)
	}
	if n := u.userCount(name); n != 1 {
		t.Errorf("%d users of that name, want exactly 1", n)
	}
}

// Criterion 6: a deleted user's name is free, and whoever takes it starts with nothing of theirs.
func TestADeletedUsersNameCanBeReusedAndTheNewUserInheritsNothing(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin := u.member(true)
	film := u.film(4096)
	name, password := "reused-"+uuid.NewString()[:8], strong()
	first := u.created(admin, name, password)
	device := newDevice()
	oldToken := signedIn(t, "the first user", u.try(0, device, name, password))
	u.check(200, u.as(0, oldToken, "POST", "/UserItems/"+film.item+"/UserData", `{"IsFavorite":true,"Played":true,"PlaybackPositionTicks":123000000}`), "the first user's watch state")
	u.must(204, 0, admin.token, "DELETE", "/Users/"+first.Id, "", "deleting the user")

	second := u.created(admin, name, password)
	if second.Id == first.Id {
		t.Errorf("the new user has the deleted user's id")
	}
	u.check(401, u.as(1, oldToken, "GET", "/Users/Me", ""), "the deleted user's token after the name was made again")
	rows := u.rowsOf(second.Id)
	for _, table := range []string{"devices", "sessions", "user_item_data", "display_preferences"} {
		if rows[table] != "" {
			t.Errorf("the new user starts with rows in %s: %.200s", table, rows[table])
		}
	}
	token := signedIn(t, "the new user", u.try(1, newDevice(), name, password))
	var item struct{ UserData map[string]any }
	got := u.must(200, 1, token, "GET", "/Items/"+film.item, "", "the new user's view of the film")
	if err := json.Unmarshal(got.body, &item); err != nil {
		t.Fatal(err)
	}
	if item.UserData["Played"] != false || item.UserData["IsFavorite"] != false || item.UserData["PlaybackPositionTicks"] != float64(0) {
		t.Errorf("the new user inherits watch state: %v", item.UserData)
	}
}

// Criterion 5.
func TestAViewerListsOnlyThemselvesAndAnAdministratorEveryUser(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer, other := u.member(true), u.member(false), u.member(false)
	u.onlyThese(admin, viewer, other)
	u.setPolicy(admin, other.id, map[string]any{"IsDisabled": true})

	if got := u.listed(1, viewer.token, ""); len(got) != 1 || got[0].Name != viewer.name {
		t.Errorf("a viewer's GET /Users lists %v, want only %s", names(got), viewer.name)
	}
	everyone := u.listed(1, admin.token, "")
	want := []string{admin.name, viewer.name, other.name}
	sort.Strings(want)
	if !slices.Equal(names(everyone), want) {
		t.Errorf("an administrator's GET /Users lists %v, want %v", names(everyone), want)
	}
	for _, user := range everyone {
		if user.Policy["IsAdministrator"] != (user.Name == admin.name) || user.Policy["IsDisabled"] != (user.Name == other.name) {
			t.Errorf("%s is listed with IsAdministrator %v and IsDisabled %v", user.Name, user.Policy["IsAdministrator"], user.Policy["IsDisabled"])
		}
		raw, _ := json.Marshal(user)
		wantFields(t, raw, "an entry of GET /Users", []string{"Name", "Id", "HasPassword", "Policy"})
	}
	if got := names(u.listed(0, admin.token, "?isDisabled=true")); !slices.Equal(got, []string{other.name}) {
		t.Errorf("?isDisabled=true lists %v, want the disabled user", got)
	}
	if got := u.listed(0, admin.token, "?isDisabled=false"); len(got) != 2 || slices.Contains(names(got), other.name) {
		t.Errorf("?isDisabled=false lists %v, want the two enabled users", names(got))
	}
}

// Criterion 2: the providers jellyfin-web's pages ask for, and the profile page's POST /Users.
func TestTheProviderListsAndTheProfileSaveAnswerAsCaptured(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer, other := u.member(true), u.member(false), u.member(false)
	for path, want := range map[string]string{
		"/Auth/Providers":              `[{"Id":"Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider","Name":"Default"}]`,
		"/Auth/PasswordResetProviders": `[{"Id":"Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider","Name":"Default Password Reset Provider"}]`,
	} {
		got := u.must(200, 0, admin.token, "GET", path, "", "the provider list")
		var value any
		if err := json.Unmarshal(got.body, &value); err != nil {
			t.Errorf("%s: %q", path, got.body)
			continue
		}
		normal, _ := json.Marshal(value)
		if string(normal) != want {
			t.Errorf("%s: %s, want %s", path, normal, want)
		}
		u.check(401, u.as(0, "", "GET", path, ""), path+" with no token")
	}

	rename := func(replica int, token, id, name string, policy map[string]any) signInReply {
		dto := map[string]any{}
		raw := u.must(200, 0, admin.token, "GET", "/Users/"+viewer.id, "", "the user DTO").body
		if err := json.Unmarshal(raw, &dto); err != nil {
			t.Fatal(err)
		}
		dto["Name"] = name
		for field, value := range policy {
			dto["Policy"].(map[string]any)[field] = value
		}
		path := "/Users"
		if id != "" {
			path += "?userId=" + id
		}
		return u.as(replica, token, "POST", path, body(dto))
	}
	renamed := "renamed-" + uuid.NewString()[:8]
	u.check(204, rename(0, viewer.token, viewer.id, renamed, nil), "a viewer renaming themselves")
	if got := u.meDTO(1, viewer.token).Name; got != renamed {
		t.Errorf("after the rename the user is %q, want %q", got, renamed)
	}
	u.check(204, rename(0, viewer.token, viewer.id, renamed, map[string]any{"IsAdministrator": true}), "a viewer's own DTO that asks IsAdministrator")
	if u.meDTO(1, viewer.token).Policy["IsAdministrator"] != false {
		t.Errorf("POST /Users made a viewer an administrator")
	}
	u.check(403, rename(0, viewer.token, other.id, "stolen", nil), "a viewer renaming another")
	u.check(400, rename(0, admin.token, viewer.id, strings.ToUpper(other.name), nil), "a rename to a taken name in another case")
	u.check(400, rename(0, admin.token, viewer.id, "", nil), "a rename to the empty name")
	u.check(204, rename(0, admin.token, viewer.id, strings.ToUpper(renamed), nil), "a rename to the user's own name in another case")
	u.check(404, rename(0, admin.token, "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f", "nobody", nil), "an administrator saving a user nobody has")
	u.check(400, rename(0, admin.token, "not-a-guid", "nobody", nil), "an administrator saving a malformed id")
	if got := u.meDTO(1, viewer.token).Name; got != strings.ToUpper(renamed) {
		t.Errorf("after the refused renames the user is %q, want %q", got, strings.ToUpper(renamed))
	}
}

// Criterion 4: the ids of the routes an administrator may use for another account.
func TestTheAccountRoutesTakeAnotherUsersIdOnlyFromAnAdministrator(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer, other := u.member(true), u.member(false), u.member(false)
	nobody := "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
	password := body(map[string]any{"CurrentPw": rightPassword, "NewPw": strong()})
	type route struct{ method, path, body string }
	routes := []route{
		{"GET", "/Users?userId=ID", ""},
		{"POST", "/Users?userId=ID", body(map[string]any{"Name": "x-" + uuid.NewString()[:8]})},
		{"POST", "/Users/Password?userId=ID", password},
		{"POST", "/Users/ID/Password", password},
	}
	for _, r := range routes {
		path := func(id string) string { return strings.Replace(r.path, "ID", id, 1) }
		what := r.method + " " + r.path
		u.check(403, u.as(0, viewer.token, r.method, path(other.id), r.body), what+": a viewer naming another")
		u.check(403, u.as(0, viewer.token, r.method, path(nobody), r.body), what+": a viewer naming nobody")
		u.check(404, u.as(0, admin.token, r.method, path(nobody), r.body), what+": an administrator naming nobody")
		u.check(400, u.as(0, viewer.token, r.method, path("not-a-guid"), r.body), what+": a viewer with a malformed id")
		u.check(400, u.as(0, admin.token, r.method, path("not-a-guid"), r.body), what+": an administrator with a malformed id")
	}
	disagree := "?userId=" + viewer.id
	u.check(400, u.as(0, admin.token, "POST", "/Users/"+other.id+"/Password"+disagree, password), "the password alias with path and query disagreeing")
	u.check(400, u.as(0, admin.token, "POST", "/Users/"+other.id+"/Policy"+disagree, `{"IsDisabled":true}`), "Policy with path and query disagreeing")
	u.check(400, u.as(0, admin.token, "DELETE", "/Users/"+other.id+disagree, ""), "DELETE with path and query disagreeing")
	signedIn(t, "the other user after every refusal", u.try(0, newDevice(), other.name, rightPassword))
}

// Criterion 2 and the statuses of DELETE and Policy.
func TestDeleteAndPolicyAnswerUnknownAndMalformedIdsAsCaptured(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, second, viewer, gone := u.member(true), u.member(true), u.member(false), u.member(false)
	nobody := "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
	u.check(404, u.as(0, admin.token, "DELETE", "/Users/"+nobody, ""), "DELETE of a user nobody has")
	u.check(400, u.as(0, admin.token, "DELETE", "/Users/not-a-guid", ""), "DELETE of a malformed id")
	u.check(403, u.as(0, viewer.token, "DELETE", "/Users/"+gone.id, ""), "a viewer's DELETE")
	u.check(204, u.as(0, admin.token, "DELETE", "/Users/"+gone.id, ""), "DELETE of a viewer")
	u.check(404, u.as(1, admin.token, "DELETE", "/Users/"+gone.id, ""), "DELETE of a deleted user")
	u.check(404, u.policy(0, admin.token, nobody, map[string]any{"IsDisabled": false}), "Policy of a user nobody has")
	u.check(400, u.policy(0, admin.token, "not-a-guid", map[string]any{"IsDisabled": false}), "Policy of a malformed id")
	u.check(403, u.policy(0, viewer.token, viewer.id, map[string]any{"IsAdministrator": true}), "a viewer's own Policy")
	u.check(400, u.as(0, admin.token, "POST", "/Users/"+viewer.id+"/Policy", `{"IsAdministrator":null}`), "IsAdministrator null")
	u.check(400, u.as(0, admin.token, "POST", "/Users/"+viewer.id+"/Policy", `{"IsDisabled":null}`), "IsDisabled null")
	u.check(204, u.as(0, second.token, "DELETE", "/Users/"+second.id, ""), "one of two administrators deleting themselves")
	u.check(401, u.as(1, second.token, "GET", "/Users/Me", ""), "their token afterwards")
}
