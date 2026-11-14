package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-066's docs: the soak command and the replay of a seed are in docs/testing.md,
// with what each does and does not do, and nothing that runs before a merge sets
// the soak.

func TestTestingMdGivesTheSoakAndTheReplayOfASeedAndWhatEachIsFor(t *testing.T) {
	text := doc(t, "docs/testing.md")
	for _, name := range []string{"JELLYSCALE_TEST_FAULT_SEED", "JELLYSCALE_TEST_FAULT_SOAK"} {
		if !strings.Contains(text, name) {
			t.Errorf("docs/testing.md never names %s", name)
		}
	}
	if !lineMatching(text, `JELLYSCALE_TEST_FAULT_SOAK=\d+`, `go test|integration\.sh`) {
		t.Error("docs/testing.md has no line that gives the soak command: JELLYSCALE_TEST_FAULT_SOAK=<repetitions> with the command that runs the test")
	}
	if !lineMatching(text, `JELLYSCALE_TEST_FAULT_SEED=\d+`, `go test|integration\.sh`) {
		t.Error("docs/testing.md has no line that gives the replay of a seed: JELLYSCALE_TEST_FAULT_SEED=<seed> with the command that runs the test")
	}
	if !sentenceMatching(text, `JELLYSCALE_TEST_FAULT_SEED`, `replay|same steps`) {
		t.Error("docs/testing.md has no sentence that says JELLYSCALE_TEST_FAULT_SEED replays a seed")
	}
	if !sentenceMatching(text, `interleav`, `not (replayed|repeated|reproduced)|(is|are) not`) && !sentenceMatching(text, `not`, `interleav`) {
		t.Error("docs/testing.md does not say that the interleaving of the processes is not replayed")
	}
	if !sentenceMatching(text, `soak`, `flak`) {
		t.Error("docs/testing.md does not say the soak is for hunting flakes")
	}
	if !sentenceMatching(text, `soak`, `not a step before (a )?merg|never (a )?step before (a )?merg|not (run )?before (a )?merg|is not part of (the )?merg`) {
		t.Error("docs/testing.md does not say the soak is not a step before merging")
	}
}

func lineMatching(text string, patterns ...string) bool {
	for _, line := range strings.Split(text, "\n") {
		all := true
		for _, p := range patterns {
			if !regexp.MustCompile(`(?i)` + p).MatchString(line) {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// Neither test/integration.sh, CI nor the build workflow sets the soak: a normal run is a fixed number of
// schedules, and the soak is for hunting flakes by hand.
func TestNothingThatRunsBeforeAMergeSetsTheFaultSoak(t *testing.T) {
	for _, file := range []string{"test/integration.sh", "test/lib.sh", ".github/workflows/ci.yml", ".claude/workflows/build.js"} {
		if strings.Contains(doc(t, file), "JELLYSCALE_TEST_FAULT_SOAK") {
			t.Errorf("%s mentions JELLYSCALE_TEST_FAULT_SOAK: the soak is for hunting flakes by hand, and not a step before merging", file)
		}
	}
}
