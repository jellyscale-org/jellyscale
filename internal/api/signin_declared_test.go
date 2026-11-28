package api

// JS-223, criteria 2 and 3. Sign-in, sign-out and /me are declared until JS-184 serves them. Until then each answers the
// not-served refusal in the contract's envelope, and the answer gate sends a declared operation the reasons the contract
// gives it for that status, and no other. These are unit checks of the server's own answers: no Postgres is needed.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jellyscale/internal/contract"
)

func declaredServer() *Server {
	return &Server{Name: "jellyscale", ServerID: "srv", Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler)}
}

// envelopeOf decodes a refusal, whose members are all strings in the cases below.
func envelopeOf(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the answer %.160q is not an object of strings: %v", raw, err)
	}
	return fields
}

// Each declared operation answers not-served, as JSON with exactly reason and action, whatever the method and token.
func TestSignInSignOutAndMeAreDeclaredUntilServed(t *testing.T) {
	site := httptest.NewServer(declaredServer().Handler())
	t.Cleanup(site.Close)
	cases := []struct{ name, method, path, token string }{
		{"sign-in, no token", http.MethodPost, "/jellyscale/v1/sign-in", ""},
		{"sign-out, with a token", http.MethodPost, "/jellyscale/v1/sign-out", "a-token"},
		{"me, no token", http.MethodGet, "/jellyscale/v1/me", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, site.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.token != "" {
				request.Header.Set("Authorization", "Bearer "+tc.token)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s: %d %.120q, want 404 not-served", tc.method, tc.path, response.StatusCode, raw)
			}
			if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Fatalf("content type %q, want application/json: %.120q", got, raw)
			}
			fields := envelopeOf(t, raw)
			if len(fields) != 2 || fields["reason"] != "not-served" || fields["action"] != "none" {
				t.Fatalf("the answer %s is not the not-served refusal", raw)
			}
		})
	}
}

// The gate sends a declared reason for its status with the action the contract gives it, and nothing else.
func TestTheSignInRefusalsAreAnsweredAsDeclared(t *testing.T) {
	cases := []struct {
		name, method, path string
		status             int
		reason, action     string
	}{
		{"sign-in, wrong name or password", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusUnauthorized, "credentials-not-accepted", "retry"},
		{"sign-in, too many tries", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusTooManyRequests, "too-many-tries", "retry"},
		{"sign-in, account turned off", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusForbidden, "account-off", "ask-operator"},
		{"sign-in, bad body", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusBadRequest, "invalid-body", "none"},
		{"sign-in, failure inside the server", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusInternalServerError, "server-failure", "retry"},
		{"sign-out, no token", http.MethodPost, "/jellyscale/v1/sign-out", http.StatusUnauthorized, "unauthorized", "sign-in"},
		{"me, no token", http.MethodGet, "/jellyscale/v1/me", http.StatusUnauthorized, "unauthorized", "sign-in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			declaredServer().refuseV1(recorder, request, tc.status, Reason(tc.reason), "a fixed text")
			if recorder.Code != tc.status {
				t.Fatalf("%s %s answered %d %.160q, want %d %s", tc.method, tc.path, recorder.Code, recorder.Body.String(), tc.status, tc.reason)
			}
			fields := envelopeOf(t, recorder.Body.Bytes())
			if len(fields) != 2 || fields["reason"] != tc.reason || fields["action"] != tc.action {
				t.Fatalf("the answer is %v, want reason %s with action %s and no other member", fields, tc.reason, tc.action)
			}
		})
	}
}

// Guard, passing before and after the change: the gate refuses a reason the contract does not give that operation and
// status. It answers the server-failure 500 instead, and the refused reason's text is never sent, whatever the keys are
// spelled. A contract that copied one operation's reasons onto every operation would send these reasons.
func TestSignInAnswersOnlyTheRefusalsItDeclares(t *testing.T) {
	cases := []struct {
		name, method, path string
		status             int
		reason             string
	}{
		{"sign-in, unauthorized", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusUnauthorized, "unauthorized"},
		{"sign-in, credentials at 403", http.MethodPost, "/jellyscale/v1/sign-in", http.StatusForbidden, "credentials-not-accepted"},
		{"sign-out, account turned off", http.MethodPost, "/jellyscale/v1/sign-out", http.StatusForbidden, "account-off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			declaredServer().refuseV1(recorder, request, tc.status, Reason(tc.reason), "a fixed text")
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("%s %s with %s answered %d %.160q, want the undeclared 500", tc.method, tc.path, tc.reason, recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), tc.reason) {
				t.Fatalf("%s %s sent the undeclared reason %s: %.160q", tc.method, tc.path, tc.reason, recorder.Body.String())
			}
		})
	}
}

// JS-223, criterion 2 and the reviewer's item 7. The action a sign-in refusal carries is the one the contract gives its
// reason. checkContract compares actions for the transport reasons only, so the three reasons sign-in adds are checked here.
func TestTheSignInReasonsSendTheActionTheContractGivesThem(t *testing.T) {
	cases := []struct {
		method, path, reason string
		status               int
	}{
		{http.MethodPost, "/jellyscale/v1/sign-in", "credentials-not-accepted", http.StatusUnauthorized},
		{http.MethodPost, "/jellyscale/v1/sign-in", "too-many-tries", http.StatusTooManyRequests},
		{http.MethodPost, "/jellyscale/v1/sign-in", "account-off", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			given, listed := contract.Default().Reasons[tc.reason]
			if !listed || given.Kind != "refusal" {
				t.Fatalf("the contract does not list %s as a refusal", tc.reason)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			declaredServer().refuseV1(recorder, request, tc.status, Reason(tc.reason), "a fixed text")
			fields := envelopeOf(t, recorder.Body.Bytes())
			if recorder.Code != tc.status || fields["reason"] != tc.reason || fields["action"] != given.Action {
				t.Fatalf("%s answered %d %v, want %d with the contract's action %q", tc.reason, recorder.Code, fields, tc.status, given.Action)
			}
		})
	}
}
