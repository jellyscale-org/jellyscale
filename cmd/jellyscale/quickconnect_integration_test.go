//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// JS-037, Quick Connect, with real api processes over Postgres. The statuses and fields Jellyfin 12.4 answers are
// compat/captures/quick-connect-12.4.json's, which quickconnect_capture_integration_test.go asks jellyscale too. Time
// passes by moving the stored times back (qcSite.pass).

const (
	// A code not approved is gone this long after Initiate.
	qcPendingLife = 10 * time.Minute
	// An approved code can be redeemed for this long after its approval: Jellyfin 12.4 did at 9 minutes 49 seconds
	// and did not at 10 minutes 11, however long after Initiate the approval came (the capture's "expiry" rows).
	qcApprovedLife = 10 * time.Minute
	// qcCap is the most codes that may be live at once (D-079).
	qcCap = 100
)

// Criterion 5. Every step is answered by a process that was not there for the step before, which was killed (no
// goodbye) in between: no code, approval or token is in a process.
func TestAQuickConnectSignInSurvivesEveryReplicaBeingKilledBetweenSteps(t *testing.T) {
	q := newQCSite(t, 1, nil)
	viewer := q.member(false)
	at := 0
	next := func() int {
		t.Helper()
		i := q.add(nil)
		q.apis[at].kill()
		at = i
		return i
	}
	code := q.start(at, newTV())
	q.mustBePending(next(), code, "the first Connect, after the replica that initiated was killed")
	q.approve(next(), viewer, code)
	if status, approved := q.authenticated(next(), code); status != 200 || !approved {
		t.Fatalf("Connect after the replica that approved was killed: %d Authenticated %v, want it approved", status, approved)
	}
	signed := q.redeemed(next(), code)
	if got := q.me(next(), signed.AccessToken); got != 200 {
		t.Fatalf("the token after the replica that gave it was killed: GET /Users/Me %d, want 200", got)
	}
	if signed.User.Name != viewer.name {
		t.Errorf("signed in as %q, want %q", signed.User.Name, viewer.name)
	}
}

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

// Criterion 6. A code is six decimal digits; the secret alone reads a code's state and redeems it; the code gives no
// token anywhere; Connect never carries one. That no code is live for two devices is
// internal/api/quickconnect_unique_integration_test.go's, which forces the collision.
func TestOnlyTheSecretReadsOrRedeemsASixDigitCode(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	var code qcCode
	// A hundred, so that a build that drops a code's leading zeros (one code in ten) shows.
	for i := range qcCap {
		code = q.start(i, newTV())
		if !sixDigits.MatchString(code.Code) {
			t.Fatalf("code %q is not six decimal digits", code.Code)
		}
	}
	pendingConnect := q.connectSecret(0, code.Secret)
	q.approve(1, viewer, code)
	for what, got := range map[string]signInReply{
		"Connect with the code as the secret":    q.connectSecret(0, code.Code),
		"Connect with the code and no secret":    q.call(1, "GET", "/QuickConnect/Connect?Code="+code.Code, "", "Authorization", code.device.header("")),
		"redemption with the code as the secret": q.redeem(0, code.device, code.Code),
		"redemption with the code and no secret": q.call(1, "POST", "/Users/AuthenticateWithQuickConnect", `{"Code":"`+code.Code+`"}`, "Authorization", code.device.header("")),
	} {
		if got.status/100 != 4 || strings.Contains(string(got.body), "AccessToken") || strings.Contains(string(got.body), code.Secret) {
			t.Errorf("%s: %d %q, want a refusal that tells nothing", what, got.status, got.body)
		}
	}
	approvedConnect := q.connectSecret(0, code.Secret)
	signed := q.redeemed(1, code)
	after := q.connectSecret(0, code.Secret)
	for what, got := range map[string]signInReply{"pending": pendingConnect, "approved": approvedConnect, "redeemed": after} {
		var fields map[string]any
		json.Unmarshal(got.body, &fields) //nolint:errcheck // A refusal after redemption need be no object.
		for name := range fields {
			if strings.Contains(strings.ToLower(name), "token") || strings.EqualFold(name, "User") {
				t.Errorf("Connect (%s) has the field %q", what, name)
			}
		}
		if strings.Contains(string(got.body), signed.AccessToken) || strings.Contains(string(got.body), viewer.token) {
			t.Errorf("Connect (%s) carries a token: %q", what, got.body)
		}
	}
}

// qcUnknown is what each route answers for a code and a secret nobody was given.
type qcUnknown struct {
	approve, connect, redeem signInReply
}

func (q *qcSite) unknown(approver qcAccount) qcUnknown {
	return qcUnknown{
		approve: q.authorize(0, approver.token, "000000", ""),
		connect: q.connectSecret(0, "0000deadbeef"),
		redeem:  q.redeem(0, newTV(), "0000deadbeef"),
	}
}

func (q *qcSite) wantUnknownSecret(code qcCode, unknown qcUnknown, what string) {
	q.t.Helper()
	for route, got := range map[string][2]signInReply{
		"Connect":    {q.connectSecret(0, code.Secret), unknown.connect},
		"redemption": {q.redeem(1, code.device, code.Secret), unknown.redeem},
	} {
		if got[0].status != got[1].status || string(got[0].body) != string(got[1].body) {
			q.t.Errorf("%s: %s answers %d %q, want the unknown secret's %d %q", what, route, got[0].status, got[0].body, got[1].status, got[1].body)
		}
	}
}

// Criterion 7, first half. Nine minutes and fifty seconds after Initiate a pending code is there and can be approved; ten
// minutes and a second after, it is gone for approval, Connect and redemption.
func TestACodeNotApprovedWithinTenMinutesIsGone(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer, checker := q.member(false), q.member(false)
	early, late := q.start(0, newTV()), q.start(1, newTV())
	q.pass(qcPendingLife - 10*time.Second)
	q.mustBePending(1, early, "9m50s after Initiate")
	q.approve(0, viewer, early)
	q.redeemed(1, early)
	q.pass(11 * time.Second)
	unknown := q.unknown(checker)
	if got := q.authorize(1, viewer.token, late.Code, ""); got.status != unknown.approve.status || got.status/100 != 4 {
		t.Errorf("approving a code 10m01s old: %d %q, want what an unknown code gets, %d", got.status, got.body, unknown.approve.status)
	}
	q.wantUnknownSecret(late, unknown, "10m01s after Initiate")
}

