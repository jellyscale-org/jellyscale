package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// JS-030: what the docs say about the liveness URL, the one reaper, the leases of
// subtitle rows and the way back across the release. A phrase is looked for
// where the criterion names the document, in text with its white space and case
// folded; the checks fail on today's docs, which have none of it.

func paragraphOf(t *testing.T, file, from string) string {
	t.Helper()
	text := doc(t, file)
	i := strings.Index(text, from)
	if i < 0 {
		t.Fatalf("%s has no %q", file, from)
	}
	rest := text[i:]
	if j := strings.Index(rest[len(from):], "\n"); j >= 0 {
		rest = rest[:len(from)+j]
	}
	return flat(rest)
}

func docSection(t *testing.T, file, heading string) string {
	t.Helper()
	text := doc(t, file)
	i := strings.Index(text, heading+"\n")
	if i < 0 {
		t.Fatalf("%s has no heading %q", file, heading)
	}
	rest := text[i+len(heading):]
	if j := regexp.MustCompile(`(?m)^## `).FindStringIndex(rest); j != nil {
		rest = rest[:j[0]]
	}
	return flat(rest)
}

func archRule(t *testing.T, n string) string {
	t.Helper()
	for _, line := range strings.Split(doc(t, "ARCHITECTURE.md"), "\n") {
		if strings.HasPrefix(line, n+". ") {
			return flat(line)
		}
	}
	t.Fatalf("ARCHITECTURE.md has no rule %s", n)
	return ""
}

func wantAll(t *testing.T, where, text string, patterns map[string]string) {
	t.Helper()
	for what, pattern := range patterns {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("%s does not say %s (looked for /%s/)", where, what, pattern)
		}
	}
}

const livenessFlag = "--liveness-database-url"

func TestTheDocsNameTheLivenessURLAndWhatItAndTheMainURLMayBe(t *testing.T) {
	connections := regexp.MustCompile(`connections`)
	for _, d := range []struct{ where, text string }{
		{"docs/running.md (Configuration)", flat(doc(t, "docs/running.md"))},
		{"docs/transcoding.md (Connections)", paragraphOf(t, "docs/transcoding.md", "Connections:")},
		{"ARCHITECTURE.md (roles table)", paragraphOf(t, "ARCHITECTURE.md", "| `worker` |")},
		{"ARCHITECTURE.md (control-plane ceiling)", flat(doc(t, "ARCHITECTURE.md"))},
	} {
		if !strings.Contains(d.text, livenessFlag) {
			t.Errorf("%s never names %s", d.where, livenessFlag)
		}
		if !connections.MatchString(d.text) {
			t.Errorf("%s says nothing of connections", d.where)
		}
	}
	for _, d := range []struct{ where, text string }{
		{"docs/running.md", flat(doc(t, "docs/running.md"))},
		{"docs/transcoding.md (Connections)", paragraphOf(t, "docs/transcoding.md", "Connections:")},
	} {
		wantAll(t, d.where, d.text, map[string]string{
			"the liveness URL must be a direct, session-mode connection":    `liveness.{0,80}(direct, session-mode|direct and session-mode|session-mode and direct|direct, session mode)|(direct, session-mode|direct and session-mode).{0,120}liveness`,
			"the main URL may sit behind a pooler in session mode at least": `main (database )?(url|connection).{0,200}session.mode`,
			"what breaks behind a mode the main URL may not use":            `(transaction.mode|statement.mode).{0,400}(break|fail|stop|lose|lost|wrong|not work|cannot|can't|never)`,
			"what N workers cost Postgres in direct connections (2N)":       `(2n|2 ?[x×*] ?n|two (direct )?connections (for|per|a|each|of every) (each )?worker).{0,200}direct|direct.{0,200}(2n|2 ?[x×*] ?n)`,
		})
	}
	// Observed on PgBouncer 1.26.1 in transaction mode: the lock stays held on the pooler's server
	// connection after the worker is gone, so a killed worker is never seen as dead and its jobs wait their lease.
	wantAll(t, "docs/running.md (a transaction-mode pooler on the liveness URL)", flat(doc(t, "docs/running.md")), map[string]string{
		"that a killed worker behind a transaction-mode liveness URL is not noticed and its jobs wait their lease": `liveness.{0,600}transaction.mode.{0,600}(lease)|transaction.mode.{0,600}liveness.{0,600}(lease)`,
	})
	wantAll(t, "docs/running.md (the max_connections arithmetic)", flat(doc(t, "docs/running.md")), map[string]string{
		"the liveness connections in the max_connections arithmetic": `max_connections.{0,600}(2n|2 ?[x×*] ?n|liveness)|(2n|2 ?[x×*] ?n|liveness).{0,600}max_connections`,
	})
}

