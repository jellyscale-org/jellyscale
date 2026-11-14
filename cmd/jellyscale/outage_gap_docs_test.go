package main

import (
	"strings"
	"testing"
)

// JS-085. A snapshot is refreshed, not written through, so a session first seen
// inside the last refresh interval before an outage is in none: the docs say so
// rather than promise every session seen within the window.
func TestDocsSayASessionStartedJustBeforeAnOutageWaits(t *testing.T) {
	for path, doc := range map[string]string{"../../DOMAINS.md": "37. ", "../../docs/running.md": "A Postgres switchover"} {
		found := false
		for _, line := range strings.Split(read(t, path), "\n") {
			if strings.HasPrefix(line, doc) && strings.Contains(line, "in the last 5 seconds before the outage") && strings.Contains(line, "no snapshot yet") || strings.HasPrefix(line, doc) && strings.Contains(line, "not in a snapshot yet") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not say that a session begun in the last refresh interval before an outage waits for Postgres", path)
		}
	}
}