// Criterion 7, second half. The window runs from the approval, however late it came, and ends for good: a code redeemed
// in it is not redeemable after it either. After it, redemption answers as for an unknown secret. (What Connect says of
// an approved code past the window is not held: Jellyfin 12.4 knows it a minute longer.)
func TestAnApprovedCodeIsRedeemedWithinTheWindowAfterApprovalAndNeverLater(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	soon, late := q.start(0, newTV()), q.start(1, newTV())
	atOnce, atOnceLate := q.start(0, newTV()), q.start(1, newTV())
	q.approve(0, viewer, atOnce)
	q.approve(1, viewer, atOnceLate)
	// Approved 9 minutes after Initiate.
	q.pass(9 * time.Minute)
	q.approve(0, viewer, soon)
	q.approve(1, viewer, late)
	q.pass(qcApprovedLife - 9*time.Minute - 10*time.Second) // 9m50s after atOnce's approval.
	q.redeemed(0, atOnce)
	q.pass(11 * time.Second) // 10m01s after atOnce's approval; soon and late were approved 1m01s ago.
	unknown := q.redeem(0, newTV(), "0000deadbeef")
	gone := func(code qcCode, what string) {
		t.Helper()
		if got := q.redeem(1, code.device, code.Secret); got.status != 404 || got.status != unknown.status || string(got.body) != string(unknown.body) {
			t.Errorf("redemption %s: %d %q, want the unknown secret's %d %q", what, got.status, got.body, unknown.status, unknown.body)
		}
	}
	gone(atOnceLate, "10m01s after approval")
	gone(atOnce, "10m01s after approval of a code redeemed before")
	q.pass(qcApprovedLife - time.Minute - 11*time.Second) // 9m50s after the late approvals, 18m50s after their Initiate.
	q.redeemed(0, soon)
	q.pass(11 * time.Second)
	gone(late, "10m01s after a late approval")
}

// Criterion 8. "Not yet" is what Jellyfin answers a pending code's redemption (404, as an unknown secret), and the code
// is still to be approved.
func TestRedeemingBeforeApprovalAnswersNotYetAndTheCodeCanStillBeApproved(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	code := q.start(0, newTV())
	if got := q.redeem(1, code.device, code.Secret); got.status != 404 {
		t.Fatalf("redeeming a pending code: %d %q, want 404", got.status, got.body)
	}
	q.mustBePending(0, code, "after the early redemption")
	q.approve(1, viewer, code)
	q.redeemed(0, code)
}

// Criterion 8, at once: approval on one replica and redemption on another meet in Postgres (qcSite.together). The device
// gets "not yet" or its token, never anything else, and after "not yet" its next redemption gets the token.
func TestApprovalAndRedemptionAtOnceOnTwoReplicasGiveNotYetOrTheToken(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	for round := range 8 {
		code := q.start(round, newTV())
		replies := q.together([]int{0, 1},
			func() signInReply { return q.authorize(0, viewer.token, code.Code, "") },
			func() signInReply { return q.redeem(1, code.device, code.Secret) })
		approval, redemption := replies[0], replies[1]
		if approval.status != 200 {
			t.Fatalf("round %d: the approval answered %d %q, want 200", round, approval.status, approval.body)
		}
		var signed qcSignIn
		switch {
		case redemption.status == 404:
			signed = q.redeemed(round, code)
		case redemption.status == 200:
			if err := json.Unmarshal(redemption.body, &signed); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("round %d: the redemption answered %d %q, want 404 (not yet) or the sign-in", round, redemption.status, redemption.body)
		}
		if got := q.me(round, signed.AccessToken); got != 200 {
			t.Fatalf("round %d: the token the device got answers %d", round, got)
		}
	}
}

// qcAlreadyApproved is what approving an already approved code answers. Jellyfin 12.4 answers 500 (the capture);
// the named exceptions held in the replay test lists the difference, and jellyfin-web shows both alike
// (compat/captures/quick-connect-second-approval-12.4.md).
const qcAlreadyApproved = 409

// Criterion 9. Two accounts approve one code at once on two replicas: one is told yes and the device is signed in to it;
// the other is refused 409, and so is a third that comes later, and neither changes anything.
func TestTwoAccountsApprovingOneCodeAtOnceSignTheDeviceInToExactlyOne(t *testing.T) {
	q := newQCSite(t, 2, nil)
	for round := range 4 {
		// New accounts each round: a refused approval counts toward D-078's bound.
		accounts := []qcAccount{q.member(false), q.member(false)}
		later := q.member(round%2 == 1)
		code := q.start(round, newTV())
		replies := q.together([]int{0, 1},
			func() signInReply { return q.authorize(0, accounts[0].token, code.Code, "") },
			func() signInReply { return q.authorize(1, accounts[1].token, code.Code, "") })
		winner, loser := 0, 1
		if replies[1].status == 200 {
			winner, loser = 1, 0
		}
		if replies[winner].status != 200 || replies[loser].status != qcAlreadyApproved {
			t.Fatalf("round %d: the two approvals answered %s, want one 200 and one %d", round, statuses(replies), qcAlreadyApproved)
		}
		for _, again := range []qcAccount{later, accounts[loser]} {
			if got := q.authorize(round+1, again.token, code.Code, ""); got.status != qcAlreadyApproved {
				t.Errorf("round %d: a later approval of the approved code: %d %q, want %d as the loser got", round, got.status, got.body, qcAlreadyApproved)
			}
		}
		if got := q.redeemed(round, code); got.User.Name != accounts[winner].name {
			t.Errorf("round %d: signed in as %q, want %q whose approval was accepted", round, got.User.Name, accounts[winner].name)
		}
		if got := q.authorize(round, later.token, code.Code, ""); got.status != qcAlreadyApproved {
			t.Errorf("round %d: approving the redeemed code: %d %q, want %d", round, got.status, got.body, qcAlreadyApproved)
		}
		if n := q.deviceTokens(code.device, accounts[loser]) + q.deviceTokens(code.device, later); n != 0 {
			t.Errorf("round %d: the device holds %d tokens of an account whose approval was refused", round, n)
		}
	}
}

