//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/testenv"
)

// JS-123: PATCH /jellyscale/v1/users/configuration changes the fields its body names for the caller and leaves the rest,
// where Jellyfin's POST /Users/Configuration replaces the whole Configuration (compat/captures/user-configuration-12.4.md).
// Every check runs against api processes of its own on one Postgres, and reads what was stored through GET /Users/Me on
// a process other than the one that took the PATCH.
//
// Named mutant, which the first two forced checks (TestTwoPatchesOfDifferentFieldsHeldOnOneRowAreBothKept and
// TestAPatchHeldAtTheProxyMergesOntoAFullBodySaveThatLandedMeanwhile) must fail on every run: a build that merges in the
// process, from the row read at authentication, and writes the document whole (UPDATE users SET configuration = $2). The
// first fails it because both PATCHes read the document before the lock is released and the later write puts back the field
// of the first; the second because the POST's change lands between the authentication and the write. The third forced
// check (one field, the later statement wins) pins the order and the 14 untouched fields, and passes the mutant. Nothing
// here depends on how long a request takes: every wait is for a state only the test's next step ends, bounded at 30 s (a hang).
//
// The checks name no table or column except the lock they take, on users.

const (
	ucPath       = "/jellyscale/v1/users/configuration"
	ucNewCast    = "F007D354"
	ucMaxBody    = 1 << 16
	ucTrackPrefs = "/jellyscale/v1/track-preferences"
)

var ucFields = []string{
	"AudioLanguagePreference", "PlayDefaultAudioTrack", "SubtitleLanguagePreference", "DisplayMissingEpisodes", "GroupedFolders",
	"SubtitleMode", "DisplayCollectionsView", "EnableLocalPassword", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes",
	"HidePlayedInLatest", "RememberAudioSelections", "RememberSubtitleSelections", "EnableNextEpisodeAutoPlay",
}

// Compact lower-case ids, as Jellyfin answers them.
const (
	ucA = "0a0a0a0a0a0a40408a8a0a0a0a0a0a0a"
	ucB = "0b0b0b0b0b0b40408b8b0b0b0b0b0b0b"
	ucC = "0c0c0c0c0c0c40408c8c0c0c0c0c0c0c"
	ucD = "0d0d0d0d0d0d40408d8d0d0d0d0d0d0d"
	ucE = "0e0e0e0e0e0e40408e8e0e0e0e0e0e0e"
	ucF = "0f0f0f0f0f0f40408f8f0f0f0f0f0f0f"
)

// ucNew is a new Jellyfin user's Configuration (compat/captures/user-configuration-12.4.md): 15 fields and no audio language.
func ucNew() map[string]any {
	return map[string]any{
		"PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "", "DisplayMissingEpisodes": false, "GroupedFolders": []any{},
		"SubtitleMode": "Default", "DisplayCollectionsView": false, "EnableLocalPassword": false, "OrderedViews": []any{},
		"LatestItemsExcludes": []any{}, "MyMediaExcludes": []any{}, "HidePlayedInLatest": true, "RememberAudioSelections": true,
		"RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true, "CastReceiverId": ucNewCast,
	}
}

// ucAway has all 15 savable fields away from the new user's.
func ucAway() map[string]any {
	return map[string]any{
		"AudioLanguagePreference": "eng", "PlayDefaultAudioTrack": false, "SubtitleLanguagePreference": "deu", "DisplayMissingEpisodes": true,
		"GroupedFolders": []any{ucA, ucB}, "SubtitleMode": "Always", "DisplayCollectionsView": true, "EnableLocalPassword": true,
		"OrderedViews": []any{ucC}, "LatestItemsExcludes": []any{ucD}, "MyMediaExcludes": []any{ucE}, "HidePlayedInLatest": false,
		"RememberAudioSelections": false, "RememberSubtitleSelections": false, "EnableNextEpisodeAutoPlay": false, "CastReceiverId": ucNewCast,
	}
}

// ucOther is, for each field, a value other than the one ucAway stores.
var ucOther = map[string]any{
	"AudioLanguagePreference": "fra", "PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "ita", "DisplayMissingEpisodes": false,
	"GroupedFolders": []any{ucF}, "SubtitleMode": "None", "DisplayCollectionsView": false, "EnableLocalPassword": false,
	"OrderedViews": []any{ucD, ucC}, "LatestItemsExcludes": []any{}, "MyMediaExcludes": []any{ucA}, "HidePlayedInLatest": true,
	"RememberAudioSelections": true, "RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true,
}

func ucJSON(m map[string]any) string {
	raw, _ := json.Marshal(m)
	return string(raw)
}

func ucWith(base map[string]any, change map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range change {
		out[k] = v
	}
	return out
}

func ucWithout(base map[string]any, names ...string) map[string]any {
	out := ucWith(base, nil)
	for _, n := range names {
		delete(out, n)
	}
	return out
}

func ucParse(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not a JSON object: %v: %.300s", err, raw)
	}
	return out
}

// ucRig is two api processes on one Postgres and users that have saved nothing.
type ucRig struct {
	dsn   string
	a, b  *proc
	users configUsers
}

