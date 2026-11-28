//go:build integration

package api

// D-20261127-contract-amend, item 3. A refusal's Detail is for the app developer, not the viewer or the log. A viewer's
// own mistake is logged only at Debug; a failure inside the server is an Error. Neither the token nor a value from the
// request's body or query reaches either.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/testenv"
)

type refusalLog struct {
	mu   sync.Mutex
	text bytes.Buffer
}

func (l *refusalLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *refusalLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func refusalLineCount(text string) int { return strings.Count(text, `msg="v1 refusal"`) }

func refusalDetail(t *testing.T, raw []byte) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if rawDetail, ok := fields["detail"]; ok {
		var detail string
		if err := json.Unmarshal(rawDetail, &detail); err != nil {
			t.Fatal(err)
		}
		return detail
	}
	return ""
}

func TestV1RefusalsLogMistakesAtDebugAndCarryDetailOnlyInTheAnswer(t *testing.T) {
	kit := newEnv(t)
	var log refusalLog
	kit.apis[0].Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const bodySecret = "body-secret-should-not-be-logged"
	const querySecret = "query-secret-should-not-be-logged"
	cases := []struct {
		name, method, path, token, reason, detail string
		body                                      any
	}{
		{"invalid body", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, "invalid-body", "NoSuchField", json.RawMessage(`{"NoSuchField":"` + bodySecret + `"}`)},
		{"no language saved", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, "language-not-saved", "AudioLanguagePreference", json.RawMessage(`{"HideOtherAudio":true}`)},
		{"invalid playback body", http.MethodPost, "/jellyscale/v1/items/" + kit.item + "/playback-info", kit.token, "invalid-body", "", json.RawMessage(`[]`)},
		{"no token", http.MethodGet, "/jellyscale/v1/track-preferences", "", "unauthorized", "", nil},
		{"credential only in the query", http.MethodGet, "/jellyscale/v1/track-preferences?ApiKey=" + querySecret, "", "unauthorized", "", nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			if response.StatusCode == http.StatusOK {
				t.Fatalf("the base request was not a refusal: %d %.120q", response.StatusCode, raw)
			}
			if detail := refusalDetail(t, raw); tc.detail == "" && detail != "" {
				t.Errorf("the refusal carries Detail %q, want none: %.160q", detail, raw)
			} else if tc.detail != "" && !strings.Contains(detail, tc.detail) {
				t.Errorf("the refusal's Detail %q does not say %q: %.160q", detail, tc.detail, raw)
			}
			text := log.String()
			if got := refusalLineCount(text); got != i+1 {
				t.Fatalf("%d refusal lines after %d refusals: %s", got, i+1, text)
			}
			for _, want := range []string{`level=DEBUG`, `msg="v1 refusal"`, "reason=" + tc.reason} {
				if !strings.Contains(text, want) {
					t.Errorf("the log has no %q: %s", want, text)
				}
			}
			for _, above := range []string{`level=INFO`, `level=WARN`, `level=ERROR`} {
				if strings.Contains(text, above+` msg="v1 refusal"`) {
					t.Errorf("a viewer's mistake was logged at %s: %s", above, text)
				}
			}
		})
	}
	text := log.String()
	for _, secret := range []string{kit.token, bodySecret, querySecret} {
		if strings.Contains(text, secret) {
			t.Errorf("the log carries the request value %q: %s", secret, text)
		}
	}
}

func TestAServerFailureRefusalLogsAtErrorLevel(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	var log refusalLog
	broken := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Name: "jellyscale", ServerID: "srv",
		Roles: []string{RoleAPI}, Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	server := httptest.NewServer(broken.Handler())
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/jellyscale/v1/track-preferences", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Emby-Token", "a-token-the-closed-pool-cannot-check")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a failure inside the server: %d", response.StatusCode)
	}
	text := log.String()
	if got := refusalLineCount(text); got != 1 {
		t.Fatalf("%d v1 refusal lines for one failure: %s", got, text)
	}
	for _, want := range []string{`level=ERROR`, `msg="v1 refusal"`, "status=500", "reason=server-failure"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log has no %q: %s", want, text)
		}
	}
	if strings.Contains(text, "detail=") {
		t.Errorf("the log carries Detail: %s", text)
	}
}
