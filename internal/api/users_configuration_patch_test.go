package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// JS-123: PATCH /jellyscale/v1/users/configuration. These checks need nothing outside Go; the behaviour is held by
// cmd/jellyscale/users_configuration_patch_integration_test.go.

const patchConfigurationRoute = "PATCH /jellyscale/v1/users/configuration"

func TestThePatchOfTheUsersConfigurationIsAViewerRouteWithNoUserId(t *testing.T) {
	var found *Route
	for _, route := range (&Server{}).Routes() {
		if routeName(route) == patchConfigurationRoute {
			found = &route
		}
	}
	switch {
	case found == nil:
		t.Fatalf("no route %s", patchConfigurationRoute)
	case found.Status != Implemented || found.Access != AccessViewer || found.Why != "":
		t.Errorf("%s: status %q, access %q, why %q; want implemented, viewer and no why", patchConfigurationRoute, found.Status, found.Access, found.Why)
	case strings.Contains(found.Path, "{"):
		t.Errorf("%s takes a parameter: the route acts on the caller and takes no user id", patchConfigurationRoute)
	}
}

func TestTheAccessDocumentHasARowForThePatchOfTheUsersConfiguration(t *testing.T) {
	for _, row := range accessRows(t) {
		if row.key != patchConfigurationRoute {
			continue
		}
		if len(row.cells) >= 4 && (row.cells[2] != "viewer" || !strings.Contains(strings.ToLower(row.cells[3]), "own")) {
			t.Errorf("docs/access.md: the row requires %q with scope %q, want viewer and the caller's own", row.cells[2], row.cells[3])
		}
		return
	}
	t.Errorf("docs/access.md has no row for %s", patchConfigurationRoute)
}

// docs/api.md has a section of the route, with the pieces the criteria list. A reader can only check the words.
func TestTheApiDocumentDocumentsThePatchOfTheUsersConfiguration(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	at := strings.Index(text, "### "+patchConfigurationRoute)
	if at < 0 {
		t.Fatalf("docs/api.md has no section %q", "### "+patchConfigurationRoute)
	}
	section := text[at+4:]
	if next := strings.Index(section, "\n### "); next >= 0 {
		section = section[:next]
	}
	lower := strings.ToLower(section)
	for _, word := range []string{"request", "answer", "400", "401", "200", "CastReceiverId", "null", "SubtitleMode", "later"} {
		if !strings.Contains(strings.ToLower(section), strings.ToLower(word)) {
			t.Errorf("the section of %s does not say %q", patchConfigurationRoute, word)
		}
	}
	if !regexp.MustCompile(`65,?536|64 ?kb`).MatchString(lower) {
		t.Errorf("the section of %s does not say the size limit of a body (65,536 bytes)", patchConfigurationRoute)
	}
	// The limit: a whole-object save from an older read (jellyfin-web's) can still undo a change.
	if !strings.Contains(lower, "jellyfin-web") || !strings.Contains(lower, "older") {
		t.Errorf("the section of %s does not say that a whole-object save from an older read (jellyfin-web's) can still undo a change", patchConfigurationRoute)
	}
}
