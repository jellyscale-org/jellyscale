package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// When Postgres is away from every replica for over 30 seconds they all answer 503 on /readyz, and
// Caddy's health check then finds nothing to send to and answers 503 itself, which stops a film that
// would have played on once the database is back. The shipped Caddyfile falls back to every replica
// of the role, whatever /readyz says; measured against stand-in replicas that all answer 503 on
// /readyz, requests through it were answered 200 where the Caddyfile without the fallback gave 503.
func TestShippedIngressFallsBackToEveryReplicaWhenNoneIsReady(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/compose/Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	config := regexp.MustCompile(`(?m)#.*$`).ReplaceAllString(string(raw), "")
	_, fallback, found := strings.Cut(config, "handle_errors")
	if !found {
		t.Fatal("deploy/compose/Caddyfile has no handle_errors: a 503 from the ingress itself reaches the viewer")
	}
	if !strings.Contains(fallback, "err.status_code") {
		t.Error("the handle_errors block does not look at the status of the error")
	}
	// Caddy shares one health state between handlers that name a replica alike, so the fallback must
	// name each replica differently from the main blocks or it sees the same replicas as down.
	for _, replica := range []string{"api", "api2", "stream", "stream2"} {
		if !strings.Contains(fallback, replica+".:8096") {
			t.Errorf("the fallback does not name %s as %s.:8096, apart from the main block", replica, replica)
		}
		if strings.Contains(fallback, replica+":8096") {
			t.Errorf("the fallback names %s as %s:8096, the main block's own name, so it shares its health state", replica, replica)
		}
	}
	if !regexp.MustCompile(`(?m)^\s*health_fails\s+\d{6,}\s*$`).MatchString(fallback) {
		t.Error("the fallback can mark a replica down")
	}
}
