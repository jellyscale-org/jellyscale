package differential

// JS-100: GET /Search/Hints, asked of both servers.

import (
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func hintsRequest(t *testing.T) Request {
	t.Helper()
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reqs {
		if r.Route == "GET /Search/Hints" && strings.Contains(r.Path, "Series") {
			return r
		}
	}
	t.Fatal("requests.yaml has no request for GET /Search/Hints that asks for Movie,Series,Episode")
	return Request{}
}

// sameOnBothRequest is the request whose term both servers match with the same
// items, so which hints come back and their order are compared without an
// allowlist entry: Jellyfin names an episode by its whole file name and a film
// by its file name, so the three-type request above cannot be.
func sameOnBothRequest(t *testing.T) Request {
	t.Helper()
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reqs {
		if r.Route == "GET /Search/Hints" && !strings.Contains(r.Path, "Series") {
			return r
		}
	}
	t.Fatal("requests.yaml has no second GET /Search/Hints request (films only, a term both servers match the same way)")
	return Request{}
}

// The answer is {"SearchHints": [...]}, not {"Items": [...]}: a request that
// expects rows must read the list it has, or a server that answers no hints passes.
func TestARequestThatExpectsRowsReadsSearchHints(t *testing.T) {
	r := Request{Name: "r", Rows: true}
	if err := CheckAnswer(r, 200, parse(t, `{"SearchHints":[{"Id":"a","ItemId":"a","Name":"n","Type":"Movie","Artists":[]}],"TotalRecordCount":1}`)); err != nil {
		t.Errorf("an answer with a hint failed: %v", err)
	}
	for _, empty := range []string{`{"SearchHints":[],"TotalRecordCount":0}`, `{"SearchHints":null,"TotalRecordCount":0}`, `{"TotalRecordCount":0}`} {
		if err := CheckAnswer(r, 200, parse(t, empty)); err == nil {
			t.Errorf("%s passed a request that expects rows", empty)
		}
	}
}

func TestTheDifferentialTierAsksBothServersForHintsAndComparesTheirOrder(t *testing.T) {
	r := hintsRequest(t)
	if r.Method != "GET" || !r.Rows || !r.Ordered {
		t.Errorf("%q: method %s, rows %v, ordered %v; want GET, rows and ordered (the tier fails on no hints and compares their order)", r.Name, r.Method, r.Rows, r.Ordered)
	}
	query, err := url.ParseQuery(r.Path[strings.Index(r.Path, "?")+1:])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"searchTerm", "limit", "includeItemTypes", "userId"} {
		if query.Get(key) == "" {
			t.Errorf("%q has no %s, which Fluster sends", r.Name, key)
		}
	}
	for _, key := range []string{"recursive", "parentId", "fields"} {
		if _, ok := query[key]; ok {
			t.Errorf("%q sends %s, which Fluster does not", r.Name, key)
		}
	}
	for _, kind := range []string{"Movie", "Series", "Episode"} {
		if !strings.Contains(query.Get("includeItemTypes"), kind) {
			t.Errorf("%q does not ask for %s", r.Name, kind)
		}
	}
}

// What the term must hit on both servers, which name things differently
// (Jellyfin keeps the year and the whole file name, jellyscale the title): a
// film, a series and an episode of test/fixtures/make.sh by the parts of the
// names they share.
func TestTheHintsTermMatchesAFilmASeriesAndAnEpisodeOnBothServers(t *testing.T) {
	script, err := os.ReadFile("../fixtures/make.sh")
	if err != nil {
		t.Fatal(err)
	}
	film := []string{"Alpha Film", "The Beta Film", "Gamma.Film"} // movies/ in make.sh
	series := []string{"Delta Show"}
	episode := []string{"Pilot", "Second", "Return"} // what jellyscale names Delta Show S01E01 .. S02E01
	for _, name := range append(append(append([]string{}, film...), series...), episode...) {
		if !strings.Contains(string(script), name) {
			t.Fatalf("make.sh no longer has %q: update this test", name)
		}
	}
	p := hintsRequest(t).Path
	q, _ := url.ParseQuery(p[strings.Index(p, "?")+1:])
	term := strings.ToLower(q.Get("searchTerm"))
	if term == "" {
		t.Fatal("the request has no term")
	}
	for kind, names := range map[string][]string{"film": film, "series": series, "episode": episode} {
		hit := false
		for _, name := range names {
			hit = hit || strings.Contains(strings.ToLower(name), term)
		}
		if !hit {
			t.Errorf("the term %q matches no %s of the fixtures (%v)", term, kind, names)
		}
	}
}

// Jellyfin 12.4 sorts hints, so the order is part of the answer. The request
// the two servers agree on has to hold at least two hints, or the order proves
// nothing, and has to be compared in order and with rows.
func TestTheHintsRequestTheServersAgreeOnComparesAnOrderOfSeveralHints(t *testing.T) {
	r := sameOnBothRequest(t)
	if r.Method != "GET" || !r.Rows || !r.Ordered {
		t.Errorf("%q: method %s, rows %v, ordered %v; want GET, rows and ordered", r.Name, r.Method, r.Rows, r.Ordered)
	}
	q, err := url.ParseQuery(r.Path[strings.Index(r.Path, "?")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("includeItemTypes") != "Movie" || q.Get("limit") == "" || q.Get("userId") == "" {
		t.Errorf("%q: want includeItemTypes=Movie, a limit and a userId, got %v", r.Name, q)
	}
	script, err := os.ReadFile("../fixtures/e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	term := strings.ToLower(q.Get("searchTerm"))
	if len(term) < 4 {
		t.Fatalf("the term %q is too short to be named the same by both servers", term)
	}
	hits := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([^"]+ Film) \(\d{4}\)"`).FindAllStringSubmatch(string(script), -1) {
		if strings.Contains(strings.ToLower(m[1]), term) {
			hits[m[1]] = true
		}
	}
	if len(hits) < 2 {
		t.Errorf("the term %q matches %d films of test/fixtures/e2e.sh (%v), want at least two so an order is compared", term, len(hits), hits)
	}
	if limit, _ := strconv.Atoi(q.Get("limit")); limit < len(hits) {
		t.Errorf("limit %d cuts the %d matches, so the set compared depends on the cut", limit, len(hits))
	}
}

func TestNoAllowlistEntryHidesTheHintsTheServersAgreeOn(t *testing.T) {
	r := sameOnBothRequest(t)
	text, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Requests []string `yaml:"requests"`
		Path     string   `yaml:"path"`
		Paths    []string `yaml:"paths"`
	}
	if err := yaml.Unmarshal(text, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !slices.Contains(e.Requests, r.Name) {
			continue
		}
		for _, p := range append([]string{e.Path}, e.Paths...) {
			if strings.HasSuffix(p, "(ids)") {
				t.Errorf("compat/differences.yaml lists %q for %q: the hints and their order must be the same on both servers", p, r.Name)
			}
		}
	}
}