// liveOf is how many of the tokens are accepted, each distinct token counted once.
func (q *qcSite) liveOf(tokens []string) int {
	seen, live := map[string]bool{}, 0
	for _, token := range tokens {
		if token != "" && !seen[token] && q.me(0, token) == 200 {
			live++
		}
		seen[token] = true
	}
	return live
}

// Criterion 10. One secret redeemed twice, one after the other or at once on two replicas, leaves the device one live
// token and no more, one it was told.
//
// What it asks of a build's design: a token is made only through the code's row, as in
// UPDATE <code's row> SET token = coalesce(token, $new) ... RETURNING token. The row decides the one token, a second
// redemption is given that same token, and there is no other path that makes one: devices is written only by whoever
// set the row's token (it got $new back). A build that reads the code with a plain SELECT and then deletes and inserts
// in devices fails here, because together holds devices until both redemptions have read the code.
func TestOneSecretRedeemedTwiceNeverLeavesTheDeviceTwoLiveTokens(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	check := func(what string, code qcCode, replies []signInReply) {
		t.Helper()
		var tokens []string
		signedIns := 0
		for _, reply := range replies {
			var signed qcSignIn
			switch {
			case reply.status == 200 && json.Unmarshal(reply.body, &signed) == nil && signed.AccessToken != "":
				tokens = append(tokens, signed.AccessToken)
				signedIns++
			case reply.status/100 != 4:
				t.Errorf("%s: a redemption answered %d %q, want a sign-in or a refusal", what, reply.status, reply.body)
			}
		}
		if live := q.liveOf(tokens); signedIns == 0 || live != 1 {
			t.Errorf("%s: %d redemptions signed in and %d of their tokens are accepted, want at least one and exactly 1", what, signedIns, live)
		}
		if n := q.deviceTokens(code.device, viewer); n != 1 {
			t.Errorf("%s: the database holds %d tokens for the device, want 1", what, n)
		}
	}
	code := q.start(0, newTV())
	q.approve(0, viewer, code)
	check("one after the other", code, []signInReply{q.redeem(0, code.device, code.Secret), q.redeem(1, code.device, code.Secret)})
	for round := range 6 {
		code := q.start(round, newTV())
		q.approve(round, viewer, code)
		check("at once", code, q.together([]int{0, 1},
			func() signInReply { return q.redeem(0, code.device, code.Secret) },
			func() signInReply { return q.redeem(1, code.device, code.Secret) }))
	}
}

// stored is how many rows the quickconnect_* tables hold together, and how many each does.
func (q *qcSite) stored() (int, map[string]int) {
	q.t.Helper()
	total, each := 0, map[string]int{}
	for _, table := range q.quickTables() {
		var n int
		if err := q.pool.QueryRow(context.Background(), `SELECT count(*) FROM "`+table+`"`).Scan(&n); err != nil {
			q.t.Fatal(err)
		}
		total, each[table] = total+n, n
	}
	return total, each
}

// Codes do not pile up: anyone may ask for one. A redeemed code is kept while its secret can still be redeemed, for the
// same token, an Initiate in between or not. Twenty-one minutes later, past both windows, one Initiate leaves the
// quickconnect_* tables holding one row together, its own code's: the expired codes, pending, approved and redeemed,
// are deleted, by that Initiate at the latest. So a code is one row, and nothing else is kept in those tables.
func TestAfterTwentyOneMinutesAnInitiateLeavesOnlyItsOwnCodeStored(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	q.start(0, newTV()) // Left pending.
	unredeemed, redeemed := q.start(1, newTV()), q.start(0, newTV())
	q.approve(0, viewer, unredeemed)
	q.approve(1, viewer, redeemed)
	first := q.redeemed(0, redeemed)
	q.start(1, newTV())
	if again := q.redeemed(1, redeemed); again.AccessToken != first.AccessToken {
		t.Fatal("redeeming again, after another device's Initiate, gave another token")
	}
	q.pass(21 * time.Minute)
	fresh := q.start(0, newTV())
	if total, each := q.stored(); total != 1 {
		t.Errorf("21 minutes after four codes, one Initiate leaves %d rows in the quickconnect_* tables, want 1: %v", total, each)
	}
	q.mustBePending(1, fresh, "the code of the Initiate that cleared the others")
}

func dashed(id string) string {
	compact := strings.ReplaceAll(id, "-", "")
	return compact[:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:]
}

// Criterion 11. An approval is for the approver's own account, administrators included: naming themselves, in any
// spelling, no userId or an empty one. Naming anyone else is 403 and the code stays pending.
func TestAnApprovalIsAlwaysForTheApproversOwnAccount(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer, admin, other := q.member(false), q.member(true), q.member(false)
	compact := func(who qcAccount) string { return strings.ReplaceAll(who.id, "-", "") }
	for i, approver := range []qcAccount{viewer, admin} {
		for _, query := range []string{"", "&userId=", "&userId=" + compact(approver), "&userId=" + dashed(approver.id),
			"&userId=" + strings.ToUpper(compact(approver)), "&UserId=" + compact(approver)} {
			code := q.start(i, newTV())
			if got := q.authorize(i+1, approver.token, code.Code, query); got.status != 200 {
				t.Errorf("administrator %v approving with %q: %d %q, want 200", approver.admin, query, got.status, got.body)
				continue
			}
			if got := q.redeemed(i, code); got.User.Name != approver.name {
				t.Errorf("administrator %v approving with %q signed the device in as %q", approver.admin, query, got.User.Name)
			}
		}
	}
	for _, approver := range []qcAccount{viewer, admin} {
		code := q.start(0, newTV())
		for what, name := range map[string]string{"another account": compact(other), "another account, dashed": dashed(other.id),
			"nobody's id": "11111111111111111111111111111111"} {
			got := q.authorize(1, approver.token, code.Code, "&userId="+name)
			if got.status != 403 || got.header.Get("Retry-After") != "" {
				t.Errorf("administrator %v naming %s: %d %q (Retry-After %q), want 403 that is no refusal of the bound", approver.admin, what, got.status, got.body, got.header.Get("Retry-After"))
			}
			q.mustBePending(0, code, "after naming "+what)
		}
		q.approve(1, approver, code)
		if got := q.redeemed(0, code); got.User.Name != approver.name {
			t.Errorf("after refused namings the device is signed in as %q, want %q", got.User.Name, approver.name)
		}
	}
	if got := q.me(0, other.token); got != 200 {
		t.Errorf("the named account's token: %d", got)
	}
}

