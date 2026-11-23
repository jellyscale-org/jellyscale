package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-158: docs/testing.md carries the record of the deadlocks the tests met, so a reader sees which statements met,
// which locks they held and waited for, and whether the product can meet the same. Record, not machine-checked for
// truth (D-042): these checks pin that it names what the task asks it to name, each with its source.

func js158Record(t *testing.T) string {
	t.Helper()
	var section []string
	in := false
	for _, line := range strings.Split(doc(t, "docs/testing.md"), "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, "## JS-158")
		}
		if in {
			section = append(section, line)
		}
	}
	if len(section) == 0 {
		t.Fatal("docs/testing.md has no \"## JS-158\" section holding the record of the deadlocks")
	}
	return strings.Join(section, "\n")
}

func TestTheJS158RecordNamesTheQueueRunsAndQuotesTheLogLineItRelaysOn(t *testing.T) {
	text := js158Record(t)
	for _, want := range []string{
		"45041377896", "#249", "playback_integration_test.go:55",
		"45049599358", "#250", "image_store_extract_integration_test.go:371",
		"ERROR: deadlock detected (SQLSTATE 40P01)",
		// The two runs the task named as of 2026-11-20 are #223's and #239's: one shows a TRUNCATE deadlock and one does not.
		"44766363425", "44855263307",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the JS-158 record never mentions %q", want)
		}
	}
}

func TestTheJS158RecordNamesTheTwoStatementsThatDeadlockedAndTheLocksTheyHeldAndWaitedFor(t *testing.T) {
	text := js158Record(t)
	for _, want := range []string{
		"rig_integration_test.go:", "image_burn_kit_integration_test.go:", "TRUNCATE libraries, users CASCADE",
		"transcode_jobs, transcode_viewers, transcode_nodes", "CASCADE",
		"ACCESS EXCLUSIVE", "ROW EXCLUSIVE",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the JS-158 record never mentions %q: it names the kit's TRUNCATE (file:line, its tables, CASCADE) and the lock each statement held and waited for", want)
		}
	}
	writers := 0
	for _, writer := range []string{"recordSegment", "job end", "lease renewal", "sweep", "heartbeat"} {
		if strings.Contains(text, writer) {
			writers++
		}
	}
	if writers == 0 {
		t.Error("the JS-158 record never says which writer of the worker deadlocked (segment record, job end, lease renewal, sweep or heartbeat)")
	}
	if !regexp.MustCompile(`(?i)(blocked by process|log line|forced check)`).MatchString(text) {
		t.Error("the JS-158 record gives no source for the locks: a cited log line or the forced check's Postgres error")
	}
}

func TestTheJS158RecordSaysForEachRunWhetherItIsTheSameCauseAndNamesTheEarlierTest(t *testing.T) {
	text := js158Record(t)
	if got := len(regexp.MustCompile(`(?i)(same|different|unknown) cause`).FindAllString(text, -1)); got < 2 {
		t.Errorf("the JS-158 record says %d times whether a run is the same cause, want one for each of 45041377896 and 45049599358", got)
	}
	if !strings.Contains(text, "BL-178") {
		t.Error("the JS-158 record never says whether BL-178 is closed or stays open")
	}
}

func TestTheJS158RecordListsEveryProductStatementThatWritesWhatALiveWorkerWritesAndWhatAViewerWouldHaveSeen(t *testing.T) {
	text := flat(js158Record(t))
	// The snapshot read (refreshSnapshot's refreshFilms in internal/api/snapshot.go) is a statement of a stream replica
	// that locks media_sources, items and libraries; "no worker" is how the :55 runs can meet it.
	for _, want := range []string{"dead-node reap", "sweep", "grace", "scan", "migration", "refreshfilms", "no worker", "40p01"} { // flat lowercases the record.
		if !strings.Contains(text, want) {
			t.Errorf("the JS-158 record never lists the product statement %q among those that truncate, delete from or lock a table a live worker writes", want)
		}
	}
	for _, want := range []string{"can deadlock", "cannot deadlock", "viewer", "product owner", "brief"} {
		if !strings.Contains(text, want) {
			t.Errorf("the JS-158 record never says %q: each statement is judged, what a viewer would have seen is told, and a brief goes to the product owner", want)
		}
	}
}
