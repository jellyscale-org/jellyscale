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
	// JS-120 adds the marked PlaybackInfo, JS-123 PATCH /jellyscale/v1/users/configuration (users_configuration_patch_test.go).
	if len(found) != 4 {
		t.Errorf("%d routes under /jellyscale/, want the two of the track preferences, the marked PlaybackInfo and the users' configuration: %v", len(found), found)
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

func TestEndpointsYamlListsExactlyTheRoutesUnderJellyscale(t *testing.T) {
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
		`- {method: POST, path: "/jellyscale/v1/items/{itemId}/playback-info", status: implemented, role: api, access: viewer}`,
		`- {method: GET, path: "/jellyscale/v1/track-preferences", status: implemented, role: api, access: viewer}`,
		`- {method: PATCH, path: "/jellyscale/v1/track-preferences", status: implemented, role: api, access: viewer}`,
		`- {method: PATCH, path: "/jellyscale/v1/users/configuration", status: implemented, role: api, access: viewer}`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("compat/endpoints.yaml lists under /jellyscale/:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
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
