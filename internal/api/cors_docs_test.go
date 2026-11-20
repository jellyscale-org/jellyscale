package api

import (
	"regexp"
	"strings"
	"testing"
)

// JS-144 (D-016): the words the documents need. A reader can only check the words; these hold that each passage names
// its pieces, whatever the sentence around them.

func TestDomainsHoldsThatAViewersTokenNeverTravelsInACookieAndNoRouteSetsOne(t *testing.T) {
	var invariant string
	for _, line := range strings.Split(read(t, "../../DOMAINS.md"), "\n") {
		lower := strings.ToLower(line)
		if regexp.MustCompile(`^\d+\. `).MatchString(line) && strings.Contains(lower, "cookie") && strings.Contains(lower, "token") {
			invariant = lower
		}
	}
	if invariant == "" {
		t.Fatal("DOMAINS.md has no numbered invariant about a token and a cookie")
	}
	for _, word := range []string{"never", "set-cookie", "no route", "testnoroutesetsacookieandacookieisnotoken", "js-039", "js-122"} {
		if !strings.Contains(invariant, word) {
			t.Errorf("the invariant does not say %q:\n%s", word, invariant)
		}
	}
}

func TestAccessDocSaysAPreflightIsOpenAndWhyAndWhatThatExposes(t *testing.T) {
	text := read(t, "../../docs/access.md")
	lower := strings.ToLower(text)
	for _, word := range []string{"preflight", "any origin", "no token", "cookie", "private network", "sign-in", "/healthz", "/readyz", "/metrics", "not opened"} {
		if !strings.Contains(lower, word) {
			t.Errorf("docs/access.md never says %q", word)
		}
	}
	// "Every other method and path gets the catch-all's 404" is made true again: an OPTIONS preflight of a route is not one.
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), "catch-all") && strings.Contains(strings.ToLower(line), "every other method") &&
			!strings.Contains(strings.ToLower(line), "preflight") && !strings.Contains(strings.ToLower(line), "options") {
			t.Errorf("docs/access.md still says %q, which a preflight answers 2xx", line)
		}
	}
}

// The Client coverage table has an Octave row that says what Octave has done and neither names CORS as the gap nor
// calls Octave working or playing. That the row names a record of a real Octave against this build is not checked
// here: the record can only be made after Build ends, and a check that a named file exists passes on a file of one word.
func TestStatusHasAnOctaveRowThatNamesTheRecordAndDoesNotCallItWorking(t *testing.T) {
	text := read(t, "../../docs/status.md")
	section := text[strings.Index(text, "## Client coverage"):]
	if end := strings.Index(section[3:], "\n## "); end > 0 {
		section = section[:end+3]
	}
	var row string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "|") && strings.Contains(line, "Octave") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("docs/status.md's Client coverage has no Octave row")
	}
	lower := strings.ToLower(row)
	for _, word := range []string{"signed in", "library"} {
		if !strings.Contains(lower, word) {
			t.Errorf("the Octave row does not say %q: %s", word, row)
		}
	}
	for _, word := range []string{"cors", "working", "playing", "works", "plays"} {
		if strings.Contains(lower, word) {
			t.Errorf("the Octave row says %q: %s", word, row)
		}
	}
}
