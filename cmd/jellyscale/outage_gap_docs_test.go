package main

import (
	"regexp"
	"strings"
	"testing"
)

// JS-103. JS-085 stated a gap of 5 seconds for a film just started, because a snapshot is refreshed and not written
// through. The docs now state the gap that remains, under 1 second, and what closing it costs per start, in place of
// the 5 seconds, and what a window of minutes costs at 1000 sessions: what each refresh reads, what a replica holds,
// and who it now admits (a tab closed without a Stopped stays served for the window). DOMAINS 37 and docs/running.md
// say it, each in its own words, so each is read for the same things.
func outageText(t *testing.T) map[string]string {
	t.Helper()
	text := map[string]string{}
	for path, start := range map[string]string{"../../DOMAINS.md": "37. ", "../../docs/running.md": "A Postgres switchover"} {
		for _, line := range strings.Split(read(t, path), "\n") {
			if strings.HasPrefix(line, start) {
				text[path] += line + "\n"
			}
		}
		if text[path] == "" {
			t.Fatalf("%s has no %q paragraph", path, start)
		}
	}
	return text
}

func TestDocsSayWhatRemainsOfTheGapForAFilmJustStarted(t *testing.T) {
	for path, doc := range outageText(t) {
		if strings.Contains(doc, "in the last 5 seconds before the outage") || regexp.MustCompile(`(?i)not in a snapshot yet|no snapshot yet`).MatchString(doc) {
			t.Errorf("%s still says a session begun in the 5 seconds before an outage is in no snapshot and waits", path)
		}
		for what, pattern := range map[string]string{
			"the gap that remains, under 1 second, for a film just started": `(?is)(started|begun|begins|began|start of)[^.]{0,200}\b(under|less than|within|in the last) 1 second|\b(under|less than|within|in the last) 1 second[^.]{0,200}(started|begun|begins|began|start of)`,
			"what closing it costs per start":                               `(?is)\bper start\b`,
		} {
			if !regexp.MustCompile(pattern).MatchString(doc) {
				t.Errorf("%s does not say %s", path, what)
			}
		}
	}
}

func TestDocsSayWhatAWindowOfMinutesCostsAt1000Sessions(t *testing.T) {
	size := `\d[\d.,]* ?(KB|kB|MB|GB|bytes)`
	for path, doc := range outageText(t) {
		for what, pattern := range map[string]string{
			"what each refresh reads at 1000 sessions in the window, in bytes": `(?is)1000 sessions.{0,400}(refresh|reads).{0,300}` + size + `|(refresh|reads).{0,300}1000 sessions.{0,300}` + size + `|(refresh|reads).{0,300}` + size + `.{0,300}1000 sessions`,
			"what a replica holds at 1000 sessions in the window, in bytes":    `(?is)\b(holds|keeps)\s+(about |roughly |up to |some |around |at most )?` + size + `.{0,200}1000 sessions|1000 sessions.{0,300}\b(holds|keeps)\s+(about |roughly |up to |some |around |at most )?` + size,
			"that a tab closed without a Stopped stays served for the window":  `(?is)(tab|browser|player)[^.]{0,80}closed[^.]{0,80}without (a )?Stopped[^.]{0,300}(stays|remains|is still|are still|served)[^.]{0,200}window`,
		} {
			if !regexp.MustCompile(pattern).MatchString(doc) {
				t.Errorf("%s does not say %s", path, what)
			}
		}
	}
}
