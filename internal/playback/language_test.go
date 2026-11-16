package playback

import "testing"

// JS-117. These name what the task implies and do not compile until it is built:
// SameLanguage(a, b string) bool, the one comparison of languages (hiding and the
// default tracks use it too), and Source.OriginalLanguage() string, the default
// audio track's language tag as the file wrote it, or "" when the film has none.

// The traps of golang.org/x/text/language that a plain "same base" build falls into:
// it parses und and any unparseable tag to base en at low confidence (so they would
// equal English), mis, mul and zxx parse to themselves (so each would equal itself),
// and nor, nob and nno have three bases, as do zho, cmn and yue.
func TestSameLanguageComparesLanguagesNotSpellings(t *testing.T) {
	one := [][]string{
		{"ger", "deu", "de", "de-DE", "GER", "De-at"},
		{"fre", "fra", "fr", "fr-CA", "FRE"},
		{"chi", "zho", "zh", "zh-TW", "zh-Hant", "ZHO"},
		{"pt-br", "por", "pt", "pt-PT"},
		{"eng", "en", "en-GB", "ENG"},
		// The two joined by hand: a macrolanguage and its members.
		{"nor", "nob", "nno", "no", "nb", "nn"},
		{"zho", "chi", "zh", "zh-TW", "cmn", "yue"},
	}
	for _, spellings := range one {
		for _, a := range spellings {
			for _, b := range spellings {
				if !SameLanguage(a, b) {
					t.Errorf("SameLanguage(%q, %q) = false, want true (one language, either order)", a, b)
				}
			}
		}
	}
	differ := [][2]string{
		{"eng", "deu"}, {"eng", "enm"}, {"enm", "eng"}, {"deu", "fra"}, {"pt-br", "eng"},
		{"nor", "swe"}, {"nob", "dan"}, {"nno", "swe"}, {"no", "da"}, {"swe", "nor"},
		{"zho", "jpn"}, {"cmn", "jpn"}, {"yue", "jpn"}, {"zh", "ko"}, {"jpn", "zh-TW"},
		{"nor", "zho"}, {"deu", "nld"},
	}
	for _, pair := range differ {
		if SameLanguage(pair[0], pair[1]) {
			t.Errorf("SameLanguage(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestATagWithNoLanguageEqualsNothingItselfIncluded(t *testing.T) {
	// The empty tag, und, mis, mul, zxx, malformed tags, and a well-formed one
	// that names no language (xyz).
	none := []string{"", "und", "UND", "mis", "mul", "zxx", "english", "???", "xyz", "e", "und-DE"}
	known := []string{"eng", "en", "deu", "de", "fra", "jpn", "zho", "nor", "nob"}
	for _, a := range none {
		for _, b := range none {
			if SameLanguage(a, b) {
				t.Errorf("SameLanguage(%q, %q) = true: a tag with no language equals nothing, itself included", a, b)
			}
		}
		for _, k := range known {
			if SameLanguage(a, k) || SameLanguage(k, a) {
				t.Errorf("SameLanguage(%q, %q) = true in one order: %q has no language and equals no language (und parses to en)", a, k, a)
			}
		}
	}
}

func audio(index int, language string, isDefault bool) Stream {
	return Stream{Index: index, Type: "Audio", Language: language, IsDefault: isDefault}
}

func TestOriginalLanguageIsTheDefaultAudiosOwn(t *testing.T) {
	for _, c := range []struct {
		name    string
		streams []Stream
		want    string
	}{
		{"eng default, deu", []Stream{audio(1, "eng", true), audio(2, "deu", false)}, "eng"},
		{"deu, eng default", []Stream{audio(1, "deu", false), audio(2, "eng", true)}, "eng"},
		{"deu, eng and no flag: the first", []Stream{audio(1, "deu", false), audio(2, "eng", false)}, "deu"},
		{"jpn default, eng default: the first flagged", []Stream{audio(1, "jpn", true), audio(2, "eng", true)}, "jpn"},
		{"untagged first, eng: none, never the next tagged one", []Stream{audio(1, "", false), audio(2, "eng", false)}, ""},
		{"untagged default, eng: none", []Stream{audio(1, "", true), audio(2, "eng", false)}, ""},
		{"no audio", []Stream{{Index: 0, Type: "Video"}}, ""},
		{"no streams", nil, ""},
		{"the tag as the file wrote it, not normalised", []Stream{audio(1, "ger", true), audio(2, "eng", false)}, "ger"},
		{"und on the default is none even before eng", []Stream{audio(1, "und", true), audio(2, "eng", false)}, ""},
		{"mis", []Stream{audio(1, "mis", true), audio(2, "eng", false)}, ""},
		{"mul", []Stream{audio(1, "mul", true), audio(2, "eng", false)}, ""},
		{"zxx", []Stream{audio(1, "zxx", true), audio(2, "eng", false)}, ""},
		{"unparseable", []Stream{audio(1, "???", true), audio(2, "eng", false)}, ""},
		{"a video and a subtitle with languages and flags change nothing", []Stream{
			{Index: 0, Type: "Video", Language: "jpn", IsDefault: true},
			audio(1, "deu", false), audio(2, "eng", false),
			{Index: 3, Type: "Subtitle", Language: "fra", IsDefault: true},
		}, "deu"},
		{"a subtitle flagged default before the audio", []Stream{
			{Index: 0, Type: "Subtitle", Language: "fra", IsDefault: true}, audio(1, "eng", false)}, "eng"},
		{"an external subtitle in the original language is not audio", []Stream{
			audio(1, "", false), {Index: 2, Type: "Subtitle", Language: "eng", IsExternal: true}}, ""},
	} {
		if got := (Source{Streams: c.streams}).OriginalLanguage(); got != c.want {
			t.Errorf("%s: OriginalLanguage = %q, want %q", c.name, got, c.want)
		}
	}
}
