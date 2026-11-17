//go:build integration

package api

import "testing"

// The details page asks for the item several times a view, so a viewer with an audio language saved must not make it
// send more statements than one with none: the file's audio tracks are read in the statement that reads the sources.
// A film of two sources, on both routes of the item.
func TestTheItemSendsNoMoreStatementsWithAnAudioLanguageSaved(t *testing.T) {
	f := audioEnv(t)
	logs := traceReplicas(t, f.env)
	german := f.listener(t, "german", "deu", false)
	plain := f.listener(t, "plain", "", true)
	film, _ := f.film(t, "Two versions", []track{eng(true), deu(false)}, []track{deu(false), eng(true)})
	for _, alias := range []bool{false, true} {
		count := func(v viewer) []string {
			f.item(t, 0, v, film, alias) // Warm, so a first read is not counted.
			logs[0].take()
			f.item(t, 0, v, film, alias)
			return logs[0].take()
		}
		saved, base := count(german), count(plain)
		if len(saved) > len(base) {
			t.Errorf("alias %v: %d statements with an audio language saved, %d without:\n%s\nagainst\n%s", alias, len(saved), len(base), listing(saved), listing(base))
		}
	}
}

// A film whose duration the probe could not read can't be copied or transcoded, so it plays directly: whatever the
// viewer prefers or names, DefaultAudioStreamIndex names the track that plays, the file's own, with no job.
func TestAFilmWithNoProbedDurationNamesTheTrackThatPlays(t *testing.T) {
	f := audioEnv(t)
	german := f.listener(t, "german", "deu", false)
	film, ids := f.film(t, "No duration", []track{eng(true), deu(false)}, []track{deu(false), eng(true)})
	mustExec(t, f.pool, "UPDATE items SET run_time_ticks = 0 WHERE id = $1::uuid", film)
	for who, extra := range map[string]map[string]any{"saved deu": nil, "names the German track": {"AudioStreamIndex": 2}} {
		before := decided(t)
		reply := f.ask(t, 0, german, film, extra)
		for i, want := range []int{1, 2} { // Each source's flagged track, else its first.
			source := sourceNamed(t, reply, ids[i])
			if how, _ := copyOf(t, source); how != "direct" || chosen(source) != want {
				t.Errorf("%s, source %d: plays as %s with DefaultAudioStreamIndex %d, want a direct play of track %d", who, i, how, chosen(source), want)
			}
		}
		wantDecisions(t, who, before, map[string]float64{"direct_play": 2})
	}
}
