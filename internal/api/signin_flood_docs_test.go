package api

import (
	"strings"
	"testing"
)

// JS-172 (D-050): the words the documents need. docs/access.md states the bound of unknown names and its slack as numbers an
// operator can work out, and docs/testing.md holds what was observed before the check was amended.

func TestAccessDocStatesTheBoundOfUnknownNamesAndItsSlackAndCitesTheDecision(t *testing.T) {
	text := read(t, "../../docs/access.md")
	var paragraph string
	for _, p := range strings.Split(text, "\n") {
		if strings.Contains(p, "100,000 unknown names") {
			paragraph = strings.ToLower(p)
		}
	}
	if paragraph == "" {
		t.Fatal("docs/access.md no longer states 100,000 unknown names")
	}
	for _, word := range []string{
		"hashed", "fewest failures", "oldest", "no account's count",
		"in flight", "databasemaxconns", "16", "all api replicas", "at most 100,000", "alone", "d-050",
	} {
		if !strings.Contains(paragraph, word) {
			t.Errorf("the passage on unknown names does not say %q (the slack: over the bound by no more than the sign-ins in flight at once on all api replicas together, each replica's databaseMaxConns, 16 by default; back to at most 100,000 with the next unknown name that fails alone):\n%s", word, paragraph)
		}
	}
}

func TestTestingDocRecordsWhatTheFloodsOfUnknownNamesWereObservedToDo(t *testing.T) {
	text := read(t, "../../docs/testing.md")
	i := strings.Index(text, "JS-172")
	if i < 0 {
		t.Fatal("docs/testing.md has no JS-172 record")
	}
	record := strings.ToLower(text[i:])
	for _, word := range []string{
		"44986442604", "signin_unknown", "signin_failures", "signin_unknown_count", "signin_device_ended",
		"-count=20", "2 cpus", "10 floods", "20,000", "16 writers", "unverified", "bl-214", "undershoot", "the build meets the rule", "d-050",
	} {
		if !strings.Contains(record, word) {
			t.Errorf("the JS-172 record in docs/testing.md does not say %q", word)
		}
	}
}
