package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-082's docs. These check the statements the task's criteria name, not the
// prose around them; doc and invariant are in image_burn_docs_test.go.

// section is the part of a markdown file from its heading to the next heading
// of the same level.
func section(t *testing.T, file, heading string) string {
	t.Helper()
	body := doc(t, file)
	_, rest, ok := strings.Cut(body, "\n"+heading+"\n")
	if !ok {
		t.Fatalf("%s has no %q section", file, heading)
	}
	level := heading[:strings.Index(heading, " ")+1]
	if end := strings.Index(rest, "\n"+level); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func sentenceMatching(text string, patterns ...string) bool {
	for _, s := range regexp.MustCompile(`(?s)[^.]+\.`).FindAllString(text, -1) {
		all := true
		for _, p := range patterns {
			if !regexp.MustCompile(`(?is)` + p).MatchString(s) {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

func TestRunningMdGivesTheUpgradeAndRollbackOrderForWorkersStreamAndApi(t *testing.T) {
	text := section(t, "docs/running.md", "## Rolling update")
	if !sentenceMatching(text, `upgrad`, `worker.*stream.*\bapi\b`) {
		t.Errorf("the Rolling update section has no sentence that gives the upgrade order, workers then stream then api")
	}
	if !sentenceMatching(text, `roll(ing)? ?back`, `\bapi\b.*stream.*worker|stream.*\bapi\b.*worker|worker.*\bapi\b.*stream|worker.*stream.*\bapi\b`) {
		t.Errorf("the Rolling update section has no sentence that gives the rollback order of workers, stream and api")
	}
	if !sentenceMatching(text, `stream.*(first|before).*\bapi\b`) || !strings.Contains(text, "HDR10") {
		t.Errorf("the section lost its rule that stream is replaced before api, and why")
	}
	if !sentenceMatching(text, `every`, `version`, `worker`) {
		t.Errorf("the section does not say why that order is the one: every version a running replica queues has a running worker that makes it")
	}
}

func TestRunningMdSaysWhatViewersGetWhenTheOrderIsNotKept(t *testing.T) {
	text := section(t, "docs/running.md", "## Rolling update")
	for _, want := range []string{"503", "queueTimeout", "never"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Rolling update section does not mention %q (the 503 a viewer gets after queueTimeout, never segments made by another version)", want)
		}
	}
	if !sentenceMatching(text, `before this release`, `worker`, `check|claim`) {
		t.Errorf("the section does not say a worker built before this release makes no version check")
	}
	if !sentenceMatching(text, `stopped`, `before`, `later output version|OutputVersion`) {
		t.Errorf("the section does not say every worker built before this release must be stopped before a replica of a later output version starts")
	}
}

func TestDomainsStatesThatAReleaseThatChangesAnySpecBumpsOutputVersion(t *testing.T) {
	found := false
	for _, line := range strings.Split(doc(t, "DOMAINS.md"), "\n") {
		if strings.Contains(line, "OutputVersion") && strings.Contains(line, "spec field") && regexp.MustCompile(`(?i)bump`).MatchString(line) &&
			regexp.MustCompile(`(?i)claim`).MatchString(line) {
			found = true
		}
	}
	if !found {
		t.Error("no paragraph of DOMAINS.md states that workers claim a job by its OutputVersion and that a release whose workers make any spec differently, a new spec field included, bumps it")
	}
}

func TestArchitectureAndStatusSayWhatIsNowTrue(t *testing.T) {
	architecture, status := doc(t, "ARCHITECTURE.md"), doc(t, "docs/status.md")
	for _, tc := range []struct{ file, text string }{
		{"ARCHITECTURE.md", architecture},
		{"docs/status.md", status},
	} {
		for _, gone := range []string{"workers reporting their `OutputVersion` and the api offering only specs every live worker supports", "workers report their `OutputVersion` and the api offers only specs every live worker supports"} {
			if strings.Contains(tc.text, gone) {
				t.Errorf("%s still says %q is to come", tc.file, gone)
			}
		}
	}
	_, line, _ := strings.Cut(status, "- Upgrades without downtime")
	line, _, _ = strings.Cut(line, "\n")
	if !strings.Contains(line, "OutputVersion") || !strings.Contains(strings.ToLower(line), "claim") {
		t.Errorf("docs/status.md's upgrades line does not say that a worker claims only jobs of its OutputVersion: %q", line)
	}
	_, departure, _ := strings.Cut(architecture, "- A worker from before a release")
	departure, _, _ = strings.Cut(departure, "\n")
	if strings.Contains(departure, "belongs to the upgrades item") || !strings.Contains(departure, "OutputVersion") {
		t.Errorf("ARCHITECTURE.md's known departure on old workers is not updated to what is true now: %q", departure)
	}
}
