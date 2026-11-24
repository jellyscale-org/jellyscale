package api

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// JS-145 (D-020), the checks that need nothing outside Go: what a page may read of a refusal, and the cost of the dummy
// hash an unknown name is compared with.

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

// The account's password and the unknown name's dummy are compared at one cost (the cost EnsureAdmin gives accounts), so the
// work does not tell a name that exists from one that does not. That one comparison is made is held by
// signin_work_integration_test.go.
func TestTheUnknownNamesDummyHashCostsWhatAnAccountsHashCosts(t *testing.T) {
	cost, err := bcrypt.Cost(dummyHash)
	if err != nil || cost != bcrypt.DefaultCost {
		t.Errorf("the dummy hash costs %d (%v); EnsureAdmin's costs %d", cost, err, bcrypt.DefaultCost)
	}
}
