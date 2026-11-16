package main

// JS-126: a request that says no_token leaves with no credential at all, and the tier decides it from the
// status alone. Names the task implies, in this package:
//
//	compareNoToken(jf, js *server, r differential.Request, allow *differential.Allowlist) error
//
// It asks both servers with the token first (both must answer 2xx: the request names something both have), then
// with none, and applies allow.CheckNoToken to the two statuses. A redirect is recorded as its 3xx, never followed.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"jellyscale/test/differential"
)

type arrival struct {
	method string
	header http.Header
	query  url.Values
	path   string
}

// credStub answers withToken when a request carries any credential and without when it carries none; a
// without of 302 redirects to /elsewhere, which would answer 200.
func credStub(t *testing.T, name string, withToken, without int, seen *[]arrival) *server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			w.WriteHeader(200)
			return
		}
		has := r.Header.Get("Authorization") != "" || r.Header.Get("X-Emby-Authorization") != "" ||
			r.Header.Get("X-Emby-Token") != "" || r.Header.Get("X-MediaBrowser-Token") != "" ||
			r.URL.Query().Get("ApiKey") != "" || r.URL.Query().Get("api_key") != ""
		if seen != nil {
			mu.Lock()
			*seen = append(*seen, arrival{r.Method, r.Header.Clone(), r.URL.Query(), r.URL.Path})
			mu.Unlock()
		}
		status := without
		if has {
			status = withToken
		}
		if status == 302 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return &server{name: name, base: srv.URL, token: "tok-1234", userID: "u1", ids: map[string]string{}, byKey: map[string]string{}}
}

func TestANoTokenRequestLeavesWithNoCredentialInAnySpelling(t *testing.T) {
	var seen []arrival
	s := credStub(t, "jellyscale", 200, 200, &seen)
	r := differential.Request{Name: "r", Method: "GET", Path: "/Videos/x/stream?static=true", NoToken: true}
	if _, _, err := s.answer(r); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("saw %d requests", len(seen))
	}
	a := seen[0]
	for _, h := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
		if a.header.Get(h) != "" {
			t.Errorf("a no-token request sent %s: %q", h, a.header.Get(h))
		}
	}
	for name := range a.query {
		if strings.EqualFold(name, "ApiKey") || strings.EqualFold(name, "api_key") {
			t.Errorf("a no-token request sent %s in the query", name)
		}
	}
	if a.query.Get("static") != "true" {
		t.Errorf("the query was lost: %v", a.query)
	}
}

func TestARequestThatDoesNotSayNoTokenStillCarriesTheToken(t *testing.T) {
	var seen []arrival
	s := credStub(t, "jellyscale", 200, 401, &seen)
	status, _, err := s.answer(differential.Request{Name: "r", Method: "GET", Path: "/Items"})
	if err != nil || status != 200 {
		t.Fatalf("status %d, %v", status, err)
	}
	if got := seen[0].header.Get("Authorization"); !strings.Contains(got, "tok-1234") {
		t.Errorf("the Authorization header is %q, want the token in it", got)
	}
}

func TestARedirectToANoTokenRequestIsRecordedAsItsStatusNotFollowed(t *testing.T) {
	s := credStub(t, "jellyfin", 200, 302, nil)
	status, _, err := s.answer(differential.Request{Name: "r", Method: "GET", Path: "/Items", NoToken: true})
	if err != nil || status != 302 {
		t.Fatalf("status %d, %v; want 302", status, err)
	}
}

func allowFor(t *testing.T, kind string) *differential.Allowlist {
	t.Helper()
	text := "[]"
	if kind != "" {
		text = "- requests: [r]\n  path: \"(status)\"\n  kind: " + kind + "\n  reason: a token is required\n"
		if kind == "gap" {
			text += "  task: JS-999\n" // a gap names the task that closes it, as the parser requires
		}
	}
	a, err := differential.ParseAllowlist(text)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestTheTierAsksBothWithTheTokenFirstAndDecidesNoTokenByStatus(t *testing.T) {
	r := differential.Request{Name: "r", Method: "GET", Path: "/x", NoToken: true}
	for _, c := range []struct {
		name                                 string
		jfWith, jfWithout, jsWith, jsWithout int
		entry                                string
		want                                 string // "" passes; else a word the failure holds
	}{
		{"same open answers", 200, 200, 200, 200, "", ""},
		{"same refusals", 200, 401, 200, 401, "", ""},
		{"a 401 with an entry", 200, 200, 200, 401, "improvement", ""},
		{"a 401 with no entry names both statuses", 200, 200, 200, 401, "", "401"},
		{"a 404 with an entry is not excused", 200, 200, 200, 404, "improvement", "404"},
		{"a 500 with an entry is not excused", 200, 200, 200, 500, "improvement", "500"},
		{"a 200 where Jellyfin refuses fails with an entry", 200, 401, 200, 200, "improvement", "200"},
		{"a 302 where Jellyfin refuses fails with a gap entry", 200, 403, 200, 302, "gap", "302"},
		{"a shared 404 fails", 200, 404, 200, 404, "", "404"},
		{"jellyfin has nothing for the token", 404, 404, 200, 200, "", "jellyfin"},
		{"jellyscale has nothing for the token", 200, 200, 404, 404, "", "jellyscale"},
		{"jellyscale answers the token with a 500", 200, 200, 500, 401, "improvement", "jellyscale"},
	} {
		t.Run(c.name, func(t *testing.T) {
			jf := credStub(t, "jellyfin", c.jfWith, c.jfWithout, nil)
			js := credStub(t, "jellyscale", c.jsWith, c.jsWithout, nil)
			err := compareNoToken(jf, js, r, allowFor(t, c.entry))
			switch {
			case c.want == "" && err != nil:
				t.Errorf("failed: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("got %v, want a failure naming %q", err, c.want)
			}
			if err != nil && c.want != "" && !strings.Contains(err.Error(), "r") {
				t.Errorf("%q does not name the request", err)
			}
		})
	}
}
