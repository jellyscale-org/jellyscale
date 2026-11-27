//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/testenv"
)

// JS-037: a client that hangs up during an approval, and a sign-in whose token is replaced before the redemption that
// made it has answered. The kit is quickconnect_kit_integration_test.go's.

// noticeHangUp is how long a replica is given to act on a client's hang-up while its statement is held. A build that
// acts on it ends the held statement within milliseconds, which ends the wait at once. A build that ignores it leaves
// no sign, so the wait then runs out: it can only let a wrong build through on a machine hundreds of times slower than
// usual, and never fails a right build.
const noticeHangUp = 2 * time.Second

// codeHold keeps every statement that reads or writes a Quick Connect table waiting in Postgres.
type codeHold struct {
	q   *qcSite
	tx  pgx.Tx
	pid int
}

func (q *qcSite) holdCodes() *codeHold {
	q.t.Helper()
	ctx := context.Background()
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		q.t.Fatal(err)
	}
	q.t.Cleanup(func() { tx.Rollback(ctx) }) //nolint:errcheck // The release has ended it on the way out.
	h := &codeHold{q: q, tx: tx}
	if _, err := tx.Exec(ctx, `LOCK TABLE "`+strings.Join(q.quickTables(), `", "`)+`" IN ACCESS EXCLUSIVE MODE`); err != nil {
		q.t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		q.t.Fatal(err)
	}
	return h
}

// waiting are the replicas' sessions that wait on this hold.
func (h *codeHold) waiting() []int {
	h.q.t.Helper()
	// Not through h.tx: pg_stat_activity is read once for a transaction.
	rows, err := h.q.pool.Query(context.Background(), `SELECT pid FROM pg_stat_activity
		WHERE application_name = ANY($1) AND $2 = ANY(pg_blocking_pids(pid))`, h.q.names, h.pid)
	if err != nil {
		h.q.t.Fatal(err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		h.q.t.Fatal(err)
	}
	return pids
}

func (h *codeHold) release() {
	h.q.t.Helper()
	if err := h.tx.Commit(context.Background()); err != nil {
		h.q.t.Fatal(err)
	}
}

// abandonedApproval sends an approval to replica i. If its statement comes to wait on the hold, the client hangs up
// then, and held is true. If it is answered without touching a code, held is false and the status is the answer's.
// The 30 seconds only bounds a hang.
func (h *codeHold) abandonedApproval(i int, who qcAccount, code string) (held bool, status int) {
	h.q.t.Helper()
	before := h.waiting()
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	request, err := http.NewRequestWithContext(ctx, "POST", h.q.apis[i%len(h.q.apis)].url("/QuickConnect/Authorize?code="+code), nil)
	if err != nil {
		h.q.t.Fatal(err)
	}
	request.Header.Set("Authorization", approverDevice(who.token).header(who.token))
	answered := make(chan int, 1)
	go func() {
		response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
		if err != nil {
			answered <- 0
			return
		}
		response.Body.Close()
		answered <- response.StatusCode
	}()
	var mine int
	deadline := time.Now().Add(30 * time.Second)
	for mine == 0 {
		select {
		case status := <-answered:
			return false, status
		default:
		}
		for _, pid := range h.waiting() {
			if !slices.Contains(before, pid) {
				mine = pid
			}
		}
		if time.Now().After(deadline) {
			h.q.t.Fatal("the approval neither answered nor came to wait on the held Quick Connect tables")
		}
		time.Sleep(5 * time.Millisecond)
	}
	hangUp()
	<-answered
	for notice := time.Now().Add(noticeHangUp); time.Now().Before(notice) && slices.Contains(h.waiting(), mine); {
		time.Sleep(20 * time.Millisecond)
	}
	return true, 0
}

// Criterion 16 (D-078). An account that hangs up while its code is being looked up still spends a guess: of 40 wrong
// codes, each abandoned there, no more than ten are looked up, and the next approval is refused.
func TestHangingUpWhileACodeIsLookedUpDoesNotGiveTheGuessBack(t *testing.T) {
	q := newQCSite(t, 2, nil)
	guesser := q.member(false)
	hold := q.holdCodes()
	lookedUp := 0
	for n := range 40 {
		held, status := hold.abandonedApproval(n, guesser, "000000")
		if held {
			lookedUp++
			continue
		}
		if status != 403 {
			t.Fatalf("approval %d answered %d with every code held, want it held or refused with 403", n+1, status)
		}
	}
	if lookedUp != 10 {
		t.Errorf("%d of the 40 abandoned approvals went on to look their code up, want 10", lookedUp)
	}
	hold.release()
	refusedWithin(t, "the next approval, the abandoned statements let go", q.authorize(1, guesser.token, "000000", ""), 840, 900)
}

// Criterion 16. Hanging up changes neither what an approval does nor what it costs: the right code, abandoned while it
// is being looked up, is approved as it would have been, and as a success it leaves no failure counted. The 30 seconds
// only bounds a hang.
func TestHangingUpWhileTheRightCodeIsLookedUpStillApprovesItAndCountsNoFailure(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	code := q.start(0, newTV())
	hold := q.holdCodes()
	if held, status := hold.abandonedApproval(1, viewer, code.Code); !held {
		t.Fatalf("the approval answered %d with every code held", status)
	}
	hold.release()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, approved := q.authenticated(0, code)
		var kept int
		if err := q.pool.QueryRow(t.Context(), `SELECT count(*) FROM signin_failures`).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		if status == 200 && approved && kept == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the hang-up: Connect %d Authenticated %v with %d failures kept, want the code approved and none kept", status, approved, kept)
		}
		time.Sleep(50 * time.Millisecond)
	}
	q.failApprovals(viewer, 10)
	refusedWithin(t, "the control: after 10 unknown codes", q.authorize(0, viewer.token, "000000", ""), 890, 900)
}

