//go:build integration

package api

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/compat/conform"
	"jellyscale/internal/db"
)

// Fluster (#102) shows suggestions while the viewer types, from
// GET /Search/Hints?searchTerm=..&limit=..&includeItemTypes=Movie,Series,Episode&userId=..
// with no recursive, parentId or fields. What Jellyfin 12.4 answers to that,
// and to the requests around it, is in compat/captures/search-hints-12.4.json
// (compat/captures/search-hints-12.4.md says how it was observed).

type hintCapture struct {
	Requests map[string]struct {
		Status int
	}
}

func hintStatus(t *testing.T, name string) int {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/search-hints-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture hintCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	r, ok := capture.Requests[name]
	if !ok {
		t.Fatalf("the capture has no request %q", name)
	}
	return r.Status
}

// hintsEnv is the env with a small library: films, series and episodes whose
// names hold "quartz" in some case, and decoys that must never be a hint.
type hintsEnv struct {
	*env
	id map[string]string // name -> compact id
}

const (
	hintFilmA    = "Quartz Alpha"
	hintFilmB    = "Quartz Beta" // no picture
	hintSeries   = "Quartz Show"
	hintEpisode1 = "Quartz Opener" // S01E02
	hintEpisode2 = "Quartz Return" // S02E01
	hintEpisode3 = "Quartz Finale" // S02E03, no picture
)

func newHintsEnv(t *testing.T) *hintsEnv {
	t.Helper()
	e := &hintsEnv{env: newEnv(t), id: map[string]string{}}
	lib := scalar[string](t, e.pool, "SELECT library_id FROM items WHERE id = $1", e.item)
	add := func(kind, name string, parent, series *string, index, parentIndex *int, tag string, extra string) string {
		t.Helper()
		id := uuid.NewString()
		dto := map[string]any{"Name": name, "Type": kind, "ImageTags": map[string]string{}}
		if tag != "" {
			dto["ImageTags"] = map[string]string{"Primary": tag}
			dto["PrimaryImageAspectRatio"] = 1.7777777777777777
		}
		if index != nil {
			dto["IndexNumber"] = *index
		}
		if parentIndex != nil {
			dto["ParentIndexNumber"] = *parentIndex
		}
		if extra != "" {
			dto["Overview"] = extra
		}
		raw, _ := json.Marshal(dto)
		var tagArg any
		if tag != "" {
			tagArg = tag
		}
		mustExec(t, e.pool, `INSERT INTO items (id, library_id, parent_id, series_id, season_id, type, name, sort_name, natural_key, index_number, parent_index_number, primary_image_tag, dto)
			VALUES ($1, $2, $3::uuid, $4::uuid, CASE WHEN $5 = 'Episode' THEN $3::uuid END, $5, $6, lower($6), $6, $7, $8, $9, $10)`,
			id, lib, parent, series, kind, name, index, parentIndex, tagArg, raw)
		e.id[name] = db.CompactID(id)
		return id
	}
	num := func(n int) *int { return &n }

	add("Movie", hintFilmA, nil, nil, nil, nil, "tagfilm", "")
	add("Movie", hintFilmB, nil, nil, nil, nil, "", "")
	// The word is in its overview, not its name.
	add("Movie", "Plain Decoy", nil, nil, nil, nil, "", "a quartz crystal")
	series := add("Series", hintSeries, nil, nil, nil, nil, "tagseries", "")
	add("Series", "Plain Show", nil, nil, nil, nil, "", "")
	season1 := add("Season", "Season 1", &series, &series, num(1), nil, "", "")
	season2 := add("Season", "Season 2", &series, &series, num(2), nil, "", "")
	add("Episode", hintEpisode1, &season1, &series, num(2), num(1), "tagep1", "")
	add("Episode", hintEpisode2, &season2, &series, num(1), num(2), "tagep2", "")
	add("Episode", hintEpisode3, &season2, &series, num(3), num(2), "", "")
	return e
}

func (e *hintsEnv) query(extra string) string {
	return "/Search/Hints?userId=" + db.CompactID(e.user) + extra
}

