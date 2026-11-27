//go:build integration

package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/db"
)

// JS-037, D-097: an approval that outlives its client, when the replica stops and when it fails. The kit is
// quickconnect_kit_integration_test.go's, and codeHold is quickconnect_disconnect_integration_test.go's.

// abandonApprovals sends one approval of each code to replica 0, waits until every one waits on the hold, and then
// hangs up every client. The 30 seconds only bounds a hang.
func (h *codeHold) abandonApprovals(who qcAccount, codes ...string) {
	h.q.t.Helper()
	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	gone := make(chan struct{}, len(codes))
	for _, code := range codes {
		request, err := http.NewRequestWithContext(ctx, "POST", h.q.apis[0].url("/QuickConnect/Authorize?code="+code), nil)
		if err != nil {
			h.q.t.Fatal(err)
		}
		request.Header.Set("Authorization", approverDevice(who.token).header(who.token))
		go func() {
			defer func() { gone <- struct{}{} }()
			response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(request)
			if err == nil {
				response.Body.Close()
			}
		}()
	}
	waitFor(h.q.t, 30*time.Second, "every approval to wait on the held Quick Connect tables", func() bool {
		return len(h.waiting()) == len(codes)
	})
	hangUp()
	for range codes {
		<-gone
	}
	// As in abandonedApproval: the replica is given time to act on the hang-ups before the test's next move.
	time.Sleep(noticeHangUp)
}

// The grace here is 8 s, not the production 30 s. At 30 s the grace coincides with the 30 s an abandoned approval is
// bounded by, so a replica that ignores its grace would look right. The 10 s allowed past the grace bounds a hang: a
// right build needs well under one. Postgres stays stalled until the replica has gone.
//
// The cut approvals were never answered by the database, so each keeps its guess: ten wrong codes cut this way leave
// the account refused on another replica. A build that gives a reservation back when shutdown cuts it fails that.
func TestApprovalsWhoseClientsAreGoneEndAtTheShutdownGraceAndKeepTheirGuesses(t *testing.T) {
	const grace, pastGrace = 8 * time.Second, 10 * time.Second
	q := newQCSite(t, 2, map[string]any{"shutdownGrace": grace.String()})
	guesser := q.member(false)
	hold := q.holdCodes()
	codes := slices.Repeat([]string{"000000"}, 10)
	hold.abandonApprovals(guesser, codes...)
	stopping := q.apis[0]
	began := time.Now()
	stopping.sigterm()
	select {
	case <-stopping.exited:
	case <-time.After(grace + pastGrace):
		t.Errorf("with Postgres stalled and the clients gone, the replica was still running %v after SIGTERM; its shutdownGrace is %v", grace+pastGrace, grace)
		select {
		case <-stopping.exited:
		case <-time.After(time.Minute):
			t.Fatal("the replica did not exit")
		}
	}
	t.Logf("the replica exited %v after SIGTERM", time.Since(began).Round(100*time.Millisecond))
	if stopping.status != nil {
		t.Errorf("the replica exited with %v", stopping.status)
	}
	hold.release()
	refusedWithin(t, "after 10 approvals cut by shutdown, the next one on the other replica", q.authorize(1, guesser.token, "000000", ""), 840, 900)
}

// The grace is the production 30 s. Shutdown does not end an approval before its grace does: one whose client is gone,
// and whose database answers after the listener has closed, is approved and counts no failure. The waits only bound a
// hang.
func TestAnAbandonedApprovalStillCompletesWhenPostgresAnswersWithinTheShutdownGrace(t *testing.T) {
	q := newQCSite(t, 2, nil)
	viewer := q.member(false)
	code := q.start(1, newTV())
	hold := q.holdCodes()
	hold.abandonApprovals(viewer, code.Code)
	stopping := q.apis[0]
	stopping.sigterm()
	waitFor(t, 20*time.Second, "the stopping replica to close its listener", func() bool { return stopping.code("/healthz") == 0 })
	// A build that cuts approvals as the listener closes is given time to do it.
	time.Sleep(noticeHangUp)
	hold.release()
	select {
	case <-stopping.exited:
	case <-time.After(time.Minute):
		t.Fatal("the replica did not exit")
	}
	if status, approved := q.authenticated(1, code); status != 200 || !approved {
		t.Errorf("Connect %d Authenticated %v, want the code approved", status, approved)
	}
	var kept int
	if err := q.pool.QueryRow(t.Context(), `SELECT count(*) FROM signin_failures`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("%d failures are kept after a successful approval, want none", kept)
	}
}

// warnedWith reports whether one warn line of the replica's log, text or JSON, names the account (by name or id) and
// gives the reason.
func warnedWith(p *proc, who qcAccount, reasons ...string) bool {
	for line := range strings.Lines(p.log.String()) {
		warn := strings.Contains(line, "level=WARN") || strings.Contains(line, `"level":"WARN"`)
		named := strings.Contains(line, who.name) || strings.Contains(line, who.id) || strings.Contains(line, db.CompactID(who.id))
		reasoned := slices.ContainsFunc(reasons, func(reason string) bool { return strings.Contains(line, reason) })
		if warn && named && reasoned {
			return true
		}
	}
	return false
}

// The approval is made to fail by Postgres cancelling its statement after the client has hung up. The code is an
// invented one no timestamp or id can contain by chance, so finding it in the log means the code was logged. The 30
// seconds only bounds a hang.
func TestAnApprovalThatFailsAfterItsClientHungUpIsLoggedAtWarnWithTheUserAndReasonAndNoCode(t *testing.T) {
	const typed = "zzqkwjxv"
	q := newQCSite(t, 1, nil)
	viewer := q.member(false)
	hold := q.holdCodes()
	if held, status := hold.abandonedApproval(0, viewer, typed); !held {
		t.Fatalf("the approval answered %d with every code held", status)
	}
	waiting := hold.waiting()
	if len(waiting) != 1 {
		t.Fatalf("%d statements wait on the hold, want the abandoned approval's alone", len(waiting))
	}
	cancelled, err := q.pool.Query(t.Context(), `SELECT pg_cancel_backend($1)`, waiting[0])
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pgx.CollectExactlyOneRow(cancelled, pgx.RowTo[bool]); err != nil || !ok {
		t.Fatalf("cancelling the approval's statement: %v %v", ok, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !warnedWith(q.apis[0], viewer, "canceling statement due to user request", "57014") {
		if time.Now().After(deadline) {
			t.Errorf("no warn line names %s and the reason the approval failed; the log:\n%s", viewer.name, q.apis[0].log)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(strings.ToLower(q.apis[0].log.String()), typed) {
		t.Errorf("the log holds the code that was typed:\n%s", q.apis[0].log)
	}
}
