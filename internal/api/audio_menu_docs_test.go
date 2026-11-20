package api

import (
	"strings"
	"testing"
)

// JS-147 (D-018): DOMAINS.md invariant 32, docs/status.md and docs/subtitles.md say what the details page's Audio menu
// lists (every audio track the viewer has not hidden, as Jellyfin 12.4 does: compat/captures/audio-menu-12.4.md) and cite
// the decision. Where the menu does not list every track (the states of criterion 4) waits for the product owner's answer
// and has no check here.
func invariant32(t *testing.T) string {
	t.Helper()
	text := read(t, "../../DOMAINS.md")
	at := strings.Index(text, "\n32. ")
	end := strings.Index(text[at+1:], "\n33. ")
	if at < 0 || end < 0 {
		t.Fatal("DOMAINS.md has no invariant 32")
	}
	return strings.ToLower(text[at : at+1+end])
}

func TestDomainsInvariant32SaysTheAudioMenuListsEveryTrackAndWhereItDoesNot(t *testing.T) {
	passage := invariant32(t)
	for _, word := range []string{"every audio", "d-018", "audio menu"} {
		if !strings.Contains(passage, word) {
			t.Errorf("invariant 32 does not say %q", word)
		}
	}
	for _, old := range []string{"one audio stream (the file's default", "lists the default audio track alone"} {
		if strings.Contains(passage, old) {
			t.Errorf("invariant 32 still says %q: the item lists every audio track", old)
		}
	}
}

func TestStatusAndSubtitlesDocsSayWhatTheAudioMenuListsAndWhereItDoesNot(t *testing.T) {
	for _, file := range []string{"../../docs/status.md", "../../docs/subtitles.md"} {
		passage := passageAfter(t, file, read(t, file), "audio menu")
		for _, word := range []string{"every audio", "d-018"} {
			if !strings.Contains(passage, word) {
				t.Errorf("%s: the passage on the details page's audio menu does not say %q:\n%s", file, word, passage)
			}
		}
	}
}

// sentences splits a document into its sentences, lower case, on a full stop and a space or the end of a line.
func sentences(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ToLower(text), "\n") {
		for _, s := range strings.Split(line, ". ") {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// JS-147 (D-029): docs/transcoding.md, where transcodeStore is described (the paragraph that starts "Add `transcodeStore`"),
// tells the operator that without it a browser's audio menu offers one track, and cites the decision.
func TestTranscodingDocsSayWithoutATranscodeStoreTheAudioMenuOffersOneTrack(t *testing.T) {
	text := read(t, "../../docs/transcoding.md")
	at := strings.Index(text, "Add `transcodeStore`")
	if at < 0 {
		t.Fatal("docs/transcoding.md no longer starts a paragraph with \"Add `transcodeStore`\": the check names where transcodeStore is described")
	}
	end := strings.Index(text[at:], "\n")
	paragraph := strings.ToLower(text[at : at+end])
	for _, word := range []string{"audio menu", "one track", "d-029"} {
		if !strings.Contains(paragraph, word) {
			t.Errorf("the paragraph describing transcodeStore does not say %q: without it a browser's audio menu offers one track (D-029)", word)
		}
	}
}

// JS-147 (D-029): a pick of an audio track that is neither the file's default nor the viewer's saved language is played as
// that track or ends in the "cannot be played at this time" dialog; it never plays on as the default. docs/transcoding.md
// says so, and no sentence about a pick says the default track plays on, except for the saved language.
func TestTranscodingDocsDoNotSayAPickedTrackFallsBackToTheDefault(t *testing.T) {
	var picks []string
	for _, s := range sentences(read(t, "../../docs/transcoding.md")) {
		if !strings.Contains(s, "audio track") && !strings.Contains(s, "default track") {
			continue
		}
		if strings.Contains(s, "pick") || strings.Contains(s, "names the track") {
			picks = append(picks, s)
			if (strings.Contains(s, "keeps playing") || strings.Contains(s, "plays on")) && strings.Contains(s, "default") && !strings.Contains(s, "saved") {
				t.Errorf("a sentence says a picked track falls back to the default: %q", s)
			}
		}
	}
	for _, s := range picks {
		if strings.Contains(s, "dialog") || strings.Contains(s, "cannot be played") || strings.Contains(s, "refus") {
			return
		}
	}
	t.Errorf("no sentence of docs/transcoding.md says what a pick of an audio track that cannot be delivered ends in (the dialog); sentences about a pick: %q", picks)
}
