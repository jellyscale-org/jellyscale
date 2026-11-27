package contract_test

// JS-183, criteria 3 to 5. A server refuses to start, which is a panic in Handler as an undeclared Access is, when its routes and
// its contract disagree. Each fixture is the embedded contract with one change, so each case fails for its own change: an operation
// the routes serve that the contract lacks (per method, not per path), a served operation marked declared, a declared one marked
// served, a stable capability (per capability and for discovery), a reason the server writes that the contract does not list, and
// a refusal reason marked as a note. A stream replica serves no v1 route and starts whatever the v1 part says.

import (
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"jellyscale/internal/api"
	"jellyscale/internal/contract"
)

const v1Prefix = "/jellyscale/v1/"

// contractWith returns the embedded contract with one change made to its generic form.
func contractWith(t *testing.T, change func(document map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(contract.Document(), &document); err != nil {
		t.Fatalf("the embedded contract is not YAML: %v", err)
	}
	change(document)
	out, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// pathItem is the operations under one path of the generic form.
func pathItem(document map[string]any, path string) map[string]any {
	paths, _ := document["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	return item
}

// operation is the operation of one method under one path of the generic form.
func operation(document map[string]any, path, method string) map[string]any {
	fields, _ := pathItem(document, path)[method].(map[string]any)
	return fields
}

// reasonEntry is one entry of the generic form's x-reasons list.
func reasonEntry(document map[string]any, code string) map[string]any {
	reasons, _ := document["x-reasons"].(map[string]any)
	entry, _ := reasons[code].(map[string]any)
	return entry
}

// refuses reports whether a server with these roles, holding this contract, panics when it builds its handler. A v1 server is the
// default.
func refuses(t *testing.T, document []byte, roles ...string) (refused bool) {
	t.Helper()
	if len(roles) == 0 {
		roles = []string{api.RoleAPI}
	}
	parsed, err := contract.Parse(document)
	if err != nil {
		t.Fatalf("the fixture is not a contract: %v", err)
	}
	defer func() {
		if recover() != nil {
			refused = true
		}
	}()
	(&api.Server{Roles: roles, Log: slog.New(slog.DiscardHandler), Contract: parsed}).Handler()
	return false
}

func TestServerStartsWithTheContractItServes(t *testing.T) {
	if refuses(t, contract.Document()) {
		t.Fatalf("the embedded contract, which the routes of this build match, refused to start the server")
	}
}

// A stream replica serves no v1 route, so a contract with no v1 part does not stop it from starting.
func TestStreamReplicaStartsWithNoV1Contract(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		paths, _ := document["paths"].(map[string]any)
		for path := range paths {
			if strings.HasPrefix(path, v1Prefix) {
				delete(paths, path)
			}
		}
	})
	if refuses(t, document, api.RoleStream) {
		t.Fatalf("a stream replica refused to start over a contract with no v1 part, which it never serves")
	}
}

func TestServerRefusesToStartWithARouteTheContractLacks(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		paths, _ := document["paths"].(map[string]any)
		delete(paths, "/jellyscale/v1/server")
	})
	if !refuses(t, document) {
		t.Fatalf("a served /jellyscale/v1/ route the contract lacks did not refuse to start")
	}
}

// A method the routes serve, which the contract lacks, under a path the contract does have: a build that compares paths only passes.
func TestServerRefusesToStartWithAServedMethodTheContractLacks(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		delete(pathItem(document, "/jellyscale/v1/track-preferences"), "patch")
	})
	if !refuses(t, document) {
		t.Fatalf("a served PATCH the contract lacks did not refuse to start")
	}
}

func TestServerRefusesToStartWithAServedOperationMarkedDeclared(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/server", "get")["x-state"] = "declared"
	})
	if !refuses(t, document) {
		t.Fatalf("a served operation marked declared did not refuse to start")
	}
}

func TestServerRefusesToStartWithADeclaredOperationMarkedServed(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/sign-in", "post")["x-state"] = "preview"
	})
	if !refuses(t, document) {
		t.Fatalf("an operation with no handler marked preview did not refuse to start")
	}
}

// Criterion 5: no capability is stable before its compatibility gate exists, so a server that would serve one refuses to start.
// The capability case marks one operation of track preferences; the discovery case marks one with no capability.
func TestServerRefusesToStartWithAStableCapability(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/track-preferences", "get")["x-state"] = "stable"
	})
	if !refuses(t, document) {
		t.Fatalf("a stable capability did not refuse to start the server")
	}
}

func TestServerRefusesToStartWithAStableDiscoveryOperation(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/server", "get")["x-state"] = "stable"
	})
	if !refuses(t, document) {
		t.Fatalf("a stable discovery operation did not refuse to start the server")
	}
}

// Criterion 3: a reason the server writes is on the contract's list as a refusal. A missing one, or one marked as a note, is refused.
func TestServerRefusesToStartWithAReasonItWritesThatTheContractLacks(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		reasons, _ := document["x-reasons"].(map[string]any)
		delete(reasons, "no-capacity")
	})
	if !refuses(t, document) {
		t.Fatalf("a reason the server writes that the contract lacks did not refuse to start the server")
	}
}

func TestServerRefusesToStartWithARefusalReasonMarkedAsANote(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		reasonEntry(document, "no-capacity")["kind"] = "note"
	})
	if !refuses(t, document) {
		t.Fatalf("a refusal reason marked as a note did not refuse to start the server")
	}
}
