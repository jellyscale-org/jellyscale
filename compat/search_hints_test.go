package compat

import (
	"os"
	"strings"
	"testing"
)

// JS-100: GET /Search/Hints is served, and the audit, the route list and the
// status page say so.
func TestSearchHintsIsAuditedAsDone(t *testing.T) {
	var found bool
	for _, r := range readRows(t) {
		if r.id() != "GET /Search/Hints" {
			continue
		}
		found = true
		if r.class != "done" {
			t.Errorf("GET /Search/Hints is %s, want done", r.class)
		}
		if strings.Contains(r.evidence, "nothing calls it") {
			t.Errorf("the evidence still says nothing calls it: %q", r.evidence)
		}
	}
	if !found {
		t.Fatal("no row for GET /Search/Hints")
	}
	if status := manifest()["GET /search/hints"]; status != "implemented" {
		t.Errorf("the API serves GET /Search/Hints as %q, want implemented", status)
	}
	endpoints, err := os.ReadFile("endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(endpoints), `{method: GET, path: "/Search/Hints", status: implemented, role: api, access: viewer}`) {
		t.Error("compat/endpoints.yaml does not list GET /Search/Hints as implemented")
	}
}
