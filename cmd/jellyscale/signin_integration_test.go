//go:build integration

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// JS-145 (D-020): failed sign-ins are bounded per name (20 in 15 minutes from devices new to the account), per known
// device (10) and per address (10), refused with 403 and Retry-After for 15 minutes, never slowed and never by
// disabling an account. Real api processes over Postgres; time passes by moving every time in the signin_* tables
// back (signInSite.shift), as the behaviour spec says. The names these tests use are new on each run and the rows are
// removed at the end. Expected values are the behaviour spec's 18 to 23.

// Wrong passwords 1 to 20 each answer 401 and the 21st attempt is refused, whether the 20 reached one replica or two
// with one killed between; a right password during the refusal is refused the same way; names that differ in letter
// case share one count; the refusal lasts 15 minutes from the 20th failure and then ends by itself, and a refused
// attempt neither counts nor lengthens it.
func TestTwentyWrongPasswordsForANameThenItIsRefusedFor15MinutesAcrossReplicasAndARestart(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("name")
	for i := 0; i < 20; i++ {
		if i == 10 {
			s.restart(0)
		}
		spelled := name
		if i%2 == 1 {
			spelled = strings.ToUpper(name) // One count for every letter case.
		}
		got := s.wrong(i, spelled)
		if got.status != 401 || string(got.body) != invalidBody {
			t.Fatalf("wrong password %d: %d %q, want 401 %q", i+1, got.status, got.body, invalidBody)
		}
	}
	// 403 with Retry-After, and every cross-origin page can read both.
	got := s.try(1, newDevice(), name, "wrong-21", "Origin", "https://app.example.test")
	refusedWithin(t, "the 21st attempt", got, 895, 900)
	if got.header.Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(strings.ToLower(got.header.Get("Access-Control-Expose-Headers")), "retry-after") {
		t.Errorf("a page on another origin cannot read the refusal: %v", got.header)
	}
	refusedWithin(t, "the right password during the refusal", s.try(0, newDevice(), name, rightPassword), 895, 900)
	refusedWithin(t, "the right password in another letter case", s.try(1, newDevice(), strings.ToUpper(name), rightPassword), 895, 900)
	// Refused attempts check nothing, count nothing and lengthen nothing: 10 minutes later what is left is 5.
	s.shift(600 * time.Second)
	refusedWithin(t, "10 minutes on", s.try(0, newDevice(), name, "wrong-22"), 290, 300)
	// 15 minutes and a second from the 20th failure: the refusal has ended by itself.
	s.shift(301 * time.Second)
	signedIn(t, "the right password once the refusal ended", s.try(1, newDevice(), name, rightPassword))
	// The count is back to 0: 20 more failures, then refused again.
	s.failN(name, 20)
	refusedWithin(t, "the 21st failure after the refusal ended", s.wrong(0, name), 895, 900)
}

// The count is of the last 15 minutes, any 15 minutes and not a clock window (behaviour spec 19).
func TestTheNameCountIsOfAnyFifteenMinutes(t *testing.T) {
	s := newSignInSite(t, 1, nil)

	// 19 failures, 14 minutes, 1 failure: that is 20 in 15 minutes.
	a := s.account("slide")
	s.failN(a, 19)
	s.shift(840 * time.Second)
	if got := s.wrong(0, a); got.status != 401 {
		t.Fatalf("the 20th failure: %d, want 401", got.status)
	}
	refusedWithin(t, "after the 20th failure", s.try(0, newDevice(), a, rightPassword), 895, 900)
	s.shift(120 * time.Second)
	refusedWithin(t, "2 minutes into the refusal, though 19 of the failures are 16 minutes old", s.try(0, newDevice(), a, rightPassword), 770, 780)
	s.shift(781 * time.Second)
	signedIn(t, "when the refusal has ended", s.try(0, newDevice(), a, rightPassword))

	// 1 failure, 14 minutes, 18 failures, 2 minutes, 2 failures: the first has gone, so 20 in 15 minutes only now.
	b := s.account("slide")
	s.failN(b, 1)
	s.shift(840 * time.Second)
	s.failN(b, 18)
	s.shift(120 * time.Second)
	s.failN(b, 2)
	refusedWithin(t, "after 20 failures inside 15 minutes", s.try(0, newDevice(), b, rightPassword), 895, 900)

	// No success resets a count: 10 failures, a sign-in, 10 more failures are 20.
	d := s.account("slide")
	s.failN(d, 10)
	signedIn(t, "a success between the failures", s.try(0, newDevice(), d, rightPassword))
	s.failN(d, 10)
	refusedWithin(t, "after 20 failures with a success between", s.wrong(0, d), 895, 900)

	// 19 failures, 15 minutes and a second, 1 failure: that is 1 in 15 minutes.
	c := s.account("slide")
	s.failN(c, 19)
	s.shift(901 * time.Second)
	s.failN(c, 1)
	if got := s.wrong(0, c); got.status != 401 {
		t.Errorf("19 failures that are 15 minutes old do not count: the next attempt is %d, want 401", got.status)
	}
}

