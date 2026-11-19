package transcode

import (
	"strings"
	"testing"
)

// JS-089's docs: DOMAINS 51, ARCHITECTURE's Known departures, docs/status.md and
// docs/running.md agree on what the stored track costs and when a cue's age still
// costs film reads, and docs/transcoding.md records what the spike found. Like
// JS-071's docs checks these name the statements the criteria name, not the
// prose around them; the numbers are for the reader.

// The cost of the stored track, per film and per run, and when a cue's age
// still costs reads of the film, in the invariant, in ARCHITECTURE and in the status.
func TestTheDocsStateWhatTheStoredTrackCostsAndWhenACuesAgeStillCostsReads(t *testing.T) {
	for _, tc := range []struct {
		file, text string
		want       []string
	}{
		{"DOMAINS.md invariant 51", invariant(t, "51"), []string{"stored track", "one read of the whole film", "asset store", "one job row", "per film", "per run", "until the track is stored", "cannot be extracted", "rollout"}},
		{"ARCHITECTURE.md", doc(t, "ARCHITECTURE.md"), []string{"stored track", "until the track is stored", "MPEG-TS", "look-back"}},
		{"docs/status.md", doc(t, "docs/status.md"), []string{"stored track", "until the track is stored", "MPEG-TS", "look-back"}},
	} {
		lower := strings.ToLower(tc.text)
		for _, want := range tc.want {
			if !strings.Contains(lower, strings.ToLower(want)) {
				t.Errorf("%s does not say %q", tc.file, want)
			}
		}
	}
}

// What the asset sweep removes and leaves alone includes the stored track.
func TestTheRunningDocSaysTheSweepRemovesTheStoredImageTrack(t *testing.T) {
	text := doc(t, "docs/running.md")
	var removes, leaves string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "- Assets, in each library's asset store"):
			removes = line
		case strings.HasPrefix(line, "- The asset sweep leaves alone what something is writing"):
			leaves = line
		}
	}
	if !strings.Contains(removes, "image track") {
		t.Errorf("docs/running.md's list of what the asset sweep removes does not name the stored image track: %q", removes)
	}
	if !strings.Contains(leaves, "extraction") {
		t.Errorf("docs/running.md does not say the sweep leaves a film's stored track alone while its extraction is queued or running: %q", leaves)
	}
}

// docs/transcoding.md records the spike's finding about the two inputs'
// alignment on ffmpeg 6.1 and 9, and a seek's bytes read and time to first
// segment in the cases the task names, for origin/main, this build before the
// track is stored and after.
func TestTheTranscodingDocRecordsTheAlignmentOfTheTwoInputsAndWhatASeekCostsWithTheTrackStored(t *testing.T) {
	text := strings.ToLower(doc(t, "docs/transcoding.md"))
	for _, want := range []string{"second input", "ffmpeg 6.1", "ffmpeg 9", "aligned", "no cue showing", "8 s old", "50 s old", "time to first segment",
		"origin/main", "before the track is stored", "after the track is stored"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/transcoding.md does not record the spike or the seek measurement: no %q", want)
		}
	}
}
