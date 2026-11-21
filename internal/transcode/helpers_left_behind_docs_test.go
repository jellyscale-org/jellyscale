package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-167 (D-046): the docs say what bounds a helper and when it is stopped.
// The behaviour is checked by helpers_left_behind_integration_test.go; these
// read the words. They look for the three cases the criteria name, in any
// wording, and for the task's id where the docs record that it was built.

var (
	seekedForward = regexp.MustCompile(`seek(s|ed|ing)? (forward|beyond|past|ahead)|forward seek`)
	seekedBack    = regexp.MustCompile(`seek(s|ed|ing)? back|backward`)
	playedPast    = regexp.MustCompile(`past its chunk`)
)

func wantsTheThreeCases(t *testing.T, where, text string) {
	t.Helper()
	text = flat(text)
	for what, re := range map[string]*regexp.Regexp{
		"a forward seek beyond the helper": seekedForward,
		"a far backward seek":              seekedBack,
		"a viewer playing past its chunk":  playedPast,
	} {
		if !re.MatchString(text) {
			t.Errorf("%s never says a helper is bounded or stopped by %s", where, what)
		}
	}
}

func TestDomains58SaysWhatBoundsAHelperAndWhenItIsStopped(t *testing.T) {
	text := invariant(t, "58")
	wantsTheThreeCases(t, "DOMAINS 58", text)
	if !regexp.MustCompile(`helper[^.]*(is |are )?(stopped|ended|cancelled)|(stops|ends|cancels) (a|the) helper`).MatchString(flat(text)) {
		t.Error("DOMAINS 58 never says when a helper is stopped")
	}
}

func TestDomains57AgreesThatAHelperReadsTheViewersWhileItRuns(t *testing.T) {
	text := flat(invariant(t, "57"))
	if !strings.Contains(text, "helper") {
		t.Error("DOMAINS 57 says when a job reads the viewers' rows and that a paused job keeps its cores without saying that a helper reads them while it runs and gives its cores back when no viewer is left for it")
	}
}

func TestTranscodingDocSaysWhatBoundsAHelperAndWhenItIsStopped(t *testing.T) {
	text := doc(t, "docs/transcoding.md")
	wantsTheThreeCases(t, "docs/transcoding.md", text)
	if !strings.Contains(text, "JS-167") {
		t.Error("docs/transcoding.md has no record of JS-167's measurements and build")
	}
}

func TestRunningDocNamesTheNewReasonOfTheEndedJobsCounter(t *testing.T) {
	var row string
	for _, line := range strings.Split(doc(t, "docs/running.md"), "\n") {
		if strings.Contains(line, "jellyscale_transcode_jobs_ended_total") {
			row = line
		}
	}
	if row == "" {
		t.Fatal("docs/running.md has no row for jellyscale_transcode_jobs_ended_total")
	}
	cells := strings.Split(row, "|")
	if len(cells) < 4 {
		t.Fatalf("the counter's row has no description cell: %q", row)
	}
	words := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z][a-z_]*)`").FindAllStringSubmatch(cells[3], -1) {
		words[m[1]] = true
	}
	for _, old := range []string{"done", "failed", "interrupted"} {
		if !words[old] {
			t.Errorf("the counter's row no longer lists `%s`", old)
		}
	}
	if len(words) < 4 {
		t.Errorf("the counter's row lists %d values of result, want the three it had and the reason of a helper ended because no viewer was left for it", len(words))
	}
}

func TestStatusLineOfParallelTranscodingSaysWhatJS167Built(t *testing.T) {
	for _, line := range strings.Split(doc(t, "docs/status.md"), "\n") {
		if strings.HasPrefix(line, "- Parallel transcoding") {
			if !strings.Contains(line, "JS-167") {
				t.Error("docs/status.md's parallel transcoding line does not mention JS-167")
			}
			return
		}
	}
	t.Fatal("docs/status.md has no parallel transcoding line")
}