// An unknown name answers exactly as a real one at the same count: 401 for wrong passwords 1 to 20, 403 from the 21st,
// the same status, body and headers (Retry-After within a second), in any letter case.
func TestAnUnknownNameIsBoundedAndAnswersAsARealOneAtTheSameCount(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	real, fake := s.account("same"), invented()
	for i := 1; i <= 20; i++ {
		spelled := real
		if i%3 == 0 {
			spelled = strings.ToUpper(real)
		}
		a, b := s.wrong(0, spelled), s.wrong(0, fake)
		if a.status != 401 || b.status != 401 || string(a.body) != string(b.body) || a.header.Get("Content-Type") != b.header.Get("Content-Type") {
			t.Fatalf("failure %d: real %d %q, unknown %d %q, want the same 401", i, a.status, a.body, b.status, b.body)
		}
	}
	a, b := s.wrong(0, real), s.wrong(0, strings.ToUpper(fake))
	refusedWithin(t, "the real name's 21st attempt", a, 895, 900)
	refusedWithin(t, "the unknown name's 21st attempt", b, 895, 900)
	if a.status != 403 || b.status != 403 {
		return
	}
	if string(a.body) != string(b.body) || len(a.header) != len(b.header) {
		t.Errorf("a refusal tells a name that exists from one that does not: real %q %v, unknown %q %v", a.body, a.header, b.body, b.header)
	}
	for key, values := range a.header {
		if key != "Date" && key != "Retry-After" && strings.Join(values, ",") != strings.Join(b.header[key], ",") {
			t.Errorf("header %s differs: real %v, unknown %v", key, values, b.header[key])
		}
	}
	ra, rb := atoi(t, a.header.Get("Retry-After")), atoi(t, b.header.Get("Retry-After"))
	if ra-rb > 1 || rb-ra > 1 {
		t.Errorf("Retry-After differs by more than a second: real %d, unknown %d", ra, rb)
	}
	// A right password for the real name is refused the same way.
	if got := s.try(0, newDevice(), real, rightPassword); string(got.body) != string(a.body) || got.status != 403 {
		t.Errorf("the right password: %d %q, want the same refusal %q", got.status, got.body, a.body)
	}
}

// The password routes: today POST /Users/AuthenticateByName alone, in every spelling of its path, are bounded the same
// way, whichever spelling an attempt used.
func TestEverySpellingOfTheSignInPathIsBoundedAndCountsTogether(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	name := s.account("spell")
	paths := []string{signInPath, strings.ToLower(signInPath), signInPath + "/", strings.ToUpper(signInPath)}
	for i := 0; i < 20; i++ {
		got, err := s.apis[0].signInReplyAt(paths[i%len(paths)], newDevice(), name, "wrong")
		if err != nil || got.status != 401 {
			t.Fatalf("attempt %d at %s: %v %d, want 401", i+1, paths[i%len(paths)], err, got.status)
		}
	}
	for _, path := range paths {
		got, err := s.apis[0].signInReplyAt(path, newDevice(), name, rightPassword)
		if err != nil {
			t.Fatal(err)
		}
		refusedWithin(t, "the 21st attempt at "+path, got, 895, 900)
	}
}

