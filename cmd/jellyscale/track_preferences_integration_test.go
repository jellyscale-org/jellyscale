//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/testenv"
)

// JS-118: a viewer's track preferences, GET and PATCH /jellyscale/v1/track-preferences, the first route of the
// namespace. They are stored (Postgres, not a process), per viewer, apart from the 16 fields of UserConfiguration.
// Every check runs against api processes of its own, on one Postgres, as the other settings checks of this package do.
//
// Named mutant, which both forced checks (TestTwoPatchesHeldOnOneRowThroughTwoProcessesAreBothKept and
// TestTwoFirstPatchesHeldOnOneRowAreBothKept) must fail on every run: a build that reads the document without a lock,
// merges the PATCH in the process and writes the document whole. Both PATCHes read the document before the test's lock
// is released (a plain read is not blocked by FOR UPDATE), both then wait on the row to write, and the second to write
// puts back the field of the first. Nothing here depends on how long a request takes: the waits are hangs bounded
// at 30 s and more.
//
// These checks name no table or column: the viewer's languages are saved through POST /Users/Configuration, and the
// only SQL is the lock the forced checks take, on users, and the query docs/running.md gives.

const tpPath = "/jellyscale/v1/track-preferences"

// tpDoc is the answer of the route: five fields, the two lists never nil.
type tpDoc struct {
	HideOtherAudio, HideOtherSubtitles        bool
	KeepAudioLanguages, KeepSubtitleLanguages []string
	ShowAllTracks                             bool
}

func tpDefaults() tpDoc {
	return tpDoc{KeepAudioLanguages: []string{}, KeepSubtitleLanguages: []string{}}
}

// tpAway is all five fields away from their defaults; tpAwayBody names all five.
func tpAway() tpDoc {
	return tpDoc{true, true, []string{"deu"}, []string{"fra"}, true}
}

const tpAwayBody = `{"HideOtherAudio":true,"HideOtherSubtitles":true,"KeepAudioLanguages":["deu"],"KeepSubtitleLanguages":["fra"],"ShowAllTracks":true}`

var tpFields = []string{"HideOtherAudio", "HideOtherSubtitles", "KeepAudioLanguages", "KeepSubtitleLanguages", "ShowAllTracks"}

// parseTP reads an answer of 200 and fails unless it is exactly the five fields, with lists that are arrays.
func parseTP(t *testing.T, raw []byte) tpDoc {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("the answer is not a JSON object: %v: %s", err, raw)
	}
	if len(fields) != len(tpFields) {
		t.Fatalf("the answer has %d fields, want exactly %v: %s", len(fields), tpFields, raw)
	}
	for _, name := range tpFields {
		value, ok := fields[name]
		if !ok {
			t.Fatalf("the answer has no %s: %s", name, raw)
		}
		if strings.HasPrefix(name, "Keep") && !bytes.HasPrefix(value, []byte("[")) {
			t.Fatalf("%s is %s, want an array (never null or absent): %s", name, value, raw)
		}
	}
	var doc tpDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return doc
}

func tpSend(t *testing.T, r *proc, method, token, path, body string) (int, []byte) {
	t.Helper()
	return doRequest(t, r, method, path, token, "application/json", body)
}

func tpGet(t *testing.T, r *proc, token string) tpDoc {
	t.Helper()
	status, raw := tpSend(t, r, "GET", token, tpPath, "")
	if status != 200 {
		t.Fatalf("GET %s on %s: %d %s", tpPath, r.addr, status, raw)
	}
	return parseTP(t, raw)
}

// tpPatch is a PATCH that must answer 200 with the document as stored, which is what the next GET answers.
func tpPatch(t *testing.T, r *proc, token, body string) tpDoc {
	t.Helper()
	status, raw := tpSend(t, r, "PATCH", token, tpPath, body)
	if status != 200 {
		t.Fatalf("PATCH %s %s on %s: %d %s", tpPath, body, r.addr, status, raw)
	}
	doc := parseTP(t, raw)
	if got := tpGet(t, r, token); !reflect.DeepEqual(got, doc) {
		t.Fatalf("PATCH %s answered %+v and the next GET answers %+v", body, doc, got)
	}
	return doc
}

// tpRefused is a PATCH that must answer the status, with a body that says what is wrong.
func tpRefused(t *testing.T, r *proc, token, body string, status int) string {
	t.Helper()
	got, raw := tpSend(t, r, "PATCH", token, tpPath, body)
	if got != status {
		t.Fatalf("PATCH %s answered %d %s, want %d", abbreviated(body), got, raw, status)
	}
	if strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("PATCH %s answered %d with no body: the answer says nothing of what is wrong", abbreviated(body), got)
	}
	return string(raw)
}

func abbreviated(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// tpSame fails unless every process answers want for the viewer.
func tpSame(t *testing.T, what string, token string, want tpDoc, procs ...*proc) {
	t.Helper()
	for _, r := range procs {
		if got := tpGet(t, r, token); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: GET on %s answers %+v, want %+v", what, r.addr, got, want)
		}
	}
}

// savePrefs saves the viewer's languages as jellyfin-web does, with POST /Users/Configuration.
func savePrefs(t *testing.T, r *proc, users configUsers, who, body string) {
	t.Helper()
	if status, raw := doRequest(t, r, "POST", "/Users/Configuration", users.token(who, 0), "application/json", body); status != 204 {
		t.Fatalf("POST /Users/Configuration %s: %d %s", body, status, raw)
	}
}

const bothLanguages = `{"AudioLanguagePreference":"eng","SubtitleLanguagePreference":"deu"}`

