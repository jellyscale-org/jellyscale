//go:build integration

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-116: a viewer's settings are saved and returned, and change no track choice. The answers are held to
// compat/captures/user-configuration-12.4.json (a real Jellyfin 12.4). The replay of every case of the capture, in
// real processes, is cmd/jellyscale/user_configuration_integration_test.go; these are the checks that need a
// replica's own pool: what a request sends to Postgres, and the rows a film has.

const configurationCapture = "../../compat/captures/user-configuration-12.4.json"

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return out
}

// savedBody is the body of the capture's "full body" case: all 16 fields changed from a new user's.
func savedBody(t *testing.T, capture *testenv.UserConfigurationCapture) string {
	t.Helper()
	return *capture.Case(t, "full body, route=path").Request.Body
}

// The way jellyfin-web saves: the whole object it was given, with fields changed.
func (e *env) save(t *testing.T, replica int, route, user, token string, body string) *http.Response {
	t.Helper()
	path := "/Users/Configuration"
	switch route {
	case "path":
		path = "/Users/" + user + "/Configuration"
	case "query":
		path += "?userId=" + user
	}
	request, err := http.NewRequest("POST", e.servers[replica].URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", `MediaBrowser Token="`+token+`"`)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response
}

func (e *env) me(t *testing.T, replica int, token string) map[string]any {
	t.Helper()
	response, raw := e.do(t, replica, "GET", "/Users/Me", token, nil)
	if response.StatusCode != 200 {
		t.Fatalf("GET /Users/Me: %d %s", response.StatusCode, raw)
	}
	return decodeObject(t, raw)["Configuration"].(map[string]any)
}

// A user whose row holds only a name and a password, as every account that exists today does, is a new Jellyfin
// user: the 15 fields of the capture and no AudioLanguagePreference, CastReceiverId F007D354.
func TestAUserWithOnlyANameAndAPasswordAnswersAsANewJellyfinUser(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configurationCapture)
	e := newEnv(t)
	want := capture.NewUser.Me
	if _, ok := want["AudioLanguagePreference"]; ok || want["CastReceiverId"] == nil {
		t.Fatalf("the capture's new user is not as this test knows it: %v", want)
	}
	for _, replica := range []int{0, 1} {
		if got := e.me(t, replica, e.token); !reflect.DeepEqual(got, want) {
			t.Errorf("GET /Users/Me on replica %d: %v\nwant (the capture's new user) %v", replica, got, want)
		}
		_, raw := e.do(t, replica, "GET", "/Users/"+db.CompactID(e.user), e.token, nil)
		if got := decodeObject(t, raw)["Configuration"]; !reflect.DeepEqual(got, any(want)) {
			t.Errorf("GET /Users/{id} on replica %d: %v", replica, got)
		}
	}
}

// GET /Localization/Cultures: the captured list, entry for entry, in the captured order; the captured status without a token.
func TestCulturesAnswerAsCaptured(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configurationCapture)
	if len(capture.Cultures.Entries) < 400 {
		t.Fatalf("the capture holds %d cultures", len(capture.Cultures.Entries))
	}
	e := newEnv(t)
	for _, path := range []string{"/Localization/Cultures", "/Localization/cultures"} {
		response, raw := e.do(t, 0, "GET", path, e.token, nil)
		if response.StatusCode != capture.Cultures.Status {
			t.Fatalf("GET %s: %d, want %d", path, response.StatusCode, capture.Cultures.Status)
		}
		var got []map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: %v: %.200s", path, err, raw)
		}
		if len(got) != len(capture.Cultures.Entries) {
			t.Fatalf("GET %s answers %d entries, the capture has %d", path, len(got), len(capture.Cultures.Entries))
		}
		for i, entry := range got {
			if !reflect.DeepEqual(entry, map[string]any(capture.Cultures.Entries[i])) {
				t.Fatalf("GET %s entry %d: %v\nwant %v", path, i, entry, capture.Cultures.Entries[i])
			}
		}
	}
	if response, _ := e.do(t, 0, "GET", "/Localization/Cultures", "", nil); response.StatusCode != capture.Cultures.WithoutToken.Status {
		t.Errorf("without a token: %d, want %d", response.StatusCode, capture.Cultures.WithoutToken.Status)
	}
}