type hint = map[string]any

// hints asks for hints and returns the status and the SearchHints list, with the
// raw document.
func (e *hintsEnv) hints(t *testing.T, extra, token string) (int, []hint, map[string]any) {
	t.Helper()
	response, body := e.do(t, 0, "GET", e.query(extra), token, nil)
	var doc map[string]any
	if response.StatusCode != 200 {
		return response.StatusCode, nil, nil
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%s: %v\n%s", extra, err, body)
	}
	list, ok := doc["SearchHints"].([]any)
	if !ok {
		t.Fatalf("%s: SearchHints is %T, want a list\n%s", extra, doc["SearchHints"], body)
	}
	var out []hint
	for _, h := range list {
		out = append(out, h.(map[string]any))
	}
	return 200, out, doc
}

func names(list []hint) []string {
	var out []string
	for _, h := range list {
		out = append(out, h["Name"].(string))
	}
	slices.Sort(out)
	return out
}

func sorted(s ...string) []string { slices.Sort(s); return s }

func TestSearchHintsAnswerEachMatchingFilmSeriesAndEpisode(t *testing.T) {
	e := newHintsEnv(t)
	status, got, doc := e.hints(t, "&searchTerm=quartz&limit=20&includeItemTypes=Movie,Series,Episode", e.token)
	if status != 200 {
		t.Fatalf("status %d, want 200 (GET /Search/Hints is answered 404 today)", status)
	}
	want := sorted(hintFilmA, hintFilmB, hintSeries, hintEpisode1, hintEpisode2, hintEpisode3)
	if !slices.Equal(names(got), want) {
		t.Errorf("hints %v, want one for each of %v (several of one type, and nothing else)", names(got), want)
	}
	types := map[string]string{hintFilmA: "Movie", hintFilmB: "Movie", hintSeries: "Series", hintEpisode1: "Episode", hintEpisode2: "Episode", hintEpisode3: "Episode"}
	for _, h := range got {
		name := h["Name"].(string)
		if h["Type"] != types[name] || h["ItemId"] != e.id[name] || h["Id"] != e.id[name] {
			t.Errorf("hint %v: want Type %s and ItemId and Id %s", h, types[name], e.id[name])
		}
	}
	if doc["TotalRecordCount"] != float64(len(want)) {
		t.Errorf("TotalRecordCount %v, want %d", doc["TotalRecordCount"], len(want))
	}
}

func TestSearchHintsFindATitleFromAPartOfItInAnotherCase(t *testing.T) {
	e := newHintsEnv(t)
	six := sorted(hintFilmA, hintFilmB, hintSeries, hintEpisode1, hintEpisode2, hintEpisode3)
	for term, want := range map[string][]string{
		"uart":       six, // a lowercase fragment of "Quartz"
		"QUAR":       six,
		"quartz%20s": {hintSeries}, // part of "Quartz Show" only; the decoys never appear
	} {
		status, got, _ := e.hints(t, "&searchTerm="+term+"&limit=20&includeItemTypes=Movie,Series,Episode", e.token)
		if status != 200 || !slices.Equal(names(got), sorted(want...)) {
			t.Errorf("searchTerm=%s: %d %v, want %v", term, status, names(got), want)
		}
	}
}

func TestSearchHintsIncludeItemTypesNarrowTheHints(t *testing.T) {
	e := newHintsEnv(t)
	for types, want := range map[string][]string{
		"Series":        {hintSeries},
		"Movie":         {hintFilmA, hintFilmB},
		"Episode":       {hintEpisode1, hintEpisode2, hintEpisode3},
		"Movie,Episode": {hintFilmA, hintFilmB, hintEpisode1, hintEpisode2, hintEpisode3},
	} {
		status, got, _ := e.hints(t, "&searchTerm=quartz&limit=20&includeItemTypes="+types, e.token)
		if status != 200 || !slices.Equal(names(got), sorted(want...)) {
			t.Errorf("includeItemTypes=%s: %d %v, want %v", types, status, names(got), want)
		}
	}
}