// Criterion 12. The token is the asking device's, as /Sessions shows it, whatever the redemption's own header says; it
// works on api and stream replicas as a password sign-in's does; the approver's own token goes on.
func TestTheTokenBelongsToTheDeviceThatAskedForTheCode(t *testing.T) {
	q := newQCSite(t, 1, nil)
	stream := start(t, "stream", q.dsn, nil)
	viewer, admin := q.member(false), q.member(true)
	asker, other := newTV(), newTV()
	code := q.start(0, asker)
	q.approve(0, viewer, code)
	got := q.redeem(0, other, code.Secret)
	var signed qcSignIn
	if got.status != 200 || json.Unmarshal(got.body, &signed) != nil || signed.AccessToken == "" {
		t.Fatalf("redemption with another device's header: %d %q", got.status, got.body)
	}
	want := [4]string{asker.id, asker.name, asker.app, asker.version}
	info := signed.SessionInfo
	if answered := [4]string{info.DeviceId, info.DeviceName, info.Client, info.ApplicationVersion}; answered != want || info.UserName != viewer.name {
		t.Errorf("the sign-in's SessionInfo: %v of %q, want %v of %q", answered, info.UserName, want, viewer.name)
	}
	if status := q.me(0, signed.AccessToken); status != 200 {
		t.Fatalf("the token on the api replica: %d", status)
	}
	onStream := func(token string) int {
		request := &qcSite{signInSite: &signInSite{t: t, apis: []*proc{stream}}}
		return request.call(0, "GET", "/System/Configuration/encoding", "", "X-Emby-Token", token).status
	}
	if status := onStream(signed.AccessToken); status != 200 {
		t.Errorf("the token on the stream replica: %d, want 200", status)
	}
	// jellyscale lists a session once its device has reported (as for a password sign-in).
	if ping := q.call(0, "POST", "/Sessions/Playing/Ping", "", "X-Emby-Token", signed.AccessToken); ping.status != 204 {
		t.Fatalf("the new token's ping: %d %q", ping.status, ping.body)
	}
	sessions := q.call(0, "GET", "/Sessions", "", "X-Emby-Token", admin.token)
	var rows []struct{ DeviceId, DeviceName, Client, ApplicationVersion, UserName string }
	if sessions.status != 200 || json.Unmarshal(sessions.body, &rows) != nil {
		t.Fatalf("/Sessions: %d %q", sessions.status, sessions.body)
	}
	found := 0
	for _, row := range rows {
		if row.DeviceId == other.id {
			t.Errorf("/Sessions shows the device whose header came with the redemption: %+v", row)
		}
		if row.DeviceId == asker.id {
			found++
			if shown := [4]string{row.DeviceId, row.DeviceName, row.Client, row.ApplicationVersion}; shown != want || row.UserName != viewer.name {
				t.Errorf("/Sessions shows %v of %q, want %v of %q", shown, row.UserName, want, viewer.name)
			}
		}
	}
	if found != 1 {
		t.Errorf("/Sessions has %d rows for the asking device, want 1", found)
	}
	if status := q.me(0, viewer.token); status != 200 {
		t.Errorf("the approver's own token: %d, want 200", status)
	}
	q.logout(0, signed.AccessToken)
	if api, media := q.me(0, signed.AccessToken), onStream(signed.AccessToken); api != 401 || media != 401 {
		t.Errorf("after signing out: api %d and stream %d, want 401", api, media)
	}
}

// Criterion 13. As a password sign-in on that device does: the device's old token of the approver's account is replaced,
// its token of another account is kept.
func TestACodeSignInReplacesTheDevicesOldTokenOfThatAccountOnly(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer, other := q.member(false), q.member(false)
	tv := newTV()
	mine := signedIn(t, "the viewer's password on the TV", q.try(0, tv.id, viewer.name, rightPassword))
	theirs := signedIn(t, "another account's password on the TV", q.try(1, tv.id, other.name, rightPassword))
	code := q.start(0, tv)
	q.approve(1, viewer, code)
	fresh := q.redeemed(0, code).AccessToken
	for what, check := range map[string][2]any{
		"the device's old token of the approver's account": {mine, 401},
		"the device's token of another account":            {theirs, 200},
		"the new token":                                    {fresh, 200},
	} {
		if got := q.me(1, check[0].(string)); got != check[1] {
			t.Errorf("%s: GET /Users/Me %d, want %v", what, got, check[1])
		}
	}
}

