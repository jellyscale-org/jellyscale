package compat

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// JS-116: the three routes that save and list a viewer's settings are built, and the books say so.

var userConfigurationRoutes = []struct{ method, path, status string }{
	{"POST", "/Users/Configuration", "implemented"},
	{"POST", "/Users/{userId}/Configuration", "alias"},
	{"GET", "/Localization/Cultures", "implemented"},
}

func TestTheSettingsRoutesAreDoneInTheCoverageTable(t *testing.T) {
	byID := map[string]row{}
	for _, r := range readRows(t) {
		byID[r.id()] = r
	}
	for _, want := range userConfigurationRoutes {
		r, ok := byID[want.method+" "+want.path]
		if !ok {
			t.Fatalf("no row for %s %s", want.method, want.path)
		}
		if r.class != "done" || r.slice != "-" {
			t.Errorf("%s: class %q slice %q, want done and -", r.id(), r.class, r.slice)
		}
	}
}

func TestTheSettingsRoutesAreServedWithTheirStatus(t *testing.T) {
	served := manifest()
	for _, want := range userConfigurationRoutes {
		if got := served[want.method+" "+normalize(want.path)]; got != want.status {
			t.Errorf("%s %s is %q in the routes jellyscale serves, want %q", want.method, want.path, got, want.status)
		}
	}
}

// endpoints.yaml is generated (go run ./cmd/jellyscale --print-routes); a stale file is as wrong as a missing route.
func TestEndpointsYamlHoldsTheSettingsRoutes(t *testing.T) {
	raw, err := os.ReadFile("endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range userConfigurationRoutes {
		line := regexp.MustCompile(`(?m)^  - \{method: ` + want.method + `, path: "` + regexp.QuoteMeta(want.path) + `", status: (\w+),`).FindStringSubmatch(string(raw))
		if line == nil || line[1] != want.status {
			t.Errorf("compat/endpoints.yaml has no line for %s %s with status %s: %v", want.method, want.path, want.status, line)
		}
	}
}

func TestTheObsoleteSaveRouteStandsForTheCurrentOne(t *testing.T) {
	raw, err := os.ReadFile("conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := `{method: POST, route: "/Users/{userId}/Configuration", stands_for: "/Users/Configuration"}`
	if !strings.Contains(string(raw), want) {
		t.Errorf("compat/conform.yaml has no alias %s", want)
	}
}

// Jellyfin answers the same CastReceiverId to every user, so jellyscale does too and no difference is left to list.
func TestNoDifferenceIsKeptForCastReceiverId(t *testing.T) {
	raw, err := os.ReadFile("differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CastReceiverId") {
		t.Error("compat/differences.yaml still names CastReceiverId")
	}
}

// docs/status.md says which settings are saved and which act, and which task makes the rest act.
func TestStatusSaysTheSettingsAreSavedAndWhichActOn(t *testing.T) {
	raw, err := os.ReadFile("../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	status := string(raw)
	for _, stale := range []string{
		"User management: create, delete, disable, change a password, preferences: **not built**",
		"which this slice does not have",
	} {
		if strings.Contains(status, stale) {
			t.Errorf("docs/status.md still says %q", stale)
		}
	}
	if regexp.MustCompile(`(?i)preferences[^\n]*not built`).MatchString(status) {
		t.Error("docs/status.md still says preferences are not built")
	}
	line := ""
	for _, l := range strings.Split(status, "\n") {
		if strings.Contains(l, "AudioLanguagePreference") && strings.Contains(l, "JS-121") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("docs/status.md has no line that names AudioLanguagePreference and JS-121")
	}
	for _, want := range []string{"saved", "PlayDefaultAudioTrack", "SubtitleMode", "SubtitleLanguagePreference", "JS-041"} {
		if !strings.Contains(line, want) {
			t.Errorf("the settings line of docs/status.md lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "not built") {
		t.Errorf("the settings line of docs/status.md says not built: %s", line)
	}
}