// Jellyfin 12.4 cuts the matches to limit and then answers the cut's size as
// TotalRecordCount, not the number of matches (limit 3 of 4: 3 hints, 3; limit 10
// of 4: 4 hints, 4), so the count is checked against what the capture says.
func TestSearchHintsHoldExactlyLimitHintsAndCountThemAsJellyfinDoes(t *testing.T) {
	e := newHintsEnv(t)
	all := "&searchTerm=quartz&includeItemTypes=Movie,Series,Episode&limit="
	for limit, wantHints := range map[string]int{"1": 1, "4": 4, "5": 5, "6": 6, "20": 6} {
		status, got, doc := e.hints(t, all+limit, e.token)
		if status != 200 || len(got) != wantHints || doc["TotalRecordCount"] != float64(wantHints) {
			t.Errorf("limit %s: status %d, %d hints, TotalRecordCount %v; want %d and %d", limit, status, len(got), doc["TotalRecordCount"], wantHints, wantHints)
		}
	}
	// A page of hints is a different page of one list, never a different list.
	_, first, _ := e.hints(t, all+"2", e.token)
	_, more, _ := e.hints(t, all+"4", e.token)
	if len(first) == 2 && len(more) == 4 && !slices.Equal(names(first), names(more[:2])) {
		t.Errorf("limit 2 gave %v, the first two of limit 4 are %v", names(first), names(more[:2]))
	}
}

func TestSearchHintsOfAnEpisodeCarryItsOwnSeasonAndNumber(t *testing.T) {
	e := newHintsEnv(t)
	_, got, _ := e.hints(t, "&searchTerm=quartz&limit=20&includeItemTypes=Episode", e.token)
	want := map[string][2]float64{hintEpisode1: {1, 2}, hintEpisode2: {2, 1}, hintEpisode3: {2, 3}} // season, number
	if len(got) != len(want) {
		t.Fatalf("%d episode hints, want %d", len(got), len(want))
	}
	for _, h := range got {
		w := want[h["Name"].(string)]
		if h["ParentIndexNumber"] != w[0] || h["IndexNumber"] != w[1] {
			t.Errorf("%s: ParentIndexNumber %v, IndexNumber %v; want %v and %v", h["Name"], h["ParentIndexNumber"], h["IndexNumber"], w[0], w[1])
		}
	}
}

func TestSearchHintsCarryTheImageTagTheItemHas(t *testing.T) {
	e := newHintsEnv(t)
	_, got, _ := e.hints(t, "&searchTerm=quartz&limit=20&includeItemTypes=Movie,Series,Episode", e.token)
	if len(got) != 6 {
		t.Fatalf("%d hints, want 6", len(got))
	}
	withPicture := 0
	for _, h := range got {
		name := h["Name"].(string)
		_, body := e.do(t, 0, "GET", "/Items/"+e.id[name]+"?userId="+db.CompactID(e.user), e.token, nil)
		var item struct{ ImageTags map[string]string }
		if err := json.Unmarshal(body, &item); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, body)
		}
		tag, hasTag := h["PrimaryImageTag"]
		if want := item.ImageTags["Primary"]; want != "" {
			withPicture++
			if tag != want {
				t.Errorf("%s: PrimaryImageTag %v, GET /Items/{id} has ImageTags.Primary %q", name, tag, want)
			}
		} else if hasTag && tag != nil {
			t.Errorf("%s has no picture, yet PrimaryImageTag is %q (want it absent or null)", name, tag)
		}
	}
	if withPicture != 4 { // a film, a series and two episodes
		t.Errorf("%d hints had a picture to compare, want 4", withPicture)
	}
}

func TestSearchHintsForATermThatMatchesNothingAreAnEmptyList(t *testing.T) {
	e := newHintsEnv(t)
	response, body := e.do(t, 0, "GET", e.query("&searchTerm=zzz&limit=10&includeItemTypes=Movie,Series,Episode"), e.token, nil)
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if string(doc["SearchHints"]) != "[]" || string(doc["TotalRecordCount"]) != "0" {
		t.Errorf("SearchHints %s, TotalRecordCount %s; want [] and 0 (a list, not null)", doc["SearchHints"], doc["TotalRecordCount"])
	}
}

