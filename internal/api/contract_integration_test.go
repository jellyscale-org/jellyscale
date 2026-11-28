//go:build integration

package api

// JS-183, criteria 1 to 3. The contract is served to anyone. Identity is answered with no token and lists every capability
// with its state. Every refusal on /jellyscale/v1/ is a JSON object whose keys are exactly the ones the envelope gives, whose
// reason is on the contract's list as a refusal, and whose action is the one the list gives it. The oracle is the contract the
// server serves, never the file. Each answer is also asked for a cookie: a token never travels in one (DOMAINS invariant 63).

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releasedReasons are the transport reasons this release locks. A later release may add to the list; removing or renaming
// one is a break (D-20261127-contract-amend, item 2), so the list is held here, not read from the contract it guards.
// Reasons used only by preview capabilities may be reworded until their capability becomes stable.
var releasedReasons = []string{"unauthorized", "not-found", "method-not-allowed", "server-failure"}

// reasonInfo is one entry of the contract's x-reasons list.
type reasonInfo struct{ Kind, Action string }

// servedContract reads GET /jellyscale/v1/contract, which is open to anyone, and returns it as a generic document.
func servedContract(t *testing.T, kit *env) map[string]any {
	t.Helper()
	response, raw := kit.do(t, 0, http.MethodGet, "/jellyscale/v1/contract", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /jellyscale/v1/contract with no token: %d %.120q, want 200", response.StatusCode, raw)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("the served contract is not YAML: %v", err)
	}
	return document
}

// reasonsOf reads the contract's closed list of reasons, by code.
func reasonsOf(t *testing.T, document map[string]any) map[string]reasonInfo {
	t.Helper()
	listed, ok := document["x-reasons"].(map[string]any)
	if !ok || len(listed) == 0 {
		t.Fatalf("the served contract has no x-reasons list")
	}
	reasons := map[string]reasonInfo{}
	for code, entry := range listed {
		fields, _ := entry.(map[string]any)
		kind, _ := fields["kind"].(string)
		action, _ := fields["action"].(string)
		reasons[code] = reasonInfo{Kind: kind, Action: action}
	}
	return reasons
}

// exactKeys decodes a JSON object whose keys are the required ones and the optional ones, and no others. The keys are exact:
// a decoder that folds case would take "reason" for "Reason", which the contract does not give.
func exactKeys(t *testing.T, raw []byte, required []string, optional ...string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("not a JSON object: %v: %.160q", err, raw)
	}
	allowed := append(slices.Clone(required), optional...)
	for key := range fields {
		if !slices.Contains(allowed, key) {
			t.Fatalf("the key %q is not one the contract gives: %.160q", key, raw)
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			t.Fatalf("the key %q is missing: %.160q", key, raw)
		}
	}
	return fields
}

// stringField reads one key of an exact object as a string.
func stringField(t *testing.T, fields map[string]json.RawMessage, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(fields[key], &value); err != nil {
		t.Fatalf("%s is not a string: %v", key, err)
	}
	return value
}

// isSignInRefusal reports whether raw is exactly the sign-in refusal: the keys Reason and Action, and no others.
func isSignInRefusal(raw []byte) bool {
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	return len(fields) == 2 && fields["Reason"] == "unauthorized" && fields["Action"] == "sign-in"
}

// noCookie fails if the answer sets a cookie.
func noCookie(t *testing.T, response *http.Response) {
	t.Helper()
	if cookies := response.Header.Values("Set-Cookie"); len(cookies) > 0 {
		t.Fatalf("the answer sets a cookie: %q", cookies)
	}
}

// allowedSet reads an Allow header as the set of methods it names.
func allowedSet(value string) map[string]bool {
	methods := map[string]bool{}
	for _, method := range strings.Split(value, ",") {
		methods[strings.ToUpper(strings.TrimSpace(method))] = true
	}
	return methods
}

func TestContractIsServedWithNoTokenAsAVersionedOpenAPI31Document(t *testing.T) {
	kit := newEnv(t)
	response, _ := kit.do(t, 0, http.MethodGet, "/jellyscale/v1/contract", "", nil)
	noCookie(t, response)
	document := servedContract(t, kit)
	if version, _ := document["openapi"].(string); !strings.HasPrefix(version, "3.1") {
		t.Fatalf("openapi %q, want 3.1.x", version)
	}
	info, _ := document["info"].(map[string]any)
	if version, _ := info["version"].(string); version == "" {
		t.Fatalf("the contract has no info.version")
	}
	if paths, _ := document["paths"].(map[string]any); len(paths) == 0 {
		t.Fatalf("the contract describes no path")
	}
	reasonsOf(t, document)
}

