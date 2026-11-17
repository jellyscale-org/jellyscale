package api

import (
	"strings"
	"testing"
)

// JS-121: docs/transcoding.md says what the viewer's audio language costs, and docs/status.md says it acts.

func TestTranscodingDocSaysAPreferredTrackThatIsNotTheDefaultIsACopyThatTakesAWorkerSlot(t *testing.T) {
	passage := passageAfter(t, "docs/transcoding.md", read(t, "../../docs/transcoding.md"), "audiolanguagepreference")
	for _, word := range []string{"direct play", "default track", "copied", "worker slot", "browser", "issecondaryaudio"} {
		if !strings.Contains(passage, word) {
			t.Errorf("docs/transcoding.md: the passage on the audio language does not say %q:\n%s", word, passage)
		}
	}
}

func TestStatusMarksTheAudioSettingsAsActing(t *testing.T) {
	text := read(t, "../../docs/status.md")
	passage := passageAfter(t, "docs/status.md", text, "playdefaultaudiotrack")
	for _, word := range []string{"audiolanguagepreference", "acting"} {
		if !strings.Contains(passage, word) {
			t.Errorf("docs/status.md: the passage on the playback settings does not say %q:\n%s", word, passage)
		}
	}
	if strings.Contains(strings.ToLower(text), "playdefaultaudiotrack wait for js-121") || strings.Contains(strings.ToLower(text), "wait for js-121") {
		t.Error("docs/status.md still says the audio settings wait for JS-121")
	}
}
