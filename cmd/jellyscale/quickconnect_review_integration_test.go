//go:build integration

package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// JS-037, the regression checks of the review after the build. The kit is quickconnect_kit_integration_test.go's.

// Criterion 6. A code is any of the million six-digit ones, not a part of them. A uniform draw puts all of 1000 codes at
// or below 65535 with a chance of 0.065536^1000, which is nothing.
func TestAThousandCodesUseTheWholeSixDigitRange(t *testing.T) {
	q := newQCSite(t, 1, nil)
	highest := 0
	for i := range 10 * qcCap {
		if i%qcCap == 0 {
			q.empty() // Room under the cap of live codes.
		}
		code := q.start(0, newTV())
		value, err := strconv.Atoi(code.Code)
		if err != nil || !sixDigits.MatchString(code.Code) {
			t.Fatalf("code %q is not six decimal digits", code.Code)
		}
		highest = max(highest, value)
	}
	if highest <= 65535 {
		t.Errorf("the highest of %d codes is %06d: no code is above 65535, so most of the six-digit codes are never drawn", 10*qcCap, highest)
	}
}

// reportTo makes the kit fail the subtest, until it ends.
func (q *qcSite) reportTo(t *testing.T) {
	parent := q.t
	q.t = t
	t.Cleanup(func() { q.t = parent })
}

