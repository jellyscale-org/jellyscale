package differential

// These tests pin down how the differential tier makes two servers' answers
// comparable, and when it calls them different. They reference the API the task
// implies (package differential in test/differential):
//
//	Normalize(doc any, ids map[string]string, ordered bool) any
//	    a copy of doc (the argument is not changed) in which
//	    - every id in ids (key: the server's own id, dashed or not, any case; value: the
//	      canonical key, "Type/Name") becomes "<id:KEY>", also inside longer strings;
//	    - tokens, ETags, timestamps, session ids and server ids become "<masked>",
//	      the key staying, so that a missing key is still a difference;
//	    - unless ordered, every array is sorted by its normalized JSON encoding.
//	ItemKeys(doc any) map[string]string
//	    id -> "Type/Name" for every object in doc with string Id, Name and Type.
//	Diff(jellyfin, jellyscale any) []Difference
//	    Difference{Path string; Jellyfin, Jellyscale any}; Path like "Items[2].UserData.Played".
//	ParseAllowlist(yaml string) (*Allowlist, error)
//	    entries: route, path, reason, and either kind: improvement | non-goal, or kind: gap with task: JS-nnn.
//	(*Allowlist).Filter(route string, diffs []Difference) []Difference   the unlisted ones
//	(*Allowlist).Unused() []Entry   one Entry per (entry, request) not needed so far
//	(*Allowlist).Entries() []Entry                                       all of them; Entry{Route, Path, Kind, Task, Reason string}

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func paths(diffs []Difference) []string {
	var out []string
	for _, d := range diffs {
		out = append(out, d.Path)
	}
	return out
}

func TestSameItemUnderDifferentIdsNormalizesEqual(t *testing.T) {
	jf := parse(t, `{"Id":"1069c0ddbff1bccf43b43c6a8d9c93b9","Name":"Alpha Film (2021)","Type":"Movie","UserData":{"Key":"1069c0dd-bff1-bccf-43b4-3c6a8d9c93b9","ItemId":"1069c0ddbff1bccf43b43c6a8d9c93b9"}}`)
	js := parse(t, `{"Id":"b3f2a8d0-1111-4222-8333-444455556666","Name":"Alpha Film (2021)","Type":"Movie","UserData":{"Key":"b3f2a8d0-1111-4222-8333-444455556666","ItemId":"b3f2a8d0111142228333444455556666"}}`)
	a := Normalize(jf, ItemKeys(jf), false)
	b := Normalize(js, ItemKeys(js), false)
	if d := Diff(a, b); len(d) != 0 {
		t.Fatalf("same item, different ids: %v", paths(d))
	}
}