// A body that is not JSON is 400 as today and counts nothing.
func TestABodyThatIsNotJSONCountsNothing(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	name := s.account("body")
	s.failN(name, 19)
	for i := 0; i < 5; i++ {
		got, err := s.apis[0].signInBody(signInPath, newDevice(), []byte("not json at all"))
		if err != nil || got.status != 400 {
			t.Fatalf("a body that is not JSON: %v %d, want 400", err, got.status)
		}
	}
	if got := s.wrong(0, name); got.status != 401 {
		t.Errorf("the 20th failure after 5 bad bodies: %d, want 401 (a bad body counted)", got.status)
	}
	refusedWithin(t, "the 21st", s.wrong(0, name), 895, 900)
}

// Nine mistypes and then the right password sign in, each mistype answered 401, with the per-address bound on: on a known
// device whatever others do to the name, and on a new device while others' failures on the name are at most 10.
func TestAViewerWhoMistypesNineTimesSignsInOnTheTenth(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	t.Run("on a new device while others failed 10 times on the name", func(t *testing.T) {
		name := s.account("viewer")
		for i := 0; i < 10; i++ {
			if got := s.wrong(i, name, behindProxy(randomV4())...); got.status != 401 {
				t.Fatalf("another's failure %d: %d, want 401", i+1, got.status)
			}
		}
		viewer, device := randomV4(), newDevice()
		for i := 0; i < 9; i++ {
			if got := s.try(i, device, name, "typo", behindProxy(viewer)...); got.status != 401 {
				t.Fatalf("mistype %d: %d %q, want 401", i+1, got.status, got.body)
			}
		}
		signedIn(t, "the right password after nine mistypes", s.try(1, device, name, rightPassword, behindProxy(viewer)...))
	})
	t.Run("on a known device whatever others do to the name", func(t *testing.T) {
		name := s.account("viewer")
		known := newDevice()
		signedIn(t, "first sign-in", s.try(0, known, name, rightPassword, behindProxy(randomV4())...))
		for i := 0; i < 25; i++ { // Strangers from many addresses, past the name's 20.
			s.wrong(i, name, behindProxy(randomV4())...)
		}
		refusedWithin(t, "a stranger is refused", s.wrong(0, name, behindProxy(randomV4())...), 1, 900)
		viewer := randomV4()
		for i := 0; i < 9; i++ {
			if got := s.try(i, known, name, "typo", behindProxy(viewer)...); got.status != 401 {
				t.Fatalf("mistype %d on the known device: %d %q, want 401", i+1, got.status, got.body)
			}
		}
		signedIn(t, "the right password on the known device", s.try(0, known, name, rightPassword, behindProxy(viewer)...))
	})
}

// Under guessing from many addresses the name is refused to strangers, and its owner signs in at the first try from a device
// that still holds a token and from one that signed out; the two exceptions of a device's age and of another account.
func TestTheOwnerSignsInOnAKnownDeviceWhileTheNameIsRefusedToStrangers(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("owner")
	holding, signedOut := newDevice(), newDevice()
	signedIn(t, "holding device", s.try(0, holding, name, rightPassword))
	bystander := signedIn(t, "a device that signs in once and is then left alone", s.try(0, newDevice(), name, rightPassword))
	out := signedIn(t, "device to sign out", s.try(1, signedOut, name, rightPassword))
	s.logout(1, out)
	s.failN(name, 20)
	refusedWithin(t, "a stranger's right password", s.try(0, newDevice(), name, rightPassword), 895, 900)
	refusedWithin(t, "an empty DeviceId is a stranger", s.try(0, "", name, rightPassword), 895, 900)
	signedIn(t, "the device that holds a token, at the first try", s.try(0, holding, name, rightPassword))
	signedIn(t, "the device that signed out, at the first try", s.try(1, signedOut, name, rightPassword))
	if status, _ := s.get(0, "/Users/Me", bystander); status != 200 {
		t.Errorf("a refusal removed a token: /Users/Me %d", status)
	}
}

func TestADeviceStaysKnownFor30DaysAfterItsTokenEnds(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	for _, tc := range []struct {
		days int
		want int
	}{{29, 200}, {31, 403}} {
		name := s.account("age")
		device := newDevice()
		token := signedIn(t, "sign-in", s.try(0, device, name, rightPassword))
		s.logout(0, token)
		s.shift(time.Duration(tc.days) * 24 * time.Hour)
		s.failN(name, 20)
		got := s.try(0, device, name, rightPassword)
		if got.status != tc.want {
			t.Errorf("a device whose token ended %d days ago, with the name refused to strangers: %d %q, want %d", tc.days, got.status, got.body, tc.want)
		}
	}
}