// With settings saved, GET /Users/Me sends Postgres one statement: the token's, as before this task. The
// settings ride on the row every request reads to authenticate.
func TestUsersMeWithSettingsSavedSendsOneStatement(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configurationCapture)
	e := newEnv(t)
	logs := traceReplicas(t, e)
	body := savedBody(t, capture)
	for _, route := range []string{"path", "query", "none"} {
		if status := e.save(t, 0, route, db.CompactID(e.user), e.token, body).StatusCode; status != 204 {
			t.Fatalf("save by %s: %d, want the captured 204", route, status)
		}
		for replica, log := range logs {
			log.take()
			got := e.me(t, replica, e.token)
			sent := log.take()
			if len(sent) != 1 || !strings.Contains(strings.ToLower(sent[0]), "devices") {
				t.Errorf("GET /Users/Me on replica %d after a save by %s sent %d statements, want the token's one: %v", replica, route, len(sent), sent)
			}
			want := capture.Config(capture.Case(t, "full body, route=path").After.Me["U"])
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replica %d after a save by %s: %v, want %v", replica, route, got, want)
			}
		}
	}
}

// A PlaybackInfo sends as many statements as it did before this task: a user with settings saved costs it
// nothing more than a new user. 2 is what it sent on the build before JS-116 (5cf3231) for the film of newEnv with
// no profile (counted with statementLog on that build).
func TestPlaybackInfoSendsTheSameStatementsWhateverTheSettings(t *testing.T) {
	capture := testenv.LoadUserConfigurationCapture(t, configurationCapture)
	e := newEnv(t)
	log := traceReplicas(t, e)[0]
	count := func() int {
		log.take()
		e.playbackInfo(t, 0, e.item, map[string]any{})
		return len(log.take())
	}
	before := count()
	if before != playbackInfoStatementsBeforeJS116 {
		t.Errorf("PlaybackInfo sends %d statements, %d on the build before JS-116", before, playbackInfoStatementsBeforeJS116)
	}
	if status := e.save(t, 0, "path", db.CompactID(e.user), e.token, savedBody(t, capture)).StatusCode; status != 204 {
		t.Fatalf("save: %d, want 204", status)
	}
	if after := count(); after != before {
		t.Errorf("PlaybackInfo sent %d statements with settings saved and %d before", after, before)
	}
}

const playbackInfoStatementsBeforeJS116 = 2

