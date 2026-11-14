package transcode

import (
	"regexp"
	"strings"
	"testing"
)

// DOMAINS 25 holds the table of worker-liveness timings and the states of the
// state machine, the same as the code, and is shorter than the 811 words it had,
// with every guarantee it stated kept.

// invariant25 is the text of invariant 25 in DOMAINS.md.
func invariant25(t *testing.T) string {
	t.Helper()
	text := doc(t, "DOMAINS.md")
	start := regexp.MustCompile(`(?m)^25\. `).FindStringIndex(text)
	if start == nil {
		t.Fatal("DOMAINS.md has no invariant 25")
	}
	rest := text[start[0]:]
	if end := regexp.MustCompile(`(?m)^26\. `).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return rest
}

func TestInvariant25IsShorterThanItWas(t *testing.T) {
	if n := len(strings.Fields(invariant25(t))); n >= 811 {
		t.Errorf("DOMAINS 25 is %d words, want fewer than 811", n)
	}
}

// Each row of the code's table is a line of DOMAINS 25 with the row's name, its
// derivation from the lease and its value at 10 s; and the doc has no row the
// code lacks. A row, derivation or value changed on one side only fails here.
func TestInvariant25HoldsTheCodesTimingTableRowForRow(t *testing.T) {
	text := invariant25(t)
	var tableLines []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			tableLines = append(tableLines, line)
		}
	}
	for _, row := range livenessTable {
		value := row.Of(defaultLease).String()
		found := false
		for _, line := range tableLines {
			if strings.Contains(line, row.Name) && strings.Contains(line, row.Derivation) && regexp.MustCompile(`(^|[^0-9.])`+regexp.QuoteMeta(value)+`([^0-9a-z]|$)`).MatchString(line) {
				found = true
			}
		}
		if !found {
			t.Errorf("DOMAINS 25 has no table line with %q, %q and %s", row.Name, row.Derivation, value)
		}
	}
	// Header and separator lines are the two besides the rows.
	if want := len(livenessTable) + 2; len(tableLines) != want {
		t.Errorf("DOMAINS 25 has %d table lines, want %d (a header, a separator and a row for each of the %d timings)", len(tableLines), want, len(livenessTable))
	}
}

// The states and every transition, named as in the code, are in DOMAINS 25: a
// line with the state it leaves, the event and the state it enters.
func TestInvariant25HasTheStatesAndTheirTransitions(t *testing.T) {
	text := invariant25(t)
	for _, s := range allLiveStates {
		if s == stRegistering || s == stStopped || !strings.Contains(text, s.String()) {
			if s == stRegistering || s == stStopped {
				continue
			}
			t.Errorf("DOMAINS 25 never names the state %q", s.String())
		}
	}
	for _, tr := range livenessTransitions {
		found := false
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, tr.From.String()) && strings.Contains(line, tr.Event.String()) && strings.Contains(line, tr.To.String()) {
				found = true
			}
		}
		if !found {
			t.Errorf("DOMAINS 25 has no line with %q, the event %q and %q", tr.From, tr.Event, tr.To)
		}
	}
}

// Every guarantee invariant 25 states today is still stated.
func TestInvariant25StillStatesEveryGuarantee(t *testing.T) {
	text := strings.ToLower(strings.Join(strings.Fields(invariant25(t)), " "))
	for name, pattern := range map[string]string{
		"every job has a kill path":                                  `every job has a kill path`,
		"no ffmpeg outlives its lease":                               `no ffmpeg outlives its lease`,
		"losing the lock is not losing the lease":                    `losing the (liveness )?lock is not (yet )?losing the lease`,
		"the three conditions for forgetting":                        `(all three|three conditions)`,
		"its own lock in the same snapshot":                          `own lock.{0,80}(same|snapshot)|(same|snapshot).{0,80}own lock`,
		"its own lock not ended within the hold":                     `own lock.{0,60}(not ended|has not ended|ended)`,
		"a replica forgets nobody":                                   `replica forgets nobody`,
		"a worker without its row claims nothing":                    `without its row.{0,40}claims.{0,40}renews.{0,40}stores?`,
		"one row per lock":                                           `(one row per lock|inserted once per lock)`,
		"no replacement starts while the old ffmpeg runs":            `no replacement starts while`,
		"the bound on a slow database":                               `(database|postgres).{0,80}(answer|bound|slow).{0,200}(look|quarter|bound)`,
		"killed during an outage, forgotten up to the hold later":    `killed (during|in) an outage.{0,80}forgotten`,
		"a job ended because Postgres was away is cancelled":         `cancelled.{0,40}never .?failed`,
		"a worker that only lost its lock lets an extraction finish": `only lost its lock.{0,80}extraction`,
	} {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("DOMAINS 25 no longer states: %s", name)
		}
	}
}