// A device known only to another account, or whose token was replaced, is as the spec says: another account's is a stranger,
// a replaced token (a new sign-in on the same device) keeps the device known.
func TestADeviceKnownToAnotherAccountIsAStranger(t *testing.T) {
	s := newSignInSite(t, 1, nil)
	mine, other := s.account("mine"), s.account("other")
	device := newDevice()
	signedIn(t, "the other account's device", s.try(0, device, other, rightPassword))
	s.failN(mine, 20)
	refusedWithin(t, "a device known to another account", s.try(0, device, mine, rightPassword), 895, 900)
}

// A known device has its own count of 10, off the name's; a guesser who holds a known DeviceId gets 10 passwords from it and
// 20 more as a stranger, 30 for the name, and then every attempt is refused.
func TestAKnownDeviceHasItsOwnCountOfTen(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("device")
	known := newDevice()
	token := signedIn(t, "sign-in", s.try(0, known, name, rightPassword))
	for i := 0; i < 10; i++ {
		if got := s.try(i, known, name, "typo"); got.status != 401 {
			t.Fatalf("failure %d from the known device: %d, want 401", i+1, got.status)
		}
	}
	refusedWithin(t, "the 11th from the known device", s.try(0, known, name, "typo"), 895, 900)
	refusedWithin(t, "the right password from the known device", s.try(1, known, name, rightPassword), 895, 900)
	// Its failures added nothing to the name's count: strangers still have all 20.
	s.failN(name, 20)
	refusedWithin(t, "the 21st stranger", s.wrong(0, name), 895, 900)
	// 10 + 20 passwords were checked, no more, from any address.
	if status, _ := s.get(1, "/Users/Me", token); status != 200 {
		t.Errorf("failed and refused attempts with the device's id removed its token: /Users/Me %d", status)
	}
}

// 30 checked passwords for a guesser who holds one DeviceId, whatever replica they reach.
func TestAGuesserWithOneKnownDeviceIdGetsThirtyCheckedPasswords(t *testing.T) {
	s := newSignInSite(t, 2, nil)
	name := s.account("thirty")
	known := newDevice()
	signedIn(t, "the owner's sign-in", s.try(0, known, name, rightPassword))
	checked := 0
	for i := 0; i < 40; i++ {
		device := known
		if i >= 10 {
			device = newDevice()
		}
		got := s.try(i, device, name, "guess")
		if got.status == 401 {
			checked++
		}
	}
	if checked != 30 {
		t.Errorf("%d passwords were checked, want 30 (10 from the known device, 20 as strangers)", checked)
	}
	refusedWithin(t, "after 30", s.try(0, known, name, "guess"), 1, 900)
	refusedWithin(t, "after 30, a new device", s.wrong(1, name), 1, 900)
}

// An address is refused after 10 failures across all names, whatever the name or device, and a success resets nothing;
// an IPv6 address counts as its /64 and an IPv4-mapped one as its IPv4; with this bound on one address alone never
// causes a name to be refused to others.
func TestAnAddressIsRefusedAfterTenFailuresAcrossNames(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	addr := randomV4()
	via := behindProxy(addr)
	known := s.account("addr")
	device := newDevice()
	signedIn(t, "the known device's sign-in", s.try(0, device, known, rightPassword, behindProxy(randomV4())...))
	for i := 0; i < 5; i++ {
		if got := s.wrong(i, invented(), via...); got.status != 401 {
			t.Fatalf("failure %d: %d, want 401", i+1, got.status)
		}
	}
	signedIn(t, "a success from the address", s.try(0, newDevice(), s.account("addr"), rightPassword, via...)) // Resets nothing.
	for i := 5; i < 10; i++ {
		if got := s.wrong(i, s.account("addr"), via...); got.status != 401 {
			t.Fatalf("failure %d: %d, want 401", i+1, got.status)
		}
	}
	refusedWithin(t, "the 11th, an unknown name", s.wrong(0, invented(), via...), 895, 900)
	refusedWithin(t, "a right password from a known device", s.try(1, device, known, rightPassword, via...), 895, 900)
	got := s.wrong(0, known, via...)
	refusedWithin(t, "another name from the address", got, 895, 900)
	signedIn(t, "another address is not refused", s.try(0, newDevice(), s.account("addr"), rightPassword, behindProxy(randomV4())...))
}

