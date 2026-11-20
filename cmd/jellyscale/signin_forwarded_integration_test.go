//go:build integration

package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-145 (D-022, amending D-020): the address of a sign-in from a declared proxy is the rightmost X-Forwarded-For entry
// that is not a declared proxy, and no other entry. When the header is missing, every entry is a declared proxy, or that
// entry does not parse, the per-address bound is not applied to the request: it neither counts nor is refused per address,
// and the bounds per name and per known device apply. A declared proxy's own address is never counted or refused. In
// every check here the test's own address, 127.0.0.1, is the declared proxy.

// noUsableEntry is each way a request from a declared proxy can carry no entry that counts.
var noUsableEntry = []struct {
	name    string
	proxies []string
	headers func(left string) []string // left is an address the client put to the left of the entry that counts.
}{
	{"no header", []string{"127.0.0.1"}, func(string) []string { return nil }},
	{"the only entry is the proxy itself", []string{"127.0.0.1"}, func(string) []string { return []string{"X-Forwarded-For", "127.0.0.1"} }},
	{"every entry is a declared proxy", []string{"127.0.0.1", "10.0.0.0/8"}, func(string) []string { return []string{"X-Forwarded-For", "10.1.2.3, 10.4.5.6"} }},
	// The entry that counts is the rightmost that is no proxy; when it does not parse, the one left of it is not taken
	// instead (a client could have written it).
	{"the entry that counts does not parse", []string{"127.0.0.1"}, func(left string) []string { return []string{"X-Forwarded-For", left + ", not-an-address"} }},
}

// Eleven wrong passwords through a declared proxy with no usable entry, then a right password by the same path, signs in:
// the proxy's address is not counted for its clients. Fails on a build that counts the proxy's own address (today's: the
// 11th is refused by the per-address bound) or that falls back to the entry left of one that does not parse.
func TestElevenFailuresThroughADeclaredProxyWithNoUsableEntryThenTheRightPasswordSignsIn(t *testing.T) {
	for _, tc := range noUsableEntry {
		t.Run(tc.name, func(t *testing.T) {
			s := newSignInSite(t, 1, map[string]any{"trustedProxies": tc.proxies})
			left := randomV4()
			via := tc.headers(left)
			name := s.account("via")
			failures, refusals := s.total(failuresMetric), s.total(addrRefusals)
			for i := 1; i <= 11; i++ {
				if got := s.wrong(0, invented(), via...); got.status != 401 {
					t.Fatalf("wrong password %d of 11, for an invented name, through the proxy with no usable entry: %d %q, want 401", i, got.status, got.body)
				}
			}
			signedIn(t, "the right password by the same path, from a new device", s.try(0, newDevice(), name, rightPassword, via...))
			if got := s.total(addrRefusals) - refusals; got != 0 {
				t.Errorf("the address refusals metric rose by %v, want 0", got)
			}
			if got := s.total(failuresMetric) - failures; got != 11 {
				t.Errorf("the failed sign-in metric rose by %v, want 11: a failure with no usable entry is still a failed sign-in", got)
			}
			// Exempt from refusal is not enough: no address is counted at all, neither the proxy's nor one left of the
			// entry that does not parse.
			dump := s.dump()
			for _, addr := range []string{"127.0.0.1", "10.1.2.3", "10.4.5.6", left} {
				if strings.Contains(dump, `"`+addr+`"`) {
					t.Errorf("the signin_* tables hold %s, which no request here may count:\n%s", addr, dump)
				}
			}
		})
	}
}

// With no usable entry the bounds per name and per known device still apply. Fails on today's build, where the proxy's
// address is refused at the 11th attempt before either bound is reached, and on a build that applies no bound at all to
// such a request.
func TestWithNoUsableForwardedEntryTheNameAndKnownDeviceBoundsStillApply(t *testing.T) {
	t.Run("per name", func(t *testing.T) {
		s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
		name := s.account("noentry")
		refusals := s.total(nameRefusals)
		s.failN(name, 20) // No header: through the declared proxy with no entry.
		refusedWithin(t, "the 21st for the name", s.wrong(0, name), 895, 900)
		if got := s.total(nameRefusals) - refusals; got != 1 {
			t.Errorf("the name refusals metric rose by %v, want 1", got)
		}
	})
	t.Run("per known device", func(t *testing.T) {
		s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
		name, device := s.account("noentry"), newDevice()
		signedIn(t, "the device's sign-in", s.try(0, device, name, rightPassword))
		refusals := s.total(deviceRefusals)
		for i := 1; i <= 10; i++ {
			if got := s.try(i, device, name, "typo"); got.status != 401 {
				t.Fatalf("wrong password %d of 10 on the known device: %d %q, want 401", i, got.status, got.body)
			}
		}
		refusedWithin(t, "the 11th on the known device", s.try(0, device, name, "typo"), 895, 900)
		if got := s.total(deviceRefusals) - refusals; got != 1 {
			t.Errorf("the device refusals metric rose by %v, want 1 (the refusal is the known device's, no other bound's)", got)
		}
	})
}