func newUCRig(t *testing.T) *ucRig {
	t.Helper()
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	return &ucRig{dsn: dsn, a: a, b: b, users: newConfigUsers(t, dsn, "uc"+uuid.NewString()[:8])}
}

func (g *ucRig) token(who string) string { return g.users.token(who, 0) }

// save stores a whole Configuration as jellyfin-web does, with Jellyfin's route.
func ucSave(t *testing.T, r *proc, token string, config map[string]any) {
	t.Helper()
	if status, raw := doRequest(t, r, "POST", "/Users/Configuration", token, "application/json", ucJSON(ucWithout(config, "CastReceiverId"))); status != 204 {
		t.Fatalf("POST /Users/Configuration: %d %s", status, raw)
	}
}

// ucMe is the Configuration GET /Users/Me answers.
func ucMe(t *testing.T, r *proc, token string) map[string]any {
	t.Helper()
	status, raw := doRequest(t, r, "GET", "/Users/Me", token, "", "")
	if status != 200 {
		t.Fatalf("GET /Users/Me on %s: %d %s", r.addr, status, raw)
	}
	config, _ := ucParse(t, raw)["Configuration"].(map[string]any)
	return config
}

func ucSame(t *testing.T, what, token string, want map[string]any, procs ...*proc) {
	t.Helper()
	for _, r := range procs {
		if got := ucMe(t, r, token); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: GET /Users/Me on %s answers %s, want %s", what, r.addr, ucJSON(got), ucJSON(want))
		}
	}
}

// ucPatch is a PATCH that must answer 200 with the whole 16-field Configuration, which is what the next GET /Users/Me
// answers.
func ucPatch(t *testing.T, r, reader *proc, token, body string) map[string]any {
	t.Helper()
	status, raw := doRequest(t, r, "PATCH", ucPath, token, "application/json", body)
	if status != 200 {
		t.Fatalf("PATCH %s %s on %s: %d %s", ucPath, abbreviated(body), r.addr, status, raw)
	}
	answer := ucParse(t, raw)
	if _, has := answer["AudioLanguagePreference"]; len(answer) != 16-map[bool]int{true: 0, false: 1}[has] || answer["CastReceiverId"] != ucNewCast {
		t.Fatalf("PATCH %s answered %s: want the whole Configuration, 16 fields (AudioLanguagePreference absent until saved), CastReceiverId %s", abbreviated(body), raw, ucNewCast)
	}
	if got := ucMe(t, reader, token); !reflect.DeepEqual(got, answer) {
		t.Fatalf("PATCH %s answered %s and the next GET /Users/Me answers %s", abbreviated(body), raw, ucJSON(got))
	}
	return answer
}

// ucRefused is a PATCH that must answer 400 with a text, which names the field when one is given.
func ucRefused(t *testing.T, r *proc, token, body, field string) {
	t.Helper()
	status, raw := doRequest(t, r, "PATCH", ucPath, token, "application/json", body)
	if status != 400 {
		t.Fatalf("PATCH %s answered %d %s, want 400", abbreviated(body), status, raw)
	}
	if strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("PATCH %s answered 400 with no text", abbreviated(body))
	}
	if field != "" && !strings.Contains(string(raw), field) {
		t.Errorf("PATCH %s answered 400 %q: the text does not name %s", abbreviated(body), raw, field)
	}
}

// Each of the 15 fields patched alone, from a document with all 15 away from their defaults, changes that field and leaves
// the other 14; so does one body naming three. The answer is the whole Configuration, as the next GET /Users/Me; the
// results are read on the other process, and again after both processes restart.
func TestEachFieldPatchedAloneChangesOnlyThatFieldAndSurvivesARestart(t *testing.T) {
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	type result struct {
		token string
		want  map[string]any
	}
	var results []result
	cases := []map[string]any{}
	for _, field := range ucFields {
		cases = append(cases, map[string]any{field: ucOther[field]})
	}
	cases = append(cases, map[string]any{"SubtitleMode": "Smart", "GroupedFolders": []any{ucC}, "AudioLanguagePreference": "OriginalLanguage"})
	for n, change := range cases {
		token := newConfigUsers(t, dsn, fmt.Sprintf("ucone%d%s", n, uuid.NewString()[:6])).token("U", 0)
		writer, reader := a, b
		if n%2 == 1 {
			writer, reader = b, a
		}
		ucSave(t, writer, token, ucAway())
		want := ucWith(ucAway(), change)
		if got := ucPatch(t, writer, reader, token, ucJSON(change)); !reflect.DeepEqual(got, want) {
			t.Errorf("PATCH %s answered %s, want %s", ucJSON(change), ucJSON(got), ucJSON(want))
		}
		ucSame(t, "after PATCH "+ucJSON(change), token, want, a, b)
		results = append(results, result{token, want})
	}
	a.kill()
	b.kill()
	a, b = bothProcesses(t, dsn)
	for _, r := range results {
		ucSame(t, "after both processes restarted", r.token, r.want, a, b)
	}
}