func TestTheDocsSayThereIsOneReaperHowItIsChosenAndReplacedAndWhatItCostsAt50And400Workers(t *testing.T) {
	for _, d := range []struct{ where, text string }{
		{"ARCHITECTURE.md rule 4", archRule(t, "4")},
		{"ARCHITECTURE.md rule 6", archRule(t, "6")},
		{"DOMAINS.md 25", flat(invariant(t, "25"))},
		{"docs/transcoding.md", flat(doc(t, "docs/transcoding.md"))},
	} {
		wantAll(t, d.where, d.text, map[string]string{
			"that there is one reaper":          `(one|a single|single) reaper`,
			"how the reaper is chosen":          `reaper.{0,300}(chosen|elected|picked|selected|takes the role|holds the role)|(chosen|elected|picked|selected).{0,200}reaper`,
			"that a killed reaper is replaced":  `reaper.{0,300}(killed|dies|dead|stopped|slow|lost its lock|lock is lost).{0,300}(replace|takes? (over|the role)|another)`,
			"what reaping costs at 50 workers":  `50 workers`,
			"what reaping costs at 400 workers": `400 workers`,
		})
	}
}

func TestTheDocsStateTheNewBoundOfASubtitleRowOfAKilledWorkerAndTheFallback(t *testing.T) {
	for _, d := range []struct{ where, text string }{
		{"DOMAINS.md 25", flat(invariant(t, "25"))},
		{"DOMAINS.md 43", flat(invariant(t, "43"))},
		{"docs/transcoding.md", flat(doc(t, "docs/transcoding.md"))},
	} {
		wantAll(t, d.where, d.text, map[string]string{
			"that a forgotten worker's subtitle rows are cut with its row": `(subtitle|extraction|chapter).{0,300}(forgotten|forgets|forgot|cut).{0,300}(half a second|forgottenlease)|(half a second|forgottenlease).{0,300}(subtitle|extraction|chapter)`,
		})
	}
	wantAll(t, "docs/transcoding.md", flat(doc(t, "docs/transcoding.md")), map[string]string{
		"the fallback when no connection closes (the lease)": `(subtitle|extraction|chapter).{0,400}(no connection|closes? no|host that loses power|partition).{0,300}lease|lease.{0,300}(no connection|closes? no).{0,300}(subtitle|extraction|chapter)`,
	})
	for _, old := range []struct{ where, text, phrase string }{
		{"docs/transcoding.md", flat(doc(t, "docs/transcoding.md")), "subtitle extractions and chapter pictures keep their 10 second lease, whoever dies"},
		{"DOMAINS.md 25", flat(invariant(t, "25")), "subtitle and chapter-picture jobs keep their 10 second lease"},
	} {
		if strings.Contains(old.text, old.phrase) {
			t.Errorf("%s still says %q, which is no longer so for a worker that is killed", old.where, old.phrase)
		}
	}
}

func TestTheRollingUpdateNotesHowToRollBackAcrossTheLivenessChangesAndWhatAMixedFleetDoes(t *testing.T) {
	text := docSection(t, "docs/running.md", "## Rolling update")
	wantAll(t, "docs/running.md (Rolling update)", text, map[string]string{
		"the flag by name": regexp.QuoteMeta(livenessFlag),
		"that the release before stops at start on the flag":                                            `(release before|previous release|older (release|worker)|old worker).{0,300}(stops?|refuses|exits?|fails?).{0,200}(start|at start)|(stops?|refuses|exits?).{0,200}--liveness-database-url`,
		"to remove the flag before such a worker starts":                                                `(remove|drop|take out|unset).{0,120}(flag|--liveness-database-url).{0,300}(before|until)`,
		"that the main URL is made direct and session-mode again":                                       `(direct|session).{0,200}(again|back)|(again|back).{0,200}(direct|session)`,
		"an order for workers and replicas, up and back, or none":                                       `(roll|order|first|last).{0,300}(workers?).{0,300}(replicas?)|(no order|none is needed|order does not matter|in any order)`,
		"that old and new workers each still look for the dead":                                         `(workers? of the (release|version) before|old workers?).{0,300}(look|reap|forget).{0,200}(dead|beside)`,
		"that no live worker of either release is forgotten":                                            `no live worker.{0,200}(forgotten|forgets)|(never|not) forgotten.{0,200}live`,
		"that a subtitle row of a worker an old worker forgot waits out its lease":                      `subtitle.{0,300}(old worker|release before|older).{0,300}(waits? out|full lease|lease runs out|lease as before)|(old worker|release before|older).{0,300}subtitle.{0,300}(waits? out|full lease|lease)`,
		"that a worker from before the liveness lock is never forgotten and is taken over at its lease": `(before the liveness lock|took no lock|no lock).{0,300}(never forgotten|not forgotten|is never forgotten).{0,300}lease|(never forgotten|not forgotten).{0,300}(before the liveness lock|took no lock).{0,300}lease`,
	})
}
