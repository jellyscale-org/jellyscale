//go:build integration

package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"jellyscale/internal/testenv"
)

// JS-145 (D-020), continued: parallel attempts, what is logged and counted, start-up, outages and what a refusal
// leaves alone.

// 200 wrong passwords at once for one name from devices new to it, over two replicas: exactly 20 are checked.
func TestTwoHundredAtOnceForOneNameCheckExactlyTwentyPasswords(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("flood")
	failures, refusals := s.total(failuresMetric), s.total(nameRefusals)
	counts := parallel(200, func(i int) signInReply { return s.quiet(i, newDevice(), name, "guess") })
	if counts[401] != 20 || counts[403] != 180 || len(counts) != 2 {
		t.Errorf("answers %v, want 20 x 401 and 180 x 403 and nothing else", counts)
	}
	if got := s.total(failuresMetric) - failures; got != 20 {
		t.Errorf("the failed sign-in metric rose by %v over both replicas, want 20", got)
	}
	if got := s.total(nameRefusals) - refusals; got != 180 {
		t.Errorf("the name refusals metric rose by %v, want 180", got)
	}
}

// 200 at once from one address over 200 names: exactly 10 are checked.
func TestTwoHundredAtOnceFromOneAddressCheckExactlyTenPasswords(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	via := behindProxy(randomV4())
	failures, refusals := s.total(failuresMetric), s.total(addrRefusals)
	counts := parallel(200, func(i int) signInReply { return s.quiet(i, newDevice(), invented(), "guess", via...) })
	if counts[401] != 10 || counts[403] != 190 || len(counts) != 2 {
		t.Errorf("answers %v, want 10 x 401 and 190 x 403 and nothing else", counts)
	}
	if got := s.total(failuresMetric) - failures; got != 10 {
		t.Errorf("the failed sign-in metric rose by %v, want 10", got)
	}
	if got := s.total(addrRefusals) - refusals; got != 190 {
		t.Errorf("the address refusals metric rose by %v, want 190", got)
	}
}

// 200 at once from one known device: exactly 10 are checked.
func TestTwoHundredAtOnceFromOneKnownDeviceCheckExactlyTenPasswords(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("known")
	device := newDevice()
	signedIn(t, "the device's sign-in", s.try(0, device, name, rightPassword))
	failures, refusals := s.total(failuresMetric), s.total(deviceRefusals)
	counts := parallel(200, func(i int) signInReply { return s.quiet(i, device, name, "guess") })
	if counts[401] != 10 || counts[403] != 190 || len(counts) != 2 {
		t.Errorf("answers %v, want 10 x 401 and 190 x 403 and nothing else", counts)
	}
	if got := s.total(failuresMetric) - failures; got != 10 {
		t.Errorf("the failed sign-in metric rose by %v, want 10", got)
	}
	if got := s.total(deviceRefusals) - refusals; got != 190 {
		t.Errorf("the device refusals metric rose by %v, want 190", got)
	}
}

// slog text line fields: key=value, with a quoted value when it has a space.
func logLines(p *proc, msg string) []string {
	var out []string
	for _, line := range strings.Split(p.log.String(), "\n") {
		if strings.Contains(line, "msg=\""+msg+"\"") || strings.Contains(line, "msg="+msg+" ") {
			out = append(out, line)
		}
	}
	return out
}

func field(line, key string) (string, bool) {
	m := regexp.MustCompile(`(?:^| )` + key + `=("(?:[^"\\]|\\.)*"|\S*)`).FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.Trim(m[1], `"`), true
}