// A viewer who has saved nothing and patches one field reads that field and the new user's value for the other 14
// (AudioLanguagePreference absent); {} answers 200 with the stored document and changes nothing.
func TestAViewerWhoSavedNothingPatchesOneFieldAndReadsTheNewUsersValuesForTheRest(t *testing.T) {
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	for _, field := range ucFields {
		token := newConfigUsers(t, dsn, "ucfirst"+uuid.NewString()[:8]).token("U", 0)
		want := ucWith(ucNew(), map[string]any{field: ucOther[field]})
		if got := ucPatch(t, a, b, token, ucJSON(map[string]any{field: ucOther[field]})); !reflect.DeepEqual(got, want) {
			t.Errorf("a first PATCH of %s answered %s, want %s", field, ucJSON(got), ucJSON(want))
		}
		ucSame(t, "after a first PATCH of "+field, token, want, a, b)
	}
	fresh := newConfigUsers(t, dsn, "ucempty"+uuid.NewString()[:8])
	if got := ucPatch(t, a, b, fresh.token("U", 0), `{}`); !reflect.DeepEqual(got, ucNew()) {
		t.Errorf("{} by a viewer who saved nothing answered %s, want the new user's %s", ucJSON(got), ucJSON(ucNew()))
	}
	saved := newConfigUsers(t, dsn, "ucemptysaved"+uuid.NewString()[:8]).token("U", 0)
	ucSave(t, a, saved, ucAway())
	if got := ucPatch(t, b, a, saved, `{}`); !reflect.DeepEqual(got, ucAway()) {
		t.Errorf("{} answered %s, want the stored %s", ucJSON(got), ucJSON(ucAway()))
	}
	ucSame(t, "after {}", saved, ucAway(), a, b)
}

// A list that is named replaces the stored list whole; a repeated id stays repeated, in the order sent, compact and lower case.
func TestAListThatIsNamedReplacesTheStoredListWholeAndKeepsItsOrderAndRepeats(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	ucSave(t, g.a, token, ucAway()) // GroupedFolders [a, b].
	for n, c := range []struct {
		sent string
		want []any
	}{
		{`["` + ucC + `"]`, []any{ucC}},
		{`[]`, []any{}},
		{`["` + ucB + `","` + ucA + `","` + ucB + `"]`, []any{ucB, ucA, ucB}},
		{`["0A0A0A0A-0A0A-4040-8A8A-0A0A0A0A0A0A","` + ucC + `"]`, []any{ucA, ucC}}, // Dashed and upper case read compact and lower case.
	} {
		for _, field := range []string{"GroupedFolders", "OrderedViews", "LatestItemsExcludes", "MyMediaExcludes"} {
			writer, reader := g.a, g.b
			if n%2 == 1 {
				writer, reader = g.b, g.a
			}
			got := ucPatch(t, writer, reader, token, fmt.Sprintf(`{%q:%s}`, field, c.sent))
			if !reflect.DeepEqual(got[field], any(c.want)) {
				t.Errorf("%s %s answered %v, want %v", field, c.sent, got[field], c.want)
			}
		}
	}
}

