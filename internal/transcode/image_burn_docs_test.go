package transcode

import (
	"os"
	"strings"
	"testing"
)

// JS-071 removes a known departure, and its docs must say so and say what the
// replacement costs. These check the statements the task's criteria name, not
// the prose around them; the measurements themselves are for the reader.

func doc(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// invariant is the paragraph of DOMAINS.md that begins "n. ".
func invariant(t *testing.T, n string) string {
	t.Helper()
	for _, line := range strings.Split(doc(t, "DOMAINS.md"), "\n") {
		if strings.HasPrefix(line, n+". ") {
			return line
		}
	}
	t.Fatalf("DOMAINS.md has no invariant %s", n)
	return ""
}

func TestTheDocsNoLongerHoldTheMissedCueDeparture(t *testing.T) {
	for _, tc := range []struct{ file, text, gone string }{
		{"DOMAINS.md", invariant(t, "51"), "Known departure"},
		{"DOMAINS.md", invariant(t, "51"), "is not drawn by that run until the next cue"},
		{"DOMAINS.md", invariant(t, "22"), "except where a PGS cue began before a restart"},
		{"DOMAINS.md", invariant(t, "58"), "gets no helpers"},
		{"docs/status.md", doc(t, "docs/status.md"), "misses that cue until the next"},
		{"docs/status.md", doc(t, "docs/status.md"), "not for a burnt image subtitle"},
		{"ARCHITECTURE.md", doc(t, "ARCHITECTURE.md"), "A burnt-in PGS cue that began before a transcode job's restart point is not drawn"},
	} {
		if strings.Contains(tc.text, tc.gone) {
			t.Errorf("%s still says %q", tc.file, tc.gone)
		}
	}
}

// What can still miss a showing cue is MPEG-TS (and a Matroska file with no
// cue points for the track), with its bound, in the invariant, in
// ARCHITECTURE's Known departures and in the status; and the invariant states
// the cost of learning the times per film and per run, and what a run reads by
// the showing cue's age.
func TestTheDocsStateWhatCanStillMissACueAndWhatKnowingCostsAndAgree(t *testing.T) {
	for _, tc := range []struct{ file, text string }{
		{"DOMAINS.md invariant 51", invariant(t, "51")},
		{"ARCHITECTURE.md", doc(t, "ARCHITECTURE.md")},
		{"docs/status.md", doc(t, "docs/status.md")},
	} {
		for _, want := range []string{"MPEG-TS", "look-back"} {
			if !strings.Contains(tc.text, want) {
				t.Errorf("%s does not mention %q, the film that can still miss a showing cue and its bound", tc.file, want)
			}
		}
	}
	for _, want := range []string{"per film", "per run", "age"} {
		if !strings.Contains(invariant(t, "51"), want) {
			t.Errorf("DOMAINS.md invariant 51 does not state %q: where cue times come from, what that read costs per film and per run, and what a run reads by the cue's age", want)
		}
	}
}

// docs/transcoding.md records a seek's bytes and time to first segment through
// a worker, for origin/main and for the build, in the cases the task names.
func TestTheTranscodingDocRecordsWhatASeekCosts(t *testing.T) {
	text := strings.ToLower(doc(t, "docs/transcoding.md"))
	for _, want := range []string{"burnt image subtitle", "bytes", "time to first segment", "origin/main", "no cue showing", "8 s old", "50 s old", "first mid-film run"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/transcoding.md does not record the seek measurement: no %q", want)
		}
	}
}
