//go:build integration

package api

// JS-183, criterion 3: the refusals the table of contract_integration_test.go cannot see. Every viewer operation answers the
// sign-in refusal, as JSON, with no token. A PlaybackInfo the server refuses for want of a worker is a 200 with no sources, and
// the reason sits beside Jellyfin's ErrorCode. A failure inside the server answers 500 with the server-failure reason.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

// Every operation that needs a token answers no token with the sign-in refusal: JSON, exactly its two keys, and no cookie.
func TestEveryViewerOperationRefusesNoTokenWithTheSignInRefusal(t *testing.T) {
	kit := newEnv(t)
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"track preferences", http.MethodGet, "/jellyscale/v1/track-preferences", nil},
		{"track preferences saved", http.MethodPatch, "/jellyscale/v1/track-preferences", json.RawMessage(`{"HideOtherAudio":false}`)},
		{"configuration saved", http.MethodPatch, "/jellyscale/v1/users/configuration", json.RawMessage(`{"AudioLanguagePreference":"deu"}`)},
		{"playback info", http.MethodPost, "/jellyscale/v1/items/" + kit.item + "/playback-info", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, "", tc.body)
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with no token: %d %.120q, want 401", tc.method, tc.path, response.StatusCode, raw)
			}
			noCookie(t, response)
			if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type %q, want application/json", contentType)
			}
			if !isSignInRefusal(raw) {
				t.Fatalf("the body %.160q is not the sign-in refusal {reason: unauthorized, action: sign-in}", raw)
			}
		})
	}
}

// A PlaybackInfo that needs a transcode when no worker runs is a 200 with no sources. Its ErrorCode is Jellyfin's, and beside it
// the reason and its action are jellyscale's (criterion 3), the same answer on every replica.
func TestPlaybackInfoWithNoWorkerAnswersTheCapacityReason(t *testing.T) {
	kit, source, _ := decisionEnv(t, refusalCase{}.film()) // the film the no-worker case of decision_refusals_integration_test.go asks for
	mustExec(t, kit.pool, "TRUNCATE transcode_nodes")
	body := chrome(t)
	body["MediaSourceId"] = source
	response, raw := kit.do(t, 0, http.MethodPost, "/jellyscale/v1/items/"+kit.item+"/playback-info", kit.token, body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo with no worker: %d %.160q, want 200", response.StatusCode, raw)
	}
	noCookie(t, response)
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("PlaybackInfo is not a JSON object: %v", err)
	}
	var sources []json.RawMessage
	if err := json.Unmarshal(reply["MediaSources"], &sources); err != nil || len(sources) != 0 {
		t.Fatalf("PlaybackInfo with no worker lists sources: %.300q", raw)
	}
	if code := stringField(t, reply, "ErrorCode"); code != "RateLimitExceeded" {
		t.Fatalf("ErrorCode %q, want RateLimitExceeded: %.300q", code, raw)
	}
	if _, ok := reply["reason"]; !ok {
		t.Fatalf("the capacity refusal in the 200 body has no reason: %.300q", raw)
	}
	if reason, action := stringField(t, reply, "reason"), stringField(t, reply, "action"); reason != "no-capacity" || action != "retry" {
		t.Fatalf("the capacity refusal says reason %q and action %q, want no-capacity and retry: %.300q", reason, action, raw)
	}
}

// A failure inside a handler answers 500 with the server-failure reason. A pool that is closed fails every query at once, so the
// failure is made without an outage to wait out.
func TestAFailureInsideTheServerAnswersTheServerFailureReason(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	broken := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Name: "jellyscale", ServerID: "srv",
		Roles: []string{RoleAPI}, Log: failureLog(t)}
	server := httptest.NewServer(broken.Handler())
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/jellyscale/v1/track-preferences", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Emby-Token", "a-token-the-broken-pool-cannot-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a failure inside the server: %d %.120q, want 500", response.StatusCode, raw)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content type %q, want application/json", contentType)
	}
	fields := exactKeys(t, raw, []string{"reason", "action"}, "retryAfterSeconds")
	if reason, action := stringField(t, fields, "reason"), stringField(t, fields, "action"); reason != "server-failure" || action != "retry" {
		t.Fatalf("the 500 says reason %q and action %q, want server-failure and retry", reason, action)
	}
}