// Values are read as POST /Users/Configuration reads them: for each value, the field after a POST of it alone and after a
// PATCH of it alone is the same, and a value the POST refuses is 400 here. null is the one difference: the POST takes it
// for some fields, the PATCH refuses it for all.
func TestAValueIsReadAsPostUsersConfigurationReadsIt(t *testing.T) {
	g := newUCRig(t)
	u, o := g.token("U"), g.token("O")
	values := []struct {
		field, value string
		accepted     bool
	}{
		{"SubtitleMode", `"always"`, true}, {"SubtitleMode", `"ALWAYS"`, true}, {"SubtitleMode", `1`, true}, {"SubtitleMode", `0`, true},
		{"SubtitleMode", `2`, true}, {"SubtitleMode", `3`, true}, {"SubtitleMode", `4`, true}, {"SubtitleMode", `"OnlyForced"`, true},
		{"SubtitleMode", `"Sometimes"`, false}, {"SubtitleMode", `""`, false}, {"SubtitleMode", `5`, false}, {"SubtitleMode", `-1`, false},
		{"SubtitleMode", `1.5`, false}, {"SubtitleMode", `true`, false}, {"SubtitleMode", `["Always"]`, false},
		{"AudioLanguagePreference", `""`, true}, {"AudioLanguagePreference", `"OriginalLanguage"`, true}, {"AudioLanguagePreference", `"deu"`, true},
		{"AudioLanguagePreference", `1`, false}, {"AudioLanguagePreference", `true`, false},
		{"SubtitleLanguagePreference", `""`, true}, {"SubtitleLanguagePreference", `"fra"`, true}, {"SubtitleLanguagePreference", `1`, false},
		{"GroupedFolders", `["0A0A0A0A-0A0A-4040-8A8A-0A0A0A0A0A0A"]`, true}, {"GroupedFolders", `["` + ucA + `","` + ucA + `"]`, true},
		{"OrderedViews", `[]`, true}, {"LatestItemsExcludes", `["` + ucB + `"]`, true}, {"MyMediaExcludes", `["` + ucC + `"]`, true},
		{"GroupedFolders", `"` + ucA + `"`, false}, {"GroupedFolders", `["not-an-id"]`, false}, {"OrderedViews", `[1]`, false},
		{"LatestItemsExcludes", `[""]`, false}, {"MyMediaExcludes", `{}`, false},
		{"PlayDefaultAudioTrack", `false`, true}, {"PlayDefaultAudioTrack", `"true"`, false}, {"HidePlayedInLatest", `"false"`, false},
		{"DisplayMissingEpisodes", `1`, false}, {"EnableLocalPassword", `true`, true}, {"RememberSubtitleSelections", `"x"`, false},
	}
	for _, c := range values {
		body := fmt.Sprintf(`{%q:%s}`, c.field, c.value)
		// The POST is the reference: a new user's document with this one field, in a user of its own each time.
		ucSave(t, g.a, o, ucNew()) // A POST of the new user's own fields resets O.
		postStatus, _ := doRequest(t, g.a, "POST", "/Users/Configuration", o, "application/json", body)
		if (postStatus == 204) != c.accepted {
			t.Fatalf("POST %s answered %d: this check's table is wrong about what Jellyfin takes", body, postStatus)
		}
		ucSave(t, g.a, u, ucNew())
		status, raw := doRequest(t, g.b, "PATCH", ucPath, u, "application/json", body)
		if !c.accepted {
			if status != 400 || !strings.Contains(string(raw), c.field) {
				t.Errorf("PATCH %s answered %d %s, want 400 naming %s", body, status, raw, c.field)
			}
			ucSame(t, "after the refused "+body, u, ucNew(), g.a, g.b)
			continue
		}
		if status != 200 {
			t.Errorf("PATCH %s answered %d %s, want 200 (the POST took it)", body, status, raw)
			continue
		}
		want := ucMe(t, g.a, o)
		if got := ucParse(t, raw); !reflect.DeepEqual(got, want) {
			t.Errorf("PATCH %s answered %s, the POST of it leaves %s", body, raw, ucJSON(want))
		}
		ucSame(t, "after PATCH "+body, u, want, g.a, g.b)
	}
	// What Jellyfin's capture says, so that a wrong reading by both routes is seen.
	for _, c := range []struct {
		body  string
		field string
		want  any
	}{
		{`{"SubtitleMode":"always"}`, "SubtitleMode", "Always"}, {`{"SubtitleMode":1}`, "SubtitleMode", "Always"}, {`{"SubtitleMode":0}`, "SubtitleMode", "Default"},
		{`{"AudioLanguagePreference":""}`, "AudioLanguagePreference", ""}, {`{"AudioLanguagePreference":"OriginalLanguage"}`, "AudioLanguagePreference", "OriginalLanguage"},
		{`{"GroupedFolders":["0A0A0A0A-0A0A-4040-8A8A-0A0A0A0A0A0A"]}`, "GroupedFolders", []any{ucA}},
	} {
		if got := ucPatch(t, g.a, g.b, u, c.body); !reflect.DeepEqual(got[c.field], c.want) {
			t.Errorf("PATCH %s reads %s as %v, want %v", c.body, c.field, got[c.field], c.want)
		}
	}
}

// A 400 names the field and stores nothing, and a body with one good field and one refused stores neither.
func TestARefusedFieldIsNamedAndNothingOfTheBodyIsStored(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	ucSave(t, g.a, token, ucAway())
	cases := []struct{ body, field string }{
		{`{"Bogus":1}`, "Bogus"}, {`{"CastReceiverId":"ABCD1234"}`, "CastReceiverId"}, {`{"CastReceiverId":"F007D354"}`, "CastReceiverId"},
		{`{"subtitleMode":"Always"}`, "subtitleMode"}, {`{"SUBTITLEMODE":"Always"}`, "SUBTITLEMODE"}, {`{"audiolanguagepreference":"deu"}`, "audiolanguagepreference"},
		{`{"SubtitleMode":"Always","SubtitleMode":"None"}`, "SubtitleMode"}, {`{"GroupedFolders":[],"GroupedFolders":[]}`, "GroupedFolders"},
		{`{"SubtitleMode":"Sometimes"}`, "SubtitleMode"}, {`{"SubtitleMode":5}`, "SubtitleMode"},
		{`{"GroupedFolders":["` + ucC + `","nope"]}`, "GroupedFolders"}, {`{"MyMediaExcludes":[7]}`, "MyMediaExcludes"},
		{`{"PlayDefaultAudioTrack":"true"}`, "PlayDefaultAudioTrack"}, {`{"SubtitleLanguagePreference":3}`, "SubtitleLanguagePreference"},
		// One good field beside a refused one stores neither.
		{`{"SubtitleMode":"None","Bogus":1}`, "Bogus"}, {`{"HidePlayedInLatest":true,"GroupedFolders":["nope"]}`, "GroupedFolders"},
		{`{"AudioLanguagePreference":"deu","SubtitleMode":null}`, "SubtitleMode"}, {`{"EnableLocalPassword":false,"CastReceiverId":"X"}`, "CastReceiverId"},
	}
	for _, field := range ucFields {
		cases = append(cases, struct{ body, field string }{`{"` + field + `":null}`, field})
		cases = append(cases, struct{ body, field string }{`{"` + field + `":null,"SubtitleLanguagePreference":"zzz"}`, field})
	}
	for n, c := range cases {
		r := []*proc{g.a, g.b}[n%2]
		ucRefused(t, r, token, c.body, c.field)
		ucSame(t, "after the refused "+c.body, token, ucAway(), g.a, g.b)
	}
}