// Each refusal is logged with the bound that refused, the address and the account's stored name; an unknown name's text
// is never logged, at debug level either, nor stored as typed, and no password is; refusals are counted by bound alone and
// failed sign-ins with no label.
func TestEachRefusalIsLoggedWithItsBoundAndNeverWithAnUnknownNameOrAPassword(t *testing.T) {
	t.Setenv("JELLYSCALE_DEBUG", "1") // Debug level: the unknown name's text must not be there either.
	s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	p := s.apis[0]
	secret := "hunter2-" + uuid.NewString()[:8] // What a viewer types into the name box.
	password := "pw-" + uuid.NewString()[:8]

	byName := s.account("Mixed")
	for i := 0; i < 20; i++ {
		s.try(0, newDevice(), byName, password, behindProxy(randomV4())...)
	}
	nameAddr := randomV4()
	refusedWithin(t, "name", s.try(0, newDevice(), strings.ToUpper(byName), password, behindProxy(nameAddr)...), 1, 900)

	for i := 0; i < 20; i++ {
		s.try(0, newDevice(), secret, password, behindProxy(randomV4())...)
	}
	refusedWithin(t, "unknown name", s.try(0, newDevice(), strings.ToUpper(secret), password, behindProxy(randomV4())...), 1, 900)

	byDevice, known := s.account("Dev"), newDevice()
	signedIn(t, "sign-in", s.try(0, known, byDevice, rightPassword, behindProxy(randomV4())...))
	for i := 0; i < 10; i++ {
		s.try(0, known, byDevice, password, behindProxy(randomV4())...)
	}
	deviceAddr := randomV4()
	refusedWithin(t, "device", s.try(0, known, byDevice, password, behindProxy(deviceAddr)...), 1, 900)

	hot := randomV4()
	for i := 0; i < 10; i++ {
		s.try(0, newDevice(), invented(), password, behindProxy(hot)...)
	}
	refusedWithin(t, "address", s.try(0, newDevice(), strings.ToUpper(byDevice), password, behindProxy(hot)...), 1, 900)

	lines := logLines(p, "sign-in refused")
	want := map[string]string{"name": nameAddr, "device": deviceAddr, "address": hot}
	for bound, address := range want {
		found := false
		for _, line := range lines {
			if b, _ := field(line, "bound"); b == bound {
				// The unknown name's refusal is a name-bound line too, from another address and with no user: only
				// the real account's is held to nameAddr (lead's correction, the implementer's dispute).
				if _, has := field(line, "user"); bound == "name" && !has {
					continue
				}
				found = true
				if a, _ := field(line, "address"); a != address {
					t.Errorf("the %s refusal logs address %q, want %q (the one counted): %s", bound, a, address, line)
				}
				// D-023: an attributed address is named under `address` alone; `peer` and the forwarded header are what a
				// line has when none was attributed (signin_line_integration_test.go).
				for _, other := range []string{"peer", "forwarded_unverified"} {
					if v, has := field(line, other); has {
						t.Errorf("the %s refusal, whose address was attributed, also has %s=%q: %s", bound, other, v, line)
					}
				}
				if !strings.Contains(line, "level=WARN") {
					t.Errorf("the %s refusal is not logged at WARN: %s", bound, line)
				}
			}
		}
		if !found {
			t.Errorf("no `sign-in refused` line has bound=%s; the log has %d such lines:\n%s", bound, len(lines), strings.Join(lines, "\n"))
		}
	}
	withUser, withoutUser := 0, 0
	for _, line := range lines {
		b, _ := field(line, "bound")
		user, has := field(line, "user")
		switch {
		case !has && b == "name":
			withoutUser++
		case has && (b == "name" && user == byName || b != "name" && user == byDevice):
			withUser++
		default:
			t.Errorf("a %s refusal logs user=%q (has: %v); want the stored name of an account, and none for a name that is no account: %s", b, user, has, line)
		}
	}
	if withoutUser != 1 || withUser < 3 {
		t.Errorf("%d refusals are logged with no account name and %d with one, want 1 (the unknown name's) and 3", withoutUser, withUser)
	}
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), strings.ToLower(secret)) {
			t.Errorf("an unknown name is logged: %s", line)
		}
	}
	for _, text := range []string{secret, strings.ToUpper(secret), password} {
		if strings.Contains(p.log.String(), text) {
			t.Errorf("the log holds %q, which a viewer typed (a name that is no account, or a password)", text)
		}
	}
	if dump := strings.ToLower(s.dump()); strings.Contains(dump, strings.ToLower(secret)) || strings.Contains(dump, password) {
		t.Errorf("the signin_* tables hold what a viewer typed as an unknown name or as a password:\n%s", dump)
	}

	series := s.scrape(p)
	for _, bound := range []string{"name", "device", "address"} {
		if series[`jellyscale_signin_refusals_total{bound="`+bound+`"}`] < 1 {
			t.Errorf("no refusal is counted for bound %q: %v", bound, series)
		}
	}
	if series[failuresMetric] < 60 {
		t.Errorf("%s = %v, want at least the 60 wrong passwords of this test", failuresMetric, series[failuresMetric])
	}
	for name := range series {
		if strings.HasPrefix(name, "jellyscale_signin_") && strings.Contains(name, "{") &&
			!regexp.MustCompile(`^jellyscale_signin_refusals_total\{bound="(name|device|address)"\}$`).MatchString(name) {
			t.Errorf("series %s: only the refusals have a label, and it is the bound alone", name)
		}
	}
}

