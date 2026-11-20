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
