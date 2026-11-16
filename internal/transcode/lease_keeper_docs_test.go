package transcode

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// JS-105: DOMAINS 25 and docs/transcoding.md (The bounds) say that a worker
// which cannot renew stops its job the cutMargin before the lease can lapse, 9.5
// seconds after its last renewal at the defaults, and no longer that it runs its
// ffmpeg "at the latest its lease" or "until its own lease check (10 seconds after
// its last renewal)". The guarantees DOMAINS 25 stated are kept by
// TestInvariant25StillStatesEveryGuarantee, which runs beside these.

func flat(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func TestDomains25SaysAWorkerThatCannotRenewStopsTheCutMarginBeforeTheLeaseCanLapse(t *testing.T) {
	text := flat(invariant25(t))
	if !regexp.MustCompile(`the ` + "`cutmargin`" + ` before (its|the) lease can lapse`).MatchString(text) {
		t.Error("DOMAINS 25 never says that a worker which cannot renew stops its job the cutMargin before the lease can lapse")
	}
	if !regexp.MustCompile(`(^|[^0-9.])9\.5 ?(s|seconds)([^a-z]|$)`).MatchString(text) {
		t.Error("DOMAINS 25 does not give the 9.5 seconds after its last renewal (lease less cutMargin at the defaults)")
	}
	for _, old := range []string{"at the latest its lease", "its own lease check"} {
		if strings.Contains(text, old) {
			t.Errorf("DOMAINS 25 still says %q", old)
		}
	}
}

func TestTranscodingDocBoundsSayAWorkerThatCannotRenewStopsBeforeTheLeaseCanLapse(t *testing.T) {
	text := flat(doc(t, "docs/transcoding.md"))
	i := strings.Index(text, "the bounds:")
	if i < 0 {
		t.Fatal("docs/transcoding.md has no 'The bounds' paragraph")
	}
	bounds := text[i:]
	if j := strings.Index(bounds, "connections:"); j > 0 {
		bounds = bounds[:j]
	}
	if !regexp.MustCompile(`half a second before (its|the) lease can lapse`).MatchString(bounds) {
		t.Error("The bounds never say that a worker which cannot renew stops its job half a second before the lease can lapse")
	}
	if !regexp.MustCompile(`9\.5 seconds after its last renewal`).MatchString(bounds) {
		t.Error("The bounds do not say 9.5 seconds after its last renewal")
	}
	for _, old := range []string{"its own lease check (10 seconds after its last renewal)", "at the latest its lease"} {
		if strings.Contains(bounds, old) {
			t.Errorf("The bounds still say %q", old)
		}
	}
}

// JS-105: two connections per worker beside the pool, and the fence is a renewal.
// ARCHITECTURE.md (the worker's runtime needs, rule 6's liveness cost, and the
// worker count the stress table's connections imply), DOMAINS 25, docs/running.md
// and docs/transcoding.md (Connections) say that a worker holds two connections
// of its own, one for its liveness lock and one for its lease keeper, and that
// the check before a put is a renewal.

func TestTheDocsStateTwoConnectionsPerWorker(t *testing.T) {
	two := regexp.MustCompile(`two (direct |more |extra |additional )?(postgres )?connections|a second connection|one more connection for (its|the) lease|connection for (its|the) lease keeper`)
	arch := flat(doc(t, "ARCHITECTURE.md"))
	for name, text := range map[string]string{
		"ARCHITECTURE.md":     arch,
		"docs/running.md":     flat(doc(t, "docs/running.md")),
		"docs/transcoding.md": flat(doc(t, "docs/transcoding.md")),
		"DOMAINS 25":          flat(invariant25(t)),
	} {
		if !two.MatchString(text) || !strings.Contains(text, "lease keeper") {
			t.Errorf("%s does not state that a worker holds two connections of its own, one for the liveness lock and one for the lease keeper", name)
		}
	}
	// The two places of ARCHITECTURE.md that priced one: the worker's runtime needs (the roles table) and rule 6.
	for _, old := range []string{"plus one direct, session-mode connection for its liveness lock)", "liveness costs one postgres connection per worker process"} {
		if strings.Contains(arch, old) {
			t.Errorf("ARCHITECTURE.md still says %q", old)
		}
	}
	if strings.Contains(flat(doc(t, "docs/running.md")), "each worker also holds one more connection for its liveness lock") {
		t.Error("docs/running.md still says each worker holds one more connection, for its liveness lock")
	}
}

func TestTheDocsSayTheCheckBeforeAPutIsARenewal(t *testing.T) {
	fence := regexp.MustCompile(`(check|fence)[^.]{0,80}before (a|every|each) (put|store|segment)[^.]{0,120}renewal|renewal[^.]{0,120}(before|ahead of) (a|every|each) (put|store)`)
	for name, text := range map[string]string{
		"DOMAINS 25":          flat(invariant25(t)),
		"docs/transcoding.md": flat(doc(t, "docs/transcoding.md")),
		"ARCHITECTURE.md":     flat(doc(t, "ARCHITECTURE.md")),
	} {
		if !fence.MatchString(text) {
			t.Errorf("%s does not say that the check before a put is a renewal of the job's lease, sent after the ask", name)
		}
	}
}

// test/stress.sh runs the stress driver, whose fake workers hold what a real
// worker does: a liveness lock's connection and, since JS-105, a keeper's. The
// table in ARCHITECTURE.md counts connections.
func TestTheStressScriptsFakeWorkersHoldAKeepersConnectionToo(t *testing.T) {
	driver, err := os.ReadFile("stress_driver_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?i)keeper`).Match(driver) {
		t.Error("stress_driver_test.go does not open a lease keeper's connection for each fake worker: the stress table's connection counts would be one per worker too low")
	}
}
