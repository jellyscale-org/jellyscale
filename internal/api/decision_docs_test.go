package api

import (
	"regexp"
	"strings"
	"testing"
)

// JS-153: docs/running.md tells an operator how to read the decision line and the three counters. The list of reasons is
// test/reasonsdocs's (it needs the code's list); these are the words around it.

func running(t *testing.T) string { return read(t, "../../docs/running.md") }

func TestRunningMdsMetricsTableListsTheThreeCountersWithTheirLabelsAndSaysWhatTheyCount(t *testing.T) {
	text := running(t)
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "| `jellyscale_playbackinfo_") {
			rows = append(rows, line)
		}
	}
	want := []string{"`jellyscale_playbackinfo_decisions_total{method}`", "`jellyscale_playbackinfo_refusals_total{reason}`", "`jellyscale_playbackinfo_decision_reasons_total{method,reason}`"}
	if len(rows) != len(want) {
		t.Fatalf("the metrics table has %d playbackinfo rows, want %d:\n%s", len(rows), len(want), strings.Join(rows, "\n"))
	}
	joined := strings.ToLower(strings.Join(rows, "\n"))
	for _, name := range want {
		if !strings.Contains(strings.Join(rows, "\n"), name) {
			t.Errorf("the metrics table has no row for %s", name)
		}
	}
	for _, phrase := range []string{"answers", "not plays", "exceed", "api"} {
		if !strings.Contains(joined, phrase) {
			t.Errorf("the rows of the three counters never say %q (they count PlaybackInfo answers, not plays; the sum of reasons may exceed the decisions)", phrase)
		}
	}
}

func TestRunningMdsShowsHowToReadTheLineAndTheCounters(t *testing.T) {
	text := running(t)
	lower := strings.ToLower(text)
	for _, c := range []struct{ what, pattern string }{
		{"the line's message", `playback decision`},
		{"finding a play session's decision by its id", `play_session=`},
		{"finding the decisions of an item by its id", `item=`},
		{"getting from an item id as the line prints it to the film", `(?s)from items.{0,120}where id\s*=`},
		{"a query of re-encodes by reason summed over the replicas", `(?s)sum\s*(by\s*\([^)]*reason[^)]*\))?\s*\(\s*(increase|rate)?\(?jellyscale_playbackinfo_decision_reasons_total\{[^}]*method="reencode"`},
	} {
		if !regexp.MustCompile(c.pattern).MatchString(lower) {
			t.Errorf("docs/running.md does not show %s (pattern %s)", c.what, c.pattern)
		}
	}
}
