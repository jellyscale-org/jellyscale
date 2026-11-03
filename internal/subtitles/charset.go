package subtitles

import (
	"bytes"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding/ianaindex"
	xunicode "golang.org/x/text/encoding/unicode"
	xlanguage "golang.org/x/text/language"
)

// tieMargin is how many points of chardet's 1 to 100 confidence a result may
// trail the best by and still be preferred for matching the language in the
// file's name. Where the best is further ahead the name is not consulted: a
// file named .zh that is plainly Shift_JIS is Shift_JIS.
const tieMargin = 10

// legacyText converts text that is not UTF-8 to UTF-8. chardet (ICU's detector)
// ranks the character sets the bytes could be in, and the first of them that
// decodes the bytes is used, unless one within tieMargin of it that decodes
// too is in the language the file's name gives (ISO 639-3 or a tag, or empty).
// ok is false when no ranked set decodes the bytes. Control bytes do not make
// a file unreadable; ffmpeg copes with them as it does in UTF-8.
//
// This is a guess, and chardet is wrong for some files; the remedy is to
// convert the file to UTF-8 (README: Subtitle files and fonts beside the film).
func legacyText(data []byte, language string) (text []byte, ok bool) {
	ranked, err := chardet.NewTextDetector().DetectAll(data)
	if err != nil {
		return nil, false
	}
	// DetectAll orders equal confidences by which goroutine finished first. The
	// Windows set goes first because it holds the ISO one's letters and more.
	slices.SortStableFunc(ranked, func(a, b chardet.Result) int {
		if a.Confidence != b.Confidence {
			return b.Confidence - a.Confidence
		}
		aWin, bWin := strings.HasPrefix(a.Charset, "windows-"), strings.HasPrefix(b.Charset, "windows-")
		if aWin != bWin {
			if aWin {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Charset, b.Charset)
	})
	want := languageCode(language)
	var first []byte
	var top int
	for _, r := range ranked {
		decoded, ok := decodeAs(r.Charset, data)
		if !ok {
			continue
		}
		if first == nil {
			first, top = decoded, r.Confidence
			if want == "" || r.Language == want {
				break
			}
			continue
		}
		if top-r.Confidence > tieMargin {
			break
		}
		if r.Language == want {
			return decoded, true
		}
	}
	return first, first != nil
}

// mostlyUTF8 reads text that is UTF-8 but for a stray byte or a last character
// cut short, which fails utf8.Valid and which chardet then ranks anywhere
// (a single Latin-1 byte in Japanese UTF-8 makes it say Shift_JIS). The bad
// bytes are dropped. ok is false unless there is non-ASCII text and bad bytes
// are at most a tenth of it: a Latin-1 file reads as bad bytes alone.
func mostlyUTF8(data []byte) (text []byte, ok bool) {
	bad, good := 0, 0
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		i += size
		switch {
		case r != utf8.RuneError || size > 1:
			text = utf8.AppendRune(text, r)
			if r >= 0x80 {
				good++
			}
		case !utf8.FullRune(data[i-1:]):
			// Cut short at the end of the file.
		default:
			bad++
		}
	}
	return text, good > 0 && bad*10 <= good
}

// languageCode is the two letter code chardet gives a language, from the name's
// ISO 639-3 code or tag ("jpn", "zh-tw"), or empty.
func languageCode(language string) string {
	tag, err := xlanguage.Parse(language)
	if err != nil {
		return ""
	}
	base, _ := tag.Base()
	return base.String()
}

// decodeAs reads data in the character set chardet names. ok is false when the
// set is unknown, is a Unicode one (UTF-16 that is real has been converted
// already, so this would be a misreading), or has a byte that is no character.
// A last character cut short is dropped, as ffmpeg would see a cut UTF-8 file.
func decodeAs(name string, data []byte) ([]byte, bool) {
	if strings.HasPrefix(name, "UTF-") {
		return nil, false
	}
	enc, err := ianaindex.IANA.Encoding(strings.Replace(name, "GB-18030", "GB18030", 1))
	if err != nil || enc == nil {
		return nil, false
	}
	text, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return nil, false
	}
	if bytes.HasSuffix(text, []byte("\uFFFD")) {
		text = text[:len(text)-len("\uFFFD")]
	}
	return text, utf8.Valid(text) && !bytes.ContainsRune(text, utf8.RuneError)
}

// utf8Text converts data to UTF-8 where ffmpeg can't be trusted to: UTF-16 with
// a byte order mark, or without one when the file holds the ASCII that every
// subtitle format has ("-->" or "Dialogue:") spread over two bytes each, as it
// is whatever language the text is in. ok is false for anything else.
func utf8Text(data []byte) (text []byte, ok bool) {
	order := xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM)
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
	case hasMarker(data, true):
	case hasMarker(data, false):
		order = xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM)
	default:
		return nil, false
	}
	text, err := order.NewDecoder().Bytes(data)
	return text, err == nil && utf8.Valid(text) && !bytes.ContainsRune(text, utf8.RuneError)
}

// hasMarker says whether data holds "-->" or "Dialogue:" in UTF-16, little
// endian or big.
func hasMarker(data []byte, littleEndian bool) bool {
	for _, marker := range []string{"-->", "Dialogue:"} {
		var wide []byte
		for _, b := range []byte(marker) {
			if littleEndian {
				wide = append(wide, b, 0)
			} else {
				wide = append(wide, 0, b)
			}
		}
		// The text starts at an even offset; the other order's bytes can read as
		// this one's marker one byte along.
		for i := 0; i < len(data); i += 2 {
			if bytes.HasPrefix(data[i:], wide) {
				return true
			}
		}
	}
	return false
}
