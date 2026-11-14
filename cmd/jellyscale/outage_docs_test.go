package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-085. The docs say what a stream replica serves from its snapshot of active
// sessions' tokens while Postgres is away, which sessions count, how often it is
// refreshed, and what that costs. They said before that nothing is kept and that
// every request waits.
func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// DOMAINS invariant 37 states the window of recent activity as "window of N
// seconds" (30 to 120: jellyfin-web reports progress every ten seconds, paused or not, so a live player is seen at least three times in it, and the tests wait it out), the refresh interval as "refresh every N seconds" (1 to 10, and the window at
// least three refreshes and 5 seconds, which the integration tests wait on), that
// every stream replica keeps the snapshot, that direct play and the stored
// segments are what it serves from it, that a token signed out after the last
// refresh stays usable until Postgres is back, and that a failed refresh keeps
// the last snapshot only while Postgres is away; it no longer says nothing is
// kept in memory.
func TestDomainsSaysWhatIsServedFromTheSnapshotDuringAnOutage(t *testing.T) {
	var invariant string
	for _, line := range strings.Split(read(t, "../../DOMAINS.md"), "\n") {
		if strings.HasPrefix(line, "37. ") {
			invariant = line
		}
	}
	if invariant == "" {
		t.Fatal("DOMAINS.md has no invariant 37")
	}
	if strings.Contains(invariant, "Nothing is kept in memory") {
		t.Error("invariant 37 still says nothing is kept in memory")
	}
	// The numbers are read from two fixed phrases, the same in internal/api/outage_memory_integration_test.go
	// (windowPhrase, refreshPhrase): the invariant already holds other "N seconds" (the usual 90), so a pattern that
	// took the nearest number to a word would read the wrong one.
	window := regexp.MustCompile(`(?i)\bwindow of (\d{1,3}) seconds`).FindStringSubmatch(invariant)
	refresh := regexp.MustCompile(`(?i)\brefresh(?:es)?,? every (\d{1,2}) seconds`).FindStringSubmatch(invariant)
	if window == nil || refresh == nil {
		t.Fatalf("invariant 37 must say \"window of N seconds\" (30 to 120: jellyfin-web reports progress every ten seconds, paused or not, so a live player is seen at least three times in it, and the tests wait it out) and \"refresh every N seconds\" (or \"refreshes every N seconds\") (1 to 10): found %v and %v", window, refresh)
	}
	seconds, _ := strconv.Atoi(window[1])
	every, _ := strconv.Atoi(refresh[1])
	if seconds < 30 || seconds > 120 || every < 1 || every > 10 || seconds < 3*every+5 {
		t.Errorf("invariant 37 states a window of %d seconds and a refresh every %d seconds: want a window of 30 to 120 and a refresh of 1 to 10, and a window of at least three refreshes and 5 seconds", seconds, every)
	}
	for what, pattern := range map[string]string{
		"a snapshot of tokens":                                                      `(?i)snapshot`,
		"that every stream replica keeps one":                                       `(?is)(every|each|any) stream replica`,
		"that direct play is served from it":                                        `(?is)snapshot.{0,800}direct play|direct play.{0,800}snapshot`,
		"that a segment in the store is served from it":                             `(?is)snapshot.{0,800}(stored|in the store|transcode store)|(stored|in the store|transcode store).{0,800}snapshot`,
		"that a token signed out after a refresh stays usable":                      `(?is)(revoked|signed out).{0,300}(stays|remains|is still) usable|usable.{0,300}(revoked|signed out)`,
		"that it holds until Postgres is back":                                      `(?is)usable.{0,300}until Postgres is back`,
		"that it is the sessions playing or seen within the window that count":      `(?is)(playing|session).{0,300}(seen|active)`,
		"that a failed refresh keeps the last snapshot only while Postgres is away": `(?is)refresh.{0,300}(fail|cannot|can't).{0,300}(last|previous).{0,100}only while Postgres is away|only while Postgres is away.{0,300}(last|previous) snapshot`,
	} {
		if !regexp.MustCompile(pattern).MatchString(invariant) {
			t.Errorf("invariant 37 does not say %s", what)
		}
	}
}

