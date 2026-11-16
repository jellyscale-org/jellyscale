package compat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// JS-117 observed first: compat/captures/original-language-12.4.md and its JSON record what a
// real Jellyfin 12.4 answers about a film's original language for four films scanned with no
// metadata provider. This holds the record to the criteria: four films, each with the item's
// OriginalLanguage (absent, null or a value, said which) and, for every audio stream, the five fields.
func TestTheOriginalLanguageCaptureIsComplete(t *testing.T) {
	raw, err := os.ReadFile("captures/original-language-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Image, Version, Date string
		Films                []struct {
			Name, Scenario string
			Answers        map[string]struct {
				OriginalLanguage any
				MediaStreams     []map[string]any
				Sources          [][]map[string]any `json:"MediaSources[].MediaStreams"`
			}
		}
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if capture.Image != "jellyfin/jellyfin:12.4" || capture.Date == "" {
		t.Errorf("the capture names image %q and date %q", capture.Image, capture.Date)
	}
	if len(capture.Films) != 4 {
		t.Fatalf("%d films, want 4: (eng default, deu), (deu, eng default), (untagged first, eng), (no audio)", len(capture.Films))
	}
	wantAudio := []int{2, 2, 2, 0}
	for i, film := range capture.Films {
		for _, route := range []string{"GET /Items/{id}", "GET /Users/{userId}/Items/{id}", "POST /Items/{id}/PlaybackInfo", "GET /Items/{id}/PlaybackInfo"} {
			answer, ok := film.Answers[route]
			if !ok {
				t.Errorf("%s: no answer of %s", film.Name, route)
				continue
			}
			if strings.HasPrefix(route, "GET /") && !strings.Contains(route, "PlaybackInfo") && answer.OriginalLanguage != "<absent>" {
				t.Errorf("%s %s: OriginalLanguage %v is not recorded as absent", film.Name, route, answer.OriginalLanguage)
			}
			if len(answer.Sources) != 1 || len(answer.Sources[0]) != wantAudio[i] {
				t.Errorf("%s %s: %d sources, audio streams %v, want %d audio streams", film.Name, route, len(answer.Sources), answer.Sources, wantAudio[i])
			}
			for _, list := range answer.Sources {
				for _, stream := range list {
					for _, field := range []string{"Language", "IsDefault", "IsOriginal", "LocalizedOriginal", "DisplayTitle"} {
						if _, ok := stream[field]; !ok {
							t.Errorf("%s %s: a stream lacks %s", film.Name, route, field)
						}
					}
				}
			}
		}
	}
	notes, err := os.ReadFile("captures/original-language-12.4.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"jellyfin/jellyfin:12.4", "2026-11-15", "EnableInternetProviders", "MetadataFetchers", "absent"} {
		if !strings.Contains(string(notes), word) {
			t.Errorf("the notes do not say %q", word)
		}
	}
}
