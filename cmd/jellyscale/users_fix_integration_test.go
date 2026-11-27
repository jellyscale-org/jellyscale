//go:build integration

package main

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JS-033, the fix round.

// within makes the site's helpers fail the subtest until the returned function is called.
func (u *usersSite) within(t *testing.T) (restore func()) {
	parent := u.t
	u.t = t
	return func() { u.t = parent }
}

// waitUntil asks until ok says yes. The deadline only bounds a hang.
func (u *usersSite) waitUntil(what string, ok func() bool) {
	u.t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			u.t.Fatalf("waited 30 s for %s", what)
		}
	}
}

// blockedBehind says whether a session of the application name waits on a lock the session pid holds, directly or
// through other waiting sessions.
func (u *usersSite) blockedBehind(pid int, application string) (blocked bool) {
	u.t.Helper()
	if err := u.pool.QueryRow(context.Background(), `WITH RECURSIVE chain(pid) AS (
			SELECT $1::int
			UNION SELECT a.pid FROM pg_stat_activity a JOIN chain c ON c.pid = ANY(pg_blocking_pids(a.pid)))
		SELECT EXISTS (SELECT 1 FROM pg_stat_activity a JOIN chain c USING (pid) WHERE a.application_name = $2 AND a.pid <> $1)`,
		pid, application).Scan(&blocked); err != nil {
		u.t.Fatal(err)
	}
	return blocked
}

// approvedWhileHeld holds the change, which replica 0 makes, at its commit, and meanwhile sends the approval to replica 1:
// its token is still good there, as the change cannot be seen. It lets the change commit once the approval has answered
// or waits behind the change.
func (u *usersSite) approvedWhileHeld(change func() signInReply, token string, code qcCode) (changed, approved signInReply) {
	u.t.Helper()
	release := u.holdAtCommit(u.names[0])
	changing := make(chan signInReply, 1)
	go func() { changing <- change() }()
	var holder int
	u.waitUntil("the change to come to its commit", func() bool {
		select {
		case got := <-changing:
			u.t.Fatalf("the change answered %d %.200q without being held at its commit", got.status, got.body)
		default:
		}
		err := u.pool.QueryRow(context.Background(), `SELECT pid FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND wait_event = 'advisory'`, u.names[0]).Scan(&holder)
		if err != nil && err != pgx.ErrNoRows {
			u.t.Fatal(err)
		}
		return err == nil
	})
	approving := make(chan signInReply, 1)
	go func() { approving <- u.authorize(1, token, code.Code, "") }()
	answered := false
	u.waitUntil("the approval to answer or to wait behind the change", func() bool {
		select {
		case approved = <-approving:
			answered = true
			return true
		default:
		}
		return u.blockedBehind(holder, u.names[1])
	})
	release()
	if !answered {
		approved = <-approving
	}
	return <-changing, approved
}

