package compat

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"jellyscale/internal/testenv"
)

// JS-041 observed first: compat/captures/subtitles-default-12.4.md and its JSON record which subtitle track a real Jellyfin 12.4
// names as DefaultSubtitleStreamIndex for each film, mode and language, and what the request, a progress report and the item
// change. This holds the record to the criteria, and holds the rule written down in the notes to every cell, so a rule that
// fits the prose and not the cells cannot be built from it. These checks pass on the capture alone; the checks that fail
// today are the ones that hold jellyscale to it (internal/playback, internal/api, test/e2e/subtitles.mjs).

var (
	modes     = []string{"Default", "Always", "OnlyForced", "None", "Smart"}
	languages = []string{"", "eng", "deu"}
)

func loadSubtitlesDefault(t *testing.T) *testenv.SubtitlesDefaultCapture {
	return testenv.LoadSubtitlesDefaultCapture(t, "captures/subtitles-default-12.4.json")
}

func filmNames(c *testenv.SubtitlesDefaultCapture) []string {
	var names []string
	for name := range c.Films {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasTag(f testenv.SubtitleFilm, match func(testenv.SubtitleTrack) bool) bool {
	return slices.ContainsFunc(f.Subtitles, match)
}

// every returns the film's cells keyed by mode and language, and says if any cell is missing or twice.
func cellsOf(t *testing.T, name string, f testenv.SubtitleFilm) map[[2]string]testenv.SubtitleCell {
	t.Helper()
	cells := map[[2]string]testenv.SubtitleCell{}
	for _, cell := range f.Results {
		key := [2]string{cell.Mode, cell.LanguagePreference}
		if _, twice := cells[key]; twice {
			t.Errorf("%s: the cell %v is there twice", name, key)
		}
		cells[key] = cell
	}
	for _, mode := range modes {
		for _, language := range languages {
			if _, ok := cells[[2]string{mode, language}]; !ok {
				t.Errorf("%s: no cell of mode %s and language %q", name, mode, language)
			}
		}
	}
	return cells
}

func TestTheSubtitleDefaultCaptureIsComplete(t *testing.T) {
	c := loadSubtitlesDefault(t)
	if c.Image != "jellyfin/jellyfin:12.4" || c.Date == "" || c.Version == "" {
		t.Errorf("the capture names image %q, version %q, date %q", c.Image, c.Version, c.Date)
	}

	// The 19 first films keep their 15 cells each: 285 cells.
	old := 0
	for _, name := range []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "B1", "B2", "B3", "B4", "C1", "C2", "C3", "C4", "C5", "C6"} {
		f, ok := c.Films[name]
		if !ok {
			t.Errorf("the first capture's film %s is gone", name)
			continue
		}
		old += len(cellsOf(t, name, f))
	}
	if old != 285 {
		t.Errorf("%d cells of the 19 first films, want 285", old)
	}
	for name, f := range c.Films {
		if len(cellsOf(t, name, f)) != 15 {
			t.Errorf("%s has %d cells, want 15 (5 modes x 3 languages)", name, len(f.Results))
		}
		for _, cell := range f.Results {
			if cell.WithProfile.DefaultSubtitleStreamIndex == nil != (cell.DefaultSubtitleStreamIndex == nil) ||
				cell.WithProfile.DefaultSubtitleStreamIndex != nil && *cell.WithProfile.DefaultSubtitleStreamIndex != *cell.DefaultSubtitleStreamIndex {
				t.Errorf("%s %s %q: the answer with jellyfin-web's DeviceProfile differs from the one with no profile", name, cell.Mode, cell.LanguagePreference)
			}
		}
	}

	// Smart with each subtitle language x each audio language on at least 6 films, among them B3, B4, A7 and a film with an
	// English and a Japanese audio track, for which the audio preference changes which audio plays.
	smart := map[string]map[[3]any]testenv.SmartCell{}
	for _, cell := range c.Smart {
		if smart[cell.Film] == nil {
			smart[cell.Film] = map[[3]any]testenv.SmartCell{}
		}
		smart[cell.Film][[3]any{cell.AudioLanguagePreference, cell.LanguagePreference, cell.PlayDefaultAudioTrack}] = cell
	}
	cellsWithPlayOff := 0
	for film, byKey := range smart {
		full := true
		for _, audio := range []string{"", "eng", "jpn"} {
			for _, language := range languages {
				if _, ok := byKey[[3]any{audio, language, false}]; !ok {
					full = false
				}
			}
		}
		if !full {
			t.Errorf("%s: the Smart matrix lacks a cell of audio ('', eng, jpn) x language ('', eng, deu) with PlayDefaultAudioTrack off", film)
		}
		for key := range byKey {
			if key[2] == false {
				cellsWithPlayOff++
			}
		}
	}
	if len(smart) < 6 || cellsWithPlayOff < 54 {
		t.Errorf("Smart on %d films, %d cells with PlayDefaultAudioTrack off; want at least 6 and 54", len(smart), cellsWithPlayOff)
	}
	for _, film := range []string{"B3", "B4", "A7"} {
		if smart[film] == nil {
			t.Errorf("the Smart matrix has no film %s", film)
		}
	}
	changed := false
	for film, byKey := range smart {
		tracks := c.Films[film].AudioTracks()
		languagesOfAudio := map[string]bool{}
		for _, a := range tracks {
			languagesOfAudio[a.Language] = true
		}
		if !languagesOfAudio["eng"] || !languagesOfAudio["jpn"] {
			continue
		}
		played := map[int]bool{}
		for _, cell := range byKey {
			played[cell.DefaultAudioStreamIndex] = true
		}
		changed = changed || len(played) > 1
	}
	if !changed {
		t.Error("no film with an eng and a jpn audio track plays another audio track as AudioLanguagePreference changes")
	}

	// The films the languages criterion names, every mode x language, with what a transcode does to a PGS default.
	has := func(what string, match func(testenv.SubtitleTrack) bool) []string {
		t.Helper()
		var films []string
		for name, f := range c.Films {
			if hasTag(f, match) {
				films = append(films, name)
			}
		}
		if len(films) == 0 {
			t.Errorf("no film with %s", what)
		}
		return films
	}
	has("a subtitle tagged ger", func(s testenv.SubtitleTrack) bool { return s.Tag != nil && *s.Tag == "ger" })
	has("a subtitle tagged deu", func(s testenv.SubtitleTrack) bool { return s.Tag != nil && *s.Tag == "deu" })
	has("an untagged subtitle", func(s testenv.SubtitleTrack) bool { return s.Language == nil && s.Tag == nil && s.Codec != "" })
	has("an untagged subtitle file", func(s testenv.SubtitleTrack) bool { return s.Language == nil && s.IsExternal })
	pgs := has("a PGS track inside the film", func(s testenv.SubtitleTrack) bool { return s.Image() && !s.IsExternal })
	has("a PGS file beside the film", func(s testenv.SubtitleTrack) bool { return s.Image() && s.IsExternal })
	burnt := 0
	for _, name := range pgs {
		for _, cell := range c.Films[name].Results {
			a := cell.WithProfile
			if a.DefaultSubtitleStreamIndex == nil {
				continue
			}
			for _, s := range c.Films[name].Subtitles {
				if s.Index == *a.DefaultSubtitleStreamIndex && s.Image() {
					burnt++
					if a.SupportsDirectPlay || a.TranscodingUrl["SubtitleMethod"] != "Encode" || a.DeliveryMethod == nil || *a.DeliveryMethod != "Encode" {
						t.Errorf("%s %s %q: an image default is not a transcode that burns it: %+v", name, cell.Mode, cell.LanguagePreference, a)
					}
				}
			}
		}
	}
	if burnt < 10 {
		t.Errorf("%d cells with a PGS default, want at least 10", burnt)
	}

	// A progress report: with RememberSubtitleSelections on and off, a track and -1; each asked for the same viewer's film, the
	// same viewer's other film and another viewer's.
	seen := map[[2]bool]bool{}
	for _, r := range c.Remember {
		seen[[2]bool{r.RememberSubtitleSelections, r.ReportedSubtitleStreamIndex == -1}] = true
		if len(r.Steps) < 2 || r.Steps[len(r.Steps)-1].After != "stop" {
			t.Errorf("a report case of %d steps, want progress and stop among them", len(r.Steps))
		}
	}
	for _, want := range [][2]bool{{true, false}, {true, true}, {false, false}, {false, true}} {
		if !seen[want] {
			t.Errorf("no report case with RememberSubtitleSelections %v and reporting -1 %v", want[0], want[1])
		}
	}

	// A request that names SubtitleStreamIndex -1 and 99 under mode Always, and a track of each kind.
	named := map[string]bool{}
	for _, n := range c.Named {
		if n.Mode == "Always" && n.SubtitleStreamIndex != nil {
			named[strings.Join([]string{n.Film, strconv.Itoa(*n.SubtitleStreamIndex)}, "|")] = true
		}
	}
	for _, want := range []string{"A5|-1", "A5|99", "A5|0", "A5|3", "P2|3", "B3|2"} {
		if !named[want] {
			t.Errorf("no named cell %s under mode Always", want)
		}
	}

	// The item, by each mode and language.
	itemModes := map[string]bool{}
	for _, item := range c.Items {
		itemModes[item.Mode+"|"+item.LanguagePreference] = true
	}
	if len(itemModes) != 15 || len(c.Items) < 70 {
		t.Errorf("%d item answers over %d modes x languages, want every one of 15", len(c.Items), len(itemModes))
	}

	// Which code jellyfin-web saves for German.
	if got := c.German.SavedByTheSubtitlesPage.Saved.SubtitleLanguagePreference; got != "deu" || c.German.SavedByTheSubtitlesPage.Option.Label != "German" {
		t.Errorf("jellyfin-web's Subtitles page saved %q for the option %+v", got, c.German.SavedByTheSubtitlesPage.Option)
	}
	if len(c.German.Cultures) != 1 {
		t.Errorf("%d cultures for German", len(c.German.Cultures))
	}

}

func TestTheSubtitleDefaultCaptureFitsTheRuleWrittenDown(t *testing.T) {
	c := loadSubtitlesDefault(t)
	check := func(what string, tracks []testenv.SubtitleTrack, mode, saved, audio string, want *int) {
		t.Helper()
		index, ok := testenv.JellyfinSubtitleDefault(tracks, mode, saved, audio)
		switch {
		case want == nil && ok:
			t.Errorf("%s: Jellyfin answered no subtitle, the rule says %d", what, index)
		case want != nil && (!ok || index != *want):
			t.Errorf("%s: Jellyfin answered %d, the rule says %d (%v)", what, *want, index, ok)
		}
	}
	cells := 0
	for _, name := range filmNames(c) {
		f := c.Films[name]
		for _, cell := range f.Results {
			cells++
			audio := f.AudioLanguage(*cell.WithProfile.DefaultAudioStreamIndex)
			check(name+" "+cell.Mode+" "+quote(cell.LanguagePreference), f.Subtitles, cell.Mode, cell.LanguagePreference, audio, cell.DefaultSubtitleStreamIndex)
		}
	}
	for _, cell := range c.Smart {
		cells++
		f := c.Films[cell.Film]
		check("Smart "+cell.Film+" audio "+quote(cell.AudioLanguagePreference)+" play "+onOff(cell.PlayDefaultAudioTrack)+" "+quote(cell.LanguagePreference),
			f.Subtitles, "Smart", cell.LanguagePreference, f.AudioLanguage(cell.DefaultAudioStreamIndex), cell.DefaultSubtitleStreamIndex)
	}
	if cells < 606 {
		t.Errorf("%d cells held to the rule, want at least 606", cells)
	}

	// The request names a track: with jellyfin-web's profile the number is the answer, whatever it is; with no profile it is
	// ignored and the mode's default answers.
	films := map[string]testenv.SubtitleFilm{}
	for name, f := range c.Films {
		films[name] = f
	}
	for _, n := range c.Named {
		f := films[n.Film]
		var mode *testenv.SubtitleCell
		for i := range f.Results {
			if f.Results[i].Mode == n.Mode && f.Results[i].LanguagePreference == n.LanguagePreference {
				mode = &f.Results[i]
			}
		}
		profile, plain := n.WithProfile.DefaultSubtitleStreamIndex, n.DefaultSubtitleStreamIndex
		if (plain == nil) != (mode.DefaultSubtitleStreamIndex == nil) || (plain != nil && *plain != *mode.DefaultSubtitleStreamIndex) {
			t.Errorf("%s %s %q naming %v with no profile: %v, the mode's own %v", n.Film, n.Mode, n.LanguagePreference, n.SubtitleStreamIndex, plain, mode.DefaultSubtitleStreamIndex)
		}
		want := mode.DefaultSubtitleStreamIndex
		if n.SubtitleStreamIndex != nil {
			want = n.SubtitleStreamIndex
		}
		if (profile == nil) != (want == nil) || (profile != nil && *profile != *want) {
			t.Errorf("%s %s %q naming %v with a profile: %v, want %v", n.Film, n.Mode, n.LanguagePreference, n.SubtitleStreamIndex, profile, want)
		}
	}

	// The item says what PlaybackInfo says, in every answer, and lists the film's subtitles.
	for _, item := range c.Items {
		var want any = "<absent>"
		for _, cell := range c.Films[item.Film].Results {
			if cell.Mode == item.Mode && cell.LanguagePreference == item.LanguagePreference && cell.DefaultSubtitleStreamIndex != nil {
				want = float64(*cell.DefaultSubtitleStreamIndex)
			}
		}
		if item.DefaultSubtitle != want || item.PlaybackInfo != want {
			t.Errorf("the item of %s, %s %q: %v, PlaybackInfo %v, the cell %v", item.Film, item.Mode, item.LanguagePreference, item.DefaultSubtitle, item.PlaybackInfo, want)
		}
		var listed []int
		for _, s := range c.Films[item.Film].Subtitles {
			listed = append(listed, s.Index)
		}
		if !slices.Equal(listed, item.SubtitleIndexes) {
			t.Errorf("the item of %s lists subtitles %v, PlaybackInfo %v", item.Film, item.SubtitleIndexes, listed)
		}
	}

	// A default is a listed Subtitle stream at its own position, a text track changes no method, and an image one is burnt.
	for _, name := range filmNames(c) {
		f := c.Films[name]
		for _, cell := range f.Results {
			a := cell.WithProfile
			if a.DefaultSubtitleStreamIndex == nil {
				if a.AtPosition != nil || !a.SupportsDirectPlay {
					t.Errorf("%s %s %q: no default and yet %+v", name, cell.Mode, cell.LanguagePreference, a)
				}
				continue
			}
			if a.AtPosition == nil || a.AtPosition.Type != "Subtitle" || a.AtPosition.Index != *a.DefaultSubtitleStreamIndex {
				t.Errorf("%s %s %q: MediaStreams at the default is %+v", name, cell.Mode, cell.LanguagePreference, a.AtPosition)
			}
			var track testenv.SubtitleTrack
			for _, s := range f.Subtitles {
				if s.Index == *a.DefaultSubtitleStreamIndex {
					track = s
				}
			}
			if track.Image() == a.SupportsDirectPlay || (a.DeliveryMethod != nil && *a.DeliveryMethod == "Encode") != track.Image() {
				t.Errorf("%s %s %q: default %d image %v, direct play %v, delivery %v", name, cell.Mode, cell.LanguagePreference, track.Index, track.Image(), a.SupportsDirectPlay, a.DeliveryMethod)
			}
			if track.Image() && a.TranscodingUrl["SubtitleStreamIndex"] != strconv.Itoa(track.Index) {
				t.Errorf("%s %s %q: the TranscodingUrl burns %q, the default is %d", name, cell.Mode, cell.LanguagePreference, a.TranscodingUrl["SubtitleStreamIndex"], track.Index)
			}
			if a.TranscodingUrl != nil && a.TranscodingUrl["AudioStreamIndex"] != strconv.Itoa(*a.DefaultAudioStreamIndex) {
				t.Errorf("%s %s %q: the TranscodingUrl names audio %q, the default is %d", name, cell.Mode, cell.LanguagePreference, a.TranscodingUrl["AudioStreamIndex"], *a.DefaultAudioStreamIndex)
			}
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func quote(s string) string { return "'" + s + "'" }

// What a progress report changes, as the notes say it: with RememberSubtitleSelections on, a Progress that names a track
// other than the default changes that viewer's next default for that film, and nothing else; with it off, nothing; Start
// changes nothing.
func TestTheSubtitleDefaultCaptureHoldsWhatAProgressReportChanges(t *testing.T) {
	c := loadSubtitlesDefault(t)
	for _, r := range c.Remember {
		before := r.Before.SameViewerSameFilm
		var progress, stop, start *testenv.RememberStep
		for i := range r.Steps {
			switch r.Steps[i].After {
			case "start":
				start = &r.Steps[i]
			case "progress":
				progress = &r.Steps[i]
			case "stop":
				stop = &r.Steps[i]
			}
		}
		if start == nil || progress == nil || stop == nil {
			t.Fatalf("a report case with no start, progress or stop: %+v", r.Steps)
		}
		wantSame := before
		if r.RememberSubtitleSelections && float64(r.ReportedSubtitleStreamIndex) != before {
			wantSame = float64(r.ReportedSubtitleStreamIndex)
		}
		if *start != withAfter(r.Before, "start") {
			t.Errorf("remember %v reporting %d: Start changed an answer: %+v", r.RememberSubtitleSelections, r.ReportedSubtitleStreamIndex, *start)
		}
		for _, step := range []*testenv.RememberStep{progress, stop} {
			if step.SameViewerSameFilm != wantSame || step.SameViewerSameFilmItem != wantSame {
				t.Errorf("remember %v reporting %d, after %s: the same film answers %v and its item %v, want %v", r.RememberSubtitleSelections, r.ReportedSubtitleStreamIndex, step.After, step.SameViewerSameFilm, step.SameViewerSameFilmItem, wantSame)
			}
			if step.SameViewerOtherFilm != r.Before.SameViewerOtherFilm || step.OtherViewerSameFilm != r.Before.OtherViewerSameFilm {
				t.Errorf("remember %v reporting %d, after %s: another film or viewer changed: %+v", r.RememberSubtitleSelections, r.ReportedSubtitleStreamIndex, step.After, *step)
			}
		}
	}
}

func withAfter(s testenv.RememberStep, after string) testenv.RememberStep {
	s.After = after
	return s
}
