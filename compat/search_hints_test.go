package compat

import (
	"os"
	"strings"
	"testing"
)

// JS-100: GET /Search/Hints is served, and the audit, the route list and the
// status page say so. Fluster (#102) calls it while the viewer types.
func TestSearchHintsIsAuditedAsDoneOnFlustersCall(t *testing.T) {
	var found bool
	for _, r := range readRows(t) {
		if r.id() != "GET /Search/Hints" {
			continue
		}
		found = true
		if r.class != "done" {
			t.Errorf("GET /Search/Hints is %s, want done", r.class)
		}
		if !strings.Contains(r.evidence, "#102") || !strings.Contains(r.evidence, "Fluster") {
			t.Errorf("the evidence does not name Fluster's call (#102): %q", r.evidence)
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
	if !strings.Contains(string(endpoints), `{method: GET, path: "/Search/Hints", status: implemented, role: api}`) {
		t.Error("compat/endpoints.yaml does not list GET /Search/Hints as implemented")
	}
}

func TestStatusPageNoLongerListsSearchHintsAmongTheRoutesAnswered404(t *testing.T) {
	status, err := os.ReadFile("../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(status), "\n") {
		if strings.Contains(l, "answered 404:") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("docs/status.md has no line listing routes answered 404")
	}
	if strings.Contains(line, "/Search/Hints") {
		t.Error("docs/status.md still lists /Search/Hints among the routes answered 404")
	}
}