// An approval whose token was checked before a password change, set, reset or disable committed must be worth nothing
// once it has: the code redeems nothing, also once a disabled account is enabled again. The wrong build: remove the
// account's approved codes in the change's transaction, and let an approval written afterwards stand.
func TestAnApprovalThatRacesAChangeOfTheAccountSignsNoDeviceIn(t *testing.T) {
	type race struct {
		u                 *usersSite
		admin, who        qcAccount
		secondDeviceToken string
	}
	for _, c := range []struct {
		name string
		// approver is the token that approves, one the change ends.
		approver func(r race) string
		change   func(r race) signInReply
		after    func(r race)
	}{
		{"own password change on another device", func(r race) string { return r.secondDeviceToken },
			func(r race) signInReply {
				return r.u.as(0, r.who.token, "POST", "/Users/Password?userId="+r.who.id, body(map[string]any{"CurrentPw": rightPassword, "NewPw": strong()}))
			}, nil},
		{"an administrator sets the password", func(r race) string { return r.who.token },
			func(r race) signInReply {
				return r.u.as(0, r.admin.token, "POST", "/Users/Password?userId="+r.who.id, body(map[string]any{"NewPw": strong()}))
			}, nil},
		{"an administrator resets the password", func(r race) string { return r.who.token },
			func(r race) signInReply {
				return r.u.as(0, r.admin.token, "POST", "/Users/Password?userId="+r.who.id, `{"ResetPassword":true}`)
			}, nil},
		{"a disable, and the account enabled again", func(r race) string { return r.who.token },
			func(r race) signInReply {
				return r.u.policy(0, r.admin.token, r.who.id, map[string]any{"IsDisabled": true})
			},
			func(r race) { r.u.setPolicy(r.admin, r.who.id, map[string]any{"IsDisabled": false}) }},
	} {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			u := newUsersSite(t, 2, nil)
			r := race{u: u, admin: u.member(true), who: u.member(false)}
			r.secondDeviceToken = signedIn(t, "a second device", u.try(0, newDevice(), r.who.name, rightPassword))
			code := u.start(1, newTV())
			changed, approved := u.approvedWhileHeld(func() signInReply { return c.change(r) }, c.approver(r), code)
			if changed.status != 204 {
				t.Fatalf("the change: %d %.200q, want 204", changed.status, changed.body)
			}
			if approved.status == 0 || approved.status >= 500 {
				t.Errorf("the racing approval: %d %.200q", approved.status, approved.body)
			}
			t.Logf("the racing approval answered %d %.100q", approved.status, approved.body)
			redeemsNothing := func(when string) {
				t.Helper()
				unknown := u.redeem(1, newTV(), "0000deadbeef")
				got := u.redeem(1, code.device, code.Secret)
				if got.status != unknown.status || string(got.body) != string(unknown.body) {
					t.Errorf("redeeming the racing code %s: %d %.200q, want the unknown secret's %d %q", when, got.status, got.body, unknown.status, unknown.body)
				}
				var signed qcSignIn
				if got.status == 200 && json.Unmarshal(got.body, &signed) == nil {
					if status := u.me(1, signed.AccessToken); status != 401 {
						t.Errorf("the token it gave %s: GET /Users/Me is %d, want 401", when, status)
					}
				}
			}
			redeemsNothing("after the change committed")
			if c.after != nil {
				c.after(r)
				redeemsNothing("once the account is enabled again")
			}
			if n := u.deviceTokens(code.device, r.who); n != 0 {
				t.Errorf("%d tokens were issued to the device that showed the code", n)
			}
		})
	}
}

// Two administrators reset each other at once, through two processes, both authenticated before either writes. The first
// to commit ends the other's token, so the other's reset must not apply: one is refused, and an enabled administrator who
// can sign in remains. The wrong build: apply a reset without looking again, in its transaction, at the caller.
func TestOfTwoAdministratorsResettingEachOtherOneIsRefusedAndOneCanStillSignIn(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	a, b := u.member(true), u.member(true)
	u.onlyThese(a, b)
	ctx := context.Background()
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // The commit below has ended it on the way out.
	var holder int
	if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holder); err != nil {
		t.Fatal(err)
	}
	// Plain reads, the tokens' checks among them, go on; writes and row locks wait.
	if _, err := tx.Exec(ctx, "LOCK TABLE users IN EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	pairs := [2]struct{ actor, target qcAccount }{{a, b}, {b, a}}
	var replies [2]signInReply
	var done [2]chan struct{}
	var wg sync.WaitGroup
	for n, pair := range pairs {
		done[n] = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[n])
			replies[n] = u.as(n, pair.actor.token, "POST", "/Users/Password?userId="+pair.target.id, `{"ResetPassword":true}`)
		}()
	}
	for n := range pairs {
		u.waitUntil("a reset to wait behind the held users table", func() bool {
			select {
			case <-done[n]:
				t.Fatalf("a reset answered %d %.200q while users was held", replies[n].status, replies[n].body)
			default:
			}
			return u.blockedBehind(holder, u.names[n])
		})
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	got := []int{replies[0].status, replies[1].status}
	sort.Ints(got)
	if got[0] != 204 || (got[1] != 401 && got[1] != 403) {
		t.Errorf("the two resets answered %v (%.100q, %.100q), want one 204 and one refused with 401 or 403", got, replies[0].body, replies[1].body)
	}
	canSignIn := 0
	for _, account := range []qcAccount{a, b} {
		reply := u.try(0, newDevice(), account.name, rightPassword)
		var signed struct {
			User userDTO
		}
		if reply.status == 200 && json.Unmarshal(reply.body, &signed) == nil &&
			signed.User.Policy["IsAdministrator"] == true && signed.User.Policy["IsDisabled"] == false {
			canSignIn++
		}
	}
	if canSignIn != 1 {
		t.Errorf("%d enabled administrators can sign in afterwards, want 1", canSignIn)
	}
}

