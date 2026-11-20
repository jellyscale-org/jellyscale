package compat

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// JS-147 observed first: compat/captures/audio-menu-12.4.md and its JSON record what a real Jellyfin 12.4 puts in the
// details page's Audio menu and what jellyfin-web does with a pick. This holds the record to the criteria of the task, so
// the checks that read it (internal/api, test/e2e) read what was seen, not what a build would like.

type menuTrack struct {
	Index        int
	DisplayTitle string
	IsDefault    bool
	Language     *string
}

type menuItem struct {
	Film                    string
	AudioLanguagePreference string
	PlayDefaultAudioTrack   bool
	DefaultAudioStreamIndex any
	Audio                   []menuTrack
	ItemAudioIndexes        []int
}

type menuEntry struct{ Value, Label string }

type menuState struct {
	ContainerHidden bool
	Disabled        bool
	Options         []menuEntry
	Selected        string
}

type playerEntry struct {
	ID       string
	Label    string
	Selected bool
}

type playInfo struct {
	AudioStreamIndex        any
	DefaultAudioStreamIndex any
	SupportsDirectPlay      bool
	TranscodingUrl          *struct{ AudioStreamIndex, VideoCodec, TranscodeReasons string }
}

type menuPick struct {
	Film                    string
	AudioLanguagePreference string
	PlayDefaultAudioTrack   bool
	SelectedBefore, Picked  string
	Tone                    int
	PlayerMenu              []playerEntry
	Infos                   []playInfo
	OnReturn, AfterReload   menuState
	AfterSignIn             menuState
	Failed                  *string
}

type menuCapture struct {
	Image, Version, Date string
	Films                []struct {
		Name  string
		Audio []struct {
			Language  *string
			IsDefault bool
		}
	}
	Items []menuItem
	Web   struct {
		Menus []struct {
			menuState
			Film                    string
			AudioLanguagePreference string
		}
		Picks []menuPick
	}
}