// GET /jellyscale/v1/server answers, with no token, the server's name, id, version and API version, and every capability the
// contract gives with its state. Preview and declared capabilities are listed; none is stable in this release.
func TestServerIdentityIsAnsweredWithNoTokenWithEveryCapabilityAndItsState(t *testing.T) {
	kit := newEnv(t)
	response, raw := kit.do(t, 0, http.MethodGet, "/jellyscale/v1/server", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /jellyscale/v1/server with no token: %d %.120q, want 200", response.StatusCode, raw)
	}
	noCookie(t, response)
	fields := exactKeys(t, raw, []string{"Name", "Id", "Version", "ApiVersion", "Capabilities"})
	if name, id, version := stringField(t, fields, "Name"), stringField(t, fields, "Id"), stringField(t, fields, "Version"); name == "" || id == "" || version == "" {
		t.Fatalf("identity has an empty name, id or version: name %q, id %q, version %q", name, id, version)
	}
	if apiVersion := stringField(t, fields, "ApiVersion"); apiVersion != "v1" {
		t.Fatalf("ApiVersion %q, want v1", apiVersion)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(fields["Capabilities"], &entries); err != nil {
		t.Fatalf("Capabilities is not a list: %v", err)
	}
	states := map[string]string{}
	for _, entry := range entries {
		capability := exactKeys(t, entry, []string{"Id", "State"})
		states[stringField(t, capability, "Id")] = stringField(t, capability, "State")
	}
	want := map[string]string{"track-preferences": "preview", "user-configuration": "preview", "playback-info": "preview", "sign-in": "declared"}
	if !maps.Equal(states, want) {
		t.Fatalf("capabilities %v, want %v", states, want)
	}
}

// Every refusal on /jellyscale/v1/ is JSON {Reason, Action, RetryAfterSeconds?}. The reason is on the contract's list as a refusal,
// and the action is the one the list gives it and the one named here. A 405 names its methods in Allow. A declared operation is
// refused as not served.
func TestEveryV1RefusalIsJSONWithAReasonOnTheContractsList(t *testing.T) {
	kit := newEnv(t)
	reasons := reasonsOf(t, servedContract(t, kit))
	cases := []struct {
		name, method, path, token string
		body                      any
		status                    int
		reason, action            string
	}{
		{"no token", http.MethodGet, "/jellyscale/v1/track-preferences", "", nil, http.StatusUnauthorized, "unauthorized", "sign-in"},
		{"unknown token", http.MethodGet, "/jellyscale/v1/track-preferences", "not-a-token", nil, http.StatusUnauthorized, "unauthorized", "sign-in"},
		{"unknown route", http.MethodGet, "/jellyscale/v1/no-such-route", kit.token, nil, http.StatusNotFound, "not-found", "none"},
		{"wrong method", http.MethodPut, "/jellyscale/v1/track-preferences", kit.token, map[string]any{}, http.StatusMethodNotAllowed, "method-not-allowed", "none"},
		{"declared, not served", http.MethodPost, "/jellyscale/v1/sign-in", "", map[string]any{}, http.StatusNotFound, "not-served", "none"},
		{"bad body", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, json.RawMessage(`{"HideOtherAudio":"yes"}`), http.StatusBadRequest, "invalid-body", "none"},
		{"another user's id", http.MethodPost, "/jellyscale/v1/items/" + kit.item + "/playback-info", kit.token, map[string]any{"UserId": kit.other}, http.StatusForbidden, "forbidden", "none"},
		{"unknown item", http.MethodPost, "/jellyscale/v1/items/00000000000000000000000000000000/playback-info", kit.token, map[string]any{}, http.StatusNotFound, "not-found", "none"},
		{"switch with no language saved", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, json.RawMessage(`{"HideOtherAudio":true}`), http.StatusConflict, "language-not-saved", "choose-another"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			if response.StatusCode != tc.status {
				t.Fatalf("%s %s: %d %.120q, want %d", tc.method, tc.path, response.StatusCode, raw, tc.status)
			}
			if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type %q, want application/json", contentType)
			}
			noCookie(t, response)
			if tc.status == http.StatusMethodNotAllowed {
				if methods := allowedSet(response.Header.Get("Allow")); !methods["GET"] || !methods["PATCH"] || methods["PUT"] {
					t.Fatalf("Allow %q, want the methods track preferences has: GET and PATCH", response.Header.Get("Allow"))
				}
			}
			fields := exactKeys(t, raw, []string{"Reason", "Action"}, "RetryAfterSeconds", "Detail")
			reason, action := stringField(t, fields, "Reason"), stringField(t, fields, "Action")
			listed, ok := reasons[reason]
			if !ok || reason != tc.reason {
				t.Fatalf("reason %q, want %q, which the contract lists: %v", reason, tc.reason, ok)
			}
			if listed.Kind != "refusal" || action != tc.action || listed.Action != tc.action {
				t.Fatalf("reason %q is kind %q with action %q on the list, and answered with %q; want a refusal with action %q",
					reason, listed.Kind, listed.Action, action, tc.action)
			}
		})
	}
}

// Criterion 3: within v1 a reason is never removed or renamed. Each released reason is still on the served list.
func TestEveryReleasedReasonIsStillOnTheContractsList(t *testing.T) {
	kit := newEnv(t)
	reasons := reasonsOf(t, servedContract(t, kit))
	for _, code := range releasedReasons {
		if _, ok := reasons[code]; !ok {
			t.Errorf("reason %q was released and is no longer on the contract's list", code)
		}
	}
}