var writesSignIn = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from)\s+"?signin_`)

// With the key absent the per-address bound is off: a warning naming the key at every start-up and a metric that says so;
// with it declared (also empty) the metric says on and no warning is logged.
func TestWithoutTrustedProxiesTheAddressBoundIsOffAndTheStartUpSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   map[string]any
		on      float64
		warning bool
	}{{"absent", nil, 0, true}, {"declared", map[string]any{"trustedProxies": []string{"127.0.0.1"}}, 1, false}, {"empty", map[string]any{"trustedProxies": []string{}}, 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSignInSite(t, 1, tc.extra)
			p := s.apis[0]
			if got := s.scrape(p)["jellyscale_signin_address_bound"]; got != tc.on {
				t.Errorf("jellyscale_signin_address_bound = %v, want %v", got, tc.on)
			}
			warned := false
			for _, line := range strings.Split(p.log.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "trustedProxies") {
					warned = true
				}
			}
			if warned != tc.warning {
				t.Errorf("a WARN line naming trustedProxies at start-up: %v, want %v\n%s", warned, tc.warning, p.log)
			}
		})
	}
}

// With `[]` declared, a forwarded header from a loopback or private peer is logged once per process however many
// requests carry one; a declared proxy's header is not.
func TestAForwardedHeaderFromAnUndeclaredPeerIsLoggedOncePerProcess(t *testing.T) {
	const message = "forwarded header from an undeclared proxy"
	s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{}})
	for i := 0; i < 5; i++ {
		s.wrong(0, invented(), "X-Forwarded-For", randomV4())
	}
	lines := logLines(s.apis[0], message)
	if len(lines) != 1 {
		t.Fatalf("%d lines say %q, want exactly one (log: %s)", len(lines), message, s.apis[0].log)
	}
	if peer, _ := field(lines[0], "peer"); peer != "127.0.0.1" {
		t.Errorf("peer=%q, want 127.0.0.1: %s", peer, lines[0])
	}
	declared := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	declared.wrong(0, invented(), "X-Forwarded-For", randomV4())
	if got := logLines(declared.apis[0], message); len(got) != 0 {
		t.Errorf("a declared proxy's header is logged as undeclared: %v", got)
	}
}

// No password is answered as wrong or right when its count cannot be read or stored, alike for a real and an unknown name;
// what was reached before the outage holds after it.
func TestNoPasswordIsAnsweredWhileTheBoundsCannotBeReadOrStored(t *testing.T) {
	proxy := testenv.NewDBProxy(t, testDatabaseURL(t))
	s := newSignInSiteVia(t, 1, nil, proxy.DSN()+"&default_query_exec_mode=exec")
	real, fake := s.account("outage"), invented()
	s.failN(real, 20)
	s.failN(fake, 20)

	notAnAnswer := func(what string, got signInReply) {
		t.Helper()
		if got.status < 500 {
			t.Errorf("%s: %d %q, want a failure of the server (no 200, 401 or 403)", what, got.status, got.body)
		}
	}
	proxy.Away()
	notAnAnswer("a wrong password for a real name, Postgres away", s.try(0, newDevice(), real, "wrong"))
	notAnAnswer("the right password, Postgres away", s.try(0, newDevice(), real, rightPassword))
	notAnAnswer("a password for an unknown name, Postgres away", s.try(0, newDevice(), fake, "wrong"))
	proxy.Back()
	refusedWithin(t, "after the outage, the real name's refusal holds", s.try(0, newDevice(), real, rightPassword), 1, 900)
	refusedWithin(t, "after the outage, the unknown name's refusal holds", s.try(0, newDevice(), fake, "wrong"), 1, 900)

	// A failure that cannot be stored: the statement that writes to a signin_ table never reaches Postgres, or is lost on
	// its way back. Neither is answered 401.
	for _, when := range []string{"before", "after"} {
		for _, name := range []string{s.account("store"), invented()} {
			var hit atomic.Bool
			cut := func() { hit.Store(true); proxy.Sever() }
			match := func(sent []byte) bool { return writesSignIn.Match(sent) }
			if when == "before" {
				proxy.OnStatement(match, cut, nil)
			} else {
				proxy.OnStatement(match, nil, cut)
			}
			got := s.try(0, newDevice(), name, "wrong")
			if !hit.Load() {
				t.Fatalf("the failed sign-in of %q sent no statement that writes to a signin_ table, so its failure was not stored before it was answered: %d %q", name, got.status, got.body)
			}
			notAnAnswer("a wrong password whose failure could not be stored ("+when+" the write reaches Postgres) for "+name, got)
		}
	}
}