// A body that is not an object, not JSON, empty, followed by more text or over 65,536 bytes is 400 and stores nothing; one of
// exactly 65,536 bytes is 200. No refused body logs a change.
func TestABodyThatIsNotOneObjectOrIsTooLongIsRefusedAndAnExactLimitIsAccepted(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	ucSave(t, g.a, token, ucAway())
	padded := func(body string, size int) string { return body + strings.Repeat(" ", size-len(body)) }
	marks := [2]int{len(infoLines(g.a)), len(infoLines(g.b))}
	change := `{"SubtitleLanguagePreference":"spa"}`
	for _, body := range []string{
		"not json", "", " ", "null", "[1,2]", "[]", `"x"`, "42", "true", `{} x`, `{}{}`, change + change, change + ` {"SubtitleMode":"None"}`,
		`{"SubtitleLanguagePreference":"spa"`, padded(change, ucMaxBody+1), padded(change, ucMaxBody+1000),
	} {
		ucRefused(t, g.b, token, body, "")
		ucSame(t, "after the refused body "+abbreviated(body), token, ucAway(), g.a, g.b)
	}
	if lines := infoLines(g.b); len(lines) != marks[1] {
		t.Errorf("refused bodies logged at Info: %v", lines[marks[1]:])
	}
	want := ucWith(ucAway(), map[string]any{"SubtitleLanguagePreference": "spa"})
	if got := ucPatch(t, g.a, g.b, token, padded(change, ucMaxBody)); !reflect.DeepEqual(got, want) {
		t.Errorf("a valid body of exactly %d bytes answered %s", ucMaxBody, ucJSON(got))
	}
	ucSame(t, "after the body of exactly 65,536 bytes", token, want, g.a, g.b)
}

// The route acts on the caller: an administrator's PATCH with no id or with their own id (any spelling) changes the
// administrator's settings alone and another viewer's 16 fields stay as they were; a PATCH naming another user's id is
// refused with 403 for everyone, an administrator included, and changes nothing (JS-127, D-075); no token and an unknown
// token are 401.
func TestThePatchActsOnTheCallerOnly(t *testing.T) {
	g := newUCRig(t)
	viewer, other, admin := g.token("U"), g.token("O"), g.token("A")
	ucSave(t, g.a, viewer, ucAway())
	ucSave(t, g.a, other, ucWith(ucAway(), map[string]any{"SubtitleMode": "Smart", "AudioLanguagePreference": "ita"}))
	otherWant := ucWith(ucAway(), map[string]any{"SubtitleMode": "Smart", "AudioLanguagePreference": "ita"})
	u, a := g.users["U"], g.users["A"]
	adminWant := ucNew()
	for n, query := range []string{"", "?userId=" + a.compact(), "?userid=" + a.id, "?UserId=" + strings.ToUpper(a.compact())} {
		field, value := "SubtitleLanguagePreference", fmt.Sprintf("l%d", n)
		status, raw := doRequest(t, []*proc{g.a, g.b}[n%2], "PATCH", ucPath+query, admin, "application/json", fmt.Sprintf(`{%q:%q}`, field, value))
		if status != 200 {
			t.Fatalf("the administrator's PATCH%s: %d %s", query, status, raw)
		}
		adminWant[field] = value
		if got := ucParse(t, raw); !reflect.DeepEqual(got, adminWant) {
			t.Errorf("the administrator's PATCH%s answered %s, want their own %s", query, raw, ucJSON(adminWant))
		}
		ucSame(t, "the administrator after PATCH"+query, admin, adminWant, g.a, g.b)
		ucSame(t, "the viewer after the administrator's PATCH"+query, viewer, ucAway(), g.a, g.b)
		ucSame(t, "the second viewer after the administrator's PATCH"+query, other, otherWant, g.a, g.b)
	}
	// Naming another user is refused for the administrator, and nothing of anyone's changes.
	for _, query := range []string{"?userId=" + u.compact(), "?userid=" + u.id, "?UserId=" + strings.ToUpper(u.compact())} {
		if status, raw := doRequest(t, g.a, "PATCH", ucPath+query, admin, "application/json", `{"SubtitleLanguagePreference":"zzz"}`); status != 403 {
			t.Errorf("the administrator's PATCH%s naming another user: %d %s, want 403", query, status, raw)
		}
		ucSame(t, "the administrator after the refused PATCH"+query, admin, adminWant, g.a, g.b)
		ucSame(t, "the viewer after the refused PATCH"+query, viewer, ucAway(), g.a, g.b)
	}
	// A viewer's PATCH with ?userId= naming the administrator or the other viewer is refused too.
	for _, named := range []string{g.users["O"].compact(), a.compact()} {
		if status, raw := doRequest(t, g.b, "PATCH", ucPath+"?userId="+named, viewer, "application/json", `{"SubtitleMode":"None"}`); status != 403 {
			t.Fatalf("a viewer's PATCH naming another user: %d %s, want 403", status, raw)
		}
	}
	ucSame(t, "the viewer after naming another user", viewer, ucAway(), g.a, g.b)
	ucSame(t, "the other viewer after the first named them", other, otherWant, g.a, g.b)
	ucSame(t, "the administrator after the viewer named them", admin, adminWant, g.a, g.b)

	for _, r := range []*proc{g.a, g.b} {
		for _, bad := range []string{"", strings.Repeat("0", 32), uuid.NewString()} {
			for _, body := range []string{`{"SubtitleMode":"None"}`, `{}`, `not json`} {
				if status, raw := doRequest(t, r, "PATCH", ucPath, bad, "application/json", body); status != 401 {
					t.Errorf("PATCH %s with token %q on %s: %d %s, want 401", body, bad, r.addr, status, raw)
				}
			}
		}
	}
	ucSame(t, "the viewer after the 401s", viewer, ucAway(), g.a, g.b)
}

