package differential

// JS-126: a request of requests.yaml can be sent with no token, and the tier compares what the two
// servers answer to it by status alone. Names the task implies:
//
//	Request.NoToken bool `yaml:"no_token"`
//	(*Allowlist).CheckNoToken(request string, jellyfin, jellyscale int) error
//
// Observed on jellyfin/jellyfin:12.4 (2026-11-16, one synthetic film with a text subtitle and an attached
// font, no token in any spelling): 200 for GET and HEAD /Videos/{id}/stream?static=true, GET stream.mkv,
// Subtitles/2/Stream.vtt and Subtitles/2/0/Stream.vtt, Attachments/3 (the font, 2543520 bytes), GET
// /Items/{id}/Images/Primary, /Users/Public and /System/Info/Public; 401 for master.m3u8, /Items/{id}/Download,
// /Items/{id}, /Users, /Items and /Sessions. X-Emby-Token alone was refused (401) on GET /Users/Me where
// Authorization: MediaBrowser Token=... and ?ApiKey=... were accepted.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// theTwelve are the requests that ask both servers with no token. The name is what compat/differences.yaml
// entries name; method and path say what each is.
var theTwelve = []struct {
	name, method string
	path         *regexp.Regexp
	query        string // must be in the path's query
}{
	{"no token: the static stream", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/stream\?`), "static=true"},
	{"no token: the static stream, HEAD", "HEAD", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/stream\?`), "static=true"},
	{"no token: stream.mkv", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/stream\.mkv`), ""},
	{"no token: the text subtitle", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/\{\{[^}]+\}\}/Subtitles/\d+/Stream\.vtt`), ""},
	{"no token: the text subtitle from an offset", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/\{\{[^}]+\}\}/Subtitles/\d+/[1-9]\d*/Stream\.vtt`), ""},
	{"no token: the font attachment", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/\{\{[^}]+\}\}/Attachments/\d+`), ""},
	{"no token: the master playlist", "GET", regexp.MustCompile(`^/Videos/\{\{[^}]+\}\}/master\.m3u8`), ""},
	{"no token: download", "GET", regexp.MustCompile(`^/Items/\{\{[^}]+\}\}/Download`), ""},
	{"no token: the item", "GET", regexp.MustCompile(`^/Items/\{\{[^}]+\}\}(\?|$)`), ""},
	{"no token: the poster", "GET", regexp.MustCompile(`^/Items/\{\{[^}]+\}\}/Images/Primary`), ""},
	{"no token: the public users", "GET", regexp.MustCompile(`^/Users/Public`), ""},
	{"no token: the public system info", "GET", regexp.MustCompile(`^/System/Info/Public`), ""},
}

func TestARequestCanSayItIsSentWithNoToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.yaml")
	text := "- name: a\n  path: /Users/Public\n  no_token: true\n- name: b\n  path: /Users/Public\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs, err := LoadRequests(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reqs[0].NoToken || reqs[1].NoToken {
		t.Fatalf("got %+v", reqs)
	}
}

func TestTheTwelveRequestsAskedWithNoTokenAreInTheListAndSayNoToken(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Request{}
	for _, r := range reqs {
		byName[r.Name] = r
	}
	for _, want := range theTwelve {
		r, ok := byName[want.name]
		if !ok {
			t.Errorf("requests.yaml has no request named %q", want.name)
			continue
		}
		if !r.NoToken {
			t.Errorf("%q does not say no_token: true", want.name)
		}
		if r.Method != want.method || !want.path.MatchString(r.Path) || !strings.Contains(r.Path, want.query) {
			t.Errorf("%q is %s %s, want %s %s with %q", want.name, r.Method, r.Path, want.method, want.path, want.query)
		}
	}
}

func allowlistOf(t *testing.T, kind string) *Allowlist {
	t.Helper()
	text := "- requests: [r]\n  path: \"(status)\"\n  kind: " + kind + "\n  reason: jellyscale requires a token here\n"
	if kind == "gap" {
		text += "  task: JS-999\n"
	}
	a, err := ParseAllowlist(text)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestANoTokenRequestIsComparedByItsStatusAlone(t *testing.T) {
	a, _ := ParseAllowlist("[]")
	for _, c := range []struct {
		jf, js int
		ok     bool
	}{
		{200, 200, true}, {206, 206, true}, {302, 302, true}, {401, 401, true}, {403, 403, true},
		{404, 404, false}, {500, 500, false}, {400, 400, false},
	} {
		err := a.CheckNoToken("r", c.jf, c.js)
		if (err == nil) != c.ok {
			t.Errorf("jellyfin %d, jellyscale %d: got %v, want ok=%v", c.jf, c.js, err, c.ok)
		}
	}
}

func TestStatusesThatDifferWithNoEntryFailNamingTheRequestAndBothStatuses(t *testing.T) {
	a, _ := ParseAllowlist("[]")
	err := a.CheckNoToken("no token: the static stream", 200, 401)
	if err == nil {
		t.Fatal("200 against 401 with no entry passed")
	}
	for _, want := range []string{"no token: the static stream", "200", "401"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not name %q", err, want)
		}
	}
}

func TestAnEntryExcusesJellyscales401AndNothingElse(t *testing.T) {
	a := allowlistOf(t, "improvement")
	if err := a.CheckNoToken("r", 200, 401); err != nil {
		t.Errorf("Jellyfin 200, jellyscale 401 with an entry: %v", err)
	}
	// An entry for another request excuses nothing here.
	if err := a.CheckNoToken("other", 200, 401); err == nil {
		t.Error("an entry for r excused the request other")
	}
	for _, c := range [][2]int{{200, 404}, {200, 500}, {200, 302}, {302, 200}, {404, 401}, {401, 404}, {200, 206}} {
		if err := a.CheckNoToken("r", c[0], c[1]); err == nil {
			t.Errorf("jellyfin %d, jellyscale %d passed with an entry", c[0], c[1])
		}
	}
}

// Never looser than Jellyfin: no kind of entry lets jellyscale answer a request that Jellyfin refuses.
func TestJellyscaleOpeningWhatJellyfinRefusesFailsWhateverEntryThereIs(t *testing.T) {
	for _, kind := range []string{"improvement", "non-goal", "gap"} {
		a := allowlistOf(t, kind)
		for _, js := range []int{200, 206, 302} {
			for _, jf := range []int{401, 403} {
				if err := a.CheckNoToken("r", jf, js); err == nil {
					t.Errorf("a %s entry let jellyscale answer %d where Jellyfin answers %d", kind, js, jf)
				}
			}
		}
	}
	a, _ := ParseAllowlist("[]")
	if err := a.CheckNoToken("r", 403, 200); err == nil {
		t.Error("jellyscale 200 against Jellyfin 403 with no entry passed")
	}
}