// Criterion 14. Initiate without a device field is 400, as Jellyfin refuses it, and stores nothing: 144 of them leave
// room for all 100 live codes.
func TestInitiateWithoutADeviceFieldIsRefusedAndStoresNothing(t *testing.T) {
	q := newQCSite(t, 2, nil)
	fields := []string{"DeviceId", "Device", "Client", "Version"}
	headers := []string{""}
	for _, missing := range fields {
		var without, empty []string
		for _, field := range fields {
			if field != missing {
				without = append(without, field+`="x"`)
				empty = append(empty, field+`="x"`)
				continue
			}
			empty = append(empty, field+`=""`)
		}
		headers = append(headers, "MediaBrowser "+strings.Join(without, ", "), "MediaBrowser "+strings.Join(empty, ", "))
	}
	for round := range 16 {
		for n, header := range headers {
			args := []string{}
			if header != "" {
				args = []string{"Authorization", header}
			}
			if got := q.call(round+n, "POST", "/QuickConnect/Initiate", "", args...); got.status != 400 {
				t.Fatalf("Initiate with %q: %d %q, want 400", header, got.status, got.body)
			}
		}
	}
	for i := range qcCap {
		q.start(i, newTV())
	}
}

// Criterion 14. Initiate, Connect and redemption read no token: an unknown, a signed-out or another account's token, in
// any spelling, changes neither the answers nor whose the device becomes.
func TestInitiateConnectAndRedemptionReadNoToken(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer, bystander := q.member(false), q.member(false)
	revoked := signedIn(t, "a token to sign out", q.try(0, newDevice(), viewer.name, rightPassword))
	q.logout(0, revoked)
	for kind, token := range map[string]string{"unknown": "0123456789abcdef0123456789abcdef", "signed out": revoked, "another account's": bystander.token} {
		for _, spelling := range []string{"Authorization", "X-Emby-Token", "ApiKey"} {
			what := kind + " token as " + spelling
			tv := newTV()
			query, headers := "", []string{"Authorization", tv.header("")}
			switch spelling {
			case "Authorization":
				headers = []string{"Authorization", tv.header(token)}
			case "X-Emby-Token":
				headers = append(headers, "X-Emby-Token", token)
			case "ApiKey":
				query = "ApiKey=" + url.QueryEscape(token)
			}
			got := q.call(0, "POST", "/QuickConnect/Initiate?"+query, "", headers...)
			code := qcCode{device: tv}
			if got.status != 200 || json.Unmarshal(got.body, &code) != nil || code.Secret == "" {
				t.Fatalf("Initiate with an %s: %d %q, want a code", what, got.status, got.body)
			}
			if got := q.call(1, "GET", "/QuickConnect/Connect?Secret="+code.Secret+"&"+query, "", headers...); got.status != 200 {
				t.Errorf("Connect with an %s: %d %q", what, got.status, got.body)
			}
			q.approve(0, viewer, code)
			redeemed := q.call(1, "POST", "/Users/AuthenticateWithQuickConnect?"+query, `{"Secret":"`+code.Secret+`"}`, headers...)
			var signed qcSignIn
			if redeemed.status != 200 || json.Unmarshal(redeemed.body, &signed) != nil || signed.User.Name != viewer.name || signed.SessionInfo.DeviceId != tv.id {
				t.Errorf("redemption with an %s: %d %q, want the approver's sign-in for the asking device", what, redeemed.status, redeemed.body)
			}
		}
	}
	if a, b := q.me(0, bystander.token), q.me(0, revoked); a != 200 || b != 401 {
		t.Errorf("the tokens that were sent along answer %d and %d, want 200 and still 401", a, b)
	}
}

// Criterion 15. An account deleted after approving redeems nothing: the device gets what an unknown secret gets.
func TestAnAccountDeletedAfterApprovingRedeemsNothing(t *testing.T) {
	q := newQCSite(t, 2, nil)
	leaver, checker := q.member(false), q.member(false)
	code := q.start(0, newTV())
	q.approve(0, leaver, code)
	if _, err := q.pool.Exec(t.Context(), "DELETE FROM users WHERE id = $1", leaver.id); err != nil {
		t.Fatal(err)
	}
	unknown := q.unknown(checker)
	got := q.redeem(1, code.device, code.Secret)
	if got.status != unknown.redeem.status || got.status != 404 || string(got.body) != string(unknown.redeem.body) {
		t.Errorf("redemption after the approver was deleted: %d %q, want the unknown secret's %d %q", got.status, got.body, unknown.redeem.status, unknown.redeem.body)
	}
	if n := q.deviceTokens(code.device, leaver); n != 0 {
		t.Errorf("the device holds %d tokens of the deleted account", n)
	}
}

// failApprovals makes n failed approvals by the account, each of an unknown code, and none may be refused by the bound.
func (q *qcSite) failApprovals(who qcAccount, n int) {
	q.t.Helper()
	for i := range n {
		got := q.authorize(i, who.token, "000000", "")
		if got.status/100 != 4 || got.status == 401 || got.status == 403 {
			q.t.Fatalf("failed approval %d of %d: %d %q, want the unknown code's answer", i+1, n, got.status, got.body)
		}
	}
}

