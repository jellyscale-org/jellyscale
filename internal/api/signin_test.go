package api

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// JS-145 (D-020), the checks that need nothing outside Go: what a real Jellyfin 12.4 and its jellyfin-web did (the
// capture, written by test/differential/capture_signin.py), the routes that take a password, what a page may read of a
// refusal, what compat/differences.yaml and the docs say.

// signinCapture is compat/captures/signin-12.4.json.
type signinCapture struct {
	Image         string `json:"image"`
	ServerVersion string `json:"server_version"`
	Default       struct {
		Policy map[string]any `json:"policy"`
		Wrong  []signinRow    `json:"wrong"`
		Right  signinRow      `json:"right"`
	} `json:"default"`
	Lockout struct {
		Limit         int            `json:"limit"`
		PolicyBefore  map[string]any `json:"policy_before"`
		Wrong         []signinRow    `json:"wrong"`
		RightAfter    signinRow      `json:"right_after"`
		WrongAfter    signinRow      `json:"wrong_after"`
		PolicyAfter   map[string]any `json:"policy_after"`
		RightAfter30s signinRow      `json:"right_after_30s"`
		PolicyEnabled map[string]any `json:"policy_enabled"`
		RightEnabled  signinRow      `json:"right_enabled"`
	} `json:"lockout"`
	UnknownName struct {
		Unknown       []signinRow `json:"unknown"`
		WrongPassword []signinRow `json:"wrong_password"`
	} `json:"unknown_name"`
	Web []struct {
		Status       int    `json:"status"`
		RetryAfter   string `json:"retry_after"`
		Kind         string `json:"kind"`
		Shows        string `json:"shows"`
		OnSignInForm bool   `json:"on_sign_in_form"`
	} `json:"web"`
}

