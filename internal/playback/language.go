package playback

import (
	"strings"

	"golang.org/x/text/language"
)

// languageKey names the language of an ISO 639 or BCP 47 tag, or "" when the tag
// names none: empty, und, mis, mul, zxx, the private-use range qaa to qtz, an
// unknown or malformed tag. The parser reads und and a malformed tag as English
// at low confidence, so only an exact base counts.
func languageKey(tag string) string {
	parsed, err := language.Parse(strings.TrimSpace(tag))
	if err != nil {
		return ""
	}
	base, confidence := parsed.Base()
	if confidence != language.Exact {
		return ""
	}
	key := base.String()
	switch {
	case key == "und" || key == "mis" || key == "mul" || key == "zxx":
		return ""
	case key >= "qaa" && key <= "qtz":
		return ""
	case key == "no" || key == "nb" || key == "nn":
		return "no" // Norwegian: a macrolanguage and its two written forms.
	case key == "zh" || key == "cmn" || key == "yue":
		return "zh"
	}
	return key
}

// SameLanguage reports whether two tags name one language, whatever their
// spelling (ger, deu, de-DE). A tag with no language equals nothing, itself
// included. Hiding fewer tracks is the safe error, so the macrolanguages Norwegian
// and Chinese are joined with their members by hand: the parser gives them
// different bases.
func SameLanguage(a, b string) bool {
	key := languageKey(a)
	return key != "" && key == languageKey(b)
}

// OriginalLanguage is the language tag, as the file wrote it, of the default
// audio track (the first flagged, else the first), or "" when the file has none
// or that track names no language. Only that one track decides.
func (s Source) OriginalLanguage() string {
	index := s.DefaultAudio()
	if audio, ok := s.audio(index); ok && languageKey(audio.Language) != "" {
		return audio.Language
	}
	return ""
}