// bcrypt reads a key cyclically with a NUL after it, so "password\x00password" is the same key as "password": a new
// password holding a NUL byte is refused like a commonly guessed one, and nothing is stored.
func TestANewPasswordWithANULByteIsRefusedAndNothingIsStored(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	passwords := map[string]string{
		"a common password repeated around a NUL": "password\x00password",
		"a strong password with a NUL":            strong() + "\x00tail",
	}
	refused := func(t *testing.T, got signInReply) {
		t.Helper()
		u.check(400, got, "setting it")
		if got.status == 400 && strings.TrimSpace(string(got.body)) == "" {
			t.Errorf("the 400 has no message")
		}
	}
	unchanged := func(t *testing.T, who qcAccount, other, password string) {
		t.Helper()
		u.check(401, u.try(0, newDevice(), who.name, "password"), "signing in with plain 'password'")
		u.check(401, u.try(0, newDevice(), who.name, password), "signing in with the refused password")
		u.check(200, u.as(0, other, "GET", "/Users/Me", ""), "the account's other token")
		u.check(200, u.try(0, newDevice(), who.name, rightPassword), "signing in with the old password")
	}
	for what, password := range passwords {
		t.Run(strings.ReplaceAll("POST /Users/New, "+what, " ", "_"), func(t *testing.T) {
			defer u.within(t)()
			admin := u.member(true)
			name := "nul-" + uuid.NewString()[:8]
			refused(t, u.as(0, admin.token, "POST", "/Users/New", body(map[string]any{"Name": name, "Password": password})))
			if n := u.userCount(name); n != 0 {
				t.Errorf("%d users were stored", n)
				u.check(401, u.try(0, newDevice(), name, "password"), "signing in with plain 'password'")
			}
		})
		for _, route := range passwordRoutes {
			for _, caller := range []string{"own change", "administrator's set"} {
				t.Run(strings.ReplaceAll(route.name+", "+caller+", "+what, " ", "_"), func(t *testing.T) {
					defer u.within(t)()
					admin, who := u.member(true), u.member(false)
					other := signedIn(t, "a second device", u.try(0, newDevice(), who.name, rightPassword))
					token, fields := admin.token, map[string]any{"NewPw": password}
					if caller == "own change" {
						token, fields["CurrentPw"] = who.token, rightPassword
					}
					refused(t, u.as(0, token, "POST", route.path(who.id), body(fields)))
					unchanged(t, who, other, password)
				})
			}
		}
	}
}

// Names are unique by Postgres's lower(), under which a long s is not an s: an account whose name only Go's case folding
// takes for adminUser is an ordinary one, to rename and delete. The wrong build: strings.EqualFold.
func TestOnlyTheAccountTheDatabaseTakesForAdminUserIsProtected(t *testing.T) {
	suffix := uuid.NewString()[:8]
	configured := "boss-adv-" + suffix
	u := newUsersSite(t, 1, map[string]any{"adminUser": configured})
	admin, renames, deleted := u.member(true), u.member(false), u.member(false)
	var configuredID string
	for _, user := range u.listed(0, admin.token, "") {
		if user.Name == configured {
			configuredID = user.Id
		}
	}
	if configuredID == "" {
		t.Fatalf("GET /Users does not list the configured administrator %s", configured)
	}
	rename := func(who qcAccount, name string) signInReply {
		return u.as(0, who.token, "POST", "/Users?userId="+who.id, body(map[string]any{"Name": name}))
	}
	for who, lookAlike := range map[*qcAccount]string{&renames: "bo\u017fs-adv-" + suffix, &deleted: "bos\u017f-adv-" + suffix} {
		if got := rename(*who, lookAlike); got.status != 204 {
			t.Fatalf("a viewer taking the name %q, which the database holds apart from %q: %d %.200q, want 204", lookAlike, configured, got.status, got.body)
		}
	}
	u.check(204, rename(renames, "ordinary-"+suffix), "a look-alike renaming itself again")
	u.check(204, u.as(0, admin.token, "DELETE", "/Users/"+deleted.id, ""), "deleting a look-alike account")
	u.check(403, u.as(0, admin.token, "DELETE", "/Users/"+configuredID, ""), "deleting the configured administrator itself")
}

