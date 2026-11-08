package differential

import (
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
		"the master playlist of a transcode",
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