func TestIdsInsideUrlsAreMapped(t *testing.T) {
	ids := map[string]string{"1069c0ddbff1bccf43b43c6a8d9c93b9": "Movie/Alpha Film (2021)"}
	got := Normalize(parse(t, `{"U":"/Videos/1069C0DDBFF1BCCF43B43C6A8D9C93B9/master.m3u8?MediaSourceId=1069c0ddbff1bccf43b43c6a8d9c93b9"}`), ids, false)
	want := "/Videos/<id:Movie/Alpha Film (2021)>/master.m3u8?MediaSourceId=<id:Movie/Alpha Film (2021)>"
	if got.(map[string]any)["U"] != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDifferentItemsStayDifferentAfterIdMapping(t *testing.T) {
	jf := parse(t, `{"Id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Name":"Alpha Film (2021)","Type":"Movie"}`)
	js := parse(t, `{"Id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","Name":"The Beta Film (2019)","Type":"Movie"}`)
	d := Diff(Normalize(jf, ItemKeys(jf), false), Normalize(js, ItemKeys(js), false))
	if got := paths(d); !reflect.DeepEqual(got, []string{"Id", "Name"}) {
		t.Fatalf("want Id and Name to differ, got %v", got)
	}
}

func TestTokensTimestampsAndEtagsAreMaskedButKeysStay(t *testing.T) {
	jf := parse(t, `{"AccessToken":"abc","Etag":"1a8b","MediaSources":[{"ETag":"9ab6","Id":"x"}],"DateCreated":"2026-11-07T16:15:41.8158016Z","PlaySessionId":"s1","ServerId":"48b8","Url":"/a.m3u8?api_key=SECRET1&PlaySessionId=p1&x=1"}`)
	js := parse(t, `{"AccessToken":"zzz","Etag":"ffff","MediaSources":[{"ETag":"0000","Id":"x"}],"DateCreated":"2025-01-01T00:00:00Z","PlaySessionId":"s2","ServerId":"9999","Url":"/a.m3u8?api_key=OTHER&PlaySessionId=p2&x=1"}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 0 {
		t.Fatalf("masked values still differ: %v", paths(d))
	}
	// A masked field that one server leaves out is still a difference.
	delete(js.(map[string]any), "Etag")
	d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false))
	if got := paths(d); !reflect.DeepEqual(got, []string{"Etag"}) {
		t.Fatalf("a missing Etag must differ, got %v", got)
	}
}

func TestMaskingDoesNotHideOrdinaryValues(t *testing.T) {
	jf := parse(t, `{"RunTimeTicks":300300000,"Name":"Alpha","Url":"/a?x=1"}`)
	js := parse(t, `{"RunTimeTicks":300300001,"Name":"Alpha","Url":"/a?x=2"}`)
	got := paths(Diff(Normalize(jf, nil, false), Normalize(js, nil, false)))
	if !reflect.DeepEqual(got, []string{"RunTimeTicks", "Url"}) {
		t.Fatalf("got %v", got)
	}
}

func TestUnorderedRoutesIgnoreArrayOrder(t *testing.T) {
	jf := parse(t, `{"Items":[{"Name":"A"},{"Name":"B"},{"Name":"C"}]}`)
	js := parse(t, `{"Items":[{"Name":"C"},{"Name":"A"},{"Name":"B"}]}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) != 0 {
		t.Fatalf("order must not matter here: %v", paths(d))
	}
}

func TestOrderedRoutesCompareArrayOrder(t *testing.T) {
	jf := parse(t, `{"Items":[{"Name":"A"},{"Name":"B"}]}`)
	js := parse(t, `{"Items":[{"Name":"B"},{"Name":"A"}]}`)
	got := paths(Diff(Normalize(jf, nil, true), Normalize(js, nil, true)))
	if !reflect.DeepEqual(got, []string{"Items[0].Name", "Items[1].Name"}) {
		t.Fatalf("a sorted listing in another order must differ, got %v", got)
	}
}

func TestNormalizeLeavesItsArgumentAlone(t *testing.T) {
	doc := parse(t, `{"Id":"aa","Etag":"e","Items":[{"Name":"B"},{"Name":"A"}]}`)
	before, _ := json.Marshal(doc)
	Normalize(doc, map[string]string{"aa": "Movie/X"}, false)
	if after, _ := json.Marshal(doc); string(after) != string(before) {
		t.Fatalf("the input changed: %s", after)
	}
}

func TestDiffReportsPathsForMissingExtraAndRetypedValues(t *testing.T) {
	jf := parse(t, `{"Items":[],"TotalRecordCount":0,"Gone":1,"T":1}`)
	js := parse(t, `{"Items":[{"Name":"Alpha"}],"TotalRecordCount":1,"New":1,"T":"1"}`)
	got := paths(Diff(jf, js))
	for _, want := range []string{"TotalRecordCount", "Gone", "New", "T"} {
		found := false
		for _, p := range got {
			found = found || p == want
		}
		if !found {
			t.Errorf("no difference at %s in %v", want, got)
		}
	}
	items := false
	for _, p := range got {
		items = items || strings.HasPrefix(p, "Items")
	}
	if !items {
		t.Errorf("an item present on one side only must differ under Items: %v", got)
	}
	if d := Diff(jf, jf); len(d) != 0 {
		t.Errorf("a document differs from itself: %v", paths(d))
	}
}

// The PR #32 shape: Jellyfin answers an empty list to mediaTypes=Book.
func TestAFilmWhereJellyfinHasNoneIsADifference(t *testing.T) {
	jf := parse(t, `{"Items":[],"TotalRecordCount":0,"StartIndex":0}`)
	js := parse(t, `{"Items":[{"Name":"Alpha Film (2021)","Type":"Movie"}],"TotalRecordCount":1,"StartIndex":0}`)
	if d := Diff(Normalize(jf, nil, false), Normalize(js, nil, false)); len(d) == 0 {
		t.Fatal("no difference")
	}
}

// The allowlist is scoped to requests, by the name in requests.yaml, not to a route:
//
//	Entry{Requests []string; Path, Kind, Task, Reason string}   (no Route)
//	(*Allowlist).Filter(request string, diffs []Difference) []Difference
//	(*Allowlist).CheckRequests(reqs []Request) error   names every entry request that is not in reqs
const allowlist = `
- requests: [a film's details, an episode's details]
  path: MediaSources[].Path
  kind: improvement
  reason: jellyscale shows the library-relative path, not the server's mount point
- requests: [a film's details]
  path: CanDelete
  kind: non-goal
  reason: PRODUCT.md leaves out deleting from the API
- requests: [a film's details]
  path: Chapters
  kind: gap
  task: JS-023
  reason: chapters not listed yet
`

func TestAllowlistKeepsOnlyUnlistedDifferences(t *testing.T) {
	a, err := ParseAllowlist(allowlist)
	if err != nil {
		t.Fatal(err)
	}
	diffs := []Difference{{Path: "MediaSources[0].Path"}, {Path: "CanDelete"}, {Path: "Name"}, {Path: "MediaSources[0].Size"}}
	got := paths(a.Filter("a film's details", diffs))
	if !reflect.DeepEqual(got, []string{"Name", "MediaSources[0].Size"}) {
		t.Fatalf("unlisted differences: %v", got)
	}
}

// The root cause of the first attempt passing two broken builds: an entry needed
// by one request hid the same path in every other request to the route.
func TestAllowlistEntryHidesNothingInAnotherRequest(t *testing.T) {
	a, _ := ParseAllowlist(allowlist)
	// An episode's details share the route of a film's details, and the entry for Path.
	got := a.Filter("an episode's details", []Difference{{Path: "MediaSources[0].Path"}, {Path: "CanDelete"}})
	if !reflect.DeepEqual(paths(got), []string{"CanDelete"}) {
		t.Fatalf("an entry for another request allowed this, or the entry for this one did not: %v", paths(got))
	}
	if got := a.Filter("a request no entry names", []Difference{{Path: "MediaSources[0].Path"}}); len(got) != 1 {
		t.Fatalf("a request without entries had a difference allowed: %v", got)
	}
}

// QA's two broken builds: a film missing from a list, and a list in the wrong
// order, in a request that shares its route with one the allowlist covers.
func TestAllowlistedElementDifferenceInOneRequestDoesNotHideMissingElementInAnother(t *testing.T) {
	a, err := ParseAllowlist("- {requests: [latest in the Shows library], path: '[]', kind: improvement, reason: r}\n" +
		"- {requests: [the films library, newest first], path: 'Items[].Name', kind: gap, task: JS-068, reason: r}\n")
	if err != nil {
		t.Fatal(err)
	}
	missing := Diff([]any{map[string]any{"Name": "A"}, map[string]any{"Name": "B"}}, []any{map[string]any{"Name": "A"}})
	if got := a.Filter("latest in the Shows library", missing); len(got) != 0 {
		t.Fatalf("the entry should hide it for its own request: %v", got)
	}
	if got := a.Filter("latest films in the Movies library", missing); len(got) != 1 {
		t.Fatalf("a film missing from the Movies list was hidden by the Shows entry: %v", got)
	}
	swapped := Diff(Normalize(parse(t, `{"Items":[{"Name":"Alpha Film"},{"Name":"The Beta Film"}]}`), nil, true),
		Normalize(parse(t, `{"Items":[{"Name":"The Beta Film"},{"Name":"Alpha Film"}]}`), nil, true))
	if len(swapped) == 0 {
		t.Fatal("an ordered request must see a swapped order")
	}
	if got := a.Filter("the films library, by name", swapped); len(got) != len(swapped) {
		t.Fatalf("a wrong sort order was hidden by the entry for the newest-first request: %v", got)
	}
}

func TestAllowlistPathDoesNotMatchAsPrefixOrSubstring(t *testing.T) {
	a, _ := ParseAllowlist(allowlist)
	got := a.Filter("a film's details", []Difference{{Path: "CanDeleteAll"}, {Path: "X.CanDelete"}, {Path: "MediaSources[0].Path2"}})
	if len(got) != 3 {
		t.Fatalf("a path matched loosely: %v", got)
	}
}

func TestAllowlistReportsEntriesThatNoLongerOccur(t *testing.T) {
	a, _ := ParseAllowlist(allowlist)
	a.Filter("a film's details", []Difference{{Path: "MediaSources[3].Path"}, {Path: "CanDelete"}})
	unused := a.Unused()
	if len(unused) != 2 {
		t.Fatalf("want the Chapters entry and the Path entry for the episode unused, got %+v", unused)
	}
}

// Unused reports each (entry, request) pair that was not needed, as an Entry
// whose Requests holds that one request.
func TestAllowlistEntryUnusedForOneOfItsRequestsIsReported(t *testing.T) {
	a, _ := ParseAllowlist(allowlist)
	a.Filter("a film's details", []Difference{{Path: "MediaSources[0].Path"}, {Path: "CanDelete"}, {Path: "Chapters"}})
	unused := a.Unused()
	if len(unused) != 1 || !reflect.DeepEqual(unused[0].Requests, []string{"an episode's details"}) || unused[0].Path != "MediaSources[].Path" {
		t.Fatalf("want only the Path entry for the episode's details, got %+v", unused)
	}
}

func TestAnEntryNamingNoExistingRequestFails(t *testing.T) {
	a, err := ParseAllowlist(allowlist)
	if err != nil {
		t.Fatal(err)
	}
	reqs := []Request{{Name: "a film's details"}, {Name: "an episode's details"}}
	if err := a.CheckRequests(reqs); err != nil {
		t.Fatalf("every named request exists: %v", err)
	}
	typo, _ := ParseAllowlist("- {requests: [a film's detials], path: Foo, kind: improvement, reason: r}\n")
	err = typo.CheckRequests(reqs)
	if err == nil || !strings.Contains(err.Error(), "a film's detials") {
		t.Fatalf("want an error naming the missing request, got %v", err)
	}
	mixed, _ := ParseAllowlist("- {requests: [a film's details, gone], path: Foo, kind: improvement, reason: r}\n")
	if err := mixed.CheckRequests(reqs); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("one missing name among real ones must fail, got %v", err)
	}
}

func TestAllowlistRefusesAnEntryWithoutAReason(t *testing.T) {
	for name, text := range map[string]string{
		"no reason":          "- {requests: [r], path: Foo, kind: improvement}\n",
		"no requests":        "- {path: Foo, kind: improvement, reason: x}\n",
		"empty requests":     "- {requests: [], path: Foo, kind: improvement, reason: x}\n",
		"only a route":       "- {route: GET /Items, path: Foo, kind: improvement, reason: x}\n",
		"no path":            "- {requests: [r], kind: improvement, reason: x}\n",
		"unknown kind":       "- {requests: [r], path: Foo, kind: whim, reason: x}\n",
		"gap without a task": "- {requests: [r], path: Foo, kind: gap, reason: x}\n",
		"gap with a bad id":  "- {requests: [r], path: Foo, kind: gap, task: soon, reason: x}\n",
	} {
		if _, err := ParseAllowlist(text); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