func loadAudioMenu(t *testing.T) menuCapture {
	t.Helper()
	raw, err := os.ReadFile("captures/audio-menu-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture menuCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

func (c menuCapture) item(t *testing.T, film, language string) menuItem {
	t.Helper()
	for _, i := range c.Items {
		if i.Film == film && i.AudioLanguagePreference == language {
			return i
		}
	}
	t.Fatalf("no item cell for %s with %q saved", film, language)
	return menuItem{}
}

func (c menuCapture) menu(t *testing.T, film, language string) menuState {
	t.Helper()
	for _, m := range c.Web.Menus {
		if m.Film == film && m.AudioLanguagePreference == language {
			return m.menuState
		}
	}
	t.Fatalf("no menu for %s with %q saved", film, language)
	return menuState{}
}

func values(m menuState) []string {
	var out []string
	for _, o := range m.Options {
		out = append(out, o.Value)
	}
	return out
}

// The film of the criterion: at least 4 audio tracks, two in one language, one untagged, the default flag not on the first.
func TestTheMenuCaptureHasAFilmOfFourTracksWithTwoInOneLanguageOneUntaggedAndTheFlagNotFirst(t *testing.T) {
	c := loadAudioMenu(t)
	if c.Image != "jellyfin/jellyfin:12.4" || c.Version != "12.4.0" {
		t.Errorf("recorded on %s %s, want jellyfin/jellyfin:12.4 12.4.0", c.Image, c.Version)
	}
	item := c.item(t, "Menu Four", "")
	if len(item.Audio) < 4 {
		t.Fatalf("Menu Four lists %d audio streams, want at least 4", len(item.Audio))
	}
	languages := map[string]int{}
	untagged := false
	for _, a := range item.Audio {
		if a.Language == nil {
			untagged = true
		} else {
			languages[*a.Language]++
		}
	}
	two := false
	for _, n := range languages {
		two = two || n >= 2
	}
	if !two || !untagged || item.Audio[0].IsDefault {
		t.Errorf("Menu Four: two tracks in one language %v, an untagged one %v, default flag on the first %v", two, untagged, item.Audio[0].IsDefault)
	}
}

// What the item lists, whoever asks: every audio stream in the file's order, in both lists, each with its label.
func TestJellyfinsItemListsEveryAudioStreamAndNamesTheViewersTrack(t *testing.T) {
	c := loadAudioMenu(t)
	for _, tc := range []struct {
		film, language string
		indexes        []int
		chosen         any
	}{
		{"Menu Babel", "", []int{1, 2, 3, 4}, float64(1)},
		{"Menu Babel", "deu", []int{1, 2, 3, 4}, float64(2)},
		{"Menu Four", "", []int{1, 2, 3, 4}, float64(3)},
		{"Menu Four", "deu", []int{1, 2, 3, 4}, float64(2)},
		{"Menu Mono", "", []int{1}, float64(1)},
		{"Menu Silent", "", nil, "<absent>"},
	} {
		item := c.item(t, tc.film, tc.language)
		var listed []int
		titles := map[string]bool{}
		for _, a := range item.Audio {
			listed = append(listed, a.Index)
			titles[a.DisplayTitle] = true
			if a.DisplayTitle == "" {
				t.Errorf("%s: audio %d has no label", tc.film, a.Index)
			}
		}
		if !slices.Equal(listed, tc.indexes) || !slices.Equal(item.ItemAudioIndexes, tc.indexes) || item.DefaultAudioStreamIndex != tc.chosen {
			t.Errorf("%s, saved %q: MediaSources lists %v, MediaStreams %v, DefaultAudioStreamIndex %v; want %v and %v", tc.film, tc.language, listed, item.ItemAudioIndexes, item.DefaultAudioStreamIndex, tc.indexes, tc.chosen)
		}
	}
	// Two tracks in one language are labelled alike by Jellyfin; the labels of Menu Babel differ.
	titles := map[string]bool{}
	for _, a := range c.item(t, "Menu Babel", "").Audio {
		titles[a.DisplayTitle] = true
	}
	if len(titles) != 4 {
		t.Errorf("Menu Babel has %d different labels, want 4", len(titles))
	}
}

// The menu: order (the default first, then by Index), selection, and whether it is shown and enabled.
func TestTheMenuCaptureRecordsOrderLabelsSelectionAndWhetherItIsShown(t *testing.T) {
	c := loadAudioMenu(t)
	for _, tc := range []struct {
		film, language string
		order          []string
		selected       string
		hidden, off    bool
	}{
		{"Menu Babel", "", []string{"1", "2", "3", "4"}, "1", false, false},
		{"Menu Babel", "deu", []string{"1", "2", "3", "4"}, "2", false, false},
		{"Menu Four", "", []string{"3", "1", "2", "4"}, "3", false, false},
		{"Menu Four", "deu", []string{"3", "1", "2", "4"}, "2", false, false},
		{"Menu Mono", "", []string{"1"}, "1", false, true},
		{"Menu Silent", "", nil, "", true, true},
	} {
		m := c.menu(t, tc.film, tc.language)
		if !slices.Equal(values(m), tc.order) || m.Selected != tc.selected || m.ContainerHidden != tc.hidden || m.Disabled != tc.off {
			t.Errorf("%s, saved %q: entries %v selected %q hidden %v disabled %v; want %v %q %v %v", tc.film, tc.language, values(m), m.Selected, m.ContainerHidden, m.Disabled, tc.order, tc.selected, tc.hidden, tc.off)
		}
		// Each entry's text is the stream's DisplayTitle.
		titles := map[string]string{}
		for _, a := range c.item(t, tc.film, tc.language).Audio {
			titles[itoa(a.Index)] = a.DisplayTitle
		}
		for _, o := range m.Options {
			if o.Label != titles[o.Value] {
				t.Errorf("%s: entry %s reads %q, its stream's DisplayTitle is %q", tc.film, o.Value, o.Label, titles[o.Value])
			}
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// What a pick that is neither the selected entry nor the file's default does, and what a pick of the default does.
func TestTheMenuCaptureRecordsWhatAPickSendsPlaysAndLeavesSelected(t *testing.T) {
	c := loadAudioMenu(t)
	if len(c.Web.Picks) < 3 {
		t.Fatalf("%d picks recorded, want the German viewer's 3 and 1 and the other viewer's 3", len(c.Web.Picks))
	}
	for _, p := range c.Web.Picks {
		if p.Failed != nil {
			t.Fatalf("a pick failed while recorded: %s", *p.Failed)
		}
		if len(p.Infos) != 1 || p.Infos[0].AudioStreamIndex != p.Picked || p.Infos[0].DefaultAudioStreamIndex != mustNumber(p.Picked) {
			t.Errorf("pick %s (saved %q): PlaybackInfo %+v; want the menu's value sent and the same index named", p.Picked, p.AudioStreamIndexLanguage(), p.Infos)
			continue
		}
		first := p.Picked == "1"
		if p.Infos[0].SupportsDirectPlay != first || (p.Infos[0].TranscodingUrl == nil) != first {
			t.Errorf("pick %s: direct play %v, transcoding url %v; want direct play only for the file's first track", p.Picked, p.Infos[0].SupportsDirectPlay, p.Infos[0].TranscodingUrl)
		}
		wantTone := 440 + 110*(mustInt(p.Picked)-1)
		if p.Tone < wantTone-15 || p.Tone > wantTone+15 {
			t.Errorf("pick %s: the sound's strongest tone is %d Hz, want %d", p.Picked, p.Tone, wantTone)
		}
		var shown []string
		for _, e := range p.PlayerMenu {
			if e.Selected {
				shown = append(shown, e.ID)
			}
		}
		if !slices.Equal(shown, []string{p.Picked}) {
			t.Errorf("pick %s: the player's audio menu selects %v", p.Picked, shown)
		}
		// Jellyfin remembers the pick for the viewer: a page loaded again, and a new sign-in, select it.
		if p.AfterReload.Selected != p.Picked || p.AfterSignIn.Selected != p.Picked {
			t.Errorf("pick %s: selected %q on reload and %q after signing in again", p.Picked, p.AfterReload.Selected, p.AfterSignIn.Selected)
		}
	}
	german := c.Web.Picks[0]
	if german.SelectedBefore != "2" || german.Picked != "3" || german.Infos[0].TranscodingUrl.TranscodeReasons != "SecondaryAudioNotSupported" {
		t.Errorf("the German viewer's pick of 3: %+v", german)
	}
}

func (p menuPick) AudioStreamIndexLanguage() string { return p.AudioLanguagePreference }

func mustInt(s string) int {
	var n int
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		panic(err)
	}
	return n
}

func mustNumber(s string) any { return float64(mustInt(s)) }
