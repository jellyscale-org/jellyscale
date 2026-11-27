//go:build integration

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// JS-033: the Policy (criteria 7, 8 and 18; D-093, D-094, D-020).

// A change made through one process acts on the next request through another one.
func TestAdministratorAndDisabledChangesActOnTheNextRequestThroughAnotherProcess(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer := u.member(true), u.member(false)
	u.check(403, u.as(1, viewer.token, "GET", "/Auth/Providers", ""), "before the promotion")
	u.setPolicy(admin, viewer.id, map[string]any{"IsAdministrator": true})
	u.check(200, u.as(1, viewer.token, "GET", "/Auth/Providers", ""), "the next request after the promotion, through the other process")
	u.setPolicy(admin, viewer.id, map[string]any{"IsAdministrator": false})
	u.check(403, u.as(1, viewer.token, "GET", "/Auth/Providers", ""), "a demoted administrator on an administrator route")
	u.check(200, u.as(1, viewer.token, "GET", "/Users/Me", ""), "the demoted user's token still works")
	u.setPolicy(admin, viewer.id, map[string]any{"IsDisabled": true})
	u.check(401, u.as(1, viewer.token, "GET", "/Users/Me", ""), "the next request after the disable, through the other process")
	u.setPolicy(admin, viewer.id, map[string]any{"IsDisabled": false})
	u.check(401, u.as(1, viewer.token, "GET", "/Users/Me", ""), "an old token after re-enabling")
	signedIn(t, "the right password after re-enabling", u.try(1, newDevice(), viewer.name, rightPassword))
}

// canDownload is what the item says to the token. (A listing never carries the field.)
func (u *usersSite) canDownload(token string, film usersFilm) any {
	u.t.Helper()
	var item map[string]any
	if err := json.Unmarshal(u.must(200, 1, token, "GET", "/Items/"+film.item, "", "the item").body, &item); err != nil {
		u.t.Fatal(err)
	}
	return item["CanDownload"]
}

// The wrong builds: the shared items.dto rewritten (the other user loses the button), or the flag read when the token
// was issued.
func TestWithContentDownloadingOffThatUserCannotDownloadAndOthersStillCan(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer, other := u.member(true), u.member(false), u.member(false)
	film := u.film(4096)
	stream := start(t, "stream", u.dsn, map[string]any{"adminUser": ""})
	download := "/Items/" + film.item + "/Download"
	u.check(200, procGet(stream, viewer.token, download), "a download before")
	u.setPolicy(admin, viewer.id, map[string]any{"EnableContentDownloading": false})
	u.wantPolicy(admin, viewer.id, "after the change", map[string]any{"EnableContentDownloading": false})
	u.check(403, procGet(stream, viewer.token, download), "a download through stream by the user who may not")
	if got := u.canDownload(viewer.token, film); got != false {
		t.Errorf("that user's item says CanDownload %v, want false", got)
	}
	u.check(200, procGet(stream, other.token, download), "another user's download")
	if got := u.canDownload(other.token, film); got != true {
		t.Errorf("another user's item says CanDownload %v, want true", got)
	}
	if got := rangeStatus(stream, film, viewer.token); got != 206 {
		t.Errorf("that user's playback: %d, want 206", got)
	}
	u.setPolicy(admin, viewer.id, map[string]any{"EnableContentDownloading": true})
	u.check(200, procGet(stream, viewer.token, download), "a download once allowed again")
	if got := u.canDownload(viewer.token, film); got != true {
		t.Errorf("once allowed again the item says CanDownload %v, want true", got)
	}
}

func TestGetsOwnPolicyPostedBackAnswers204AndChangesNothing(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, viewer := u.member(true), u.member(false)
	for _, who := range []qcAccount{admin, viewer} {
		before := u.storedPolicy(admin, who.id)
		u.check(204, u.policy(0, admin.token, who.id, before), "GET's own Policy posted back")
		after, _ := json.Marshal(u.storedPolicy(admin, who.id))
		want, _ := json.Marshal(before)
		if string(after) != string(want) {
			t.Errorf("the Policy changed: %s, was %s", after, want)
		}
		if before["LoginAttemptsBeforeLockout"] != float64(-1) {
			t.Errorf("LoginAttemptsBeforeLockout is %v, want -1", before["LoginAttemptsBeforeLockout"])
		}
	}
}

// jellyfin-web's access page posts null for the two empty lists, its new-user page with "all libraries" ticked posts
// EnableAllChannels false, and no channels exist, so no value of the three channel fields changes anything.
func TestNullForAnEmptyListAndAnyChannelFieldAreNotChanges(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	policy := u.storedPolicy(admin, viewer.id)
	policy["BlockedMediaFolders"], policy["BlockedChannels"] = nil, nil
	u.check(204, u.policy(0, admin.token, viewer.id, policy), "null where GET answers []")
	policy["EnableAllChannels"], policy["EnabledChannels"], policy["BlockedChannels"] = false, []string{"a-channel"}, []string{"another"}
	u.check(204, u.policy(0, admin.token, viewer.id, policy), "other values of the channel fields")
}

