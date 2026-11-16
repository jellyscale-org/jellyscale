//go:build integration

package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"jellyscale/internal/db"
)

// JS-118, narrowed by JS-119: a setting that hides nothing changes nothing. For three films with audio and
// subtitles in two languages and a viewer whose saved languages are among them, PlaybackInfo (everything but
// PlaySessionId, which is also in its URLs) and GET /Items/{itemId} are the same before and after each keep list
// with the switches off, and with both switches on while ShowAllTracks is on (JS-119's own tests hold what the
// switches hide). The settings are PATCHed through one replica and read through the other.
func TestSettingsThatHideNothingChangeNeitherPlaybackInfoNorTheItem(t *testing.T) {
	e := newEnv(t)
	films := settingsFilms(t, e)
	if status := e.save(t, 0, "path", e.user, e.token, `{"AudioLanguagePreference":"eng","SubtitleLanguagePreference":"deu"}`).StatusCode; status != 204 {
		t.Fatalf("saving the languages: %d", status)
	}
	seen := func(film string) []string {
		t.Helper()
		var out []string
		for _, body := range []map[string]any{{}, {"SubtitleStreamIndex": 2}, {"AudioStreamIndex": 2, "SubtitleStreamIndex": -1}} {
			reply := e.playbackInfo(t, 1, film, body)
			raw, _ := json.Marshal(reply)
			text := string(raw)
			if id, _ := reply["PlaySessionId"].(string); id != "" {
				text = strings.ReplaceAll(text, id, "<PlaySessionId>")
			}
			out = append(out, text)
		}
		response, raw := e.do(t, 1, "GET", "/Items/"+db.CompactID(film), e.token, nil)
		if response.StatusCode != 200 {
			t.Fatalf("GET /Items/%s: %d %s", film, response.StatusCode, raw)
		}
		var item any
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		again, _ := json.Marshal(item)
		return append(out, string(again))
	}
	settings := []string{
		`{"KeepAudioLanguages":["deu"]}`, `{"KeepSubtitleLanguages":["eng"]}`,
		`{"HideOtherAudio":true,"HideOtherSubtitles":true,"ShowAllTracks":true}`, // Shown, whatever the switches say.
		`{"HideOtherAudio":false,"HideOtherSubtitles":false,"ShowAllTracks":false}`,
	}
	for name, film := range films {
		before := seen(film)
		if len(before) != 4 || !strings.Contains(before[0], "MediaSources") {
			t.Fatalf("%s: what was seen is not a PlaybackInfo and an item: %v", name, before)
		}
		for _, body := range settings {
			if response, raw := e.do(t, 0, "PATCH", "/jellyscale/v1/track-preferences", e.token, json.RawMessage(body)); response.StatusCode != 200 {
				t.Fatalf("%s: PATCH %s answered %d %s, want 200", name, body, response.StatusCode, raw)
			}
			if after := seen(film); !reflect.DeepEqual(after, before) {
				for i := range after {
					if after[i] != before[i] {
						t.Errorf("%s: after PATCH %s, answer %d differs:\n%s\nwas\n%s", name, body, i, after[i], before[i])
					}
				}
			}
		}
	}
}
