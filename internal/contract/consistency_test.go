package contract_test

// JS-183 review, class 2. A server refuses to start when its routes, its operations, its capabilities and the reasons its
// answers carry disagree, in both directions. Each fixture is the embedded contract with one inconsistency, so each case fails
// only for its own change. The contract's own rules hold for every replica, a stream replica included: the document is one
// artifact, whichever roles serve it. The route rules are the API replica's, as in contract_states_test.go.

import "testing"

// An operation the server serves names a capability the contract does not list.
func TestServerRefusesToStartWithAServedOperationWhoseCapabilityIsNotListed(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/track-preferences", "get")["x-capability"] = "tracking-preferences"
	})
	if !refuses(t, document) {
		t.Fatalf("a served operation whose capability x-capabilities does not list started the server")
	}
}

// A capability's operations share its state: an operation declared under a preview capability is an inconsistency.
func TestServerRefusesToStartWithAnOperationWhoseStateDiffersFromItsCapability(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/sign-in", "post")["x-capability"] = "track-preferences"
	})
	if !refuses(t, document) {
		t.Fatalf("a declared operation under a preview capability started the server")
	}
}

// A listed capability with no operation is an inconsistency, declared or not.
func TestServerRefusesToStartWithADeclaredCapabilityThatNoOperationHas(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		capabilities, _ := document["x-capabilities"].(map[string]any)
		capabilities["orphan"] = map[string]any{"id": "orphan", "state": "declared", "meaning": "no operation has it"}
	})
	if !refuses(t, document) {
		t.Fatalf("a declared capability with no operation started the server")
	}
}

// A capability's key and its id are the same name; a document that disagrees with itself is refused.
func TestServerRefusesToStartWithACapabilityWhoseIdIsNotItsKey(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		capabilities, _ := document["x-capabilities"].(map[string]any)
		capability, _ := capabilities["playback-info"].(map[string]any)
		capability["id"] = "playback"
	})
	if !refuses(t, document) {
		t.Fatalf("a capability whose id differs from its key started the server")
	}
}

// Every reason a response declares is on the closed list. A response naming one the list lacks is an inconsistency, which a
// stream replica refuses too: the contract is one document.
func TestServerRefusesToStartWithAResponseDeclaringAReasonTheListLacks(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		responses, _ := operation(document, "/jellyscale/v1/track-preferences", "get")["responses"].(map[string]any)
		unauthorized, _ := responses["401"].(map[string]any)
		unauthorized["x-reasons"] = []any{"no-such-reason"}
	})
	if !refuses(t, document) {
		t.Fatalf("a response declaring a reason the closed list lacks started the server")
	}
	if !refuses(t, document, "stream") {
		t.Fatalf("a stream replica started with a contract whose responses declare a reason the closed list lacks")
	}
}

// A contract inconsistency between capabilities and operations is refused by a stream replica as well.
func TestStreamReplicaRefusesToStartWithAnInconsistentCapability(t *testing.T) {
	document := contractWith(t, func(document map[string]any) {
		operation(document, "/jellyscale/v1/track-preferences", "get")["x-capability"] = "tracking-preferences"
	})
	if !refuses(t, document, "stream") {
		t.Fatalf("a stream replica started with a contract whose operation names a capability it does not list")
	}
}
