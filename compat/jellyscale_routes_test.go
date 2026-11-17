package compat

import (
	"slices"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

// JS-118: the routes of jellyscale's own namespace are in no Jellyfin document. Each is in the jellyscale-only list of
// coverage_test.go, has a row of coverage-12.4.tsv (done, served by itself), and is in conform.yaml's unchecked list
// with the reason, since the schema types nothing of them. JS-123 adds PATCH /jellyscale/v1/users/configuration, whose
// answer is a UserConfiguration: it is not unchecked but held to that schema (compat/conform/jellyscale_routes_test.go).
var jellyscaleRoutes = []struct{ method, path string }{
	{"GET", "/jellyscale/v1/track-preferences"}, {"PATCH", "/jellyscale/v1/track-preferences"},
	{"PATCH", "/jellyscale/v1/users/configuration"},
}

// uncheckedJellyscaleRoutes are the ones whose answer no schema types.
var uncheckedJellyscaleRoutes = jellyscaleRoutes[:2]

func TestTheJellyscaleRoutesAreInTheJellyscaleOnlyList(t *testing.T) {
	for _, want := range jellyscaleRoutes {
		if !slices.Contains(notInSpec, want.method+" "+want.path) {
			t.Errorf("%s %s is not in notInSpec (the jellyscale-only list of coverage_test.go)", want.method, want.path)
		}
	}
	served := 0
	for key := range manifest() {
		if strings.Contains(key, "/jellyscale/") {
			served++
		}
	}
	if served != 4 { // JS-120 adds POST /jellyscale/v1/items/{itemId}/playback-info, JS-123 PATCH /jellyscale/v1/users/configuration.
		t.Errorf("%d routes under /jellyscale/ are served, want 4", served)
	}
}

func TestTheJellyscaleRoutesHaveADoneRowOfTheirOwn(t *testing.T) {
	byID := map[string]row{}
	for _, r := range readRows(t) {
		byID[r.id()] = r
	}
	for _, want := range jellyscaleRoutes {
		r, ok := byID[want.method+" "+want.path]
		switch {
		case !ok:
			t.Errorf("no row for %s %s in coverage-12.4.tsv", want.method, want.path)
		case r.class != "done" || r.slice != "-" || r.servedBy != "-":
			t.Errorf("%s: class %q slice %q served_by %q, want done, - and -", r.id(), r.class, r.slice, r.servedBy)
		}
	}
}

func TestTheJellyscaleRoutesAreUncheckedInConformWithAReason(t *testing.T) {
	rules, err := conform.LoadRules("conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range uncheckedJellyscaleRoutes {
		found := false
		for _, u := range rules.Unchecked {
			if u.Method == want.method && u.Route == want.path {
				found = strings.TrimSpace(u.Reason) != ""
			}
		}
		if !found {
			t.Errorf("conform.yaml has no unchecked entry with a reason for %s %s", want.method, want.path)
		}
	}
}

// The route whose answer is a UserConfiguration is held to the schema, so it is not in the unchecked list.
func TestThePatchOfTheUsersConfigurationIsNotUncheckedInConform(t *testing.T) {
	rules, err := conform.LoadRules("conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range rules.Unchecked {
		if strings.EqualFold(u.Route, "/jellyscale/v1/users/configuration") {
			t.Errorf("conform.yaml lists %s %s as unchecked (%q): its answer is a UserConfiguration, which the schema types", u.Method, u.Route, u.Reason)
		}
	}
}