// tpRig is two api processes on one Postgres and a user of each kind with only a name and a password.
type tpRig struct {
	dsn   string
	a, b  *proc
	users configUsers
}

func newTPRig(t *testing.T) *tpRig {
	t.Helper()
	dsn := testenv.DatabaseURL(t)
	a, b := bothProcesses(t, dsn)
	return &tpRig{dsn: dsn, a: a, b: b, users: newConfigUsers(t, dsn, "tp"+uuid.NewString()[:8])}
}

func (g *tpRig) token(who string) string { return g.users.token(who, 0) }

// viewer is a user who has saved both languages through the first process, so that both switches may go on.
func (g *tpRig) viewer(t *testing.T, who string) string {
	t.Helper()
	savePrefs(t, g.a, g.users, who, bothLanguages)
	return g.token(who)
}

// Both processes answer the defaults to a viewer who saved nothing: five fields, false, false, [], [], false, the lists
// as arrays in the raw text too, and still arrays after a PATCH that names neither list and after one that empties one.
func TestAViewerWhoSavedNothingAnswersTheDefaultsWithListsThatAreArrays(t *testing.T) {
	g := newTPRig(t)
	token := g.token("U")
	for _, r := range []*proc{g.a, g.b} {
		status, raw := tpSend(t, r, "GET", token, tpPath, "")
		if status != 200 {
			t.Fatalf("GET on %s: %d %s", r.addr, status, raw)
		}
		if !reflect.DeepEqual(parseTP(t, raw), tpDefaults()) || strings.Contains(string(raw), "null") {
			t.Errorf("a viewer who saved nothing gets %s on %s, want false, false, [], [], false", raw, r.addr)
		}
	}
	savePrefs(t, g.a, g.users, "U", bothLanguages)
	for _, body := range []string{`{"ShowAllTracks":true}`, `{"KeepAudioLanguages":["deu"],"KeepSubtitleLanguages":["fra"]}`, `{"KeepAudioLanguages":[]}`, `{"KeepSubtitleLanguages":[]}`} {
		status, raw := tpSend(t, g.a, "PATCH", token, tpPath, body)
		if status != 200 || strings.Contains(string(raw), "null") {
			t.Fatalf("PATCH %s: %d %s, want 200 and no null", body, status, raw)
		}
		parseTP(t, raw)
	}
	if got := tpGet(t, g.b, token); !reflect.DeepEqual(got, tpDoc{false, false, []string{}, []string{}, true}) {
		t.Errorf("after the PATCHes the document is %+v", got)
	}
}

// From five values that are all away from their defaults, each field patched alone leaves the other four; {} changes
// nothing and answers the document.
func TestEachFieldPatchedAloneLeavesTheOtherFourAsStored(t *testing.T) {
	g := newTPRig(t)
	token := g.viewer(t, "U")
	cases := []struct {
		body   string
		change func(*tpDoc)
	}{
		{`{"HideOtherAudio":false}`, func(d *tpDoc) { d.HideOtherAudio = false }},
		{`{"HideOtherSubtitles":false}`, func(d *tpDoc) { d.HideOtherSubtitles = false }},
		{`{"KeepAudioLanguages":["ita"]}`, func(d *tpDoc) { d.KeepAudioLanguages = []string{"ita"} }},
		{`{"KeepSubtitleLanguages":[]}`, func(d *tpDoc) { d.KeepSubtitleLanguages = []string{} }},
		{`{"ShowAllTracks":false}`, func(d *tpDoc) { d.ShowAllTracks = false }},
		{`{}`, func(*tpDoc) {}},
	}
	for n, c := range cases {
		patcher := g.a
		if n%2 == 1 {
			patcher = g.b // The reset and the change go through one process, the reads through both.
		}
		if got := tpPatch(t, patcher, token, tpAwayBody); !reflect.DeepEqual(got, tpAway()) {
			t.Fatalf("PATCH of all five answered %+v, want %+v", got, tpAway())
		}
		want := tpAway()
		c.change(&want)
		if got := tpPatch(t, patcher, token, c.body); !reflect.DeepEqual(got, want) {
			t.Errorf("PATCH %s answered %+v, want %+v", c.body, got, want)
		}
		tpSame(t, "after PATCH "+c.body, token, want, g.a, g.b)
	}
}

// A first PATCH by a viewer who has saved nothing answers the four fields it did not name at their defaults.
func TestAFirstPatchAnswersTheFieldsItDidNotNameAtTheirDefaults(t *testing.T) {
	for _, c := range []struct {
		body string
		want tpDoc
	}{
		{`{"HideOtherAudio":true}`, tpDoc{true, false, []string{}, []string{}, false}},
		{`{"HideOtherSubtitles":true}`, tpDoc{false, true, []string{}, []string{}, false}},
		{`{"KeepAudioLanguages":["deu"]}`, tpDoc{false, false, []string{"deu"}, []string{}, false}},
		{`{"KeepSubtitleLanguages":["fra"]}`, tpDoc{false, false, []string{}, []string{"fra"}, false}},
		{`{"ShowAllTracks":true}`, tpDoc{false, false, []string{}, []string{}, true}},
	} {
		g := newTPRig(t)
		token := g.viewer(t, "U") // Saves the settings of UserConfiguration, not a document of these.
		if got := tpPatch(t, g.a, token, c.body); !reflect.DeepEqual(got, c.want) {
			t.Errorf("a first PATCH %s answered %+v, want %+v", c.body, got, c.want)
		}
		tpSame(t, "after the first PATCH "+c.body, token, c.want, g.a, g.b)
	}
}