// A refusal to demote, disable or delete the last enabled administrator is logged at warn with the user and the reason.
func TestARefusalToLeaveNoAdministratorIsLoggedWithTheUserAndTheReason(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	u.onlyThese(admin, viewer)
	for _, c := range []struct {
		name string
		do   func() signInReply
	}{
		{"demote", func() signInReply {
			return u.policy(0, admin.token, admin.id, map[string]any{"IsAdministrator": false})
		}},
		{"disable", func() signInReply { return u.policy(0, admin.token, admin.id, map[string]any{"IsDisabled": true}) }},
		{"delete", func() signInReply { return u.as(0, admin.token, "DELETE", "/Users/"+admin.id, "") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := func() int { return len(u.logLines("WARN", admin.name, "administrator")) }
			before := lines()
			if got := c.do(); got.status != 403 {
				t.Fatalf("%s of the last administrator: %d %.200q, want 403", c.name, got.status, got.body)
			}
			// The log arrives through a pipe: the deadline only bounds the wait for a line that never comes.
			for deadline := time.Now().Add(10 * time.Second); lines() == before; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("no new warn line names %s and says why the %s was refused", admin.name, c.name)
				}
			}
		})
	}
}

// compat/differences.yaml, "users: the list filtered by isHidden": nobody is hidden.
func TestNobodyIsHiddenWhenTheUserListIsFilteredByIsHidden(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer, off := u.member(true), u.member(false), u.member(false)
	u.onlyThese(admin, viewer, off)
	u.setPolicy(admin, off.id, map[string]any{"IsDisabled": true})
	everyone := []string{admin.name, viewer.name, off.name}
	sort.Strings(everyone)
	for query, want := range map[string][]string{
		"?isHidden=true":                   nil,
		"?IsHidden=true":                   nil,
		"?isHidden=true&isDisabled=false":  nil,
		"?isHidden=false":                  everyone,
		"?isHidden=false&isDisabled=true":  {off.name},
		"?isHidden=false&isDisabled=false": slices.DeleteFunc(slices.Clone(everyone), func(name string) bool { return name == off.name }),
	} {
		if got := names(u.listed(0, admin.token, query)); !slices.Equal(got, want) {
			t.Errorf("an administrator's GET /Users%s lists %v, want %v", query, got, want)
		}
	}
	if got := names(u.listed(0, viewer.token, "?isHidden=false")); !slices.Equal(got, []string{viewer.name}) {
		t.Errorf("a viewer's GET /Users?isHidden=false lists %v, want themselves", got)
	}
	hidden := names(u.listed(0, viewer.token, "?isHidden=true"))
	t.Logf("a viewer's GET /Users?isHidden=true lists %v", hidden)
	if slices.ContainsFunc(hidden, func(name string) bool { return name != viewer.name }) {
		t.Errorf("a viewer's GET /Users?isHidden=true lists another account: %v", hidden)
	}
}

// D-105: an account with no password has no current one to prove, so its own change is 403 and stores nothing.
func TestAUserWithNoPasswordCannotChangeTheirOwn(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	for _, route := range passwordRoutes {
		t.Run(route.name, func(t *testing.T) {
			defer u.within(t)()
			who := u.member(false)
			// As a migrated account is: no password, and a token from another way of signing in.
			if _, err := u.pool.Exec(context.Background(), "UPDATE users SET password_hash = '' WHERE id = $1::uuid", who.id); err != nil {
				t.Fatal(err)
			}
			next := strong()
			for how, fields := range map[string]map[string]any{
				"no CurrentPw":       {"NewPw": next},
				"an empty CurrentPw": {"CurrentPw": "", "NewPw": next},
				"a wrong CurrentPw":  {"CurrentPw": "wrong", "NewPw": next},
			} {
				u.check(403, u.as(0, who.token, "POST", route.path(who.id), body(fields)), "its own change with "+how)
			}
			u.check(200, u.as(1, who.token, "GET", "/Users/Me", ""), "its token afterwards")
			u.check(401, u.try(1, newDevice(), who.name, next), "the password the refused change asked for")
			u.check(401, u.try(1, newDevice(), who.name, ""), "the empty password")
		})
	}
}

