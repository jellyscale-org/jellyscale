package compat

import (
	"os"
	"slices"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

// JS-120: POST /jellyscale/v1/items/{itemId}/playback-info is PlaybackInfo with every track listed and marked. It is
// in no Jellyfin document, so it is in the jellyscale-only list, has no row of coverage-12.4.tsv, is in
// endpoints.yaml for viewers, and its answers are held to PlaybackInfo's schema through conform.yaml's aliases.
const markedRoute = "/jellyscale/v1/items/{itemId}/playback-info"

func TestTheMarkedPlaybackInfoRouteIsServedForViewersAndInTheJellyscaleOnlyList(t *testing.T) {
	if !slices.Contains(notInSpec, "POST "+markedRoute) {
		t.Errorf("POST %s is not in notInSpec (the jellyscale-only list of coverage_test.go)", markedRoute)
	}
	if _, ok := manifest()["POST "+normalize(markedRoute)]; !ok {
		t.Errorf("POST %s is not a route of the server", markedRoute)
	}
	raw, err := os.ReadFile("endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := `{method: POST, path: "` + markedRoute + `", status: implemented, role: api, access: viewer}`
	if !strings.Contains(string(raw), want) {
		t.Errorf("compat/endpoints.yaml has no line %s", want)
	}
}

func TestTheMarkedPlaybackInfoAnswersAreHeldToPlaybackInfosSchema(t *testing.T) {
	rules, err := conform.LoadRules("conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	aliased := false
	for _, a := range rules.Aliases {
		if a.Method == "POST" && a.Route == markedRoute {
			aliased = a.StandsFor == "/Items/{itemId}/PlaybackInfo"
		}
	}
	if !aliased {
		t.Errorf("conform.yaml has no alias of POST %s standing for /Items/{itemId}/PlaybackInfo", markedRoute)
	}
	for _, u := range rules.Unchecked {
		if u.Route == markedRoute {
			t.Errorf("conform.yaml lists POST %s as unchecked: its answer is PlaybackInfo's and the schema types it", markedRoute)
		}
	}
}

func TestApiDocsDocumentTheMarkedPlaybackInfoWithItsTwoAddedFields(t *testing.T) {
	raw, err := os.ReadFile("../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	at := strings.Index(text, "### POST "+markedRoute)
	if at < 0 {
		t.Fatalf("docs/api.md has no section \"### POST %s\"", markedRoute)
	}
	section := text[at+4:]
	if next := strings.Index(section, "\n##"); next >= 0 {
		section = section[:next]
	}
	section = strings.ToLower(section)
	for _, word := range []string{"hidden", "originallanguage", "every track", "playbackinfo", "401", "404", "400", "ratelimitexceeded"} {
		if !strings.Contains(section, word) {
			t.Errorf("the section on the route does not say %q:\n%s", word, section)
		}
	}
}
