//go:build integration

package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// JS-145 (D-023): the `sign-in refused` line says what its address is. When the per-address bound attributed an address
// to the request, the line names it under `address` (TestEachRefusalIsLoggedWithItsBoundAndNeverWithAnUnknownNameOrAPassword).
// When none was attributed (`trustedProxies` absent, or a declared proxy with no usable forwarded entry) the line names the
// peer under `peer`, has no `address` field, and carries the X-Forwarded-For header as received under `forwarded_unverified`:
// quoted, cut to 256 bytes, control characters escaped, and absent when no header came. That value is the peer's word and
// never counts toward a bound, decides a refusal or labels a metric.

// refusedByName makes wrong passwords for a real account, each from a new device, until the bound per name refuses one, and
// returns the stored name. No bound per address is on in the sites these checks use, so the name bound is what refuses.
func refusedByName(t *testing.T, s *signInSite, prefix string, headers ...string) string {
	t.Helper()
	name := s.account(prefix)
	for i := 1; i <= 40; i++ { // Bounds a build that never refuses; the bound is a few attempts.
		got := s.wrong(0, name, headers...)
		if got.status == 403 {
			return name
		}
		if got.status != 401 {
			t.Fatalf("wrong password %d for %q: %d %q, want 401 or a refusal", i, name, got.status, got.body)
		}
	}
	t.Fatalf("40 wrong passwords for %q were never refused by the bound per name", name)
	return ""
}

// lineOf is the one `sign-in refused` line that names user. The process writes it before answering, but its output
// reaches p.log through a copying goroutine, so the line may arrive just after the answer; the wait is a hang bound.
func lineOf(t *testing.T, p *proc, user string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		for _, line := range logLines(p, "sign-in refused") {
			if u, _ := field(line, "user"); u == user {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no `sign-in refused` line names user=%s:\n%s", user, p.log.String())
		}
	}
}

// quoted is the value of a key whose value the log wrote quoted, as the program that wrote it meant it (unquoted once; a
// build that quotes by itself and lets the logger quote again is unquoted twice).
func quoted(line, key string) (string, bool) {
	m := regexp.MustCompile(`(?:^| )` + key + `=("(?:[^"\\]|\\.)*"|\S*)`).FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	if v, err := strconv.Unquote(m[1]); err == nil {
		return v, true
	}
	return m[1], true
}

func unquotedTwice(sent, got string) bool {
	if got == sent {
		return true
	}
	again, err := strconv.Unquote(got)
	return err == nil && again == sent
}

var unattributedSites = []struct {
	name  string
	extra map[string]any
}{
	{"no trustedProxies", nil},
	{"a declared proxy with no usable entry", map[string]any{"trustedProxies": []string{"127.0.0.1"}}},
}

// Fails today: the line names the peer under `address` and never carries the header.
func TestARefusalWithNoAttributedAddressNamesThePeerAndTheForwardedHeaderAsReceived(t *testing.T) {
	long := strings.Repeat("a", 256) + strings.Repeat("b", 44)
	// Each header but the plain address is unusable in the declared-proxy site, and none is read in the other.
	headers := []struct {
		name string
		sent string // "" is no header
	}{
		{"no header", ""},
		{"an address", "198.18.7.7"},
		{"not an address", "not-an-address"},
		{"the proxy itself", "127.0.0.1"},
		{"300 bytes", long},
		{"a tab, a quote and a backslash", "198.18.7.8,\tq\"x\\y"},
	}
	for _, site := range unattributedSites {
		t.Run(site.name, func(t *testing.T) {
			s := newSignInSite(t, 1, site.extra)
			p := s.apis[0]
			for _, h := range headers {
				if site.extra != nil && h.sent == "198.18.7.7" {
					continue // A declared proxy attributes it: the line names it under address (the other check).
				}
				var via []string
				if h.sent != "" {
					via = []string{"X-Forwarded-For", h.sent}
				}
				name := refusedByName(t, s, "line", via...)
				line := lineOf(t, p, name)
				if got, _ := field(line, "peer"); got != "127.0.0.1" {
					t.Errorf("%s: the line names peer=%q, want the peer's address 127.0.0.1: %s", h.name, got, line)
				}
				if got, has := field(line, "address"); has {
					t.Errorf("%s: the line has address=%q though none was attributed to the request: %s", h.name, got, line)
				}
				got, has := quoted(line, "forwarded_unverified")
				switch {
				case h.sent == "" && has:
					t.Errorf("%s: no header came and the line has forwarded_unverified=%q: %s", h.name, got, line)
				case h.sent == "" && !has:
				case !has:
					t.Errorf("%s: the line has no forwarded_unverified field: %s", h.name, line)
				case h.sent == long:
					if strings.Count(got, "a") != 256 || strings.Contains(got, "b") {
						t.Errorf("%s: forwarded_unverified holds %d a's and %d b's, want the first 256 bytes of the header (256 a's, no b)", h.name, strings.Count(got, "a"), strings.Count(got, "b"))
					}
				case !unquotedTwice(h.sent, got):
					t.Errorf("%s: forwarded_unverified=%q, want the header as received, %q", h.name, got, h.sent)
				}
				if strings.ContainsAny(line, "\t\r\x1b") {
					t.Errorf("%s: the line holds a raw control character, which must be escaped: %q", h.name, line)
				}
			}
		})
	}
}

// What the header says is the peer's word: with no proxy declared, 12 failures of invented names each with another
// forwarded address, and 12 more with one address, refuse nothing per address, count nothing under any address of the
// header, and the header is in no metric series. Passes today by construction; it holds the line's new field to being only
// a log field.
func TestTheForwardedHeaderOfAnUnattributedRequestNeverCountsRefusesOrLabelsAMetric(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	marker := "marker-" + uuid.NewString()[:8]
	failures, refusals := s.total(failuresMetric), s.total(addrRefusals)
	var sent []string
	for i := 0; i < 12; i++ {
		v := randomV4()
		sent = append(sent, v)
		if got := s.wrong(0, invented(), "X-Forwarded-For", v); got.status != 401 {
			t.Fatalf("wrong password %d with a forwarded address each of its own: %d %q, want 401", i+1, got.status, got.body)
		}
	}
	same := randomV4()
	sent = append(sent, same, marker)
	for i := 0; i < 12; i++ {
		if got := s.wrong(0, invented(), "X-Forwarded-For", same); got.status != 401 {
			t.Fatalf("wrong password %d with one forwarded address: %d %q, want 401", i+1, got.status, got.body)
		}
	}
	refusedByName(t, s, "label", "X-Forwarded-For", marker) // A line that carries the marker exists.
	if got := s.total(failuresMetric) - failures; got < 24 {
		t.Errorf("the failed sign-in metric rose by %v, want at least 24", got)
	}
	if got := s.total(addrRefusals) - refusals; got != 0 {
		t.Errorf("the address refusals metric rose by %v, want 0: a forwarded header decides nothing here", got)
	}
	dump := s.dump()
	for _, v := range sent {
		if strings.Contains(dump, v) {
			t.Errorf("the signin_* tables hold %s, which only a forwarded header named:\n%s", v, dump)
		}
		for series := range s.scrape(s.apis[0]) {
			if strings.Contains(series, v) {
				t.Errorf("a metric series carries the forwarded value %s: %s", v, series)
			}
		}
	}
}
