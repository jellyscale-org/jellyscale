//go:build integration

package main

import (
	"regexp"
	"testing"

	"jellyscale/internal/testenv"
)

// A right password leaves no failure behind, also when the statement that gives its reservation back is cut, before
// Postgres runs it or after (JS-145 evidenced finding): 20 wrong passwords after it are still 20 x 401 and then 403.
func TestARightPasswordWhoseReleaseIsCutLeavesNoFailure(t *testing.T) {
	releases := regexp.MustCompile(`DELETE FROM signin_failures WHERE id = ANY`)
	for _, when := range []string{"before", "after"} {
		t.Run(when, func(t *testing.T) {
			proxy := testenv.NewDBProxy(t, testDatabaseURL(t))
			s := newSignInSiteVia(t, 1, nil, proxy.DSN()+"&default_query_exec_mode=exec")
			name := s.account("release")
			cut := proxy.Sever
			match := func(sent []byte) bool { return releases.Match(sent) }
			if when == "before" {
				proxy.OnStatement(match, cut, nil)
			} else {
				proxy.OnStatement(match, nil, cut)
			}
			if got := s.try(0, newDevice(), name, rightPassword); got.status != 200 {
				t.Fatalf("the right password, release cut %s Postgres: %d %q, want 200", when, got.status, got.body)
			}
			for i := 1; i <= 20; i++ {
				if got := s.wrong(0, name); got.status != 401 {
					t.Fatalf("wrong password %d after it: %d, want 401", i, got.status)
				}
			}
			if got := s.wrong(0, name); got.status != 403 {
				t.Fatalf("the 21st wrong password: %d, want 403", got.status)
			}
		})
	}
}