// A client behind a declared proxy that appends its peer's address to X-Forwarded-For (as nginx's
// $proxy_add_x_forwarded_for does) is counted as itself whatever it sends: no header, one that does not parse, one naming
// the proxy's address or another client's. The header that arrives is what the client sent, then its own address.
//
// This passes on today's build, which reads the header from the right and stops at the first entry. It is here for the
// build that D-022 asks for, which must give up on a request with no usable entry without giving a client a way to make its
// own request one. It fails on each of these one-line mutants of Client (internal/signin/address.go), shown by overlay:
// "any entry that does not parse means no address", "any entry naming a declared proxy means no address", "the leftmost
// entry that is no proxy counts".
func TestAClientBehindAnAppendingProxyIsCountedAsItselfWhateverItSends(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	client, other := randomV4(), randomV4()
	sent := []string{"", "not-an-address", "127.0.0.1", other, "not-an-address, 127.0.0.1", other + ", 127.0.0.1"}
	appended := func(i int) []string {
		if prefix := sent[i%len(sent)]; prefix != "" {
			return []string{"X-Forwarded-For", prefix + ", " + client}
		}
		return []string{"X-Forwarded-For", client}
	}
	for i := 0; i < 10; i++ {
		if got := s.wrong(i, invented(), appended(i)...); got.status != 401 {
			t.Fatalf("failure %d, the client sending %q: %d %q, want 401", i+1, sent[i%len(sent)], got.status, got.body)
		}
	}
	for i := range sent {
		refusedWithin(t, "the client's 11th, sending "+strconv.Quote(sent[i]), s.wrong(i, invented(), appended(i)...), 1, 900)
	}
	if got := s.wrong(0, invented(), "X-Forwarded-For", other); got.status != 401 {
		t.Errorf("the other client, whose address the first one sent: %d, want 401 (it failed nothing)", got.status)
	}
}

// A sign-in from a declared proxy with no usable forwarded entry is logged once per process as a warning naming
// `trustedProxies`, and each one, right password or wrong, is counted in a metric with no address or account label, which
// docs/running.md names for alerting (D-022). The metric's name is not given by the criterion: the check finds the
// jellyscale_* metric that stood still for sign-ins with a usable entry and then rose by exactly the number of such
// sign-ins (7, where the failed sign-ins are 5 and all sign-ins 10), and asks docs/running.md to name it. That a right
// password with no usable entry is counted too is this check's reading of "each one" (a sign-in, not a failure).
func TestASignInWithNoUsableForwardedEntryIsWarnedOncePerProcessAndCounted(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	name := s.account("warn")
	snapshot := func() map[string]float64 { // Each jellyscale_* metric summed over its series and both replicas.
		out := map[string]float64{}
		for _, p := range s.apis {
			for series, value := range s.scrape(p) {
				if metric, _, _ := strings.Cut(series, "{"); strings.HasPrefix(metric, "jellyscale_") {
					out[metric] += value
				}
			}
		}
		return out
	}
	before := snapshot()
	usable := behindProxy(randomV4())
	s.wrong(0, invented(), usable...)
	s.wrong(1, invented(), usable...)
	signedIn(t, "a usable entry", s.try(0, newDevice(), name, rightPassword, usable...))
	mid := snapshot()
	none := [][]string{nil, {"X-Forwarded-For", "127.0.0.1"}, {"X-Forwarded-For", randomV4() + ", not-an-address"}}
	for i := 0; i < 3; i++ {
		if got := s.wrong(i, invented(), none[i]...); got.status != 401 {
			t.Fatalf("wrong password %d with no usable entry: %d %q, want 401", i+1, got.status, got.body)
		}
	}
	for i := 0; i < 4; i++ {
		signedIn(t, "a right password with no usable entry", s.try(i, newDevice(), name, rightPassword, none[i%len(none)]...))
	}

	for i, p := range s.apis {
		warned := 0
		for _, line := range strings.Split(p.log.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "trustedProxies") {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("replica %d: %d WARN lines name trustedProxies, want exactly 1 (it had sign-ins with no usable entry)\n%s", i, warned, p.log)
		}
	}

	// The metric did not move for sign-ins with a usable entry and rose by exactly the 7 without one: a series that moves
	// with every request (the pool's acquires) moves in both phases.
	after := snapshot()
	var counted []string
	for metric := range after {
		if mid[metric]-before[metric] == 0 && after[metric]-mid[metric] == 7 {
			counted = append(counted, metric)
		}
	}
	if len(counted) != 1 {
		t.Fatalf("%d jellyscale_* metrics stayed still for 3 sign-ins with a usable entry and then rose by 7, the sign-ins with no usable forwarded entry, over both replicas: %v; want one", len(counted), counted)
	}
	labelled := map[string]string{}
	for _, p := range s.apis {
		for series := range s.scrape(p) {
			if metric, labels, _ := strings.Cut(series, "{"); forbiddenLabel.MatchString(labels) {
				labelled[metric] = series
			}
		}
	}
	if series, ok := labelled[counted[0]]; ok {
		t.Errorf("%s has an address or account label: %s", counted[0], series)
	}
	doc, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), counted[0]) {
		t.Errorf("docs/running.md does not name %s, the metric that counts sign-ins with no usable forwarded entry", counted[0])
	}
}

var forbiddenLabel = regexp.MustCompile(`(?i)\b(address|addr|ip|client|peer|user|account|username|name|device)="`)