// Criteria 16 and 21 (D-078). Ten failures, of every kind, and the account's approvals are refused for 15 minutes with
// 403 and Retry-After: no code is checked, and nothing else of the account is touched. Each refusal is logged with the
// account's name and a bound or cap and no secret, and counted.
func TestTenFailedApprovalsRefuseThatAccountsApprovalsAndNothingElse(t *testing.T) {
	for _, admin := range []bool{false, true} {
		q := newQCSite(t, 2, nil)
		guesser, neighbour := q.member(admin), q.member(false)
		expired := q.start(0, newTV())
		q.pass(qcPendingLife + time.Second)
		taken := q.start(0, newTV())
		q.approve(0, neighbour, taken)
		// An expired code answers as an unknown one does in Jellyfin 12.4, 404 (the capture).
		for _, guess := range []struct {
			code string
			want int
		}{{expired.Code, 404}, {taken.Code, qcAlreadyApproved}} {
			if got := q.authorize(1, guesser.token, guess.code, ""); got.status != guess.want {
				t.Fatalf("a failed approval: %d %q, want %d", got.status, got.body, guess.want)
			}
		}
		q.failApprovals(guesser, 8)
		valid, redeemable := q.start(0, newTV()), q.start(1, newTV())
		q.approve(0, neighbour, redeemable)
		before := q.series()
		const refusals = 3
		for i := range refusals {
			got := q.authorize(i, guesser.token, valid.Code, "")
			refusedWithin(t, "approval after 10 failures", got, 890, 900)
		}
		if t.Failed() {
			return
		}
		q.mustBePending(1, valid, "after the refused approvals")
		q.wantRefusalsCounted(before, refusals, "the guessing bound")
		if lines := q.refusalLines(guesser.name); lines < refusals {
			t.Errorf("%d log lines name the account and a bound or cap, want one for each of %d refusals", lines, refusals)
		}
		// What the refusal leaves alone.
		if got := q.me(0, guesser.token); got != 200 {
			t.Errorf("the refused account's token: %d", got)
		}
		if ping := q.call(1, "POST", "/Sessions/Playing/Ping", "", "X-Emby-Token", guesser.token); ping.status != 204 {
			t.Errorf("the refused account's playback ping: %d %q", ping.status, ping.body)
		}
		signedIn(t, "the refused account's password sign-in", q.try(0, newDevice(), guesser.name, rightPassword))
		if status, approved := q.authenticated(0, redeemable); status != 200 || !approved {
			t.Errorf("Connect while an account is refused: %d %v", status, approved)
		}
		signed := q.redeemed(1, redeemable)
		q.approve(1, neighbour, valid) // The bound is the account's own.
		q.wantNoSecretLogged(expired.Secret, taken.Secret, valid.Secret, redeemable.Secret, signed.AccessToken, guesser.token, neighbour.token)
	}
}

// Criterion 16. A refusal counts toward nothing and does not lengthen itself; it ends by itself 15 minutes after the
// tenth failure, and the count starts again.
func TestARefusedApprovalDoesNotLengthenTheRefusal(t *testing.T) {
	q := newQCSite(t, 2, nil)
	guesser := q.member(false)
	q.failApprovals(guesser, 10)
	for i := range 25 {
		refusedWithin(t, "a further approval", q.authorize(i, guesser.token, "000000", ""), 880, 900)
		if t.Failed() {
			return
		}
	}
	q.pass(600 * time.Second)
	code := q.start(0, newTV())
	refusedWithin(t, "10 minutes on", q.authorize(0, guesser.token, code.Code, ""), 270, 300)
	q.pass(301 * time.Second)
	fresh := q.start(0, newTV())
	q.approve(1, guesser, fresh)
	q.failApprovals(guesser, 10)
	refusedWithin(t, "after 10 more failures", q.authorize(0, guesser.token, "000000", ""), 890, 900)
}

// Criterion 16. The ten are of any 15 minutes, a success between them resets nothing, and an approval naming another
// account is not one of them.
func TestTheApprovalBoundCountsTheLastFifteenMinutes(t *testing.T) {
	q := newQCSite(t, 2, nil)

	sliding := q.member(false)
	q.failApprovals(sliding, 9)
	q.pass(840 * time.Second)
	q.failApprovals(sliding, 1)
	refusedWithin(t, "9 failures, 14 minutes, 1 failure", q.authorize(0, sliding.token, "000000", ""), 890, 900)

	old := q.member(false)
	q.failApprovals(old, 9)
	q.pass(901 * time.Second)
	q.failApprovals(old, 1)
	q.approve(0, old, q.start(0, newTV()))

	mixed := q.member(false)
	q.failApprovals(mixed, 5)
	q.approve(0, mixed, q.start(1, newTV()))
	other, naming := q.member(false), q.start(0, newTV())
	for i := range 12 {
		if got := q.authorize(i, mixed.token, naming.Code, "&userId="+other.id); got.status != 403 || got.header.Get("Retry-After") != "" {
			t.Fatalf("naming another account, time %d: %d (Retry-After %q), want a plain 403", i+1, got.status, got.header.Get("Retry-After"))
		}
	}
	q.failApprovals(mixed, 5)
	refusedWithin(t, "after 5 failures, a success and 5 failures", q.authorize(1, mixed.token, "000000", ""), 890, 900)
}

// Criterion 16. The bound is in Postgres: failures spread over two replicas, one of them killed and replaced, refuse the
// account on both.
func TestTheApprovalBoundHoldsAcrossReplicasAndARestart(t *testing.T) {
	q := newQCSite(t, 2, nil)
	guesser := q.member(false)
	q.failApprovals(guesser, 5)
	q.restart(0)
	q.restart(1)
	q.failApprovals(guesser, 5)
	q.restart(0)
	for i := range 2 {
		refusedWithin(t, "after the replicas restarted", q.authorize(i, guesser.token, "000000", ""), 880, 900)
	}
}

// Criterion 16. Two hundred approvals at once, on two replicas, check no more than ten codes.
func TestTwoHundredApprovalsAtOnceCheckNoMoreThanTenCodes(t *testing.T) {
	q := newQCSite(t, 2, nil)
	guesser := q.member(false)
	got := parallel(200, func(i int) signInReply { return q.authorize(i, guesser.token, "000001", "") })
	checked := 0
	for status, n := range got {
		if status != 403 {
			checked += n
		}
	}
	if got[0] != 0 || got[500] != 0 || got[401] != 0 || checked > 10 || checked == 0 || got[403] != 200-checked {
		t.Errorf("200 approvals at once answered %v, want no more than 10 codes checked and 403 for the rest", got)
	}
}

// Criterion 16, forced: no approval can finish looking its code up while twenty are sent on two replicas, so a build
// that looks before it has counted the attempt lets all twenty through. Ten per replica is under a replica's 16
// database connections.
func TestApprovalsHeldAtTheirCodeLookupCheckNoMoreThanTen(t *testing.T) {
	q := newQCSite(t, 2, nil)
	guesser := q.member(false)
	var requests []func() signInReply
	for i := range 20 {
		requests = append(requests, func() signInReply { return q.authorize(i, guesser.token, "000001", "") })
	}
	replies := q.held("ACCESS EXCLUSIVE", []int{0, 1}, requests...)
	checked, refused := 0, 0
	for _, reply := range replies {
		switch {
		case reply.status == 403:
			refused++
		case reply.status/100 == 4 && reply.status != 401:
			checked++
		}
	}
	if checked == 0 || checked > 10 || checked+refused != 20 {
		t.Errorf("20 approvals held at their lookup answered %s, want no more than 10 codes checked and 403 for the rest", statuses(replies))
	}
}