func TestSearchHintsWithoutASearchTermAnswerAsJellyfinDoesAndNeverTheLibrary(t *testing.T) {
	e := newHintsEnv(t)
	for name, extra := range map[string]string{"empty searchTerm": "&searchTerm=&limit=10", "no searchTerm": "&limit=10"} {
		response, body := e.do(t, 0, "GET", e.query(extra), e.token, nil)
		if want := hintStatus(t, name); response.StatusCode != want {
			t.Errorf("%s: status %d, Jellyfin 12.4 answers %d", name, response.StatusCode, want)
		}
		if strings.Contains(string(body), "Quartz") || strings.Contains(string(body), `"SearchHints":[{`) {
			t.Errorf("%s: the answer holds hints: %s", name, body)
		}
	}
}

func TestSearchHintsRefuseARequestWithoutAUsableToken(t *testing.T) {
	e := newHintsEnv(t)
	for name, token := range map[string]string{"no token": "", "a token no device holds": uuid.NewString()} {
		response, body := e.do(t, 0, "GET", e.query("&searchTerm=quartz&limit=10"), token, nil)
		if want := hintStatus(t, name); response.StatusCode != want {
			t.Errorf("%s: status %d, Jellyfin 12.4 answers %d", name, response.StatusCode, want)
		}
		if strings.Contains(string(body), "Quartz") {
			t.Errorf("%s: the answer holds hints: %s", name, body)
		}
	}
}

// compat/conform holds each answer to the OpenAPI document of Jellyfin 12.4: a
// SearchHintResult has SearchHints and TotalRecordCount, and every hint Artists,
// Id, ItemId, Name and Type.
func TestSearchHintsOfAFilmASeriesAndAnEpisodeFitTheSchema(t *testing.T) {
	e := newHintsEnv(t)
	path := e.query("&searchTerm=quartz&limit=20&includeItemTypes=Movie,Series,Episode")
	response, body := e.do(t, 0, "GET", path, e.token, nil)
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	var doc struct{ SearchHints []struct{ Type string } }
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, h := range doc.SearchHints {
		kinds = append(kinds, h.Type)
	}
	for _, kind := range []string{"Movie", "Series", "Episode"} {
		if !slices.Contains(kinds, kind) {
			t.Fatalf("the answer has no %s hint (%v), so it proves nothing about the schema", kind, kinds)
		}
	}
	file, err := os.ReadFile("../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := conform.LoadRules("../../compat/conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c := conform.NewChecker(schema, rules)
	for _, v := range c.Check("search hints", "GET", path, 200, response.Header.Get("Content-Type"), body) {
		t.Errorf("violation: %s", v)
	}
	if _, err := c.Summary("search hints"); err != nil {
		t.Errorf("the checker counted no JSON response: %v", err)
	}
}

// Jellyfin 12.4 answers episodes, then films, then series (compat/captures/
// search-hints-12.4.md, the term "quartz"). Within each it sorts by name, and an
// episode's name there begins with its SxxEyy, so its episodes come in season and
// episode order ("delta": S01E01, S01E02, S02E01); jellyscale names an episode by
// its title, so it orders episodes by season and number to answer the same.
func TestSearchHintsComeInTheOrderJellyfinAnswersThem(t *testing.T) {
	e := newHintsEnv(t)
	status, got, _ := e.hints(t, "&searchTerm=quartz&limit=20&includeItemTypes=Movie,Series,Episode", e.token)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	var order []string
	for _, h := range got {
		order = append(order, h["Name"].(string))
	}
	want := []string{hintEpisode1, hintEpisode2, hintEpisode3, hintFilmA, hintFilmB, hintSeries}
	if !slices.Equal(order, want) {
		t.Errorf("hints in the order %v, want %v (episodes by season and number, then films and series by name)", order, want)
	}
}
