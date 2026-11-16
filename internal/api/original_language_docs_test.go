package api

import (
	"os"
	"strings"
	"testing"
)

// JS-117: docs/status.md and DOMAINS.md say how the original language is derived: from the file
// alone, the default audio track's language (the first flagged default, else the first), none when
// that track has no language, and a film whose only audio is a dub is labelled with the dub's
// language. A reader can only check the words; this holds that each file has the passage and that
// it names the pieces, whatever the sentence around them.
func TestDocsSayHowTheOriginalLanguageIsDerived(t *testing.T) {
	for _, file := range []string{"../../docs/status.md", "../../DOMAINS.md"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(raw))
		at := strings.Index(text, "original language")
		if at < 0 {
			t.Errorf("%s does not mention the original language", file)
			continue
		}
		passage := text[max(0, at-400):min(len(text), at+1000)]
		for _, word := range []string{"default audio", "first", "flagged", "dub", "no language"} {
			if !strings.Contains(passage, word) {
				t.Errorf("%s: the passage on the original language does not say %q:\n%s", file, word, passage)
			}
		}
	}
}
