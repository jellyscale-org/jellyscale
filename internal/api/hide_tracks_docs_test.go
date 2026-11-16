package api

import (
	"os"
	"strings"
	"testing"
)

func read(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// passageAfter is the text around the first mention of the phrase, lower case, for a check of the words a passage needs.
func passageAfter(t *testing.T, file, text, phrase string) string {
	t.Helper()
	text = strings.ToLower(text)
	at := strings.Index(text, phrase)
	if at < 0 {
		t.Errorf("%s does not mention %q", file, phrase)
		return ""
	}
	return text[max(0, at-300):min(len(text), at+1500)]
}

// JS-119: DOMAINS.md holds the rule as an invariant: what is kept, a placeholder at the track's own Index, per viewer,
// lists and extraction untouched, every index still accepted. A reader can only check the words; this holds that the
// passage names the pieces, whatever the sentence around them.
func TestDomainsHoldsTheRuleForHidingTracksAsAnInvariant(t *testing.T) {
	passage := passageAfter(t, "DOMAINS.md", read(t, "../../DOMAINS.md"), "placeholder")
	for _, word := range []string{"data", "own index", "per viewer", "original language", "no language", "the track the request names", "lists", "extract", "every index", "audio language"} {
		if !strings.Contains(passage, word) {
			t.Errorf("DOMAINS.md: the invariant on placeholders does not say %q:\n%s", word, passage)
		}
	}
}

// docs/subtitles.md and docs/status.md describe it, with the limit that language families other than Norwegian and
// Chinese compare as different languages and its remedy, the keep list or "show all tracks".
func TestDocsDescribeHidingTracksAndItsLanguageFamilyLimit(t *testing.T) {
	for _, file := range []string{"../../docs/subtitles.md", "../../docs/status.md"} {
		passage := passageAfter(t, file, read(t, file), "placeholder")
		for _, word := range []string{"norwegian", "chinese", "keep", "show all tracks", "language famil"} {
			if !strings.Contains(passage, word) {
				t.Errorf("%s: the passage on hiding tracks does not say %q:\n%s", file, word, passage)
			}
		}
	}
}

// compat/differences.yaml records that an opted-in viewer's answers differ from Jellyfin's, under the requests of the
// differential tier that decode them.
func TestDifferencesRecordThatAnOptedInViewersAnswersDiffer(t *testing.T) {
	text := read(t, "../../compat/differences.yaml")
	for _, request := range []string{
		"playback info for Babel Film, for a viewer who hides the tracks outside their languages",
		"Babel Film's details, for a viewer who hides the tracks outside their languages",
	} {
		at := strings.Index(text, request)
		if at < 0 {
			t.Errorf("compat/differences.yaml does not list %q", request)
			continue
		}
		entry := strings.ToLower(text[at:min(len(text), at+1500)])
		if !strings.Contains(entry, "kind: improvement") || !strings.Contains(entry, "data") || !strings.Contains(entry, "opted") {
			t.Errorf("the entry for %q is not an improvement that says an opted-in viewer is listed Type Data placeholders:\n%s", request, entry)
		}
	}
}
