//go:build integration

package api

// D-20261127-contract-amend, item 3: the operator's detail for a v1 refusal goes to the log, not just the closed reason
// in the answer. A viewer's own mistake is a warning, a failure inside the server is an error, and neither the token nor
// anything from the request's body or query is written.

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

func TestV1RefusalsLogTheirDetailOnceWithoutTheRequest(t *testing.T) {
	kit := newEnv(t)
	var log refusalLog
	kit.apis[0].Log = slog.New(slog.NewTextHandler(&log, nil))
	const bodySecret = "body-secret-should-not-be-logged"
	const querySecret = "query-secret-should-not-be-logged"
	cases := []struct {
		name, method, path, token, reason, detail string
		body                                      any
	}{
		{"invalid body", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, "invalid-body", "the body is not valid for track preferences", json.RawMessage(`{"NoSuchField":"` + bodySecret + `"}`)},
		{"no language saved", http.MethodPatch, "/jellyscale/v1/track-preferences", kit.token, "language-not-saved", "HideOtherAudio needs AudioLanguagePreference", json.RawMessage(`{"HideOtherAudio":true}`)},
		{"invalid playback body", http.MethodPost, "/jellyscale/v1/items/" + kit.item + "/playback-info", kit.token, "invalid-body", "invalid body", json.RawMessage(`[]`)},
		{"no token", http.MethodGet, "/jellyscale/v1/track-preferences", "", "unauthorized", "unauthorized", nil},
		{"credential only in the query", http.MethodGet, "/jellyscale/v1/track-preferences?ApiKey=" + querySecret, "", "unauthorized", "unauthorized", nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			if response.StatusCode == http.StatusOK {
				t.Fatalf("the base request was not a refusal: %d %.120q", response.StatusCode, raw)
			}
			text := log.String()
			if got := refusalLineCount(text); got != i+1 {
				t.Fatalf("%d refusal lines after %d refusals: %s", got, i+1, text)
			}
			for _, want := range []string{`level=WARN`, `msg="v1 refusal"`, "reason=" + tc.reason, tc.detail} {
				if !strings.Contains(text, want) {
					t.Errorf("the log has no %q: %s", want, text)
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
		Roles: []string{RoleAPI}, Log: slog.New(slog.NewTextHandler(&log, nil))}
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
	for _, want := range []string{`level=ERROR`, `msg="v1 refusal"`, "status=500", "reason=server-failure", "detail=\"internal error\""} {
		if !strings.Contains(text, want) {
			t.Errorf("the log has no %q: %s", want, text)
		}
	}
}
