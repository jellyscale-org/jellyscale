package testenv

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"jellyscale/internal/media"
)

// The capture of which subtitle a real Jellyfin 12.4 names as DefaultSubtitleStreamIndex (JS-041),
// compat/captures/subtitles-default-12.4.json, read for the tests that hold jellyscale to it. The file's layout is made by
// test/differential/capture_subtitles_default.py and read in compat/captures/subtitles-default-12.4.md. Every index in it
// is Jellyfin's: the subtitle files first, then the film's own streams.

// SubtitleTrack is one subtitle of a captured film as Jellyfin listed it. Tag is the language the file was made with
// (ger where Jellyfin lists deu), nil for an untagged track. The 19 first films have no Tag or Codec: their tags are
// their languages and their tracks SubRip.
type SubtitleTrack struct {
	Index      int
	Language   *string
	Tag        *string
	IsDefault  bool
	IsForced   bool
	IsExternal bool
	Codec      string
}

// Image says whether the track is a bitmap track (PGS in the captured films).
func (s SubtitleTrack) Image() bool { return s.Codec == "PGSSUB" }

// WrittenTag is the language the file wore.
func (s SubtitleTrack) WrittenTag() string {
	switch {
	case s.Tag != nil:
		return *s.Tag
	case s.Language != nil && s.Codec == "":
		return *s.Language
	}
	return ""
}

// SubtitleAnswer is what a reply said about the subtitle with the DeviceProfile of transcode-12.4.json.
type SubtitleAnswer struct {
	DefaultSubtitleStreamIndex *int
	DefaultAudioStreamIndex    *int
	SupportsDirectPlay         bool
	TranscodingUrl             map[string]string
	DeliveryMethod             *string
	AtPosition                 *struct {
		Type  string
		Index int
	}
}

// SubtitleCell is one film, mode and language. DefaultSubtitleStreamIndex is nil where Jellyfin's reply had no such field.
type SubtitleCell struct {
	Mode                       string
	LanguagePreference         string `json:"language_preference"`
	DefaultSubtitleStreamIndex *int
	WithProfile                SubtitleAnswer
}

// AudioTrack is an audio track of a captured film, in Jellyfin's order.
type AudioTrack struct {
	Language  string
	IsDefault bool
}

// SubtitleFilm is a captured film: its audio tracks, its subtitles in Jellyfin's order and its 15 cells.
type SubtitleFilm struct {
	Audio     json.RawMessage // The language of the one audio track, or a list of AudioTrack.
	Subtitles []SubtitleTrack
	Results   []SubtitleCell
}

// AudioTracks is the film's audio in Jellyfin's order.
func (f SubtitleFilm) AudioTracks() []AudioTrack {
	var language string
	if json.Unmarshal(f.Audio, &language) == nil {
		return []AudioTrack{{Language: language, IsDefault: true}}
	}
	var tracks []AudioTrack
	if err := json.Unmarshal(f.Audio, &tracks); err != nil {
		panic(err)
	}
	return tracks
}

// Files is how many subtitles are files beside the film, which Jellyfin numbers first.
func (f SubtitleFilm) Files() int {
	n := 0
	for _, s := range f.Subtitles {
		if s.IsExternal {
			n++
		}
	}
	return n
}

// AudioLanguage is the language of the audio track Jellyfin played in a reply that named DefaultAudioStreamIndex.
func (f SubtitleFilm) AudioLanguage(defaultAudio int) string {
	return f.AudioTracks()[defaultAudio-(f.Files()+1)].Language
}

// SmartCell is a Smart cell of the audio x subtitle language matrix.
type SmartCell struct {
	Film                       string
	AudioLanguagePreference    string
	PlayDefaultAudioTrack      bool
	LanguagePreference         string `json:"language_preference"`
	DefaultSubtitleStreamIndex *int
	DefaultAudioStreamIndex    int
	WithProfile                SubtitleAnswer
}

// NamedCell is an answer to a request that names SubtitleStreamIndex (nil: names none).
type NamedCell struct {
	Film                       string
	Mode                       string
	LanguagePreference         string `json:"language_preference"`
	SubtitleStreamIndex        *int
	DefaultSubtitleStreamIndex *int // With no DeviceProfile.
	WithProfile                SubtitleAnswer
}

// ItemCell is GET /Items/{id}'s answer beside PlaybackInfo's; either is the string "<absent>" or a number.
type ItemCell struct {
	Film               string
	Mode               string
	LanguagePreference string `json:"language_preference"`
	DefaultSubtitle    any    `json:"DefaultSubtitleStreamIndex"`
	PlaybackInfo       any    `json:"playbackInfo"`
	SubtitleIndexes    []int  `json:"subtitleIndexes"`
}

// RememberStep is what the next answers were after one report of a play.
type RememberStep struct {
	After                   string
	SameViewerSameFilm      any
	SameViewerSameFilmItem  any
	SameViewerOtherFilm     any
	SameViewerOtherFilmItem any
	OtherViewerSameFilm     any
	OtherViewerSameFilmItem any
}

// RememberCase is a progress report naming a SubtitleStreamIndex, with RememberSubtitleSelections on or off.
type RememberCase struct {
	RememberSubtitleSelections  bool
	ReportedSubtitleStreamIndex int
	Mode                        string
	Before                      RememberStep
	Steps                       []RememberStep
}

