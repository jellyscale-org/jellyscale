package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-168 (D-046): the docs say the new rule (a helper takes the cores of any
// worker that has them, and the server keeps no room for a viewer who has not
// asked) and no longer the old. The behaviour is checked by
// helpers_room_integration_test.go; these read the words, in any wording, for
// the statements the criteria name.

func TestDomains58NoLongerKeepsRoomForAViewerWhoHasNotAsked(t *testing.T) {
	text := flat(invariant(t, "58"))
	for _, old := range []string{"only while room stays for a viewer who has not asked", "some worker must still have a job of its cost free after it"} {
		if strings.Contains(text, old) {
			t.Errorf("DOMAINS 58 still says %q", old)
		}
	}
	if !regexp.MustCompile(`never (goes|takes)[^.]*over its cores|never put[^.]*over its cores`).MatchString(text) {
		t.Error("DOMAINS 58 no longer says a helper never goes where it would put a worker over its cores")
	}
	if !regexp.MustCompile(`no room (is )?(kept|held)|keeps no room|holds no room`).MatchString(text) {
		t.Error("DOMAINS 58 never says that no room is kept for a viewer who has not asked")
	}
	if !strings.Contains(text, "advisory lock") {
		t.Error("DOMAINS 58 lost the advisory lock that serializes the decision to take cores for a helper")
	}
}

func TestTranscodingDocNoLongerSaysAHelperLeavesRoomElsewhere(t *testing.T) {
	text := doc(t, "docs/transcoding.md")
	if strings.Contains(text, "still leave that much free elsewhere") {
		t.Error("docs/transcoding.md still says a helper must leave the job's price free elsewhere")
	}
	flatText := flat(text)
	if strings.Contains(flatText, "js-168 drops the room rule") {
		t.Error("docs/transcoding.md still speaks of JS-168 as a plan")
	}
	if !strings.Contains(text, "JS-168") {
		t.Error("docs/transcoding.md has no record of JS-168")
	}
	// The advice to run with the dearest film's cores to spare stays (line 25).
	if !strings.Contains(flatText, "cores of the dearest film to spare") {
		t.Error("docs/transcoding.md lost the advice to run with the dearest film's cores to spare")
	}
	// The long measurements of the task are taken after the build, and the doc says which are not yet.
	notYet := regexp.MustCompile(`not yet taken|not yet measured|yet to be (taken|measured)`)
	said := false
	for _, line := range strings.Split(flatText, "\n") {
		said = said || (strings.Contains(line, "js-168") && notYet.MatchString(line))
	}
	if !said {
		t.Error("docs/transcoding.md does not say which of JS-168's long measurements are not yet taken")
	}
}

func TestRunningDocGivesTheAdviceAndSaysTheServerNoLongerHoldsRoom(t *testing.T) {
	text := flat(doc(t, "docs/running.md"))
	if !regexp.MustCompile(`no longer (holds|keeps)[^.]*room|keeps no room|holds no room|no room (is )?(held|kept)`).MatchString(text) {
		t.Error("docs/running.md does not say the server no longer holds room back for a viewer who has not asked")
	}
	if !regexp.MustCompile(`cores (of|for) the dearest film to spare|cores to spare`).MatchString(text) {
		t.Error("docs/running.md does not give the advice to run with the dearest film's cores to spare")
	}
	if !strings.Contains(text, "helpers never keep a viewer waiting") {
		t.Error("docs/running.md lost 'Helpers never keep a viewer waiting'")
	}
}

func TestStatusDocRecordsTheNewRuleInTheParallelTranscodingAndGapLines(t *testing.T) {
	var parallel, gap string
	for _, line := range strings.Split(doc(t, "docs/status.md"), "\n") {
		switch {
		case strings.HasPrefix(line, "- Parallel transcoding:"):
			parallel = flat(line)
		case strings.HasPrefix(line, "- A gap: on a worker as slow as the dogfood's"):
			gap = flat(line)
		}
	}
	if parallel == "" || gap == "" {
		t.Fatalf("docs/status.md lost the parallel transcoding line (%v) or the gap line (%v)", parallel != "", gap != "")
	}
	if !strings.Contains(parallel, "js-168") || !regexp.MustCompile(`no room (is )?(kept|held)|keeps no room|holds no room|no longer (keeps|holds)`).MatchString(parallel) {
		t.Error("the parallel transcoding line does not say JS-168 keeps no room back for a viewer who has not asked")
	}
	if regexp.MustCompile(`and no room is held back for a viewer who has not asked \(js-168\); js-166`).MatchString(gap) {
		t.Error("the gap line still lists JS-168 as work to come")
	}
}
