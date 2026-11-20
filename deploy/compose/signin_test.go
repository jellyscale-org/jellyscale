package compose

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// JS-145 (D-022, amending D-020): the shipped stack leaves `trustedProxies` absent. Every client reaches its ingress from the
// Docker bridge gateway's address, so a declared ingress would count that one address for all of them, and one guesser or
// one mistyping viewer would refuse everyone's sign-in for 15 minutes. With the key absent the per-address bound is off and
// the bounds per name and per known device hold. `[]` is not absent: it turns the bound on and counts the ingress's own
// address for everyone, so it fails here too. (This replaces TestTheShippedStackDeclaresItsIngressAsATrustedProxy, which
// held D-020's "the shipped compose stack declares its ingress".) What the stack does when run is held by
// TestTheShippedStackLetsTheAdministratorInAfterTenWrongPasswordsFromTheHost (stack_integration_test.go).
func TestTheShippedStackLeavesTrustedProxiesAbsent(t *testing.T) {
	keyAbsent(t, "config.json")
}

// D-022: compose.yaml carries the key commented out (a commented `"trustedProxies": [...]`, the way config.json would hold
// it), with when to declare it, so an operator who puts the stack where each client arrives with its own address finds it
// there. A comment that only mentions the key, as the one on the ingress's fixed address does today, is not the key.
func TestTheShippedComposeFileCarriesTrustedProxiesCommentedOut(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var comment []string
	for _, line := range strings.Split(string(raw)+"\n.", "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") {
			comment = append(comment, strings.ToLower(trimmed))
			continue
		}
		block := strings.Join(comment, " ")
		if commentedKey.MatchString(block) && strings.Contains(block, "declare") {
			return
		}
		comment = nil
	}
	t.Error("deploy/compose/compose.yaml has no comment that carries `trustedProxies` and says when to declare it")
}

// D-022: the client-testing stack (deploy/fixtures, docs/client-testing.md) leaves the key absent too, never `[]`: its one
// port on 127.0.0.1 is reached from the Docker gateway by every client, as the shipped stack's is. Its image bakes in
// deploy/fixtures/config.json (Dockerfile, target fixtures). Passes on the tree as built: a guard against a build that
// declares `[]` there to turn the bound on.
func TestTheFixturesStackLeavesTrustedProxiesAbsent(t *testing.T) {
	keyAbsent(t, "../fixtures/config.json")
	raw, err := os.ReadFile("../fixtures/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "trustedProxies") || strings.Contains(string(raw), "--config") {
		t.Error("deploy/fixtures/compose.yaml sets trustedProxies or another config than the image's deploy/fixtures/config.json")
	}
}

var commentedKey = regexp.MustCompile(`#.*"?trustedproxies"?\s*:\s*\[`)

func keyAbsent(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if value, ok := config["trustedProxies"]; ok {
		t.Errorf("%s sets trustedProxies to %s, want the key absent (D-022)", path, value)
	}
}