// SubtitlesDefaultCapture is the whole file.
type SubtitlesDefaultCapture struct {
	Image, Version, Date string
	Films                map[string]SubtitleFilm
	Smart                []SmartCell
	Named                []NamedCell
	Items                []ItemCell
	Remember             []RememberCase
	German               struct {
		Cultures                []map[string]any
		SavedByTheSubtitlesPage struct {
			Option struct{ Value, Label string }
			Saved  struct{ SubtitleLanguagePreference, SubtitleMode string }
		}
	}
}

// LoadSubtitlesDefaultCapture reads the capture from path (relative to the test's package).
func LoadSubtitlesDefaultCapture(t testing.TB, path string) *SubtitlesDefaultCapture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c SubtitlesDefaultCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// Rows is the film as jellyscale stores it, which numbers the film's own streams as ffprobe does and the subtitle files after
// them, not before: video 0, the audio tracks, the subtitles inside the film, then the files in file name order. indexOf maps
// the Index Jellyfin gave a track to the Index of the same track here, a track being the same by what it is: inside the film
// or a file, and its place among those. A track inside is as ffprobe tags it (ger stays ger), a file as the scan reads its
// name (ger is deu, an untagged file has no language).
func (f SubtitleFilm) Rows() (streams []media.MediaStream, indexOf map[int]int) {
	indexOf = map[int]int{}
	audio := f.AudioTracks()
	streams = append(streams, media.MediaStream{Index: 0, Type: "Video", Codec: "h264", Profile: "Main", Level: 41, VideoRangeType: "SDR", Width: 1280, Height: 720, AverageFrameRate: 24})
	for i, a := range audio {
		streams = append(streams, media.MediaStream{Index: 1 + i, Type: "Audio", Codec: "aac", Channels: 2, Language: a.Language, IsDefault: a.IsDefault})
	}
	add := func(s SubtitleTrack, language string) {
		index := len(streams)
		indexOf[s.Index] = index
		stream := media.MediaStream{Index: index, Type: "Subtitle", Language: language, IsDefault: s.IsDefault, IsForced: s.IsForced, IsExternal: s.IsExternal,
			Codec: "subrip", IsTextSubtitleStream: true, SupportsExternalStream: true}
		if s.Image() {
			stream.Codec, stream.IsTextSubtitleStream, stream.IsImageSubtitleStream = "PGSSUB", false, media.Ptr(true)
		}
		if s.IsExternal {
			stream.Path = fmt.Sprintf("film/Film.%d.srt", index)
		}
		streams = append(streams, stream)
	}
	for _, s := range f.Subtitles {
		if !s.IsExternal {
			add(s, s.WrittenTag())
		}
	}
	for _, s := range f.Subtitles {
		if s.IsExternal {
			language := ""
			if s.Language != nil {
				language = *s.Language
			}
			add(s, language)
		}
	}
	return streams, indexOf
}

// JellyfinSubtitleDefault is the rule of compat/captures/subtitles-default-12.4.md, as code (compat/subtitles_default_capture_test.go
// holds it to every captured cell), over a film's subtitles in Jellyfin's order:
// the index chosen and whether there is one. audio is the language of the audio track that plays.
func JellyfinSubtitleDefault(tracks []SubtitleTrack, mode, saved, audio string) (int, bool) {
	language := func(s SubtitleTrack) string {
		if s.Language == nil {
			return ""
		}
		return *s.Language
	}
	inSaved := func(s SubtitleTrack) bool { return saved == "" || language(s) == saved }
	nothingInSaved := saved != ""
	for _, s := range tracks {
		if language(s) == saved {
			nothingInSaved = false
		}
	}
	rank := func(s SubtitleTrack) [5]int {
		plainFirst := 0
		if s.IsForced {
			plainFirst = 1
		}
		if nothingInSaved { // The clause: with nothing in the saved language, forced comes first.
			plainFirst = 1 - plainFirst
		}
		return [5]int{b2i(!s.IsExternal), b2i(!s.IsDefault), b2i(saved == "" || language(s) != saved), plainFirst, s.Index}
	}
	first := func(keep func(SubtitleTrack) bool) (int, bool) {
		var candidates []SubtitleTrack
		for _, s := range tracks {
			if keep(s) {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) == 0 {
			return 0, false
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := rank(candidates[i]), rank(candidates[j])
			for k := range a {
				if a[k] != b[k] {
					return a[k] < b[k]
				}
			}
			return false
		})
		return candidates[0].Index, true
	}
	firstOf := func(keeps ...func(SubtitleTrack) bool) (int, bool) {
		for _, keep := range keeps {
			if index, ok := first(keep); ok {
				return index, true
			}
		}
		return 0, false
	}
	plainInSaved := func(s SubtitleTrack) bool { return inSaved(s) && !s.IsForced }
	forcedInSaved := func(s SubtitleTrack) bool { return inSaved(s) && s.IsForced }
	switch mode {
	case "Default":
		return first(func(s SubtitleTrack) bool { return s.IsExternal || s.IsForced || s.IsDefault })
	case "Always":
		return firstOf(plainInSaved, forcedInSaved)
	case "OnlyForced":
		return first(forcedInSaved)
	case "Smart":
		if saved == "" {
			return first(func(SubtitleTrack) bool { return true })
		}
		if audio != saved {
			return firstOf(plainInSaved, forcedInSaved)
		}
		return first(forcedInSaved)
	}
	return 0, false // None.
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
