package api

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// JS-118: GET and PATCH /jellyscale/v1/track-preferences, the first routes of jellyscale's own namespace. These checks
// need nothing outside Go; the behaviour is held by cmd/jellyscale/track_preferences_integration_test.go and
// track_preferences_integration_test.go in this package. Access ("viewer") is held for every route by TestAccess, which
// this task changes none of.

var trackPreferenceRoutes = []string{"GET /jellyscale/v1/track-preferences", "PATCH /jellyscale/v1/track-preferences"}

func TestTheTrackPreferenceRoutesAreTheOnlyOnesUnderJellyscaleAndAreViewerRoutes(t *testing.T) {
	found := map[string]Route{}
	for _, route := range (&Server{}).Routes() {
		if strings.HasPrefix(strings.ToLower(route.Path), "/jellyscale/") {
			found[routeName(route)] = route
		}
	}
	if len(found) != 2 {
		t.Errorf("%d routes under /jellyscale/, want exactly the two of the track preferences: %v", len(found), found)
	}
	for _, name := range trackPreferenceRoutes {
		route, ok := found[name]
		switch {
		case !ok:
			t.Errorf("no route %s", name)
		case route.Status != Implemented || route.Access != AccessViewer || route.Why != "":
			t.Errorf("%s: status %q, access %q, why %q; want implemented, viewer and no why", name, route.Status, route.Access, route.Why)
		case strings.Contains(route.Path, "{"):
			t.Errorf("%s takes a parameter: the route acts on the caller and takes no user id", name)
		}
	}
}

func TestEndpointsYamlListsExactlyTheTwoTrackPreferenceRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../compat/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "/jellyscale/") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	want := []string{
		`- {method: GET, path: "/jellyscale/v1/track-preferences", status: implemented, role: api, access: viewer}`,
		`- {method: PATCH, path: "/jellyscale/v1/track-preferences", status: implemented, role: api, access: viewer}`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("compat/endpoints.yaml lists under /jellyscale/:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheAccessDocumentHasARowForEachTrackPreferenceRoute(t *testing.T) {
	rows := map[string][]string{}
	for _, row := range accessRows(t) {
		rows[row.key] = row.cells
	}
	for _, name := range trackPreferenceRoutes {
		cells, ok := rows[name]
		if !ok {
			t.Errorf("docs/access.md has no row for %s", name)
		} else if len(cells) >= 4 && (cells[2] != "viewer" || !strings.Contains(strings.ToLower(cells[3]), "own")) {
			t.Errorf("docs/access.md: the row of %s requires %q with scope %q, want viewer and the caller's own", name, cells[2], cells[3])
		}
	}
}

// docs/api.md is a new file. A reader can only check the words; this holds that the pieces the criteria list are named.
func TestTheApiDocumentStatesTheNamespaceAndTheRoute(t *testing.T) {
	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatalf("docs/api.md: %v", err)
	}
	text := string(raw)
	lower := strings.ToLower(text)
	for _, word := range []string{
		"/jellyscale/v1/", "version", "GET /jellyscale/v1/track-preferences", "PATCH /jellyscale/v1/track-preferences",
		"HideOtherAudio", "HideOtherSubtitles", "KeepAudioLanguages", "KeepSubtitleLanguages", "ShowAllTracks",
		"AudioLanguagePreference", "SubtitleLanguagePreference", "400", "409", "401",
	} {
		if !strings.Contains(text, word) {
			t.Errorf("docs/api.md does not say %q", word)
		}
	}
	for _, word := range []string{"default", "lowercase", "three-letter", "order", "request", "answer", "20", "16 kb"} {
		if !strings.Contains(lower, word) {
			t.Errorf("docs/api.md does not say %q", word)
		}
	}
}

// docs/status.md says the settings are saved, and that JS-119 makes them act.
func TestStatusSaysTheTrackPreferencesAreSavedAndJS119MakesThemAct(t *testing.T) {
	raw, err := os.ReadFile("../../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	at := strings.Index(text, "/jellyscale/v1/track-preferences")
	if at < 0 {
		t.Fatal("docs/status.md does not name /jellyscale/v1/track-preferences")
	}
	passage := text[max(0, at-600):min(len(text), at+900)]
	for _, word := range []string{"saved", "js-119"} {
		if !strings.Contains(passage, word) {
			t.Errorf("the passage of docs/status.md on the track preferences does not say %q:\n%s", word, passage)
		}
	}
}

// Jellyfin's answers are not changed: the differential tier lists no difference for the new settings, and the
// names of the five fields appear in none of the 16 of UserConfiguration.
func TestNoDifferenceIsListedForTheTrackPreferences(t *testing.T) {
	raw, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if bad := regexp.MustCompile(`(?i)track-?preferences|jellyscale/v1|HideOther(Audio|Subtitles)|Keep(Audio|Subtitle)Languages|ShowAllTracks`).Find(raw); bad != nil {
		t.Errorf("compat/differences.yaml mentions %q: no Jellyfin route answers differently for these settings", bad)
	}
	for _, field := range reflectFieldNames(UserConfiguration{}) {
		if regexp.MustCompile(`HideOther|KeepAudio|KeepSubtitle|ShowAllTracks`).MatchString(field) {
			t.Errorf("UserConfiguration has the field %s: jellyscale's additions stay out of Jellyfin's answers", field)
		}
	}
}

func reflectFieldNames(v any) []string {
	var names []string
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		names = append(names, t.Field(i).Name)
	}
	return names
}