// compat/differences.yaml, "users: the lists the user pages read".
func TestTheListsTheUserPagesReadHoldWhatTheyShowAndNoToken(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	u.onlyThese(admin, viewer)
	u.film(4096)
	device := newTV()
	signIn := u.call(0, "POST", "/Users/AuthenticateByName", body(map[string]any{"Username": viewer.name, "Pw": rightPassword}), "Authorization", device.header(""))
	signedIn(t, "a named device", signIn)
	read := func(t *testing.T, path string, into any) []byte {
		t.Helper()
		got := u.must(200, 0, admin.token, "GET", path, "", "a list a user page reads")
		if err := json.Unmarshal(got.body, into); err != nil {
			t.Fatalf("GET %s: %.300q: %v", path, got.body, err)
		}
		return got.body
	}
	type page struct {
		Items            []map[string]any
		TotalRecordCount *int
	}

	t.Run("Channels", func(t *testing.T) {
		var channels page
		read(t, "/Channels", &channels)
		if channels.Items == nil || len(channels.Items) != 0 || channels.TotalRecordCount == nil || *channels.TotalRecordCount != 0 {
			t.Errorf("GET /Channels is no valid empty list: %+v", channels)
		}
	})
	t.Run("ParentalRatings", func(t *testing.T) {
		var ratings []any
		if raw := read(t, "/Localization/ParentalRatings", &ratings); strings.TrimSpace(string(raw)) != "[]" {
			t.Errorf("GET /Localization/ParentalRatings is %.300q, want []", raw)
		}
	})
	t.Run("MediaFolders", func(t *testing.T) {
		var names []string
		rows, err := u.pool.Query(context.Background(), "SELECT name FROM libraries")
		if err != nil {
			t.Fatal(err)
		}
		if names, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			t.Fatal(err)
		}
		var folders page
		read(t, "/Library/MediaFolders?isHidden=false", &folders)
		var listed []string
		for _, folder := range folders.Items {
			name, _ := folder["Name"].(string)
			listed = append(listed, name)
			if folder["Id"] == nil || folder["Id"] == "" || folder["Type"] != "CollectionFolder" {
				t.Errorf("the folder %q has Id %v and Type %v", name, folder["Id"], folder["Type"])
			}
		}
		sort.Strings(names)
		sort.Strings(listed)
		if len(names) == 0 || !slices.Equal(listed, names) || folders.TotalRecordCount == nil || *folders.TotalRecordCount != len(names) {
			t.Errorf("GET /Library/MediaFolders lists %v, want the libraries %v", listed, names)
		}
	})
	t.Run("Devices", func(t *testing.T) {
		var devices page
		raw := read(t, "/Devices", &devices)
		var found map[string]any
		for _, listed := range devices.Items {
			if listed["Id"] == device.id {
				found = listed
			}
		}
		if found == nil {
			t.Fatalf("GET /Devices does not list the signed-in device %s: %.500q", device.id, raw)
		}
		for field, want := range map[string]any{"Name": device.name, "AppName": device.app, "AppVersion": device.version,
			"LastUserName": viewer.name, "LastUserId": strings.ReplaceAll(viewer.id, "-", "")} {
			if found[field] != want {
				t.Errorf("the device's %s is %v, want %v", field, found[field], want)
			}
		}
		signedAt, _ := found["DateLastActivity"].(string)
		if _, err := time.Parse(time.RFC3339Nano, signedAt); err != nil {
			t.Errorf("the device's DateLastActivity is %q", signedAt)
		}
		if _, isObject := found["Capabilities"].(map[string]any); !isObject {
			t.Errorf("the device's Capabilities is %v, want an object", found["Capabilities"])
		}
		rows, err := u.pool.Query(context.Background(), "SELECT token FROM devices")
		if err != nil {
			t.Fatal(err)
		}
		tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(tokens) < 3 {
			t.Fatalf("%d tokens are stored (%v): the check would prove little", len(tokens), err)
		}
		for _, token := range tokens {
			if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(token)) {
				t.Errorf("GET /Devices holds an access token")
			}
		}
	})
}
