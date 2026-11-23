package transcode

import (
	"strings"
	"testing"
)

// JS-159: DOMAINS invariant 40 says that the guarantee holds on every path that stores a segment, and docs/testing.md's
// JS-155 row for the sighting this task fixes carries the verdict and the runs, not the reading it started from.

func TestDomains40SaysTheGuaranteeHoldsOnEveryPathThatStoresASegment(t *testing.T) {
	text := flat(invariant(t, "40"))
	for _, want := range []string{"while ffmpeg runs", "at its exit", "re-encode", "copy"} {
		if !strings.Contains(text, want) {
			t.Errorf("DOMAINS 40 never says %q: the guarantee must be stated for the upload check while ffmpeg runs and for the exit, for a re-encode and for a copy", want)
		}
	}
}

func TestTheJS155RowForTheCutReadSightingCarriesItsVerdictAndRuns(t *testing.T) {
	var row string
	for _, line := range strings.Split(doc(t, "docs/testing.md"), "\n") {
		if strings.HasPrefix(line, "| TestACauseThatPersistsStartsAtMostThreeFreshJobsAndThenFails/the_source_store_cuts_a_read") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("docs/testing.md has no JS-155 row for TestACauseThatPersistsStartsAtMostThreeFreshJobsAndThenFails/the_source_store_cuts_a_read")
	}
	for _, old := range []string{"none run (D-043)", "not reproduced"} {
		if strings.Contains(row, old) {
			t.Errorf("the row still says %q: JS-159 ran it, and the row carries the verdict and the runs", old)
		}
	}
	if !strings.Contains(row, "JS-159") {
		t.Error("the row does not name JS-159")
	}
}