// Three films: audio in two languages; subtitles in two languages; subtitles in two languages, one forced.
func settingsFilms(t *testing.T, e *env) map[string]string {
	t.Helper()
	library := scalar[string](t, e.pool, "SELECT id::text FROM libraries LIMIT 1")
	streams := map[string]string{
		"two audio languages": `[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","Language":"eng","IsDefault":true},{"Index":2,"Type":"Audio","Language":"deu"}]`,
		"two subtitle languages": `[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","Language":"eng","IsDefault":true},
			{"Index":2,"Type":"Subtitle","Language":"eng","Codec":"subrip","IsTextSubtitleStream":true},{"Index":3,"Type":"Subtitle","Language":"deu","Codec":"subrip","IsTextSubtitleStream":true}]`,
		"two subtitle languages, one forced": `[{"Index":0,"Type":"Video"},{"Index":1,"Type":"Audio","Language":"eng","IsDefault":true},
			{"Index":2,"Type":"Subtitle","Language":"eng","Codec":"subrip","IsTextSubtitleStream":true,"IsForced":true},{"Index":3,"Type":"Subtitle","Language":"deu","Codec":"subrip","IsTextSubtitleStream":true}]`,
	}
	films := map[string]string{}
	for name, list := range streams {
		id := uuid.NewString()
		mustExec(t, e.pool, `INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
			VALUES ($1, $2, 'Movie', $3, $3, $3, $4, $5)`, id, library, name, filmTicks, fmt.Sprintf(`{"Name":%q,"MediaSources":[{
			"Id":%q,"Protocol":"File","Container":"mkv","SupportsTranscoding":true,"MediaStreams":%s}]}`, name, db.CompactID(id), list))
		mustExec(t, e.pool, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
			VALUES ($1, $1, 'film.mkv', $2, 'abc123', 'mkv', '[]')`, id, len(e.body))
		films[name] = id
	}
	return films
}

// What PlaybackInfo chooses for each source of a film, by the numbers it answers (absent is nil), with and without
// a subtitle named by the client.
func defaultsOf(t *testing.T, e *env, film string) []any {
	t.Helper()
	var out []any
	for _, extra := range []map[string]any{{}, {"SubtitleStreamIndex": 2}, {"AudioStreamIndex": 2, "SubtitleStreamIndex": -1}} {
		for _, source := range e.playbackInfo(t, 0, film, extra)["MediaSources"].([]any) {
			s := source.(map[string]any)
			out = append(out, s["Id"], s["DefaultAudioStreamIndex"], s["DefaultSubtitleStreamIndex"])
		}
	}
	return out
}

// The audio defaults alone, which a viewer's subtitle settings must not move (JS-041).
func audioDefaultsOf(t *testing.T, e *env, film string) []any {
	t.Helper()
	var out []any
	for _, extra := range []map[string]any{{}, {"SubtitleStreamIndex": 2}, {"AudioStreamIndex": 2, "SubtitleStreamIndex": -1}} {
		for _, source := range e.playbackInfo(t, 0, film, extra)["MediaSources"].([]any) {
			s := source.(map[string]any)
			out = append(out, s["Id"], s["DefaultAudioStreamIndex"])
		}
	}
	return out
}

// Saving an audio setting, or one that leaves the film's own tracks the choice, changes no track choice (JS-121 made the
// audio settings act, and audio_default_integration_test.go holds what they change). A subtitle mode and language now
// choose the subtitle default (JS-041, D-025), so those two settings are held to what they must not move instead: the
// audio defaults. Three films' every source is asked, for each SubtitleMode with a language the film has, for the
// language of the default audio track, for OriginalLanguage, and for PlayDefaultAudioTrack off.
func TestSavingASettingChangesNoDefaultTrack(t *testing.T) {
	e := newEnv(t)
	films := settingsFilms(t, e)
	languages := map[string][2]string{ // The language of a film's subtitle track, of its default audio track (eng in all three).
		"two audio languages": {"eng", "eng"}, "two subtitle languages": {"deu", "eng"}, "two subtitle languages, one forced": {"deu", "eng"},
	}
	for name, film := range films {
		before := defaultsOf(t, e, film)
		audioBefore := audioDefaultsOf(t, e, film)
		if len(before) == 0 {
			t.Fatalf("%s: PlaybackInfo lists no source", name)
		}
		save := func(change map[string]any) {
			t.Helper()
			config := e.me(t, 0, e.token) // The whole object, as jellyfin-web posts it back.
			for k, v := range change {
				config[k] = v
			}
			raw, _ := json.Marshal(config)
			if status := e.save(t, 0, "path", e.user, e.token, string(raw)).StatusCode; status != 204 {
				t.Fatalf("%s: saving %v answered %d, want 204", name, change, status)
			}
			for k, v := range change {
				if got := e.me(t, 1, e.token)[k]; !reflect.DeepEqual(got, v) {
					t.Fatalf("%s: %s was saved as %v and reads back %v", name, k, v, got)
				}
			}
			_, isMode := change["SubtitleMode"]
			_, isLanguage := change["SubtitleLanguagePreference"]
			if isMode || isLanguage {
				// JS-041, D-025: the mode and language choose the subtitle default, and must leave the audio alone. The next
				// setting is held to what this one leaves, so a subtitle the mode chose is not read as an audio setting's doing.
				if after := audioDefaultsOf(t, e, film); !reflect.DeepEqual(after, audioBefore) {
					t.Errorf("%s: after saving %v the audio defaults are %v, were %v", name, change, after, audioBefore)
				}
				before, audioBefore = defaultsOf(t, e, film), audioDefaultsOf(t, e, film)
				return
			}
			if after := defaultsOf(t, e, film); !reflect.DeepEqual(after, before) {
				t.Errorf("%s: after saving %v the defaults are %v, were %v", name, change, after, before)
			}
		}
		for _, mode := range []string{"Default", "Always", "OnlyForced", "None", "Smart"} {
			save(map[string]any{"SubtitleMode": mode, "SubtitleLanguagePreference": languages[name][0]})
		}
		save(map[string]any{"AudioLanguagePreference": languages[name][1]})
		save(map[string]any{"AudioLanguagePreference": "OriginalLanguage"})
		save(map[string]any{"PlayDefaultAudioTrack": false})
	}
}
