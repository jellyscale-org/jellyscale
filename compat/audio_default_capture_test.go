package compat

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// JS-121 observed first: compat/captures/audio-default-12.4.md and its JSON record which audio track a real Jellyfin 12.4
// names as DefaultAudioStreamIndex, asked with jellyfin-web's PlaybackInfo body. This holds the record to the criteria and
// holds the rule written down to every cell, so a rule that fits the prose and not the cells cannot be built from it.

type audioTrack struct {
	Index     int
	Language  *string
	IsDefault bool
}

type audioCell struct {
	AudioLanguagePreference string
	PlayDefaultAudioTrack   bool
	DefaultAudioStreamIndex any // a number, or the string "<absent>"
	SupportsDirectPlay      bool
}

type audioFilm struct {
	Name, Scenario, Container string
	Audio                     []audioTrack
	Cells, WithoutProfile     []audioCell
}

type namedCell struct {
	Film                    string
	AudioLanguagePreference string
	PlayDefaultAudioTrack   bool
	AudioStreamIndex        any
	DefaultAudioStreamIndex any
}

type audioDefaultCapture struct {
	Image, Version, Date string
	Films                []audioFilm
	Named                []namedCell
}

func loadAudioDefault(t *testing.T) audioDefaultCapture {
	t.Helper()
	raw, err := os.ReadFile("captures/audio-default-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture audioDefaultCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

func tagOf(t audioTrack) string {
	if t.Language == nil {
		return ""
	}
	return *t.Language
}

func TestTheAudioDefaultCaptureIsComplete(t *testing.T) {
	capture := loadAudioDefault(t)
	if capture.Image != "jellyfin/jellyfin:12.4" || capture.Date == "" {
		t.Errorf("the capture names image %q and date %q", capture.Image, capture.Date)
	}
	if len(capture.Films) < 12 {
		t.Fatalf("%d films, want at least 12", len(capture.Films))
	}
	cells := 0
	seen := map[string]bool{}
	for _, film := range capture.Films {
		cells += len(film.Cells)
		for _, cell := range film.Cells {
			seen[cell.AudioLanguagePreference+"|"+boolWord(cell.PlayDefaultAudioTrack)] = true
		}
		if len(film.Cells) != len(film.WithoutProfile) {
			t.Errorf("%s: %d cells and %d asked with no DeviceProfile", film.Name, len(film.Cells), len(film.WithoutProfile))
		}
		// The first four languages x both values are the cells the task asks for; every film has all eight.
		for _, language := range []string{"", "eng", "deu", "OriginalLanguage"} {
			for _, play := range []bool{true, false} {
				if !seen[language+"|"+boolWord(play)] {
					t.Errorf("no cell of %q with PlayDefaultAudioTrack %v", language, play)
				}
			}
		}
	}
	if cells < 96 {
		t.Errorf("%d cells, want at least 96", cells)
	}

	// The films cover each case the task names.
	has := func(what string, match func(audioFilm) bool) {
		t.Helper()
		if !slices.ContainsFunc(capture.Films, match) {
			t.Errorf("no film with %s", what)
		}
	}
	flags := func(f audioFilm) (n int) {
		for _, a := range f.Audio {
			if a.IsDefault {
				n++
			}
		}
		return
	}
	tags := func(f audioFilm) (out []string) {
		for _, a := range f.Audio {
			out = append(out, tagOf(a))
		}
		return
	}
	has("the default flag on the first track only", func(f audioFilm) bool { return len(f.Audio) > 1 && f.Audio[0].IsDefault && flags(f) == 1 })
	has("the default flag on the second track only", func(f audioFilm) bool { return len(f.Audio) > 1 && f.Audio[1].IsDefault && flags(f) == 1 })
	has("no track flagged", func(f audioFilm) bool { return len(f.Audio) > 1 && flags(f) == 0 })
	has("every track flagged", func(f audioFilm) bool { return len(f.Audio) > 1 && flags(f) == len(f.Audio) })
	has("a track tagged ger and one tagged deu", func(f audioFilm) bool { return slices.Contains(tags(f), "ger") && slices.Contains(tags(f), "deu") })
	has("an untagged track", func(f audioFilm) bool { return slices.Contains(tags(f), "") })
	has("a track tagged und", func(f audioFilm) bool { return slices.Contains(tags(f), "und") })
	has("two tracks in one language", func(f audioFilm) bool {
		for i, tag := range tags(f) {
			if tag != "" && slices.Contains(tags(f)[i+1:], tag) {
				return true
			}
		}
		return false
	})
	has("a single track", func(f audioFilm) bool { return len(f.Audio) == 1 })
	has("no audio", func(f audioFilm) bool { return len(f.Audio) == 0 })
	has("no deu track, a first track in a third language (fra), eng flagged", func(f audioFilm) bool {
		return slices.Equal(tags(f), []string{"fra", "eng"}) && f.Audio[1].IsDefault && !f.Audio[0].IsDefault
	})

	// The named-index capture: at least two settings, the preferred track, another audio track and indexes that are no audio stream.
	settings := map[string]bool{}
	asked := map[string]bool{}
	for _, n := range capture.Named {
		settings[n.AudioLanguagePreference+"|"+boolWord(n.PlayDefaultAudioTrack)] = true
		asked[jsonWord(n.AudioStreamIndex)] = true
	}
	if len(settings) < 2 {
		t.Errorf("the named-index capture has %d settings, want at least 2", len(settings))
	}
	for _, want := range []string{"1", "2", "3", "4", "99", "-1", "0", "null"} {
		if !asked[want] {
			t.Errorf("the named-index capture never names AudioStreamIndex %s", want)
		}
	}
	notes, err := os.ReadFile("captures/audio-default-12.4.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"jellyfin/jellyfin:12.4", "2026-11-16", "The rule that fits", "AudioStreamIndex is named", "SecondaryAudioNotSupported", "compat/differences.yaml"} {
		if !strings.Contains(string(notes), word) {
			t.Errorf("the notes do not say %q", word)
		}
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func jsonWord(v any) string {
	raw, _ := json.Marshal(v)
	return strings.Trim(string(raw), `"`)
}

// jellyfinAudioDefault is the rule of audio-default-12.4.md, as code. Spellings as Jellyfin reads them: a track's ger is
// deu, a saved ger or de is deu, and no other spelling is changed.
func jellyfinAudioDefault(tracks []audioTrack, saved string, playDefault bool) (int, bool) {
	if len(tracks) == 0 {
		return 0, false
	}
	track := func(a audioTrack) string {
		tag := strings.ToLower(tagOf(a))
		if tag == "ger" {
			return "deu"
		}
		return tag
	}
	language := strings.ToLower(saved)
	if language == "ger" || language == "de" {
		language = "deu"
	}
	matches := func(a audioTrack) bool { return language != "" && track(a) == language }
	var flagged []audioTrack
	for _, a := range tracks {
		if a.IsDefault {
			flagged = append(flagged, a)
		}
	}
	first := func(list []audioTrack, keep func(audioTrack) bool) (audioTrack, bool) {
		for _, a := range list {
			if keep(a) {
				return a, true
			}
		}
		return audioTrack{}, false
	}
	if playDefault && len(flagged) > 0 {
		if a, ok := first(flagged, matches); ok {
			return a.Index, true
		}
		return flagged[0].Index, true
	}
	if a, ok := first(tracks, matches); ok {
		return a.Index, true
	}
	x := tracks[0]
	if len(flagged) > 0 {
		x = flagged[0]
	}
	if language == "" {
		return x.Index, true
	}
	a, _ := first(tracks, func(a audioTrack) bool { return track(a) == track(x) })
	return a.Index, true
}

func TestTheAudioDefaultCaptureFitsTheRuleWrittenDown(t *testing.T) {
	capture := loadAudioDefault(t)
	for _, film := range capture.Films {
		for _, cell := range film.Cells {
			index, ok := jellyfinAudioDefault(film.Audio, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack)
			var want any = "<absent>"
			if ok {
				want = float64(index)
			}
			if cell.DefaultAudioStreamIndex != want {
				t.Errorf("%s, saved %q, PlayDefaultAudioTrack %v: Jellyfin answered %v, the rule says %v",
					film.Name, cell.AudioLanguagePreference, cell.PlayDefaultAudioTrack, cell.DefaultAudioStreamIndex, want)
			}
		}
	}
	// A named index that is an audio track is the answer; any other is ignored along with the saved language: the file's
	// own default, the flagged track else the first.
	films := map[string]audioFilm{}
	for _, film := range capture.Films {
		films[film.Name] = film
	}
	for _, n := range capture.Named {
		film := films[n.Film]
		want := float64(film.Audio[0].Index)
		for _, a := range film.Audio {
			if a.IsDefault {
				want = float64(a.Index)
				break
			}
		}
		wanted := jsonWord(n.AudioStreamIndex)
		if wanted == "null" {
			index, _ := jellyfinAudioDefault(film.Audio, n.AudioLanguagePreference, n.PlayDefaultAudioTrack)
			want = float64(index)
		} else {
			for _, a := range film.Audio {
				if jsonWord(a.Index) == wanted {
					want = float64(a.Index)
				}
			}
		}
		if n.DefaultAudioStreamIndex != want {
			t.Errorf("%s, saved %q, PlayDefaultAudioTrack %v, AudioStreamIndex %s: Jellyfin answered %v, the rule says %v",
				n.Film, n.AudioLanguagePreference, n.PlayDefaultAudioTrack, wanted, n.DefaultAudioStreamIndex, want)
		}
	}
}

// A client whose profile has no IsSecondaryAudio condition (Fluster's mpv picks the track itself) is direct played by
// Jellyfin 12.4 in every cell, whatever track it names, and the item (jellyfin-web's details page) names the track
// PlaybackInfo names: the record says so with cells, not prose, because jellyscale's costs depend on it.
func TestTheAudioDefaultCaptureHoldsTheProfileWithNoSecondaryAudioConditionAndTheItem(t *testing.T) {
	raw, err := os.ReadFile("captures/audio-default-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Films []struct {
			Name  string
			Cells []audioCell
		}
		AnyClientProfile struct{ CodecProfiles []any }
		AnyClient        []struct {
			Film                    string
			AudioLanguagePreference string
			PlayDefaultAudioTrack   bool
			DefaultAudioStreamIndex any
			SupportsDirectPlay      bool
			TranscodingUrl          any
		}
		AnyClientNamed []struct {
			Film                    string
			AudioStreamIndex        float64
			DefaultAudioStreamIndex any
			SupportsDirectPlay      bool
			TranscodingUrl          any
		}
		Details []struct {
			Film                    string
			AudioLanguagePreference string
			PlayDefaultAudioTrack   bool
			DefaultAudioStreamIndex any
			AudioStreamIndexes      []int `json:"audioStreamIndexes"`
		}
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.AnyClientProfile.CodecProfiles) != 0 {
		t.Errorf("the profile has %d CodecProfiles, want none (so no IsSecondaryAudio condition)", len(capture.AnyClientProfile.CodecProfiles))
	}
	if len(capture.AnyClient) < 80 || len(capture.AnyClientNamed) < 6 {
		t.Errorf("%d and %d cells of the profile with no IsSecondaryAudio condition, want at least 80 and 6", len(capture.AnyClient), len(capture.AnyClientNamed))
	}
	settings := map[string]bool{}
	for _, c := range capture.AnyClient {
		settings[c.AudioLanguagePreference+"|"+boolText(c.PlayDefaultAudioTrack)] = true
		if !c.SupportsDirectPlay || c.TranscodingUrl != nil {
			t.Errorf("%s, saved %q: direct play %v, TranscodingUrl %v, want a direct play", c.Film, c.AudioLanguagePreference, c.SupportsDirectPlay, c.TranscodingUrl)
		}
		// The index is the one the Chrome profile gets in the same cell.
		for _, film := range capture.Films {
			for _, cell := range film.Cells {
				if film.Name == c.Film && cell.AudioLanguagePreference == c.AudioLanguagePreference && cell.PlayDefaultAudioTrack == c.PlayDefaultAudioTrack && cell.DefaultAudioStreamIndex != c.DefaultAudioStreamIndex {
					t.Errorf("%s, saved %q: the profile with no condition gets %v, Chrome's %v", c.Film, c.AudioLanguagePreference, c.DefaultAudioStreamIndex, cell.DefaultAudioStreamIndex)
				}
			}
		}
	}
	if len(settings) < 4 {
		t.Errorf("%d settings asked, want 4", len(settings))
	}
	for _, c := range capture.AnyClientNamed {
		if !c.SupportsDirectPlay || c.TranscodingUrl != nil || c.DefaultAudioStreamIndex != c.AudioStreamIndex {
			t.Errorf("%s naming %v: default %v, direct play %v, TranscodingUrl %v, want the named track direct played", c.Film, c.AudioStreamIndex, c.DefaultAudioStreamIndex, c.SupportsDirectPlay, c.TranscodingUrl)
		}
	}
	if len(capture.Details) < 9 {
		t.Fatalf("%d item cells, want 9 (3 films, 3 settings)", len(capture.Details))
	}
	for _, d := range capture.Details {
		found := false
		for _, film := range capture.Films {
			for _, cell := range film.Cells {
				if film.Name == d.Film && cell.AudioLanguagePreference == d.AudioLanguagePreference && cell.PlayDefaultAudioTrack == d.PlayDefaultAudioTrack {
					found = true
					if cell.DefaultAudioStreamIndex != d.DefaultAudioStreamIndex {
						t.Errorf("item of %s, saved %q, %v: DefaultAudioStreamIndex %v, PlaybackInfo's %v", d.Film, d.AudioLanguagePreference, d.PlayDefaultAudioTrack, d.DefaultAudioStreamIndex, cell.DefaultAudioStreamIndex)
					}
				}
			}
		}
		if !found {
			t.Errorf("item cell of %s, saved %q, %v, has no PlaybackInfo cell to compare", d.Film, d.AudioLanguagePreference, d.PlayDefaultAudioTrack)
		}
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