// LoginAttemptsBeforeLockout is -1 and nothing is disabled in every answer that carries a user, also while that user is refused.
func TestThePolicyNeverReportsALockoutOrADisabledAccountWhileTheNameIsRefused(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	name := s.account("policy")
	known := newDevice()
	signedIn(t, "sign-in", s.try(0, known, name, rightPassword))
	var token string
	s.failN(name, 20)
	refusedWithin(t, "the name is refused to strangers", s.wrong(0, name), 1, 900)
	reply := s.try(0, known, name, rightPassword)
	token = signedIn(t, "the known device while the name is refused", reply) // The sign-in replaced the device's token.
	var body struct {
		User struct{ Policy map[string]any }
	}
	mustJSON(t, reply.body, &body)
	for what, policy := range map[string]map[string]any{"the sign-in's User": body.User.Policy, "/Users/Me": s.policyOf(token)} {
		if policy["LoginAttemptsBeforeLockout"] != float64(-1) || policy["IsDisabled"] != false {
			t.Errorf("%s: LoginAttemptsBeforeLockout %v, IsDisabled %v, want -1 and false", what, policy["LoginAttemptsBeforeLockout"], policy["IsDisabled"])
		}
	}
}

func (s *signInSite) policyOf(token string) map[string]any {
	s.t.Helper()
	_, raw := s.get(0, "/Users/Me", token)
	var me struct{ Policy map[string]any }
	mustJSON(s.t, raw, &me)
	return me.Policy
}

func mustJSON(t *testing.T, raw []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("not JSON: %q", raw)
	}
}

// A token already issued keeps working while its account, its device and its address are all refused, and no failed or
// refused sign-in removes one: the account's other devices and an open socket are unaffected.
func TestATokenKeepsWorkingWhileItsAccountItsDeviceAndItsAddressAreRefused(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	name := s.account("token")
	device, other := newDevice(), newDevice()
	addr := randomV4()
	held := signedIn(t, "device", s.try(0, device, name, rightPassword, behindProxy(addr)...))
	bystander := signedIn(t, "the account's other device", s.try(1, other, name, rightPassword, behindProxy(randomV4())...))
	socket := s.apis[0].dial(t, held)
	for i := 0; i < 10; i++ { // The device's and the address's own count.
		s.try(i, device, name, "guess", behindProxy(addr)...)
	}
	s.failSpread(name, 20) // The name's.
	refusedWithin(t, "the name", s.wrong(0, name, behindProxy(randomV4())...), 1, 900)
	refusedWithin(t, "the device", s.try(0, device, name, "guess", behindProxy(randomV4())...), 1, 900)
	refusedWithin(t, "the address", s.wrong(0, invented(), behindProxy(addr)...), 1, 900)
	for i, token := range []string{held, bystander} {
		for replica := 0; replica < 2; replica++ {
			if status, _ := s.get(replica, "/Users/Me", token); status != 200 {
				t.Errorf("token %d on replica %d while the account, the device and the address are refused: /Users/Me %d, want 200", i, replica, status)
			}
		}
	}
	select {
	case err := <-socket.read:
		t.Errorf("the device's socket was closed: %v", err)
	default:
	}
}
