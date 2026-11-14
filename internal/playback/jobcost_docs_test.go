package playback

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// JS-087's docs: the statements the criteria name, not the prose around them.

func repoFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func numbered(t *testing.T, n string) string {
	t.Helper()
	for _, line := range strings.Split(repoFile(t, "DOMAINS.md"), "\n") {
		if strings.HasPrefix(line, n+". ") {
			return line
		}
	}
	t.Fatalf("DOMAINS.md has no invariant %s", n)
	return ""
}

var slotWord = regexp.MustCompile(`(?i)\bslots?\b`)

// The measured table is a table: a row per kind of source, with the figure the
// code uses, and the copy's figure is the one Spec.Cost returns.
func TestTheDocsHoldTheCostTable(t *testing.T) {
	text := repoFile(t, "docs/transcoding.md")
	for _, row := range []struct{ source, figure string }{
		{`1080.*SDR`, "0.8"}, {`4K.*SDR`, "1.6"}, {`1080.*(HDR|tone)`, "2.0"}, {`4K.*(HDR|tone)`, "3.2"},
	} {
		if !regexp.MustCompile(`(?mi)^\|[^\n]*` + row.source + `[^\n]*\|[^\n]*\b` + regexp.QuoteMeta(row.figure) + `\b`).MatchString(text) {
			t.Errorf("docs/transcoding.md has no table row of a %s source costing %s cores", row.source, row.figure)
		}
	}
	copied := fmt.Sprint((Spec{Copy: true}).Cost())
	if !regexp.MustCompile(`(?mi)^\|[^\n]*cop(y|ies)[^\n]*\b` + regexp.QuoteMeta(copied) + `\b`).MatchString(text) {
		t.Errorf("docs/transcoding.md has no table row of a copy costing %s cores, the figure Spec.Cost gives", copied)
	}
}

func TestTheDocsSayWhatAWorkerDeclaresAndWhatRefusesAViewer(t *testing.T) {
	text := repoFile(t, "docs/transcoding.md")
	if !regexp.MustCompile("(?m)^\\| `transcodeCores` \\|").MatchString(text) {
		t.Error("docs/transcoding.md has no `transcodeCores` row in its table of keys")
	}
	for _, gone := range []string{"Paused jobs hold slots", "all N transcode slots are busy", "capacity is the workers times"} {
		if strings.Contains(text, gone) {
			t.Errorf("docs/transcoding.md still says %q", gone)
		}
	}
	if !regexp.MustCompile(`(?is)cannot be played at this time[^.]*\bcores\b|\bcores\b[^.]*cannot be played at this time`).MatchString(text) {
		t.Error("docs/transcoding.md does not give cores short as a reason for the dialog")
	}
}

func TestRunningDocsNameTheMetricsInCores(t *testing.T) {
	text := repoFile(t, "docs/running.md")
	for _, name := range []string{"jellyscale_transcode_cores", "jellyscale_transcode_cores_busy", "jellyscale_transcode_queue_cores", "jellyscale_transcode_queue_depth"} {
		if !strings.Contains(text, "`"+name+"`") {
			t.Errorf("docs/running.md does not name %s", name)
		}
	}
	for _, gone := range []string{"jellyscale_transcode_slots"} {
		if strings.Contains(text, gone) {
			t.Errorf("docs/running.md still names %s", gone)
		}
	}
}

func TestTheInvariantsAndRulesSpeakOfCoresNotSlots(t *testing.T) {
	for _, n := range []string{"37", "53", "57", "58", "59"} {
		if slotWord.MatchString(numbered(t, n)) {
			t.Errorf("DOMAINS %s still speaks of slots", n)
		}
	}
	if !regexp.MustCompile(`(?i)\bcores?\b`).MatchString(numbered(t, "37")) {
		t.Error("DOMAINS 37 does not say what refuses a transcode now")
	}
	architecture := repoFile(t, "ARCHITECTURE.md")
	for _, line := range strings.Split(architecture, "\n") {
		switch {
		case strings.HasPrefix(line, "3. "):
			if slotWord.MatchString(line) || !regexp.MustCompile(`(?i)\bcores\b`).MatchString(line) {
				t.Errorf("ARCHITECTURE rule 3 does not speak of cores: %s", line)
			}
		case strings.HasPrefix(line, "| `worker` |"):
			if slotWord.MatchString(line) || !regexp.MustCompile(`(?i)\bcores\b`).MatchString(line) {
				t.Errorf("ARCHITECTURE's worker row does not scale on cores: %s", line)
			}
		}
	}
}