// The wrong builds: only the first field named, or the enforced field in the same body stored all the same.
func TestAChangeThatIsNotEnforcedIs400NamingEveryFieldStoresNothingAndIsLogged(t *testing.T) {
	u := newUsersSite(t, 1, nil)
	admin, viewer := u.member(true), u.member(false)
	policy := u.storedPolicy(admin, viewer.id)
	policy["IsAdministrator"] = true
	policy["EnableMediaPlayback"] = false
	policy["EnableAllFolders"] = false
	policy["MaxActiveSessions"] = 3
	got := u.policy(0, admin.token, viewer.id, policy)
	u.check(400, got, "three unenforced changes beside an enforced one")
	for _, field := range []string{"EnableMediaPlayback", "EnableAllFolders", "MaxActiveSessions"} {
		if !strings.Contains(string(got.body), field) {
			t.Errorf("the 400 does not name %s: %q", field, got.body)
		}
	}
	u.wantPolicy(admin, viewer.id, "after the 400", map[string]any{"IsAdministrator": false, "EnableMediaPlayback": true, "EnableAllFolders": true, "MaxActiveSessions": float64(0)})
	if lines := u.logLines("WARN", viewer.name, "EnableMediaPlayback", "EnableAllFolders", "MaxActiveSessions"); len(lines) == 0 {
		byID := u.logLines("WARN", viewer.id, "EnableMediaPlayback", "EnableAllFolders", "MaxActiveSessions")
		byCompactID := u.logLines("WARN", strings.ReplaceAll(viewer.id, "-", ""), "EnableMediaPlayback", "EnableAllFolders", "MaxActiveSessions")
		if len(byID)+len(byCompactID) == 0 {
			t.Errorf("no warn line names the user and the three fields")
		}
	}
	for field, value := range map[string]any{"LoginAttemptsBeforeLockout": 3, "InvalidLoginAttemptCount": 2} {
		got := u.policy(0, admin.token, viewer.id, map[string]any{field: value})
		u.check(400, got, field+" changed from what GET answers")
		if !strings.Contains(string(got.body), field) {
			t.Errorf("the 400 does not name %s: %q", field, got.body)
		}
	}
	u.wantPolicy(admin, viewer.id, "the lockout fields", map[string]any{"LoginAttemptsBeforeLockout": float64(-1), "IsDisabled": false})
}

// D-020: failed sign-ins disable nobody.
func TestFailedSignInsNeverDisableAnAccount(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin := u.member(true)
	viewer := u.member(false)
	u.failN(viewer.name, 12)
	u.wantPolicy(admin, viewer.id, "after 12 wrong passwords", map[string]any{"IsDisabled": false, "LoginAttemptsBeforeLockout": float64(-1)})
	u.check(200, u.as(1, viewer.token, "GET", "/Users/Me", ""), "the account's token after them")
}

// D-094: a Policy changes only what its body names. Jellyfin resets a missing IsAdministrator, IsDisabled and
// EnableContentDownloading, and requires the two provider ids.
func TestAPolicyBodyChangesOnlyTheFieldsItNames(t *testing.T) {
	u := newUsersSite(t, 2, nil)
	admin, target := u.member(true), u.member(false)
	u.setPolicy(admin, target.id, map[string]any{"IsAdministrator": true, "EnableContentDownloading": false})
	u.check(204, u.policy(0, admin.token, target.id, map[string]any{}), "an empty body")
	u.wantPolicy(admin, target.id, "after an empty body", map[string]any{"IsAdministrator": true, "IsDisabled": false, "EnableContentDownloading": false})
	u.setPolicy(admin, target.id, map[string]any{"IsAdministrator": false})
	u.wantPolicy(admin, target.id, "after IsAdministrator alone", map[string]any{"IsAdministrator": false, "IsDisabled": false, "EnableContentDownloading": false})
	u.setPolicy(admin, target.id, map[string]any{"IsDisabled": true})
	u.setPolicy(admin, target.id, map[string]any{"EnableContentDownloading": true})
	u.wantPolicy(admin, target.id, "after EnableContentDownloading alone", map[string]any{"IsAdministrator": false, "IsDisabled": true, "EnableContentDownloading": true})
	full := u.storedPolicy(admin, target.id)
	delete(full, "AuthenticationProviderId")
	delete(full, "PasswordResetProviderId")
	delete(full, "IsDisabled")
	u.check(204, u.policy(0, admin.token, target.id, full), "a full body without the provider ids and IsDisabled")
	u.wantPolicy(admin, target.id, "after it", map[string]any{"IsDisabled": true})
}
