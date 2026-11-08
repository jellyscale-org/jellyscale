package differential

// The shipped data files: the request list and the allowlist. They need the
// names the task implies:
//
//	LoadRequests(path string) ([]Request, error)
//	    Request{Name, Route, Method, Path string; Ordered bool}; Route is the endpoint template
//	    ("GET /Items/{itemId}"), Path the request line with the query jellyfin-web sends.
//	LoadAllowlist(path string) (*Allowlist, error)

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestEveryRequestIsARouteJellyscaleKnows(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	endpoints, err := os.ReadFile("../../compat/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reqs {
		method, path, _ := strings.Cut(r.Route, " ")
		if !strings.Contains(string(endpoints), "method: "+method+", path: \""+path+"\"") {
			t.Errorf("%s: route %q is not in compat/endpoints.yaml", r.Name, r.Route)
		}
		if r.Method != method {
			t.Errorf("%s: method %s, route says %s", r.Name, r.Method, method)
		}
	}
}

// The browse and playback-start routes the task names, with the parameters jellyfin-web sends.
func TestRequestListCoversBrowseAndPlaybackStart(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	count := func(route, in string) int {
		n := 0
		for _, r := range reqs {
			if r.Route == route && strings.Contains(strings.ToLower(r.Path), strings.ToLower(in)) {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		route, in string
		min       int
	}{
		{"GET /Users/Me", "", 1},
		{"GET /UserViews", "", 1},
		{"GET /Items/Latest", "", 1},
		{"GET /UserItems/Resume", "mediaTypes=Video", 1},
		{"GET /UserItems/Resume", "mediaTypes=Audio", 1},
		{"GET /UserItems/Resume", "mediaTypes=Book", 1},
		{"GET /Items", "sortBy=", 1},
		{"GET /Items", "searchTerm=", 1},
		{"GET /Items/{itemId}", "", 3}, // a film, an episode and a Dolby Vision film
		{"GET /Shows/{seriesId}/Seasons", "", 1},
		{"GET /Shows/{seriesId}/Episodes", "", 1},
		{"POST /Items/{itemId}/PlaybackInfo", "", 8}, // direct play and a transcode, and six for Dolby Vision and HDR10+ films
		{"GET /Videos/{itemId}/master.m3u8", "", 1},
	} {
		if n := count(c.route, c.in); n < c.min {
			t.Errorf("%s with %q: %d requests, want %d", c.route, c.in, n, c.min)
		}
	}
}

func TestRequestListNamesOnlySyntheticFixtures(t *testing.T) {
	data, err := os.ReadFile("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	make, err := os.ReadFile("../fixtures/make.sh")
	if err != nil {
		t.Fatal(err)
	}
	e2e, err := os.ReadFile("../fixtures/e2e.sh") // the HDR films
	if err != nil {
		t.Fatal(err)
	}
	// A title in the file looks like "Name (2021)"; it must be one make.sh or e2e.sh generates.
	for _, title := range regexp.MustCompile(`[A-Z][A-Za-z.]+(?: [A-Za-z]+)* \(\d{4}\)`).FindAllString(string(data), -1) {
		if !strings.Contains(string(make), title) && !strings.Contains(string(e2e), title) {
			t.Errorf("%q is no synthetic fixture", title)
		}
	}
}

// Every entry of the shipped allowlist is well formed, and a gap names a task that exists.
func TestShippedAllowlistIsWellFormed(t *testing.T) {
	a, err := LoadAllowlist("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	prd, err := os.ReadFile("../../tasks/prd.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range a.Entries() {
		if e.Kind == "gap" && !strings.Contains(string(prd), `"id": "`+e.Task+`"`) {
			t.Errorf("%s %s: task %s is not in tasks/prd.json", e.Route, e.Path, e.Task)
		}
	}
}

// compat/differences.yaml scopes every entry to requests that exist in requests.yaml.
func TestShippedAllowlistNamesOnlyRequestsThatExist(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	a, err := LoadAllowlist("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Entries()) == 0 {
		t.Fatal("no entries")
	}
	if err := a.CheckRequests(reqs); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range reqs {
		if names[r.Name] {
			t.Errorf("two requests are named %q; an entry could not tell them apart", r.Name)
		}
		names[r.Name] = true
	}
}

// same_count was a patch over route-wide entries; scoped entries make it redundant.
func TestRequestListHasNoSameCountFlag(t *testing.T) {
	text, err := os.ReadFile("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "same_count") {
		t.Error("requests.yaml still uses same_count")
	}
	doc, err := os.ReadFile("../../docs/testing.md")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "same_count") {
		t.Error("docs/testing.md still describes same_count")
	}
}

// The three broken builds of acceptance.sh need these requests to exist.
func TestRequestListHasTheRequestsTheBrokenBuildsMustFailOn(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"continue watching, Book":            "GET /UserItems/Resume",
		"latest films in the Movies library": "GET /Items/Latest",
		"the films library, by name":         "GET /Items",
	}
	for _, r := range reqs {
		if route, ok := want[r.Name]; ok && r.Route == route {
			delete(want, r.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("requests missing: %v", want)
	}
}