// Criterion 10. Once the token a code made is gone, its secret is worth nothing: redeeming it again answers as an
// unknown secret does, and makes no new token.
func TestRedeemingAgainAfterTheTokenIsGoneAnswersAsAnUnknownSecret(t *testing.T) {
	q := newQCSite(t, 2, nil)
	// Each ending returns the token that took the old one's place on the device, "" when none did.
	endings := []struct {
		name string
		end  func(t *testing.T, viewer qcAccount, code qcCode, token string) string
	}{
		{"after signing out", func(_ *testing.T, _ qcAccount, _ qcCode, token string) string {
			q.logout(0, token)
			return ""
		}},
		{"after a password sign-in on the same device", func(t *testing.T, viewer qcAccount, code qcCode, _ string) string {
			return signedIn(t, "the password sign-in on the TV", q.try(0, code.device.id, viewer.name, rightPassword))
		}},
		{"after a newer code replaced the token", func(_ *testing.T, viewer qcAccount, code qcCode, _ string) string {
			newer := q.start(0, code.device)
			q.approve(1, viewer, newer)
			return q.redeemed(0, newer).AccessToken
		}},
		{"after the approving account was deleted", func(t *testing.T, viewer qcAccount, _ qcCode, _ string) string {
			if _, err := q.pool.Exec(t.Context(), "DELETE FROM users WHERE id = $1", viewer.id); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
	}
	for _, ending := range endings {
		t.Run(ending.name, func(t *testing.T) {
			q.reportTo(t)
			viewer := q.member(false)
			code := q.start(0, newTV())
			q.approve(1, viewer, code)
			token := q.redeemed(0, code).AccessToken
			replacement := ending.end(t, viewer, code, token)
			if got := q.me(0, token); got != 401 {
				t.Fatalf("the first token answers %d, want it gone before the second redemption", got)
			}
			held := q.deviceTokens(code.device, viewer)
			unknown := q.redeem(0, newTV(), "0000deadbeef")
			for replica := range 2 {
				got := q.redeem(replica, code.device, code.Secret)
				if got.status != 404 || string(got.body) != string(unknown.body) {
					t.Errorf("second redemption on replica %d: %d %q, want the unknown secret's 404 %q", replica, got.status, got.body, unknown.body)
				}
			}
			if now := q.deviceTokens(code.device, viewer); now != held {
				t.Errorf("the second redemption changed the device's tokens from %d to %d", held, now)
			}
			if replacement == "" {
				return
			}
			if got := q.me(1, replacement); got != 200 {
				t.Errorf("the token that replaced the first one answers %d after the second redemption, want 200", got)
			}
		})
	}
}

// tokenInsertHold is the advisory lock that holds a device's new token just before it is stored. The product's cap lock
// and the other Quick Connect tests use other keys.
const tokenInsertHold = 370137

// heldAtTokenInsert holds every new devices row of the device just before it is stored, sends the requests one beside
// the other, waits until both replicas' requests wait on a lock in Postgres, and lets go. A build that replaces the
// device's token with nothing to make two replacements wait for each other has by then deleted the old rows in both,
// seen none, and goes on to store both. A build that serializes them has one held here and the other on its own lock.
// The 30 seconds only bounds a hang.
func (q *qcSite) heldAtTokenInsert(device qcDevice, requests ...func() signInReply) []signInReply {
	q.t.Helper()
	ctx := context.Background()
	drop := func() {
		if _, err := q.pool.Exec(ctx, `DROP FUNCTION IF EXISTS qc_test_hold_token() CASCADE`); err != nil {
			q.t.Error(err)
		}
	}
	q.t.Cleanup(drop) // Registered before it is made, and run after the holder below has let go.
	for _, statement := range []string{
		`CREATE FUNCTION qc_test_hold_token() RETURNS trigger LANGUAGE plpgsql
			AS $$ BEGIN PERFORM pg_advisory_xact_lock_shared(` + strconv.Itoa(tokenInsertHold) + `); RETURN NEW; END $$`,
		`CREATE TRIGGER qc_test_hold_token BEFORE INSERT ON devices FOR EACH ROW
			WHEN (NEW.device_id = '` + device.id + `') EXECUTE FUNCTION qc_test_hold_token()`,
	} {
		if _, err := q.pool.Exec(ctx, statement); err != nil {
			q.t.Fatal(err)
		}
	}
	holder, err := pgx.Connect(ctx, q.dsn)
	if err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { holder.Close(ctx) }) //nolint:errcheck // Closing releases the lock.
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, tokenInsertHold); err != nil {
		q.t.Fatal(err)
	}
	type answer struct {
		n     int
		reply signInReply
	}
	answers := make(chan answer, len(requests))
	for n, request := range requests {
		go func() { answers <- answer{n, request()} }()
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := holder.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = ANY($1) AND wait_event_type = 'Lock'`, q.names).Scan(&waiting); err != nil {
			q.t.Fatal(err)
		}
		if waiting >= len(requests) {
			break
		}
		select {
		case got := <-answers:
			q.t.Fatalf("request %d answered %d %q while new tokens were held, so the two did not meet", got.n, got.reply.status, got.reply.body)
		default:
		}
		if time.Now().After(deadline) {
			q.t.Fatalf("%d of the %d requests came to wait on a lock in Postgres", waiting, len(requests))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, tokenInsertHold); err != nil {
		q.t.Fatal(err)
	}
	replies := make([]signInReply, len(requests))
	for range requests {
		got := <-answers
		replies[got.n] = got.reply
	}
	return replies
}

// Criterion 10. One device holds two approved codes and redeems both at once, each on a replica of its own: it is left
// with one live token.
func TestTwoCodesOfOneDeviceRedeemedAtOnceOnTwoReplicasLeaveOneLiveToken(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	tv := newTV()
	first, second := q.start(0, tv), q.start(1, tv)
	q.approve(0, viewer, first)
	q.approve(1, viewer, second)
	replies := q.heldAtTokenInsert(tv,
		func() signInReply { return q.redeem(0, tv, first.Secret) },
		func() signInReply { return q.redeem(1, tv, second.Secret) })
	var tokens []string
	for _, reply := range replies {
		var signed qcSignIn
		switch {
		case reply.status == 200 && json.Unmarshal(reply.body, &signed) == nil && signed.AccessToken != "":
			tokens = append(tokens, signed.AccessToken)
		case reply.status/100 != 4:
			t.Errorf("a redemption answered %d %q, want a sign-in or a refusal", reply.status, reply.body)
		}
	}
	if live := q.liveOf(tokens); len(tokens) == 0 || live != 1 {
		t.Errorf("%d redemptions signed in and %d of their tokens are accepted, want at least one and exactly 1", len(tokens), live)
	}
	if n := q.deviceTokens(tv, viewer); n != 1 {
		t.Errorf("the database holds %d tokens for the device, want 1", n)
	}
}

// Text Postgres cannot store: a NUL byte, and a byte that is no UTF-8. Both arrive percent-encoded.
var unstorable = []string{"%00", "%ff"}

func wantBadRequest(t *testing.T, got signInReply, what string) {
	t.Helper()
	if got.status != 400 {
		t.Errorf("%s: %d %q, want 400", what, got.status, got.body)
	}
}

// Criterion 14. Text that cannot be stored is the client's mistake on every route, never the server's.
func TestTextThatCannotBeStoredIsABadRequestOnEveryQuickConnectRoute(t *testing.T) {
	q := newQCSite(t, 2, nil)
	t.Run("Initiate", func(t *testing.T) {
		q.reportTo(t)
		fields := []string{"DeviceId", "Device", "Client", "Version"}
		for _, bad := range unstorable {
			for _, broken := range fields {
				var values []string
				for _, field := range fields {
					value := "x"
					if field == broken {
						value = "x" + bad
					}
					values = append(values, field+`="`+value+`"`)
				}
				got := q.call(0, "POST", "/QuickConnect/Initiate", "", "Authorization", "MediaBrowser "+strings.Join(values, ", "))
				wantBadRequest(t, got, "Initiate with "+bad+" in "+broken)
			}
		}
		if total, each := q.stored(); total != 0 {
			t.Errorf("the refused Initiates left %d rows: %v", total, each)
		}
	})
	t.Run("Connect", func(t *testing.T) {
		q.reportTo(t)
		for _, bad := range unstorable {
			got := q.call(1, "GET", "/QuickConnect/Connect?Secret=ab"+bad, "", "Authorization", newTV().header(""))
			wantBadRequest(t, got, "Connect with "+bad+" in the secret")
		}
	})
	t.Run("redemption", func(t *testing.T) {
		q.reportTo(t)
		for _, body := range []string{`{"Secret":"\u0000"}`, `{"Secret":"ab\u0000cd"}`} {
			got := q.call(0, "POST", "/Users/AuthenticateWithQuickConnect", body, "Authorization", newTV().header(""))
			wantBadRequest(t, got, "redemption with "+body)
		}
	})
	// More refused approvals than D-078's ten, and the account still has all ten of its guesses.
	t.Run("approval, which counts no guess", func(t *testing.T) {
		q.reportTo(t)
		viewer := q.member(false)
		for round := range 6 {
			for _, bad := range unstorable {
				got := q.call(round, "POST", "/QuickConnect/Authorize?code=12"+bad, "", "Authorization", approverDevice(viewer.token).header(viewer.token))
				wantBadRequest(t, got, "approval "+strconv.Itoa(round+1)+" with "+bad+" in the code")
			}
		}
		q.failApprovals(viewer, 10)
		refusedWithin(t, "the control: after 10 unknown codes", q.authorize(0, viewer.token, "000000", ""), 890, 900)
	})
}

// Criterion 16. An approval that fails because the database refused the write was no wrong guess: twelve of them leave
// the account all ten of its guesses. The trigger stands in for any database error that says nothing about the code.
func TestAnApprovalTheDatabaseFailsDoesNotCountTowardTheBound(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	code := q.start(0, newTV())
	ctx := context.Background()
	drop := func() {
		if _, err := q.pool.Exec(ctx, `DROP FUNCTION IF EXISTS qc_test_refuse_write() CASCADE`); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(drop)
	statements := []string{`CREATE FUNCTION qc_test_refuse_write() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'the test refuses this write'; END $$`}
	for _, table := range q.quickTables() {
		statements = append(statements, `CREATE TRIGGER qc_test_refuse_write BEFORE UPDATE ON "`+table+`"
			FOR EACH ROW EXECUTE FUNCTION qc_test_refuse_write()`)
	}
	for _, statement := range statements {
		if _, err := q.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 12 {
		if got := q.authorize(i, viewer.token, code.Code, ""); got.status < 500 {
			t.Errorf("approval %d while the database refuses the write: %d %q, want a server error", i+1, got.status, got.body)
		}
	}
	drop()
	q.failApprovals(viewer, 9)
	q.approve(1, viewer, code)
	q.failApprovals(viewer, 1)
	refusedWithin(t, "the control: after 10 unknown codes", q.authorize(0, viewer.token, "000000", ""), 890, 900)
}

// Criterion 2. A pending code was added once: Connect, on another replica too, says the time Initiate said.
func TestConnectGivesAPendingCodeTheDateAddedThatInitiateGave(t *testing.T) {
	q := newQCSite(t, 2, nil)
	device := newTV()
	var initiated, connected struct{ Secret, DateAdded string }
	got := q.initiate(0, device)
	if got.status != 200 || json.Unmarshal(got.body, &initiated) != nil || initiated.DateAdded == "" {
		t.Fatalf("Initiate: %d %q, want a code with a DateAdded", got.status, got.body)
	}
	for replica := range 2 {
		got = q.connectSecret(replica, initiated.Secret)
		if got.status != 200 || json.Unmarshal(got.body, &connected) != nil {
			t.Fatalf("Connect on replica %d: %d %q", replica, got.status, got.body)
		}
		if connected.DateAdded != initiated.DateAdded {
			t.Errorf("Connect on replica %d says DateAdded %s, Initiate said %s", replica, connected.DateAdded, initiated.DateAdded)
		}
	}
}