// A language saved through this PATCH is the one the rest of jellyscale reads: the track preferences' switch needs it.
func TestALanguageSavedByThePatchLetsATrackPreferenceSwitchThrough(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	if status, raw := doRequest(t, g.a, "PATCH", ucTrackPrefs, token, "application/json", `{"HideOtherAudio":true}`); status != 409 {
		t.Fatalf("before the language: %d %s, want 409", status, raw)
	}
	ucPatch(t, g.b, g.a, token, `{"AudioLanguagePreference":"deu"}`)
	for _, r := range []*proc{g.a, g.b} {
		if status, raw := doRequest(t, r, "PATCH", ucTrackPrefs, token, "application/json", `{"HideOtherAudio":true}`); status != 200 {
			t.Errorf("after the language saved through the PATCH, on %s: %d %s, want 200", r.addr, status, raw)
		}
	}
	// A subtitle language too.
	if status, _ := doRequest(t, g.a, "PATCH", ucTrackPrefs, token, "application/json", `{"HideOtherSubtitles":true}`); status != 409 {
		t.Errorf("HideOtherSubtitles before its language: %d, want 409", status)
	}
	ucPatch(t, g.a, g.b, token, `{"SubtitleLanguagePreference":"fra"}`)
	if status, raw := doRequest(t, g.b, "PATCH", ucTrackPrefs, token, "application/json", `{"HideOtherSubtitles":true}`); status != 200 {
		t.Errorf("HideOtherSubtitles after its language: %d %s, want 200", status, raw)
	}
}