type signinRow struct {
	Status  int               `json:"status"`
	Ms      float64           `json:"ms"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func readSigninCapture(t *testing.T) signinCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/signin-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var c signinCapture
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The capture holds, from a real Jellyfin 12.4, each row of the task's first criterion, and each agrees with D-020's brief.
func TestTheSignInCaptureHoldsWhatJellyfin124AndItsWebClientDo(t *testing.T) {
	c := readSigninCapture(t)
	if c.Image != "jellyfin/jellyfin:12.4" || !strings.HasPrefix(c.ServerVersion, "12.4") {
		t.Errorf("the capture is of %s %s, want jellyfin/jellyfin:12.4", c.Image, c.ServerVersion)
	}
	// The default: no bound.
	if c.Default.Policy["LoginAttemptsBeforeLockout"] != float64(-1) || len(c.Default.Wrong) < 14 {
		t.Errorf("default policy %v with %d wrong passwords, want -1 and at least 14", c.Default.Policy, len(c.Default.Wrong))
	}
	for i, row := range c.Default.Wrong {
		if row.Status != 401 {
			t.Errorf("default wrong password %d answered %d, want 401", i+1, row.Status)
		}
	}
	if c.Default.Right.Status != 200 {
		t.Errorf("the right password after the wrong ones answered %d, want 200", c.Default.Right.Status)
	}
	// The set limit: disabled at that count, then 403 for right and wrong alike until an administrator enables it.
	l := c.Lockout
	if l.Limit < 1 || l.PolicyBefore["LoginAttemptsBeforeLockout"] != float64(l.Limit) {
		t.Fatalf("lockout limit %d with policy %v", l.Limit, l.PolicyBefore)
	}
	for i, row := range l.Wrong {
		if want := map[bool]int{true: 401, false: 403}[i < l.Limit]; row.Status != want {
			t.Errorf("wrong password %d with a limit of %d answered %d, want %d", i+1, l.Limit, row.Status, want)
		}
	}
	if l.RightAfter.Status != 403 || l.WrongAfter.Status != 403 || l.RightAfter30s.Status != 403 {
		t.Errorf("after the limit: right %d, wrong %d, right 30 s on %d, want 403 for each", l.RightAfter.Status, l.WrongAfter.Status, l.RightAfter30s.Status)
	}
	if l.PolicyAfter["IsDisabled"] != true || l.PolicyEnabled["IsDisabled"] != false || l.RightEnabled.Status != 200 {
		t.Errorf("disabled %v, then enabled %v, right password %d, want true, false, 200", l.PolicyAfter["IsDisabled"], l.PolicyEnabled["IsDisabled"], l.RightEnabled.Status)
	}
	// An unknown name answers the 401 of a wrong password, in about 1 ms against 77 ms.
	slowest, fastest := 0.0, 1e9
	for _, row := range c.UnknownName.Unknown {
		slowest = max(slowest, row.Ms)
		if row.Status != 401 || row.Body != c.UnknownName.WrongPassword[0].Body {
			t.Errorf("an unknown name answered %d %q, want the 401 of a wrong password %q", row.Status, row.Body, c.UnknownName.WrongPassword[0].Body)
		}
	}
	for _, row := range c.UnknownName.WrongPassword {
		fastest = min(fastest, row.Ms)
	}
	if slowest > 10 || fastest < 30 {
		t.Errorf("an unknown name took up to %.1f ms, a wrong password at least %.1f ms: the capture is not of the difference D-020 saw", slowest, fastest)
	}
	// What jellyfin-web shows: 401 and 403 a toast, 429 and 503 a Connection Failure dialog, with and without Retry-After.
	const invalid = "Invalid username or password. Please try again."
	const forbidden = "You are not authorized to access the server at this time. Please contact your server administrator for more information."
	seen := map[string]bool{}
	for _, row := range c.Web {
		seen[strings.Join([]string{string(rune('0' + row.Status/100)), string(rune('0' + row.Status/10%10)), string(rune('0' + row.Status%10)), row.RetryAfter}, "/")] = true
		want := map[int]string{401: invalid, 403: forbidden}[row.Status]
		switch {
		case want != "" && (row.Kind != "toast" || row.Shows != want):
			t.Errorf("jellyfin-web for %d (Retry-After %q) shows %s %q, want a toast %q", row.Status, row.RetryAfter, row.Kind, row.Shows, want)
		case want == "" && (row.Kind != "dialog" || !strings.HasPrefix(row.Shows, "Connection Failure")):
			t.Errorf("jellyfin-web for %d (Retry-After %q) shows %s %q, want the Connection Failure dialog", row.Status, row.RetryAfter, row.Kind, row.Shows)
		}
		if !row.OnSignInForm {
			t.Errorf("jellyfin-web for %d (Retry-After %q) left the sign-in form", row.Status, row.RetryAfter)
		}
	}
	if len(c.Web) != 8 || len(seen) != 8 {
		t.Errorf("the capture has %d rows (%d different), want 401, 403, 429 and 503 each with and without Retry-After", len(c.Web), len(seen))
	}
}

// A refusal's Retry-After is among the headers a page on another origin may read (D-016 gives it the three of a ranged read).
func TestARefusalsRetryAfterIsExposedToAPageOnAnotherOrigin(t *testing.T) {
	h := corsHandler(RoleAPI)
	for _, origin := range corsOrigins {
		r := corsAsk(h, "POST", "/Users/AuthenticateByName", "Origin", origin)
		exposed := strings.ToLower(r.Header().Get("Access-Control-Expose-Headers"))
		for _, want := range []string{"retry-after", "content-range", "accept-ranges", "content-length"} {
			if !strings.Contains(exposed, want) {
				t.Errorf("from %s Access-Control-Expose-Headers is %q, want it to name %s", origin, exposed, want)
			}
		}
	}
}

// Every route that takes a password is bounded the same way: this lists them, so that a new one must join the list (and
// the bound) before this passes. Today it is POST /Users/AuthenticateByName alone. A route takes a password when its path
// names one, or it says it checks a name and password itself.
func TestThePasswordRoutesAreListed(t *testing.T) {
	var found []string
	for _, route := range (&Server{}).Routes() {
		text := strings.ToLower(route.Path + " " + route.Why)
		if strings.Contains(text, "password") || strings.Contains(text, "authenticate") || strings.Contains(text, "pin") && strings.Contains(text, "forgot") {
			found = append(found, route.Method+" "+route.Path)
		}
	}
	sort.Strings(found)
	if want := []string{"POST /Users/AuthenticateByName"}; strings.Join(found, ",") != strings.Join(want, ",") {
		t.Errorf("the routes that take a password are %v, this check lists %v: a new one must be bounded as the sign-in is (signin_integration_test.go) and listed here", found, want)
	}
}

// The account's password and the unknown name's dummy are compared at one cost (the cost EnsureAdmin gives accounts), so the
// work does not tell a name that exists from one that does not. That one comparison is made is held by
// signin_work_integration_test.go.
func TestTheUnknownNamesDummyHashCostsWhatAnAccountsHashCosts(t *testing.T) {
	cost, err := bcrypt.Cost(dummyHash)
	if err != nil || cost != bcrypt.DefaultCost {
		t.Errorf("the dummy hash costs %d (%v); EnsureAdmin's costs %d", cost, err, bcrypt.DefaultCost)
	}
}

// compat/differences.yaml lists, against a differential request that reaches a refusal, that jellyscale refuses
// temporarily where Jellyfin has no bound by default, and that a set LoginAttemptsBeforeLockout is not honoured.
func TestDifferencesYamlListsTheRefusalAndTheLockoutNotHonoured(t *testing.T) {
	var entries []struct {
		Requests []string `yaml:"requests"`
		Kind     string   `yaml:"kind"`
		Reason   string   `yaml:"reason"`
		Paths    []string `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(read(t, "../../compat/differences.yaml")), &entries); err != nil {
		t.Fatal(err)
	}
	var requests []struct {
		Name  string `yaml:"name"`
		Route string `yaml:"route"`
	}
	if err := yaml.Unmarshal([]byte(read(t, "../../test/differential/requests.yaml")), &requests); err != nil {
		t.Fatal(err)
	}
	signIn := map[string]bool{}
	for _, r := range requests {
		if r.Route == "POST /Users/AuthenticateByName" {
			signIn[r.Name] = true
		}
	}
	var refusal, lockout bool
	for _, e := range entries {
		reason := strings.ToLower(e.Reason)
		reaches := false
		for _, name := range e.Requests {
			reaches = reaches || signIn[name]
		}
		if !reaches {
			continue
		}
		if strings.Contains(reason, "per name") && strings.Contains(reason, "per address") && strings.Contains(reason, "no bound") && e.Kind != "" {
			refusal = true
		}
		if strings.Contains(reason, "loginattemptsbeforelockout") && strings.Contains(reason, "not honoured") {
			lockout = true
		}
	}
	if !refusal || !lockout {
		t.Errorf("compat/differences.yaml has, against a request of test/differential/requests.yaml whose route is POST /Users/AuthenticateByName (%d of them), an entry saying jellyscale refuses temporarily per name and per address where Jellyfin has no bound by default: %v; and one saying a set LoginAttemptsBeforeLockout is not honoured: %v", len(signIn), refusal, lockout)
	}
}

