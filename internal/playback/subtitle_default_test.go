package playback

import (
	"sort"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-041: ChooseSubtitle is the subtitle a film starts with for a viewer's SubtitleMode and SubtitleLanguagePreference, as
// Jellyfin 12.4 answers (compat/captures/subtitles-default-12.4.md). It is the name and shape these checks imply and cannot
// compile without: ChooseSubtitle(streams []Stream, mode, language, audioLanguage string) (index int, ok bool), over the
// film's streams as jellyscale numbers them, with audioLanguage the language of the audio track that plays and ok false for
// no subtitle. The film's streams are Rows of the capture: the files after the film's own tracks.

func TestChooseSubtitleIsWhatJellyfinAnswersInEveryCapturedCell(t *testing.T) {
	c := testenv.LoadSubtitlesDefaultCapture(t, "../../compat/captures/subtitles-default-12.4.json")
	var names []string
	for name := range c.Films {
		names = append(names, name)
	}
	sort.Strings(names)
	cells := 0
	check := func(what string, film testenv.SubtitleFilm, mode, language, audio string, want *int) {
		t.Helper()
		cells++
		streams, indexOf := film.Rows()
		got, ok := ChooseSubtitle(streams, mode, language, audio)
		switch {
		case want == nil && ok:
			t.Errorf("%s: Jellyfin starts no subtitle, ChooseSubtitle %d", what, got)
		case want != nil && (!ok || got != indexOf[*want]):
			t.Errorf("%s: Jellyfin starts its track %d, which is %d here; ChooseSubtitle answered %d (%v)", what, *want, indexOf[*want], got, ok)
		}
	}
	for _, name := range names {
		film := c.Films[name]
		for _, cell := range film.Results {
			check(name+" "+cell.Mode+" '"+cell.LanguagePreference+"'", film, cell.Mode, cell.LanguagePreference,
				film.AudioLanguage(*cell.WithProfile.DefaultAudioStreamIndex), cell.DefaultSubtitleStreamIndex)
		}
	}
	for _, cell := range c.Smart {
		film := c.Films[cell.Film]
		check("Smart "+cell.Film+" '"+cell.LanguagePreference+"' audio '"+cell.AudioLanguagePreference+"'", film, "Smart", cell.LanguagePreference,
			film.AudioLanguage(cell.DefaultAudioStreamIndex), cell.DefaultSubtitleStreamIndex)
	}
	if cells < 606 {
		t.Errorf("%d cells replayed, want 606", cells)
	}
}

// A mode that is none or no mode starts nothing, and a saved language written another way is the same language: the tracks
// of the capture's G films are tagged ger and deu and a viewer who saved either gets both.
func TestChooseSubtitleStartsNothingForModeNoneAndReadsLanguagesByWhatTheyAre(t *testing.T) {
	c := testenv.LoadSubtitlesDefaultCapture(t, "../../compat/captures/subtitles-default-12.4.json")
	g1, _ := c.Films["G1"].Rows() // Inside the film: 2 tagged ger, 3 eng.
	g2, _ := c.Films["G2"].Rows() // Inside the film: 2 tagged deu, 3 eng.
	for name, streams := range map[string][]Stream{"G1": g1, "G2": g2} {
		for _, mode := range []string{"None", "", "Sometimes"} {
			if got, ok := ChooseSubtitle(streams, mode, "deu", "eng"); ok {
				t.Errorf("%s with mode %q started track %d", name, mode, got)
			}
		}
		for _, saved := range []string{"deu", "ger", "de", "DEU"} {
			if got, ok := ChooseSubtitle(streams, "Always", saved, "eng"); !ok || got != 2 {
				t.Errorf("%s, Always, saved %q: track %d (%v), want the German track 2", name, saved, got, ok)
			}
		}
	}
}