// ucLockWaiting waits, 30 s at most (a hang, not a measure), until a statement of the application that writes the
// configuration is waiting on a lock: the PATCH's own, not another statement of the process.
func ucLockWaiting(t *testing.T, pool *pgxpool.Pool, application string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND state = 'active' AND query ILIKE '%configuration%'
			AND pid <> pg_backend_pid()`, application).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement of %s naming the configuration is waiting on a lock 30 s after its PATCH was sent: the PATCH does not wait on the viewer's row", application)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Two PATCHes of different fields through two api processes, both held on the viewer's row (the test holds it FOR UPDATE and
// waits until each process's statement is waiting on a lock), are released together: both answer 200, each answer carries its
// own change, one carries both, and the document has both with the other 13 fields as they were. Fails if they never waited.
func TestTwoPatchesOfDifferentFieldsHeldOnOneRowAreBothKept(t *testing.T) {
	dsn := testenv.DatabaseURL(t)
	tag := uuid.NewString()[:8]
	a := start(t, "api", withApp(dsn, "uc-a-"+tag), nil)
	b := start(t, "api", withApp(dsn, "uc-b-"+tag), nil)
	users := newConfigUsers(t, dsn, "ucheld"+tag)
	token := users.token("U", 0)
	base := ucWith(ucAway(), map[string]any{"SubtitleMode": "None"})
	ucSave(t, a, token, base)
	ucMe(t, a, token) // Both processes have a connection and have answered for the viewer.
	ucMe(t, b, token)

	pool := connect(t, dsn)
	ctx := context.Background()
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			lock.Rollback(ctx) //nolint:errcheck
		}
	}
	t.Cleanup(release)
	if _, err := lock.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, users["U"].id); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		status int
		raw    []byte
	}
	answers := make([]answer, 2)
	var wg sync.WaitGroup
	for n, one := range []struct {
		r    *proc
		body string
	}{{a, `{"SubtitleMode":"Always"}`}, {b, `{"AudioLanguagePreference":"deu"}`}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, raw := doRequest(t, one.r, "PATCH", ucPath, token, "application/json", one.body)
			answers[n] = answer{status, raw}
		}()
	}
	ucLockWaiting(t, pool, "uc-a-"+tag)
	ucLockWaiting(t, pool, "uc-b-"+tag)
	release()
	wg.Wait()
	for n, got := range answers {
		if got.status != 200 {
			t.Fatalf("PATCH %d held on the viewer's row answered %d %s, want 200", n, got.status, got.raw)
		}
	}
	first, second := ucParse(t, answers[0].raw), ucParse(t, answers[1].raw)
	both := ucWith(base, map[string]any{"SubtitleMode": "Always", "AudioLanguagePreference": "deu"})
	if first["SubtitleMode"] != "Always" || second["AudioLanguagePreference"] != "deu" {
		t.Errorf("an answer lacks its own change:\n%s\n%s", answers[0].raw, answers[1].raw)
	}
	if !reflect.DeepEqual(first, both) && !reflect.DeepEqual(second, both) {
		t.Errorf("neither answer carries both changes (the later statement merges onto the earlier):\n%s\n%s", answers[0].raw, answers[1].raw)
	}
	ucSame(t, "after both PATCHes were released", token, both, a, b)
}

// ucHeldRig is a process behind the proxy, where a statement is held, and another process on the database itself. The
// proxied process sends each statement's text (exec mode), so that the hook sees the UPDATE and not a prepare of it.
type ucHeldRig struct {
	proxy         *testenv.DBProxy
	held, release chan struct{}
	patcher, rest *proc
	token, tokenB string
}

func newUCHeldRig(t *testing.T, base map[string]any) *ucHeldRig {
	t.Helper()
	dsn := testenv.DatabaseURL(t)
	proxy := testenv.NewDBProxy(t, dsn)
	g := &ucHeldRig{proxy: proxy, held: make(chan struct{}), release: make(chan struct{})}
	g.patcher = start(t, "api", proxy.DSN()+"&default_query_exec_mode=exec", nil)
	g.rest = start(t, "api", dsn, nil)
	users := newConfigUsers(t, dsn, "ucproxy"+uuid.NewString()[:8])
	g.token, g.tokenB = users.token("U", 0), users.token("U", 1)
	ucSave(t, g.rest, g.token, base)
	ucMe(t, g.patcher, g.token) // The proxied process has answered for the viewer: what is held is the PATCH, not a first connection.
	return g
}

// hold arms the proxy to hold, before it goes on to Postgres, the first statement that writes the viewer's
// configuration. The request has authenticated by then: the statement that does is a read.
func (g *ucHeldRig) hold(t *testing.T) <-chan struct{} {
	t.Helper()
	writesConfiguration := func(sent []byte) bool {
		text := strings.ToLower(string(sent))
		return strings.Contains(text, "update") && strings.Contains(text, "users") && strings.Contains(text, "configuration") && !strings.Contains(text, "last_login")
	}
	return g.proxy.OnStatement(writesConfiguration, func() {
		close(g.held)
		select {
		case <-g.release:
		case <-time.After(30 * time.Second):
		}
	}, nil)
}

// waitHeld fails when the statement was never held: a PATCH that sent no statement writing the configuration (or sent
// it before the hook), so the interleaving the test names did not happen.
func (g *ucHeldRig) waitHeld(t *testing.T, status <-chan int) {
	t.Helper()
	select {
	case <-g.held:
	case s := <-status:
		t.Fatalf("the PATCH answered %d without sending a statement that writes the users table's configuration: the hold never happened", s)
	case <-time.After(30 * time.Second):
		t.Fatal("the PATCH sent no statement that writes the users table's configuration in 30 s: the hold never happened")
	}
}

// ucAsync sends a request in the background and gives its status and body.
func ucAsync(t *testing.T, r *proc, token, method, path, body string) (status chan int, raw *[]byte) {
	var out []byte
	status = make(chan int, 1)
	go func() {
		s, b := doRequest(t, r, method, path, token, "application/json", body)
		out = b
		status <- s
	}()
	return status, &out
}

// A PATCH of one field is held at the proxy after its request authenticated, a full-body POST /Users/Configuration that
// sets another field and carries the old value of the PATCH's field answers 204 meanwhile, the PATCH is released and
// answers 200. The result, stored and answered, is the POST's 15 fields with the PATCH's change: the PATCH merges onto
// what is stored when its statement runs, not onto what the request read. A build that merges in the process fails it.
func TestAPatchHeldAtTheProxyMergesOntoAFullBodySaveThatLandedMeanwhile(t *testing.T) {
	base := ucAway()
	g := newUCHeldRig(t, base)
	hooked := g.hold(t)
	status, raw := ucAsync(t, g.patcher, g.token, "PATCH", ucPath, `{"AudioLanguagePreference":"deu"}`)
	g.waitHeld(t, status)
	posted := ucWith(base, map[string]any{"SubtitleLanguagePreference": "fra"}) // Carries the old audio language, eng.
	if s, body := doRequest(t, g.rest, "POST", "/Users/Configuration", g.tokenB, "application/json", ucJSON(ucWithout(posted, "CastReceiverId"))); s != 204 {
		t.Fatalf("the POST while the PATCH is held answered %d %s, want 204", s, body)
	}
	close(g.release)
	if s := <-status; s != 200 {
		t.Fatalf("the released PATCH answered %d %s, want 200", s, *raw)
	}
	<-hooked
	want := ucWith(posted, map[string]any{"AudioLanguagePreference": "deu"})
	if got := ucParse(t, *raw); !reflect.DeepEqual(got, want) {
		t.Errorf("the PATCH answered %s, want the POST's fields with audio deu: %s", *raw, ucJSON(want))
	}
	ucSame(t, "after the held PATCH was released", g.token, want, g.patcher, g.rest)
}

// Two PATCHes of one field: the first is held at the proxy, the second answers 200 meanwhile, the first is released and
// answers 200: the later statement wins, and the other 14 fields are untouched.
func TestTwoPatchesOfOneFieldAreDecidedByTheLaterStatement(t *testing.T) {
	base := ucAway()
	g := newUCHeldRig(t, base)
	hooked := g.hold(t)
	status, raw := ucAsync(t, g.patcher, g.token, "PATCH", ucPath, `{"SubtitleMode":"Always"}`)
	g.waitHeld(t, status)
	if got := ucPatch(t, g.rest, g.rest, g.tokenB, `{"SubtitleMode":"None"}`); got["SubtitleMode"] != "None" {
		t.Fatalf("the second PATCH answered %s, want None", ucJSON(got))
	}
	close(g.release)
	if s := <-status; s != 200 {
		t.Fatalf("the released PATCH answered %d %s, want 200", s, *raw)
	}
	<-hooked
	want := ucWith(base, map[string]any{"SubtitleMode": "Always"})
	if got := ucParse(t, *raw); !reflect.DeepEqual(got, want) {
		t.Errorf("the first PATCH answered %s, want Always and the other 14 as they were: %s", *raw, ucJSON(want))
	}
	ucSame(t, "after the first PATCH was released", g.token, want, g.patcher, g.rest)
}

// The limit, which the route does not remove: a full-body POST built from a GET taken before a PATCH, sent after the PATCH
// answered, wins whole (the PATCHed field reads its old value again), as on Jellyfin 12.4.
func TestAWholeObjectSaveFromAnOlderReadUndoesAPatch(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	ucSave(t, g.a, token, ucAway())
	older := ucMe(t, g.a, token) // What jellyfin-web holds.
	ucPatch(t, g.b, g.a, token, `{"SubtitleMode":"None","AudioLanguagePreference":"deu"}`)
	if status, raw := doRequest(t, g.a, "POST", "/Users/"+g.users["U"].compact()+"/Configuration", token, "application/json", ucJSON(older)); status != 204 {
		t.Fatalf("the POST of the older read: %d %s", status, raw)
	}
	ucSame(t, "after the POST of the older read", token, ucAway(), g.a, g.b)
}

// A change is logged at Info with the user id and the names of the fields sent, once per accepted non-empty body, in the
// process that served it. {}, a 400 and a 401 log nothing at Info.
func TestAChangeIsLoggedOnceWithTheUserIdAndTheNamesOfTheFieldsSent(t *testing.T) {
	g := newUCRig(t)
	token := g.token("U")
	u := g.users["U"]
	ucMe(t, g.a, token)
	ucMe(t, g.b, token)
	mark, markB := len(infoLines(g.a)), len(infoLines(g.b))
	for _, body := range []string{
		`{"SubtitleMode":"Always"}`, `{"GroupedFolders":["` + ucA + `"],"HidePlayedInLatest":false}`,
		`{"SubtitleMode":"Always"}`, // Nothing changes, and it is a change that is asked for: logged again.
	} {
		var names []string
		var sent map[string]json.RawMessage
		json.Unmarshal([]byte(body), &sent) //nolint:errcheck
		for name := range sent {
			names = append(names, name)
		}
		ucPatch(t, g.a, g.b, token, body)
		deadline := time.Now().Add(10 * time.Second)
		for len(infoLines(g.a)) <= mark && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		quiet(t, "PATCH "+body, g.a, mark+1)
		line := infoLines(g.a)[mark]
		mark++
		lower := strings.ToLower(line)
		if !strings.Contains(lower, strings.ToLower(u.id)) && !strings.Contains(lower, strings.ToLower(u.compact())) {
			t.Errorf("PATCH %s: the line does not hold the user id: %s", body, line)
		}
		for _, name := range ucFields {
			if mentioned, sentName := strings.Contains(lower, strings.ToLower(name)), bytes.Contains([]byte(body), []byte(`"`+name+`"`)); mentioned != sentName {
				t.Errorf("PATCH %s: the line names %s: %v, but it was sent: %v: %s", body, name, mentioned, sentName, line)
			}
		}
	}
	for _, c := range []struct {
		r     *proc
		token string
		body  string
	}{
		{g.a, token, `{}`}, {g.a, token, `{"Bogus":1}`}, {g.a, token, `{"SubtitleMode":null}`}, {g.a, token, `not json`},
		{g.a, "", `{"SubtitleMode":"None"}`}, {g.a, strings.Repeat("0", 32), `{"SubtitleMode":"None"}`},
	} {
		doRequest(t, c.r, "PATCH", ucPath, c.token, "application/json", c.body)
		quiet(t, "PATCH "+c.body, c.r, mark)
	}
	if lines := infoLines(g.b); len(lines) != markB {
		t.Errorf("the other process logged at Info, though it served no PATCH: %v", lines[markB:])
	}
}
