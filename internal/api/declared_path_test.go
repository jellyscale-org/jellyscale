package api

// A declared operation with no handler can share a path with a served one. The mux has no pattern for that method, so the
// dispatcher must answer the contract's not-served refusal rather than let the request fall through to its text catch-all.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"jellyscale/internal/contract"
)

func declaredPathServer(t *testing.T) *httptest.Server {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(contract.Document(), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	paths["/jellyscale/v1/server"].(map[string]any)["post"] = map[string]any{
		"operationId":  "postServer",
		"x-state":      "declared",
		"x-capability": "server-write",
		"responses": map[string]any{
			"404": map[string]any{"$ref": "#/components/responses/NotServed"},
		},
	}
	document["x-capabilities"].(map[string]any)["server-write"] = map[string]any{
		"id": "server-write", "state": "declared", "meaning": "A device writes its own server settings.",
	}
	raw, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := contract.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Name: "jellyscale", ServerID: "srv", Roles: []string{RoleAPI}, Log: slog.New(slog.DiscardHandler),
		Contract: parsed}
	site := httptest.NewServer(server.Handler())
	t.Cleanup(site.Close)
	return site
}

func TestADeclaredOperationOnAServedPathAnswersNotServed(t *testing.T) {
	site := declaredPathServer(t)
	response, err := http.Post(site.URL+"/jellyscale/v1/server", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("a declared method on a served path: %d %.120q, want 404", response.StatusCode, raw)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type %q, want application/json: %.120q", got, raw)
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["reason"] != "not-served" || fields["action"] != "none" {
		t.Fatalf("the answer %s is not the not-served refusal", raw)
	}

	response, err = http.Get(site.URL + "/jellyscale/v1/server")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the served GET on the same path: %d, want 200", response.StatusCode)
	}
}