func TestAnIPv6AddressCountsAsItsSlash64AndAMappedOneAsItsIPv4(t *testing.T) {
	s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	net := randomNet64()
	for i := 1; i <= 10; i++ { // Ten addresses of one /64, one failure each.
		if got := s.wrong(0, invented(), behindProxy(fmt.Sprintf("%s::%x", net, 0x10+i))...); got.status != 401 {
			t.Fatalf("failure %d: %d, want 401", i, got.status)
		}
	}
	refusedWithin(t, "an eleventh address of the same /64", s.wrong(0, invented(), behindProxy(net+"::bb")...), 895, 900)
	other := randomNet64()
	if got := s.wrong(0, invented(), behindProxy(other+"::1")...); got.status != 401 {
		t.Errorf("the next /64 is another address: %d, want 401", got.status)
	}
	v4 := randomV4()
	for i := 0; i < 5; i++ {
		s.wrong(0, invented(), behindProxy(v4)...)
		s.wrong(0, invented(), behindProxy("::ffff:"+v4)...)
	}
	refusedWithin(t, "the IPv4 address after 5 failures as itself and 5 mapped", s.wrong(0, invented(), behindProxy(v4)...), 895, 900)
	refusedWithin(t, "the same, asked mapped", s.wrong(0, invented(), behindProxy("::ffff:"+v4)...), 895, 900)
}

func TestOneAddressAloneNeverCausesANameToBeRefusedToOthers(t *testing.T) {
	s := newSignInSite(t, 2, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
	name := s.account("alone")
	guesser := randomV4()
	s.failN(name, 10, behindProxy(guesser)...)
	for i := 0; i < 20; i++ {
		refusedWithin(t, "attempt from the refused address", s.wrong(i, name, behindProxy(guesser)...), 1, 900)
	}
	signedIn(t, "the owner on a new device from another address, at the first try", s.try(0, newDevice(), name, rightPassword, behindProxy(randomV4())...))
}

// Which address counts behind a declared proxy: the entry nearest the server that is no declared proxy, whatever the
// client sent ahead of it; a header from any other peer is ignored.
func TestTheAddressBehindADeclaredProxyIsTheNearestEntryThatIsNoProxy(t *testing.T) {
	t.Run("an entry the client sent ahead changes nothing", func(t *testing.T) {
		s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"127.0.0.1"}})
		real := randomV4()
		for i := 0; i < 10; i++ {
			if got := s.wrong(0, invented(), "X-Forwarded-For", randomV4()+", "+real); got.status != 401 {
				t.Fatalf("failure %d: %d, want 401", i+1, got.status)
			}
		}
		refusedWithin(t, "the same client with yet another forged entry", s.wrong(0, invented(), "X-Forwarded-For", randomV4()+", "+real), 895, 900)
		if got := s.wrong(0, invented(), "X-Forwarded-For", real+", "+randomV4()); got.status != 401 {
			t.Errorf("the nearest entry is another client's: %d, want 401 (the entry on the left is not what counts)", got.status)
		}
	})
	t.Run("a declared proxy in the chain is skipped", func(t *testing.T) {
		s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"127.0.0.1", "10.0.0.0/8"}})
		real := randomV4()
		for i := 0; i < 10; i++ {
			s.wrong(0, invented(), "X-Forwarded-For", real+", 10.1.2.3")
		}
		refusedWithin(t, "the client behind two proxies", s.wrong(0, invented(), "X-Forwarded-For", real+", 10.9.9.9"), 895, 900)
	})
	t.Run("a header from an undeclared peer is ignored", func(t *testing.T) {
		s := newSignInSite(t, 1, map[string]any{"trustedProxies": []string{"10.0.0.0/8"}})
		for i := 0; i < 10; i++ {
			if got := s.wrong(0, invented(), "X-Forwarded-For", randomV4()); got.status != 401 {
				t.Fatalf("failure %d: %d, want 401", i+1, got.status)
			}
		}
		// The peer is 127.0.0.1, which is no declared proxy: 10 failures from one address, whatever the header says.
		refusedWithin(t, "the 11th, with a header naming a stranger", s.wrong(0, invented(), "X-Forwarded-For", randomV4()), 895, 900)
	})
}