// Criterion 17. Ten devices behind one address poll far past their codes' expiry and nothing is refused; failed polls,
// redemptions and Initiates count toward no bound. Real devices poll every 5 seconds; these ask at once, 150 times each,
// which is 12 and a half minutes of polling and asks more of a bound.
func TestPollingIsNeverRefused(t *testing.T) {
	q := newQCSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	viewer := q.member(false)
	address := behindProxy(randomV4())
	var codes []qcCode
	for i := range 10 {
		codes = append(codes, q.start(i, newTV(), address...))
	}
	poll := func(times int) map[int]int {
		var mu sync.Mutex
		var wg sync.WaitGroup
		answered := map[int]int{}
		for _, code := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range times {
					got := q.call(i, "GET", "/QuickConnect/Connect?Secret="+code.Secret, "", append([]string{"Authorization", code.device.header("")}, address...)...)
					mu.Lock()
					answered[got.status]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return answered
	}
	if got := poll(150); got[200] != 1500 {
		t.Fatalf("1500 polls of pending codes answered %v, want 200 for each", got)
	}
	q.pass(qcPendingLife + time.Minute)
	unknown := q.connectSecret(0, "0000deadbeef").status
	if got := poll(60); got[unknown] != 600 || unknown != 404 {
		t.Errorf("600 polls past the codes' expiry answered %v, want the unknown secret's 404 for each", got)
	}
	before := q.total(failuresMetric)
	for i := range 40 {
		got := q.call(i, "POST", "/Users/AuthenticateWithQuickConnect", `{"Secret":"0000deadbeef"}`, append([]string{"Authorization", newTV().header("")}, address...)...)
		if got.status != 404 {
			t.Fatalf("failed redemption %d: %d %q, want 404", i+1, got.status, got.body)
		}
		if got := q.initiate(i, newTV(), address...); got.status != 200 {
			t.Fatalf("Initiate %d from the address: %d %q, want 200", i+1, got.status, got.body)
		}
	}
	if after := q.total(failuresMetric); after != before {
		t.Errorf("Quick Connect made %v failed sign-ins", after-before)
	}
	signedIn(t, "a password sign-in from the same address", q.try(0, newDevice(), viewer.name, rightPassword, address...))
	q.approve(1, viewer, q.start(0, newTV(), address...))
}

// Criterion 18 (D-020). While the account's name is refused to strangers and the TV's address is refused for passwords,
// the TV signs in by code; from then on it is a device the account knows, with a bound of its own.
func TestThePasswordBoundsNeitherCountNorRefuseQuickConnect(t *testing.T) {
	q := newQCSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	viewer := q.member(false, behindProxy(randomV4())...)
	q.failSpread(viewer.name, 20)
	home := behindProxy(randomV4())
	for range 10 {
		if got := q.wrong(0, invented(), home...); got.status != 401 {
			t.Fatalf("a wrong password from the TV's address: %d", got.status)
		}
	}
	refusedWithin(t, "the name, to a new device", q.try(0, newDevice(), viewer.name, rightPassword, behindProxy(randomV4())...), 800, 900)
	refusedWithin(t, "the TV's address, for any password", q.try(1, newDevice(), invented(), "wrong", home...), 800, 900)
	failures := q.total(failuresMetric)

	tv := newTV()
	code := q.start(0, tv, home...)
	if got := q.call(1, "GET", "/QuickConnect/Connect?Secret="+code.Secret, "", home...); got.status != 200 {
		t.Fatalf("Connect from the refused address: %d %q", got.status, got.body)
	}
	if got := q.call(0, "POST", "/QuickConnect/Authorize?code="+code.Code, "", append([]string{"Authorization", approverDevice(viewer.token).header(viewer.token)}, home...)...); got.status != 200 {
		t.Fatalf("approval from the refused address: %d %q", got.status, got.body)
	}
	got := q.call(1, "POST", "/Users/AuthenticateWithQuickConnect", `{"Secret":"`+code.Secret+`"}`, append([]string{"Authorization", tv.header("")}, home...)...)
	signedIn(t, "the TV's code while name and address are refused", got)
	if after := q.total(failuresMetric); after != failures {
		t.Errorf("Quick Connect counted %v failed sign-ins", after-failures)
	}
	// Known: while it holds the token, and after it ended.
	signedIn(t, "the TV's password while the name is refused", q.try(0, tv.id, viewer.name, rightPassword, behindProxy(randomV4())...))
	refusedWithin(t, "a new device meanwhile", q.try(0, newDevice(), viewer.name, rightPassword, behindProxy(randomV4())...), 700, 900)

	other := newTV()
	second := q.start(0, other)
	q.approve(0, viewer, second)
	ended := q.redeemed(1, second).AccessToken
	q.logout(0, ended)
	signedIn(t, "a signed-out TV's password while the name is refused", q.try(1, other.id, viewer.name, rightPassword, behindProxy(randomV4())...))
	// Its own bound: 10 wrong passwords, then refused as a known device.
	before := q.total(deviceRefusals)
	for i := range 10 {
		if got := q.try(i, other.id, viewer.name, "wrong", behindProxy(randomV4())...); got.status != 401 {
			t.Fatalf("known device's wrong password %d: %d, want 401", i+1, got.status)
		}
	}
	refusedWithin(t, "the known device after 10", q.try(0, other.id, viewer.name, rightPassword, behindProxy(randomV4())...), 890, 900)
	if got := q.total(deviceRefusals); got != before+1 {
		t.Errorf("the per known device bound refused %v times, want once", got-before)
	}
}

// wantCapRefusal is an Initiate past the cap: not 401, which a client reads as Quick Connect turned off (Moonfin reads
// that, and the word "disabled", as off), and one of the statuses jellyfin-web was seen to answer with its own dialog.
func wantCapRefusal(t *testing.T, got signInReply, what string) {
	t.Helper()
	if got.status != 403 && got.status != 429 && got.status != 503 {
		t.Errorf("%s: %d %q, want 403, 429 or 503", what, got.status, got.body)
	}
	if strings.Contains(strings.ToLower(string(got.body)), "disabled") || strings.Contains(string(got.body), "Secret") {
		t.Errorf("%s: the body %q reads as Quick Connect turned off, or tells of a code", what, got.body)
	}
}

// Criteria 19 and 21 (D-079). A hundred codes are live across the replicas and no more; pending and approved ones count,
// redeemed and expired ones do not; no issued code is touched by a refusal, which is logged and counted.
func TestAHundredCodesAreLiveAndNoMore(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	var codes []qcCode
	for i := range qcCap {
		codes = append(codes, q.start(i, newTV()))
	}
	before := q.series()
	const refusals = 4
	for i := range refusals {
		wantCapRefusal(t, q.initiate(i, newTV()), "the 101st Initiate")
	}
	if t.Failed() {
		return
	}
	q.wantRefusalsCounted(before, refusals, "the cap")
	if lines := q.refusalLines(); lines < refusals {
		t.Errorf("%d log lines name a bound or cap, want one for each of %d refusals", lines, refusals)
	}
	for i, code := range codes {
		q.mustBePending(i, code, "an issued code after the refusals")
		if t.Failed() {
			return
		}
	}
	// Approved and unredeemed still counts.
	for _, code := range codes[:50] {
		q.approve(0, viewer, code)
	}
	wantCapRefusal(t, q.initiate(1, newTV()), "with 50 of the 100 approved")
	// A redeemed one is not live.
	secrets := []string{q.redeemed(1, codes[0]).AccessToken}
	codes = append(codes, q.start(0, newTV()))
	wantCapRefusal(t, q.initiate(1, newTV()), "after the freed place is taken")
	// A code near the end of its time still works.
	q.pass(qcPendingLife - 10*time.Second)
	q.approve(1, viewer, codes[qcCap-1])
	secrets = append(secrets, q.redeemed(0, codes[qcCap-1]).AccessToken)
	// Expired codes, approved or not, are not live.
	q.pass(2 * time.Minute)
	for i := range qcCap {
		codes = append(codes, q.start(i, newTV()))
	}
	wantCapRefusal(t, q.initiate(0, newTV()), "the 101st after the first hundred expired")
	for _, code := range codes {
		secrets = append(secrets, code.Secret)
	}
	q.wantNoSecretLogged(append(secrets, viewer.token)...)
}

// Criterion 19, at once: with 99 codes live, ten Initiates on two replicas meet in Postgres and one gets the last place.
func TestInitiatesAtOnceOnTwoReplicasTakeNoMoreThanTheLastPlace(t *testing.T) {
	q := newQCSite(t, 2, nil)
	for i := range qcCap - 1 {
		q.start(i, newTV())
	}
	var requests []func() signInReply
	for i := range 10 {
		requests = append(requests, func() signInReply { return q.initiate(i, newTV()) })
	}
	replies := q.together([]int{0, 1}, requests...)
	given := 0
	for _, reply := range replies {
		if reply.status == 200 {
			given++
			continue
		}
		wantCapRefusal(t, reply, "an Initiate that found no place")
	}
	if given != 1 {
		t.Errorf("%d of 10 Initiates at once got a code with 99 live, want 1", given)
	}
}

// Criterion 20. On by default; turned off by the api role's "quickConnect": false. A replica with it off answers Enabled
// false and 401 on the other four routes, as Jellyfin 12.4 does, and gives nothing for a code started before, which a
// replica with it on still holds.
func TestQuickConnectIsOnByDefaultAndAReplicaWithItOffGivesNothing(t *testing.T) {
	q := newQCSite(t, 1, nil)
	viewer := q.member(false)
	enabled := func(i int) string {
		got := q.call(i, "GET", "/QuickConnect/Enabled", "")
		return strings.TrimSpace(string(got.body))
	}
	if got := enabled(0); got != "true" {
		t.Fatalf("Enabled with no quickConnect key: %q, want true", got)
	}
	pending, approved := q.start(0, newTV()), q.start(0, newTV())
	q.approve(0, viewer, approved)
	off := q.add(map[string]any{"quickConnect": false})
	if got := enabled(off); got != "false" {
		t.Errorf("Enabled when off: %q, want false", got)
	}
	header := func(device qcDevice) []string { return []string{"Authorization", device.header("")} }
	for what, got := range map[string]signInReply{
		"Initiate":                        q.initiate(off, newTV()),
		"Connect of a pending code":       q.call(off, "GET", "/QuickConnect/Connect?Secret="+pending.Secret, "", header(pending.device)...),
		"Connect of an approved code":     q.call(off, "GET", "/QuickConnect/Connect?Secret="+approved.Secret, "", header(approved.device)...),
		"Connect of an unknown secret":    q.call(off, "GET", "/QuickConnect/Connect?Secret=0000deadbeef", "", header(newTV())...),
		"approval of a pending code":      q.authorize(off, viewer.token, pending.Code, ""),
		"redemption of an approved code":  q.redeem(off, approved.device, approved.Secret),
		"redemption of a pending code":    q.redeem(off, pending.device, pending.Secret),
		"redemption of an unknown secret": q.redeem(off, newTV(), "0000deadbeef"),
	} {
		if got.status != 401 || strings.Contains(string(got.body), "AccessToken") {
			t.Errorf("%s when off: %d %q, want 401 and no token", what, got.status, got.body)
		}
	}
	if n := q.deviceTokens(approved.device, viewer); n != 0 {
		t.Errorf("the replica with Quick Connect off gave the device %d tokens", n)
	}
	q.mustBePending(0, pending, "on the replica with it on, after the other refused its approval")
	q.redeemed(0, approved)
}
