//go:build integration

package main

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// JS-037, D-097: a replica whose database has stopped answering altogether, not just one table, still exits by its
// shutdown grace. The proxy's Hold freezes this replica's Postgres alone: every connection stays open, later ones are
// accepted, and not a byte passes either way, a cancel request included, as with a paused or unreachable server.
//
// The grace here is 8 s, not the production 30 s, so that it does not coincide with the 30 s an abandoned approval is
// bounded by; what a longer grace could hide is a cleanup that fits inside it. The 10 s allowed past the grace is the
// allowance of TestApprovalsWhoseClientsAreGoneEndAtTheShutdownGraceAndKeepTheirGuesses and bounds a hang: a right
// build needs well under one.
func TestAReplicaWhosePostgresAnswersNothingExitsByTheShutdownGrace(t *testing.T) {
	const grace, pastGrace = 8 * time.Second, 10 * time.Second
	for _, tc := range []struct {
		name     string
		approval bool
	}{
		{"with an approval in flight whose client is gone", true},
		{"with no approval in flight", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := testenv.NewDBProxy(t, testDatabaseURL(t))
			extra := map[string]any{"shutdownGrace": grace.String()}
			q := newQCSite(t, 1, extra)
			direct := q.dsn
			q.dsn = proxy.DSN()
			frozen := q.add(extra)
			q.dsn = direct
			// Registered after the replicas, so it runs before they are stopped.
			t.Cleanup(proxy.Release)
			if tc.approval {
				guesser := q.member(false)
				hold := q.holdCodes()
				if held, status := hold.abandonedApproval(frozen, guesser, "000000"); !held {
					t.Fatalf("the approval answered %d with every code held", status)
				}
				// The tables are let go only after the freeze, so the approval hears nothing of it.
				defer hold.release()
			}
			stopping := q.apis[frozen]
			proxy.Hold()
			// Three of the replica's one-second heartbeats go by, so its own background work is stuck in the freeze too.
			// Nothing is asserted about this pause.
			time.Sleep(3 * time.Second)
			began := time.Now()
			stopping.sigterm()
			select {
			case <-stopping.exited:
				t.Logf("the replica exited %v after SIGTERM", time.Since(began).Round(100*time.Millisecond))
				if stopping.status != nil {
					t.Errorf("the replica exited with %v", stopping.status)
				}
			case <-time.After(grace + pastGrace):
				t.Errorf("with Postgres answering nothing, the replica was still running %v after SIGTERM; its shutdownGrace is %v", grace+pastGrace, grace)
				// SIGQUIT makes the Go runtime print its goroutines and exit, which says where the shutdown is stuck.
				stopping.cmd.Process.Signal(syscall.SIGQUIT) //nolint:errcheck
				select {
				case <-stopping.exited:
				case <-time.After(time.Minute):
					t.Fatal("the replica did not exit on SIGQUIT")
				}
				t.Logf("where the main goroutine was:\n%s", mainGoroutine(stopping.log.String()))
			}
		})
	}
}

// mainGoroutine is goroutine 1's stack in the runtime's dump, or nothing when the log has none.
func mainGoroutine(log string) string {
	_, after, found := strings.Cut(log, "\ngoroutine 1 ")
	if !found {
		return ""
	}
	stack, _, _ := strings.Cut(after, "\n\n")
	return "goroutine 1 " + stack
}