// heldAfterCommit sends the request, which makes a device token through replica i's proxy, and keeps Postgres's answer
// to its COMMIT from the replica while meanwhile runs: the token is stored and the replica has not heard yet. Whatever
// the replica asks of Postgres after its commit comes after meanwhile; whatever it asked inside the transaction is
// already answered. The 30 seconds only bounds a hang.
func (q *qcSite) heldAfterCommit(proxy *testenv.DBProxy, request func() signInReply, meanwhile func()) signInReply {
	q.t.Helper()
	var issued atomic.Bool
	committed, goOn := make(chan struct{}), make(chan struct{})
	done := proxy.OnStatement(func(sent []byte) bool {
		if bytes.Contains(sent, []byte("INSERT INTO devices")) {
			issued.Store(true)
		}
		return issued.Load() && bytes.Contains(bytes.ToLower(sent), []byte("commit"))
	}, nil, func() {
		close(committed)
		<-goOn
	})
	release := func() {
		select {
		case <-goOn:
		default:
			close(goOn)
		}
	}
	defer release() // A failure in meanwhile must not leave the proxy's goroutine waiting.
	reply := make(chan signInReply, 1)
	go func() { reply <- request() }()
	select {
	case <-committed:
	case got := <-reply:
		q.t.Fatalf("the request answered %d %q and no commit of a new token was seen", got.status, got.body)
	case <-time.After(30 * time.Second):
		q.t.Fatal("no commit of a new token was seen")
	}
	meanwhile()
	release()
	got := <-reply
	<-done
	return got
}

// Criterion 10. A redemption has made its token when another sign-in or a sign-out on the device ends that token,
// before the redemption has answered. It still answers as what it did: its sign-in, or the unknown secret's 404. It is
// never a server error.
func TestARedemptionWhoseTokenIsReplacedBeforeItAnswersIsNoServerError(t *testing.T) {
	proxy := testenv.NewDBProxy(t, testDatabaseURL(t))
	q := newQCSite(t, 2, nil)
	direct := q.dsn
	q.dsn = proxy.DSN() + "&default_query_exec_mode=exec"
	proxied := q.add(nil)
	q.dsn = direct
	// Replicas 0 and 1 reach Postgres directly; q.call takes its index modulo all three, so what is done meanwhile
	// names 0 or 1.
	moves := []struct {
		name string
		// move ends the held redemption's token and returns the token that took its place, "" when none did.
		move func(t *testing.T, viewer qcAccount, code qcCode) string
	}{
		{"a password sign-in on the device", func(t *testing.T, viewer qcAccount, code qcCode) string {
			return signedIn(t, "the password sign-in on the TV", q.try(0, code.device.id, viewer.name, rightPassword))
		}},
		{"a newer code of the device redeemed", func(_ *testing.T, viewer qcAccount, code qcCode) string {
			newer := q.start(0, code.device)
			q.approve(1, viewer, newer)
			return q.redeemed(0, newer).AccessToken
		}},
		{"a sign-out of the token, which a second redemption was given", func(t *testing.T, _ qcAccount, code qcCode) string {
			token := q.redeemed(1, code).AccessToken
			q.logout(0, token)
			if got := q.me(1, token); got != 401 {
				t.Fatalf("the signed-out token answers %d", got)
			}
			return ""
		}},
	}
	for _, tc := range moves {
		t.Run(tc.name, func(t *testing.T) {
			q.reportTo(t)
			viewer := q.member(false)
			code := q.start(0, newTV())
			q.approve(1, viewer, code)
			var replacement string
			var held int
			got := q.heldAfterCommit(proxy,
				func() signInReply { return q.redeem(proxied, code.device, code.Secret) },
				func() {
					if n := q.deviceTokens(code.device, viewer); n != 1 {
						t.Fatalf("with the redemption's commit answered, the device holds %d tokens, want 1", n)
					}
					replacement = tc.move(t, viewer, code)
					held = q.deviceTokens(code.device, viewer)
				})
			switch got.status {
			case 200:
				var signed qcSignIn
				if json.Unmarshal(got.body, &signed) != nil || signed.AccessToken == "" || signed.User.Name != viewer.name ||
					signed.SessionInfo.DeviceId != code.device.id {
					t.Errorf("the redemption answered 200 %q, want the sign-in of %s on %s", got.body, viewer.name, code.device.id)
				}
				if signed.AccessToken == replacement && replacement != "" {
					t.Errorf("the redemption was given the token of the sign-in that replaced it")
				}
			case 404:
				if unknown := q.redeem(0, newTV(), "0000deadbeef"); string(got.body) != string(unknown.body) {
					t.Errorf("the redemption answered 404 %q, want the unknown secret's %q", got.body, unknown.body)
				}
			default:
				t.Errorf("the redemption answered %d %q, want 200 with its sign-in or the unknown secret's 404", got.status, got.body)
			}
			if now := q.deviceTokens(code.device, viewer); now != held {
				t.Errorf("answering the redemption changed the device's tokens from %d to %d", held, now)
			}
			if replacement != "" && q.me(0, replacement) != 200 {
				t.Errorf("the token that replaced the redemption's is not accepted")
			}
		})
	}
}