// docs/access.md states the bounds and what a viewer and an administrator see; docs/running.md says which address counts
// behind an ingress and names the metrics. The words only: the numbers are held by the process checks.
func TestAccessDocStatesTheThreeBoundsTheirNumbersAndWhatViewersRead(t *testing.T) {
	text := read(t, "../../docs/access.md")
	lower := strings.ToLower(text)
	for _, word := range []string{"per name", "per known device", "per address", "15 minutes", "30 days", "1,920", "960", "/64", "quick connect",
		"signed in before", "retry-after", "403", "trustedproxies", "not authorized to access the server at this time", "try again later", "log", "metric"} {
		if !strings.Contains(lower, word) {
			t.Errorf("docs/access.md never says %q", word)
		}
	}
	for _, number := range []string{"20 ", "10 "} {
		if !strings.Contains(text, number) {
			t.Errorf("docs/access.md never says %q", number)
		}
	}
	// "Unknown names are remembered within a fixed bound stated in docs/access.md": a number of unknown names.
	if m := regexp.MustCompile(`(?i)\b(\d[\d,]*)\s+unknown\s+names?`).FindStringSubmatch(text); m == nil {
		t.Error("docs/access.md states no number of unknown names that are remembered (such as `100,000 unknown names`)")
	} else if n := strings.ReplaceAll(m[1], ",", ""); len(n) < 6 {
		t.Errorf("docs/access.md remembers %s unknown names, which is less than the 100,000 the criteria hold", m[1])
	}
}

func TestRunningDocSaysWhichAddressCountsBehindAnIngressAndNamesTheMetrics(t *testing.T) {
	lower := strings.ToLower(read(t, "../../docs/running.md"))
	for _, word := range []string{"trustedproxies", "x-forwarded-for", "ingress", "nearest", "alert", "jellyscale_signin_failures_total", "jellyscale_signin_refusals_total", "jellyscale_signin_address_bound"} {
		if !strings.Contains(lower, word) {
			t.Errorf("docs/running.md never says %q", word)
		}
	}
}

// D-022: before declaring `trustedProxies` an operator confirms that the address counted is each client's own (a failed
// sign-in from two devices shows two addresses in the log), and docs/running.md says what follows if it is not: one guesser
// or one mistyping viewer refuses everyone behind that address for 15 minutes.
func TestRunningDocSaysHowToConfirmTheCountedAddressAndWhatFollowsIfItIsShared(t *testing.T) {
	lower := strings.ToLower(read(t, "../../docs/running.md"))
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`two (different )?devices`),
		regexp.MustCompile(`two (different )?addresses`),
		regexp.MustCompile(`everyone (else )?behind`),
		regexp.MustCompile(`mistyp`),
	} {
		if !want.MatchString(lower) {
			t.Errorf("docs/running.md never says %q", want)
		}
	}
}

// D-022: docs/access.md, where it states the three bounds, says the per-address bound applies only where the server can
// attribute a request to one client's address: off with `trustedProxies` absent, and not applied to a request from a
// declared proxy that carries no usable forwarded entry.
func TestAccessDocSaysThePerAddressBoundAppliesOnlyWhereARequestHasOneClientsAddress(t *testing.T) {
	lower := strings.ToLower(read(t, "../../docs/access.md"))
	for _, want := range []string{"usable", "forwarded", "one client"} {
		if !strings.Contains(lower, want) {
			t.Errorf("docs/access.md never says %q", want)
		}
	}
}