// docs/running.md says, where it describes an outage, that direct play and the
// stored segments go on from memory, and no longer that every request waits.
func TestRunningDocSaysDirectPlayAndStoredSegmentsGoOnThroughAnOutage(t *testing.T) {
	doc := read(t, "../../docs/running.md")
	paragraph := regexp.MustCompile(`(?m)^A Postgres switchover or restart.*$`).FindString(doc)
	if paragraph == "" {
		t.Fatal("docs/running.md has no paragraph on a Postgres switchover or restart")
	}
	for what, pattern := range map[string]string{
		"the snapshot":             `(?i)snapshot`,
		"direct play":              `(?i)direct play`,
		"segments in the store":    `(?i)(stored|in the store|transcode store)`,
		"the window in seconds":    `(?i)\bwindow of \d{1,3} seconds`,
		"a signed-out token stays": `(?is)signed out.{0,300}usable|usable.{0,300}signed out`,
	} {
		if !regexp.MustCompile(pattern).MatchString(paragraph) {
			t.Errorf("the outage paragraph of docs/running.md does not mention %s", what)
		}
	}
	if regexp.MustCompile(`(?i)requests that need the database wait for it \(up to 90 seconds for a segment\) rather than`).MatchString(paragraph) &&
		!regexp.MustCompile(`(?i)except|other than|apart from|besides|snapshot`).MatchString(paragraph) {
		t.Error("the outage paragraph still says every request waits, with no exception for what is served from memory")
	}
}

// docs/status.md no longer says nothing is cached through an outage, and
// records JS-085.
func TestStatusDocNoLongerSaysNothingIsCached(t *testing.T) {
	doc := read(t, "../../docs/status.md")
	if strings.Contains(doc, "Nothing is cached") {
		t.Error("docs/status.md still says nothing is cached")
	}
	if !regexp.MustCompile(`(?m)^.*JS-085.*(snapshot|direct play).*$`).MatchString(doc) {
		t.Error("docs/status.md does not record JS-085, with what is served from the snapshot")
	}
}

// ARCHITECTURE rule 6: a design states its database cost at the target scale. Every
// stream replica reads the snapshot every few seconds for as long as it runs, so
// the page that states the design says what a refresh reads: small rows per
// playing session, a film's stream list and keyframe index only when the replica
// holds none or they changed (a two-hour film's index is tens of kilobytes to
// megabytes, and it changes only on a rescan), and what that comes to, in bytes
// with a unit, for a number of sessions. Either DOMAINS 37 or ARCHITECTURE.md may say it.
func TestTheRefreshStatesItsDatabaseCost(t *testing.T) {
	var invariant string
	for _, line := range strings.Split(read(t, "../../DOMAINS.md"), "\n") {
		if strings.HasPrefix(line, "37. ") {
			invariant = line
		}
	}
	text := invariant + "\n" + read(t, "../../ARCHITECTURE.md")
	for what, pattern := range map[string]string{
		"that a refresh reads only a film's streams and keyframes when they are missing or changed": `(?is)refresh.{0,600}(streams|keyframes).{0,300}\b(only|unless)\b.{0,200}\b(changed|changes|change|hold|held|missing|new)\b`,
		"its cost in bytes for a number of sessions":                                                `(?is)refresh.{0,500}\b\d[\d.,]* ?(KB|kB|MB|bytes)\b.{0,300}\bsessions\b|\bsessions\b.{0,300}refresh.{0,500}\b\d[\d.,]* ?(KB|kB|MB|bytes)\b`,
	} {
		if !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("neither DOMAINS invariant 37 nor ARCHITECTURE.md says %s", what)
		}
	}
}
