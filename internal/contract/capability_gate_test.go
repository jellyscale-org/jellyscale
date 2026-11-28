package contract_test

// JS-183, criterion 4 and 5. A capability's state is as binding as its operations': a declared capability with a served
// operation, a preview capability with none, or any stable capability stops the server. Each fixture changes only the
// capability, leaving its operations as the embedded contract has them, so a build that checks operations alone fails.

import "testing"

func capabilityState(t *testing.T, id, state string) []byte {
	t.Helper()
	return contractWith(t, func(document map[string]any) {
		capabilities, _ := document["x-capabilities"].(map[string]any)
		entry, _ := capabilities[id].(map[string]any)
		entry["state"] = state
	})
}

func TestServerRefusesToStartWithACapabilityDeclaredButServed(t *testing.T) {
	if !refuses(t, capabilityState(t, "track-preferences", "declared")) {
		t.Fatalf("the server started with track-preferences declared while it serves track preferences")
	}
}

func TestServerRefusesToStartWithACapabilityPreviewButNotServed(t *testing.T) {
	if !refuses(t, capabilityState(t, "sign-in", "preview")) {
		t.Fatalf("the server started with sign-in preview while it serves no sign-in")
	}
}

func TestServerRefusesToStartWithAStableCapabilityState(t *testing.T) {
	if !refuses(t, capabilityState(t, "track-preferences", "stable")) {
		t.Fatalf("the server started with a stable capability and no compatibility gate")
	}
}
