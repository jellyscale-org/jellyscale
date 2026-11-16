package differential

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Allowlist entries name requests, so two requests with one name would let an
// entry written for one hide a difference in the other.
func TestTwoRequestsWithOneNameAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.yaml")
	text := "- name: films\n  path: /Items?IncludeItemTypes=Movie\n- name: films\n  path: /Items?IncludeItemTypes=Movie&SortBy=SortName\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRequests(path)
	if err == nil || !strings.Contains(err.Error(), `"films"`) {
		t.Fatalf("want an error naming the repeated request, got %v", err)
	}
}

func TestRowsAndStatusAreReadFromTheRequestList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.yaml")
	text := "- name: a\n  path: /Items\n  rows: true\n- name: b\n  path: /Nope\n  status: 404\n- name: c\n  path: /Items\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs, err := LoadRequests(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reqs[0].Rows || reqs[0].Status != 0 || reqs[1].Rows || reqs[1].Status != 404 || reqs[2].Rows || reqs[2].Status != 0 {
		t.Fatalf("got %+v", reqs)
	}
}

// Both servers return these on the synthetic fixtures (observed when JS-069 was
// written), so a route or fixture broken on both must fail them. The Audio and Book
// resume lists are empty on both on purpose and must not claim rows.
func TestRequestsMeantToReturnRowsSayRows(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Request{}
	for _, r := range reqs {
		byName[r.Name] = r
	}
	for _, name := range []string{
		"the home page views", "latest films in the Movies library", "latest in the Shows library",
		"continue watching, Video", "the films library, by name", "the films library, newest first",
		"the shows library, by name", "search", "the seasons of a show", "the episodes of a show",
		"the master playlist of a transcode", "the HDR films with their streams",
	} {
		if r, ok := byName[name]; !ok || !r.Rows {
			t.Errorf("%q is meant to return rows and does not say rows: true", name)
		}
	}
	for _, name := range []string{"continue watching, Audio", "continue watching, Book"} {
		if byName[name].Rows {
			t.Errorf("%q is empty on both servers and must not say rows: true", name)
		}
	}
}

// JS-116: the tier saves all 16 settings through each route, reads them back with GET /Users/Me, and compares
// the list of languages in order. The saves come last, so no request before them is asked of a user with
// settings saved; the compared answers are the user's, the list of languages and nothing else.
func TestRequestsSaveAllSettingsThroughBothRoutes(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	index := map[string]int{}
	for i, r := range reqs {
		index[r.Name] = i
	}
	fields := []string{"AudioLanguagePreference", "PlayDefaultAudioTrack", "SubtitleLanguagePreference", "DisplayMissingEpisodes",
		"GroupedFolders", "SubtitleMode", "DisplayCollectionsView", "EnableLocalPassword", "OrderedViews", "LatestItemsExcludes",
		"MyMediaExcludes", "HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay", "CastReceiverId"}
	for _, c := range []struct{ save, route, read string }{
		{"saving all 16 settings through the path route", "POST /Users/{userId}/Configuration", "the signed-in user after saving through the path route"},
		{"saving all 16 settings through the SDK route", "POST /Users/Configuration", "the signed-in user after saving through the SDK route"},
	} {
		save, ok := index[c.save]
		if !ok {
			t.Errorf("no request %q", c.save)
			continue
		}
		r := reqs[save]
		if r.Route != c.route || r.Method != "POST" {
			t.Errorf("%q is %s %s, want POST %s", c.save, r.Method, r.Route, c.route)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
			t.Errorf("%q has no JSON body: %v", c.save, err)
		}
		for _, f := range fields {
			if _, ok := body[f]; !ok {
				t.Errorf("%q does not send %s", c.save, f)
			}
		}
		if read, ok := index[c.read]; !ok || read != save+1 || reqs[read].Path != "/Users/Me" || !reqs[read].Ordered {
			t.Errorf("%q must be the ordered GET /Users/Me right after %q", c.read, c.save)
		}
	}
	cultures, ok := index["the languages a viewer can pick"]
	if !ok {
		t.Fatal("no request for GET /Localization/Cultures")
	}
	if r := reqs[cultures]; r.Route != "GET /Localization/Cultures" || !r.Ordered || !r.Rows {
		t.Errorf("the cultures request must be ordered and fail on an empty list: %+v", r)
	}
	last := index["saving all 16 settings through the path route"]
	for i, r := range reqs {
		if i < last && (strings.Contains(r.Path, "/Configuration") && r.Method == "POST") {
			t.Errorf("%q saves settings before the tier's other requests are done", r.Name)
		}
	}
	if last < len(reqs)-5 {
		t.Errorf("requests follow the saves: the saves must be the last of the file")
	}
}