// A list a PATCH names is replaced whole, never added to.
func TestAListAPatchNamesIsReplacedWholeNeverAddedTo(t *testing.T) {
	g := newTPRig(t)
	token := g.token("U")
	for _, field := range []string{"KeepAudioLanguages", "KeepSubtitleLanguages"} {
		for n, c := range []struct {
			list string
			want []string
		}{{`["deu"]`, []string{"deu"}}, {`["fra"]`, []string{"fra"}}, {`[]`, []string{}}} {
			r := []*proc{g.a, g.b}[n%2]
			doc := tpPatch(t, r, token, fmt.Sprintf(`{%q:%s}`, field, c.list))
			got := doc.KeepAudioLanguages
			if field == "KeepSubtitleLanguages" {
				got = doc.KeepSubtitleLanguages
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s %s answered %v, want %v", field, c.list, got, c.want)
			}
		}
	}
}

// Stored, not remembered by a process: the very next request through the other process answers it, with no retry, and
// both processes answer it again after both restart.
func TestWhatOneProcessStoredTheOtherAnswersAtOnceAndBothAfterARestart(t *testing.T) {
	g := newTPRig(t)
	token := g.viewer(t, "U")
	tpGet(t, g.a, token) // Both have answered for the viewer before it saves: a process that kept what it read would show it.
	tpGet(t, g.b, token)
	tpPatch(t, g.a, token, tpAwayBody)
	if got := tpGet(t, g.b, token); !reflect.DeepEqual(got, tpAway()) {
		t.Fatalf("the process that did not store it answers %+v on the next request, want %+v", got, tpAway())
	}
	second := tpDoc{false, true, []string{"ita", "spa"}, []string{}, false}
	tpPatch(t, g.b, token, `{"HideOtherAudio":false,"KeepAudioLanguages":["ita","spa"],"KeepSubtitleLanguages":[],"ShowAllTracks":false}`)
	tpSame(t, "after the second PATCH", token, second, g.a, g.b)

	g.a.kill()
	g.b.kill()
	g.a, g.b = bothProcesses(t, g.dsn)
	tpSame(t, "after both processes restarted", token, second, g.a, g.b)
}

// One viewer's document is another's own: B's GET is the defaults after A saved all five, and A's is A's after B saved.
func TestTwoViewersKeepTheirOwnTrackPreferences(t *testing.T) {
	g := newTPRig(t)
	a, b := g.viewer(t, "U"), g.viewer(t, "O")
	tpPatch(t, g.a, a, tpAwayBody)
	tpSame(t, "B after A saved", b, tpDefaults(), g.a, g.b)
	other := tpDoc{true, false, []string{"ita"}, []string{}, false}
	tpPatch(t, g.b, b, `{"HideOtherAudio":true,"KeepAudioLanguages":["ita"]}`)
	tpSame(t, "A after B saved", a, tpAway(), g.a, g.b)
	tpSame(t, "B", b, other, g.a, g.b)
}

// withApp names a process's Postgres sessions, so that a test can tell which process a waiting session belongs to.
func withApp(dsn, name string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "application_name=" + name
}

// waitingOn waits, for 30 s at most (a hang, not a measure), until one session of the application is waiting on a lock.
func waitingOn(t *testing.T, pool *pgxpool.Pool, application string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE application_name = $1 AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`, application).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session of %s is waiting on a lock 30 s after its PATCH was sent: the PATCH does not wait on the viewer's row", application)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// twoPatchesHeld: the viewer's users row is locked FOR UPDATE by the test; a PATCH through each of two api processes
// is sent; both are seen waiting on a lock in pg_stat_activity, one session of each process; the lock is released.
// Both must answer 200 and each process must then answer the merge of both.
func twoPatchesHeld(t *testing.T, savedBefore bool) {
	dsn := testenv.DatabaseURL(t)
	tag := uuid.NewString()[:8]
	a := start(t, "api", withApp(dsn, "tp-a-"+tag), nil)
	b := start(t, "api", withApp(dsn, "tp-b-"+tag), nil)
	users := newConfigUsers(t, dsn, "held"+tag)
	token := users.token("U", 0)
	savePrefs(t, a, users, "U", `{"AudioLanguagePreference":"eng"}`)
	want := tpDoc{true, false, []string{}, []string{"deu"}, false}
	if savedBefore {
		tpPatch(t, a, token, `{"ShowAllTracks":true,"KeepAudioLanguages":["fra"]}`)
		want = tpDoc{true, false, []string{"fra"}, []string{"deu"}, true}
	}
	tpGet(t, a, token) // Both processes have a connection and have answered for the viewer.
	tpGet(t, b, token)

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
	answers := make(chan answer, 2)
	var wg sync.WaitGroup
	for _, one := range []struct {
		r    *proc
		body string
	}{{a, `{"HideOtherAudio":true}`}, {b, `{"KeepSubtitleLanguages":["deu"]}`}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, raw := doRequest(t, one.r, "PATCH", tpPath, token, "application/json", one.body)
			answers <- answer{status, raw}
		}()
	}
	waitingOn(t, pool, "tp-a-"+tag)
	waitingOn(t, pool, "tp-b-"+tag)
	release()
	wg.Wait()
	close(answers)
	for got := range answers {
		if got.status != 200 {
			t.Errorf("a PATCH held on the viewer's row answered %d %s, want 200", got.status, got.raw)
		}
	}
	tpSame(t, "after both PATCHes were released", token, want, a, b)
}

// The viewer has a stored document (ShowAllTracks true, KeepAudioLanguages ['fra']); neither PATCH may be lost.
func TestTwoPatchesHeldOnOneRowThroughTwoProcessesAreBothKept(t *testing.T) { twoPatchesHeld(t, true) }

// The viewer has no document yet: neither PATCH is a 500 and neither is lost.
func TestTwoFirstPatchesHeldOnOneRowAreBothKept(t *testing.T) { twoPatchesHeld(t, false) }

// Languages are stored once each, lowercase, as three-letter codes, in the order first sent, in both lists.
func TestLanguagesAreStoredOnceEachLowercaseAsThreeLetterCodesInTheOrderFirstSent(t *testing.T) {
	g := newTPRig(t)
	token := g.token("U")
	for n, c := range []struct{ sent, want string }{
		{`["GER","deu","de","fr"]`, `["deu","fra"]`},
		{`["fr","GER","de"]`, `["fra","deu"]`},
		{`["pt-BR","zh","en"]`, `["por","zho","eng"]`},
	} {
		for _, field := range []string{"KeepAudioLanguages", "KeepSubtitleLanguages"} {
			r := []*proc{g.a, g.b}[(n+len(field))%2]
			doc := tpPatch(t, r, token, fmt.Sprintf(`{%q:%s}`, field, c.sent))
			got, _ := json.Marshal(doc.KeepAudioLanguages)
			if field == "KeepSubtitleLanguages" {
				got, _ = json.Marshal(doc.KeepSubtitleLanguages)
			}
			if string(got) != c.want {
				t.Errorf("%s %s is stored as %s, want %s", field, c.sent, got, c.want)
			}
		}
		other := []*proc{g.a, g.b}[(n+1)%2]
		if got := tpGet(t, other, token); !reflect.DeepEqual(got.KeepAudioLanguages, got.KeepSubtitleLanguages) {
			t.Errorf("the two lists of one PATCH differ: %+v", got)
		}
	}
}

// storedState gives a viewer a document that is neither the defaults nor all away, to see what a refusal leaves.
func storedState(t *testing.T, g *tpRig, who string) (string, tpDoc) {
	t.Helper()
	token := g.viewer(t, who)
	want := tpDoc{true, false, []string{"fra"}, []string{"ita"}, true}
	tpPatch(t, g.a, token, `{"HideOtherAudio":true,"KeepAudioLanguages":["fra"],"KeepSubtitleLanguages":["ita"],"ShowAllTracks":true}`)
	return token, want
}

// A list entry that names no language is 400 with the language as sent, and nothing is stored.
func TestAListEntryThatNamesNoLanguageIsRefusedWithTheLanguageAsSent(t *testing.T) {
	g := newTPRig(t)
	token, want := storedState(t, g, "U")
	for _, field := range []string{"KeepAudioLanguages", "KeepSubtitleLanguages"} {
		for _, bad := range []string{"xx", "german", "", "und", "UND", "mis", "mul", "zxx"} {
			body := fmt.Sprintf(`{%q:["deu",%q]}`, field, bad)
			raw := tpRefused(t, g.a, token, body, 400)
			if bad != "" && !strings.Contains(raw, bad) {
				t.Errorf("PATCH %s: the 400 does not say the language as sent (%q): %s", body, bad, raw)
			}
			tpSame(t, "after the refused "+body, token, want, g.a, g.b)
		}
	}
}

// More than 20 languages, an unknown field beside valid ones, and a field of the wrong type are 400 naming the field.
func TestTooManyLanguagesAnUnknownFieldAndAWrongTypeAreRefusedNamingTheField(t *testing.T) {
	g := newTPRig(t)
	token, want := storedState(t, g, "U")
	codes := []string{"deu", "fra", "ita", "spa", "por", "rus", "jpn", "kor", "zho", "ara", "hin", "tur", "pol", "nld", "swe", "nor", "dan", "fin", "ces", "hun", "ell"}
	list := func(n int) string { raw, _ := json.Marshal(codes[:n]); return string(raw) }
	for _, field := range []string{"KeepAudioLanguages", "KeepSubtitleLanguages"} {
		body := fmt.Sprintf(`{%q:%s}`, field, list(21))
		if raw := tpRefused(t, g.a, token, body, 400); !strings.Contains(raw, field) {
			t.Errorf("21 languages in %s: the 400 does not name the field: %s", field, raw)
		}
		tpSame(t, "after 21 languages in "+field, token, want, g.a, g.b)
		doc := tpPatch(t, g.b, token, fmt.Sprintf(`{%q:%s}`, field, list(20)))
		if got := map[bool][]string{false: doc.KeepAudioLanguages, true: doc.KeepSubtitleLanguages}[field == "KeepSubtitleLanguages"]; !reflect.DeepEqual(got, codes[:20]) {
			t.Errorf("20 languages in %s are stored as %v", field, got)
		}
		tpPatch(t, g.a, token, tpAwayBody) // Back to a known document.
		want = tpAway()
	}
	for _, c := range []struct{ body, name string }{
		{`{"HideOtherAudio":false,"Bogus":1}`, "Bogus"},
		{`{"ShowAllTracks":true,"hideotheraudio2":false}`, "hideotheraudio2"},
		// Names are exact: another case is an unknown field, and a name sent twice is refused (not last-one-wins), so
		// v1 never accepts a spelling it would have to take away.
		{`{"hideotheraudio":true}`, "hideotheraudio"}, {`{"SHOWALLTRACKS":true}`, "SHOWALLTRACKS"},
		{`{"ShowAllTracks":true,"showalltracks":false}`, "showalltracks"},
		{`{"ShowAllTracks":true,"ShowAllTracks":false}`, "ShowAllTracks"},
		{`{"HideOtherAudio":"true"}`, "HideOtherAudio"}, {`{"HideOtherAudio":1}`, "HideOtherAudio"}, {`{"HideOtherAudio":null}`, "HideOtherAudio"},
		{`{"HideOtherSubtitles":"true"}`, "HideOtherSubtitles"}, {`{"HideOtherSubtitles":1}`, "HideOtherSubtitles"}, {`{"HideOtherSubtitles":null}`, "HideOtherSubtitles"},
		{`{"ShowAllTracks":"true"}`, "ShowAllTracks"}, {`{"ShowAllTracks":1}`, "ShowAllTracks"}, {`{"ShowAllTracks":null}`, "ShowAllTracks"},
		{`{"KeepAudioLanguages":"deu"}`, "KeepAudioLanguages"}, {`{"KeepAudioLanguages":["deu",1]}`, "KeepAudioLanguages"}, {`{"KeepAudioLanguages":null}`, "KeepAudioLanguages"},
		{`{"KeepSubtitleLanguages":"deu"}`, "KeepSubtitleLanguages"}, {`{"KeepSubtitleLanguages":["deu",1]}`, "KeepSubtitleLanguages"}, {`{"KeepSubtitleLanguages":null}`, "KeepSubtitleLanguages"},
	} {
		if raw := tpRefused(t, g.a, token, c.body, 400); !strings.Contains(raw, c.name) {
			t.Errorf("PATCH %s: the 400 does not name %s: %s", c.body, c.name, raw)
		}
		tpSame(t, "after the refused "+c.body, token, want, g.a, g.b)
	}
}

// A body over 16 KB, a body that is not JSON, an empty body, JSON that is not an object and an object followed by
// more text are 400 saying what is wrong, and store nothing. 16384 bytes are accepted.
func TestABodyThatIsTooLongOrNotAnObjectIsRefusedAndStoresNothing(t *testing.T) {
	g := newTPRig(t)
	token := g.token("U")
	padded := func(size int) string {
		body := `{"ShowAllTracks":true}`
		return body + strings.Repeat(" ", size-len(body))
	}
	if status, raw := tpSend(t, g.a, "PATCH", token, tpPath, padded(16384)); status != 200 {
		t.Fatalf("16384 bytes of valid JSON answered %d %s, want 200", status, raw)
	}
	want := tpDoc{false, false, []string{}, []string{}, true}
	tpSame(t, "after 16384 bytes", token, want, g.a, g.b)
	tooLong := `{"ShowAllTracks":false}` + strings.Repeat(" ", 16385-len(`{"ShowAllTracks":false}`)) // Valid JSON that would change the field.
	for _, body := range []string{
		tooLong, "not json", "", "null", "[]", `"x"`, "42", `{} x`, `{}{}`, `{"ShowAllTracks":false} {"ShowAllTracks":true}`, `[{"ShowAllTracks":false}]`, `{"ShowAllTracks":false`,
	} {
		tpRefused(t, g.b, token, body, 400)
		tpSame(t, "after the refused body "+abbreviated(body), token, want, g.a, g.b)
	}
}

// A PATCH that is refused stores none of its body: not the valid list beside a bad one, and not the valid list beside a
// switch that is refused for want of a language.
func TestARefusedPatchStoresNoneOfItsBody(t *testing.T) {
	g := newTPRig(t)
	token, want := storedState(t, g, "U")
	tpRefused(t, g.a, token, `{"HideOtherAudio":true,"KeepAudioLanguages":["xx"]}`, 400)
	tpRefused(t, g.b, token, `{"ShowAllTracks":false,"KeepSubtitleLanguages":["deu"],"KeepAudioLanguages":["deu","xx"]}`, 400)
	tpSame(t, "after the 400s", token, want, g.a, g.b)

	none := g.token("O") // O has saved no language.
	tpRefused(t, g.a, none, `{"HideOtherAudio":true,"KeepAudioLanguages":["deu"],"ShowAllTracks":true}`, 409)
	tpRefused(t, g.b, none, `{"KeepSubtitleLanguages":["deu"],"HideOtherSubtitles":true}`, 409)
	tpSame(t, "after the 409s", none, tpDefaults(), g.a, g.b)
}

// A switch asks for its own language first, the caller's own, and answers 409 naming it; nothing is stored.
func TestASwitchAsksForItsOwnLanguageFirst(t *testing.T) {
	const noAudio, noSubtitle = "AudioLanguagePreference", "SubtitleLanguagePreference"
	for _, c := range []struct {
		name, config         string
		audioFree, subtitles bool // Whether the switch of audio, of subtitles, is accepted.
	}{
		{"nothing saved", `{}`, false, false},
		{"only audio", `{"AudioLanguagePreference":"eng"}`, true, false},
		{"only subtitles", `{"SubtitleLanguagePreference":"deu"}`, false, true},
		{"audio saved as the empty string", `{"AudioLanguagePreference":"","SubtitleLanguagePreference":"deu"}`, false, true},
		{"audio saved as null", `{"AudioLanguagePreference":null,"SubtitleLanguagePreference":"deu"}`, false, true},
		{"subtitles saved as the empty string", `{"AudioLanguagePreference":"eng","SubtitleLanguagePreference":""}`, true, false},
		{"audio is the original language", `{"AudioLanguagePreference":"OriginalLanguage"}`, true, false},
		{"both", bothLanguages, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTPRig(t)
			savePrefs(t, g.b, g.users, "O", bothLanguages) // Another viewer's languages never let U through.
			savePrefs(t, g.a, g.users, "U", c.config)
			token := g.token("U")
			want := tpDefaults()
			for _, one := range []struct {
				body, missing, other string
				ok                   bool
				set                  func(*tpDoc)
			}{
				{`{"HideOtherAudio":true}`, noAudio, noSubtitle, c.audioFree, func(d *tpDoc) { d.HideOtherAudio = true }},
				{`{"HideOtherSubtitles":true}`, noSubtitle, noAudio, c.subtitles, func(d *tpDoc) { d.HideOtherSubtitles = true }},
			} {
				r := g.b // The languages were saved through the other process.
				if one.ok {
					one.set(&want)
					if got := tpPatch(t, r, token, one.body); !reflect.DeepEqual(got, want) {
						t.Errorf("PATCH %s answered %+v, want %+v", one.body, got, want)
					}
					continue
				}
				if raw := tpRefused(t, r, token, one.body, 409); !strings.Contains(raw, one.missing) || strings.Contains(raw, one.other) {
					t.Errorf("PATCH %s: the 409 must name %s and only it: %s", one.body, one.missing, raw)
				}
				tpSame(t, "after the refused "+one.body, token, want, g.a, g.b)
			}
		})
	}
}

// Nothing else needs a language: with both empty, a switch set to false, either keep list and ShowAllTracks are 200.
func TestNothingButASwitchTurnedOnNeedsALanguage(t *testing.T) {
	g := newTPRig(t)
	token := g.token("U")
	for _, body := range []string{
		`{"HideOtherAudio":false}`, `{"HideOtherSubtitles":false}`, `{"HideOtherAudio":false,"HideOtherSubtitles":false}`,
		`{"KeepAudioLanguages":["deu"]}`, `{"KeepSubtitleLanguages":["fra"]}`, `{"ShowAllTracks":true}`, `{"ShowAllTracks":false}`,
	} {
		tpPatch(t, []*proc{g.a, g.b}[len(body)%2], token, body)
	}
}

// Both switches answer 200 once their language is saved through POST /Users/Configuration, also when it was saved through
// one api process and the PATCH is the next request to the other. OriginalLanguage counts as set.
func TestASwitchAnswers200OnceItsLanguageWasSavedThroughTheOtherProcess(t *testing.T) {
	for _, c := range []struct {
		config, patch string
		want          tpDoc
	}{
		{`{"AudioLanguagePreference":"eng"}`, `{"HideOtherAudio":true}`, tpDoc{true, false, []string{}, []string{}, false}},
		{`{"AudioLanguagePreference":"OriginalLanguage"}`, `{"HideOtherAudio":true}`, tpDoc{true, false, []string{}, []string{}, false}},
		{`{"SubtitleLanguagePreference":"deu"}`, `{"HideOtherSubtitles":true}`, tpDoc{false, true, []string{}, []string{}, false}},
	} {
		for _, saver := range []int{0, 1} {
			g := newTPRig(t)
			procs := []*proc{g.a, g.b}
			token := g.token("U")
			tpGet(t, procs[1-saver], token) // The process that will take the PATCH has answered for the viewer already.
			savePrefs(t, procs[saver], g.users, "U", c.config)
			if got := tpPatch(t, procs[1-saver], token, c.patch); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s then PATCH %s answered %+v, want %+v", c.config, c.patch, got, c.want)
			}
		}
	}
}

// A language cleared afterwards leaves the stored document as it was, and a PATCH that does not name the switch, or
// turns it off, still answers 200.
func TestAClearedLanguageLeavesTheStoredDocumentAsItWas(t *testing.T) {
	for _, c := range []struct{ name, clear, switchOff string }{
		{"audio saved as the empty string", `{"AudioLanguagePreference":"","SubtitleLanguagePreference":"deu"}`, "HideOtherAudio"},
		{"audio left out", `{"SubtitleLanguagePreference":"deu"}`, "HideOtherAudio"},
		{"subtitles saved as the empty string", `{"AudioLanguagePreference":"eng","SubtitleLanguagePreference":""}`, "HideOtherSubtitles"},
		{"subtitles left out", `{"AudioLanguagePreference":"eng"}`, "HideOtherSubtitles"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTPRig(t)
			token := g.viewer(t, "U")
			tpPatch(t, g.a, token, tpAwayBody)
			savePrefs(t, g.b, g.users, "U", c.clear)
			tpSame(t, "after clearing the language", token, tpAway(), g.a, g.b)
			want := tpAway()
			want.ShowAllTracks = false
			if got := tpPatch(t, g.a, token, `{"ShowAllTracks":false,"KeepAudioLanguages":["deu"]}`); !reflect.DeepEqual(got, want) {
				t.Errorf("a PATCH that does not name the switch answered %+v, want %+v", got, want)
			}
			got := tpPatch(t, g.b, token, fmt.Sprintf(`{%q:false}`, c.switchOff))
			if got.HideOtherAudio && c.switchOff == "HideOtherAudio" || got.HideOtherSubtitles && c.switchOff == "HideOtherSubtitles" {
				t.Errorf("turning %s off answered %+v", c.switchOff, got)
			}
		})
	}
}

var tpNames = regexp.MustCompile(`HideOtherAudio|HideOtherSubtitles|KeepAudioLanguages|KeepSubtitleLanguages|ShowAllTracks`)

// The two settings documents are apart: neither POST of the 16 fields touches the five, a PATCH leaves all 16 as
// saved, and none of the answers that carry the user ever names one of the five.
func TestTheTwoSettingsDocumentsAreApart(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configCapturePath)
	g := newTPRig(t)
	token := g.viewer(t, "U")
	full := *capture.Case(t, "full body, route=path").Request.Body
	tpPatch(t, g.a, token, tpAwayBody)
	for _, route := range []string{"none", "path"} {
		if status, raw := doRequest(t, g.b, "POST", savePath(route, g.users["U"].compact()), token, "application/json", full); status != 204 {
			t.Fatalf("saving the 16 fields by %s: %d %s", route, status, raw)
		}
		tpSame(t, "after POST by "+route, token, tpAway(), g.a, g.b)
	}
	before := savedConfigOf(t, g.a, g.users["U"].tokens[1])
	tpPatch(t, g.b, token, `{"HideOtherAudio":false,"KeepAudioLanguages":["ita"],"ShowAllTracks":false}`)
	tpPatch(t, g.a, token, `{"KeepSubtitleLanguages":[]}`)
	for _, r := range []*proc{g.a, g.b} {
		if got := savedConfigOf(t, r, g.users["U"].tokens[1]); !reflect.DeepEqual(got, before) {
			t.Errorf("after PATCHes, the 16 fields on %s are %v, were %v", r.addr, got, before)
		}
		for _, place := range []struct{ name, path string }{{"GET /Users/Me", "/Users/Me"}, {"GET /Users/{userId}", "/Users/" + g.users["U"].compact()}} {
			status, raw := doRequest(t, r, "GET", place.path, token, "", "")
			if status != 200 || tpNames.Match(raw) {
				t.Errorf("%s on %s: %d, names a track preference: %q", place.name, r.addr, status, tpNames.Find(raw))
			}
		}
		status, raw := r.authenticate(t, "capture-device", g.users["U"].name, configPassword)
		if status != 200 || tpNames.Match(raw) {
			t.Errorf("signing U in on %s: %d, names a track preference: %q", r.addr, status, tpNames.Find(raw))
		}
	}
}

// No token and an unknown token are 401 on GET and on PATCH, and that PATCH stores nothing.
func TestWithoutAKnownTokenGetAndPatchAre401AndStoreNothing(t *testing.T) {
	g := newTPRig(t)
	token := g.viewer(t, "U")
	for _, bad := range []string{"", uuid.NewString()} {
		for _, r := range []*proc{g.a, g.b} {
			for _, request := range []struct{ method, body string }{
				{"GET", ""}, {"PATCH", `{"ShowAllTracks":true}`}, {"PATCH", `{"KeepAudioLanguages":["deu"]}`}, {"PATCH", `not json`},
			} {
				if status, raw := tpSend(t, r, request.method, bad, tpPath, request.body); status != 401 {
					t.Errorf("%s with token %q on %s: %d %s, want 401", request.method, bad, r.addr, status, raw)
				}
			}
		}
	}
	tpSame(t, "after the refused PATCHes", token, tpDefaults(), g.a, g.b)
}

// The route acts on the caller only: an administrator's ?userId= of another viewer reads and changes the
// administrator's own, and no path with a user id in it answers 2xx.
func TestTheRouteActsOnTheCallerAndTakesNoUserId(t *testing.T) {
	g := newTPRig(t)
	viewer := g.viewer(t, "U")
	admin := g.viewer(t, "A")
	tpPatch(t, g.a, viewer, tpAwayBody)
	u := g.users["U"]
	for _, query := range []string{"?userId=" + u.compact(), "?userid=" + u.id, "?UserId=" + strings.ToUpper(u.compact())} {
		for _, r := range []*proc{g.a, g.b} {
			status, raw := tpSend(t, r, "GET", admin, tpPath+query, "")
			if status != 200 || !reflect.DeepEqual(parseTP(t, raw), tpDefaults()) {
				t.Errorf("the administrator's GET%s on %s: %d %s, want their own defaults", query, r.addr, status, raw)
			}
		}
		status, raw := tpSend(t, g.b, "PATCH", admin, tpPath+query, `{"KeepAudioLanguages":["ita"]}`)
		if status != 200 || !reflect.DeepEqual(parseTP(t, raw).KeepAudioLanguages, []string{"ita"}) {
			t.Errorf("the administrator's PATCH%s: %d %s", query, status, raw)
		}
		tpPatch(t, g.a, admin, `{"KeepAudioLanguages":[]}`)
		tpSame(t, "the viewer after the administrator's PATCH"+query, viewer, tpAway(), g.a, g.b)
	}
	for _, path := range []string{
		"/jellyscale/v1/users/" + u.compact() + "/track-preferences", "/jellyscale/v1/Users/" + u.compact() + "/track-preferences",
		"/jellyscale/v1/track-preferences/" + u.compact(), "/jellyscale/v1/users/" + u.compact(), "/jellyscale/v1/track-preferences/" + u.id,
	} {
		for _, method := range []string{"GET", "PATCH", "PUT", "POST"} {
			if status, raw := tpSend(t, g.a, method, admin, path, `{"ShowAllTracks":false}`); status/100 == 2 {
				t.Errorf("%s %s answered %d %s: a route with a user id in it", method, path, status, raw)
			}
		}
	}
	tpSame(t, "the viewer after every path with a user id", viewer, tpAway(), g.a, g.b)
}

// The write is PATCH only.
func TestPutAndPostToTheRouteAnswerNoSuccessAndStoreNothing(t *testing.T) {
	g := newTPRig(t)
	token := g.viewer(t, "U")
	for _, method := range []string{"PUT", "POST"} {
		if status, raw := tpSend(t, g.a, method, token, tpPath, tpAwayBody); status/100 == 2 {
			t.Errorf("%s answered %d %s", method, status, raw)
		}
	}
	tpSame(t, "after PUT and POST", token, tpDefaults(), g.a, g.b)
}

// Deleting a user who saved all five succeeds, leaves no row of theirs in any table, and leaves another viewer's
// document as it was.
func TestDeletingAUserWhoSavedAllFiveLeavesNoRowOfTheirs(t *testing.T) {
	g := newTPRig(t)
	gone, kept := g.viewer(t, "U"), g.viewer(t, "O")
	tpPatch(t, g.a, gone, tpAwayBody)
	tpPatch(t, g.b, kept, `{"HideOtherSubtitles":true,"KeepSubtitleLanguages":["ita"]}`)
	u := g.users["U"]
	if len(rowsHolding(t, g.dsn, u.id)) < 2 {
		t.Fatalf("fewer than two tables hold a row of the user before the delete: the check cannot see them")
	}
	if _, err := connect(t, g.dsn).Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.id); err != nil {
		t.Fatalf("deleting a user who saved all five: %v", err)
	}
	for _, spelling := range []string{u.id, u.compact()} {
		if tables := rowsHolding(t, g.dsn, spelling); len(tables) > 0 {
			t.Errorf("after the delete, %v still hold a row naming the user (%s)", tables, spelling)
		}
	}
	tpSame(t, "the other viewer after the delete", kept, tpDoc{false, true, []string{}, []string{"ita"}, false}, g.a, g.b)
}

// infoLines are the lines of a process's log at Info or above (slog's text handler writes level=INFO, WARN, ERROR).
func infoLines(p *proc) []string {
	var out []string
	for _, line := range strings.Split(p.log.String(), "\n") {
		if regexp.MustCompile(`\blevel=(INFO|WARN|ERROR)\b`).MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// quiet fails if the process wrote a line at Info or above after mark, 600 ms after the request was answered (the
// request has been answered, so a line written during it is written; this bounds a line written after the answer).
func quiet(t *testing.T, what string, p *proc, mark int) {
	t.Helper()
	time.Sleep(600 * time.Millisecond)
	if lines := infoLines(p); len(lines) != mark {
		t.Errorf("%s: %s logged %d new lines at Info or above: %v", what, p.addr, len(lines)-mark, lines[mark:])
	}
}

// Each PATCH that names a field and answers 200 logs exactly one line at Info, in the process that served it, with the
// user id and the five values as stored after it. A GET and a PATCH answered 400, 409 or 401 log nothing at Info.
func TestAPatchThatChangesSomethingLogsOneLineAtInfoInTheProcessThatServedIt(t *testing.T) {
	g := newTPRig(t)
	token := g.viewer(t, "U")
	none := g.token("O") // No language saved.
	u := g.users["U"]
	tpGet(t, g.a, token) // Warmed.
	tpGet(t, g.b, token)
	marks := func() (int, int) { return len(infoLines(g.a)), len(infoLines(g.b)) }
	markA, markB := marks()

	patched := func(body string) {
		t.Helper()
		doc := tpPatch(t, g.a, token, body)
		deadline := time.Now().Add(10 * time.Second)
		for len(infoLines(g.a)) <= markA && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		quiet(t, "PATCH "+body, g.a, markA+1)
		line := infoLines(g.a)[markA]
		markA++
		// The user id, dashed or compact, on its own.
		lower := strings.ToLower(line)
		if !strings.Contains(lower, strings.ToLower(u.id)) && !strings.Contains(lower, strings.ToLower(u.compact())) {
			t.Errorf("PATCH %s: the line does not hold the user id: %s", body, line)
		}
		// Each of the five fields with its value as stored after the PATCH (the answer), whatever the spelling of the
		// line: names and values are compared with every character but letters and digits taken out, so
		// hide_other_audio=true and "HideOtherAudio":true both read hideotheraudiotrue, a list [deu fra] reads deufra.
		squash := func(s string) string { return regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(s), "") }
		flat := squash(line)
		for name, value := range map[string]string{
			"HideOtherAudio":        fmt.Sprint(doc.HideOtherAudio),
			"HideOtherSubtitles":    fmt.Sprint(doc.HideOtherSubtitles),
			"KeepAudioLanguages":    strings.Join(doc.KeepAudioLanguages, ""),
			"KeepSubtitleLanguages": strings.Join(doc.KeepSubtitleLanguages, ""),
			"ShowAllTracks":         fmt.Sprint(doc.ShowAllTracks),
		} {
			if !strings.Contains(flat, squash(name+value)) {
				t.Errorf("PATCH %s: the line does not hold %s with its stored value %q: %s", body, name, value, line)
			}
		}
		quiet(t, "PATCH "+body+" on the other process", g.b, markB)
	}
	patched(tpAwayBody)
	// The line shows the five as stored, also the fields the PATCH did not name.
	patched(`{"ShowAllTracks":false}`)

	tpGet(t, g.a, token)
	tpGet(t, g.b, token)
	tpRefused(t, g.a, token, `{"KeepAudioLanguages":["xx"]}`, 400)
	tpRefused(t, g.a, none, `{"HideOtherAudio":true}`, 409)
	if status, _ := tpSend(t, g.a, "PATCH", "", tpPath, `{"ShowAllTracks":true}`); status != 401 {
		t.Errorf("a PATCH with no token answered %d", status)
	}
	quiet(t, "a GET and PATCHes answered 400, 409 and 401", g.a, markA)
	quiet(t, "the same on the other process", g.b, markB)
}
