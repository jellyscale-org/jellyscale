package differential

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Entry is one known difference: where it is, and why it is allowed.
type Entry struct {
	// Requests are the names in requests.yaml this entry applies to; it hides
	// nothing in any other request, even one to the same route.
	Requests []string `yaml:"requests"`
	Route    string   `yaml:"-"` // not read: an entry has no route, only requests
	Path     string   `yaml:"path"`
	Kind     string   `yaml:"kind"` // improvement, non-goal or gap
	Task     string   `yaml:"task"` // a gap names the task in tasks/tasks that closes it
	Reason   string   `yaml:"reason"`
	// Jellyscale pins a "(ids)" path to the Ids jellyscale answers today: a
	// different list, a dropped, extra or reordered item, is a new difference.
	Jellyscale []string `yaml:"jellyscale"`
}

// Allowlist is compat/differences.yaml.
type Allowlist struct {
	entries []Entry
	used    []map[string]bool // per entry, the requests that needed it
	failed  map[string]bool   // requests that failed before their differences were filtered
}

var taskPattern = regexp.MustCompile(`^JS-\d+$`)

// listed is an entry as written: one path, or paths that share a kind and a reason.
type listed struct {
	Entry `yaml:",inline"`
	Paths []string `yaml:"paths"`
}

func ParseAllowlist(text string) (*Allowlist, error) {
	var written []listed
	if err := yaml.Unmarshal([]byte(text), &written); err != nil {
		return nil, err
	}
	var entries []Entry
	for _, w := range written {
		if w.Path != "" || len(w.Paths) == 0 {
			entries = append(entries, w.Entry)
		}
		for _, path := range w.Paths {
			e := w.Entry
			e.Path = path
			entries = append(entries, e)
		}
	}
	for i, e := range entries {
		where := fmt.Sprintf("entry %d (%s)", i+1, e.Path)
		switch {
		case len(e.Requests) == 0 || e.Path == "" || e.Reason == "":
			return nil, fmt.Errorf("%s: requests, path and reason are all required", where)
		case e.Kind != "improvement" && e.Kind != "non-goal" && e.Kind != "gap":
			return nil, fmt.Errorf("%s: kind must be improvement, non-goal or gap, not %q", where, e.Kind)
		case len(e.Jellyscale) > 0 && !strings.HasSuffix(e.Path, "(ids)"):
			return nil, fmt.Errorf("%s: jellyscale pins the Ids of a path ending in (ids)", where)
		case strings.HasSuffix(e.Path, "(ids)") && len(e.Jellyscale) == 0:
			return nil, fmt.Errorf("%s: an (ids) path must pin the Ids jellyscale answers, in jellyscale", where)
		case e.Kind == "gap" && !taskPattern.MatchString(e.Task):
			return nil, fmt.Errorf("%s: a gap needs the task that closes it, as JS-nnn", where)
		}
	}
	used := make([]map[string]bool, len(entries))
	for i := range used {
		used[i] = map[string]bool{}
	}
	return &Allowlist{entries: entries, used: used, failed: map[string]bool{}}, nil
}

func LoadAllowlist(path string) (*Allowlist, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseAllowlist(string(text))
}

func (a *Allowlist) Entries() []Entry { return a.entries }

var indexPattern = regexp.MustCompile(`\[\d+\]`)

// replayedElsewhere is the start of the names that list the cells of compat/captures/audio-default-12.4.json where
// jellyscale answers another DefaultAudioStreamIndex than Jellyfin. internal/api replays those cells, not this tier,
// so they are no request of requests.yaml and no entry for them is unused.
const replayedElsewhere = "audio default: "

// replayedByTheCorsCheck is the start of the names of the requests of compat/captures/cors-12.4.json, which
// internal/api/cors_integration_test.go replays (JS-144).
const replayedByTheCorsCheck = "cors: "

// replayedByTheSubtitleDefaultCheck is the start of the names of the cells of compat/captures/subtitles-default-12.4.json
// where jellyscale answers another DefaultSubtitleStreamIndex than Jellyfin, which internal/api replays (JS-041).
const replayedByTheSubtitleDefaultCheck = "subtitle default: "

// replayedByTheQuickConnectCheck is the start of the names of the steps of compat/captures/quick-connect-12.4.json,
// which cmd/jellyscale/quickconnect_capture_integration_test.go replays (JS-037).
const replayedByTheQuickConnectCheck = "quick connect: "

// heldByTheUsersChecks names the cases of compat/captures/users-admin-12.4.json where jellyscale answers otherwise than
// Jellyfin, which cmd/jellyscale's users checks hold (JS-033). Each is named, so that a new entry needs a check.
var heldByTheUsersChecks = []string{
	"users: a viewer lists the users",
	"users: the list filtered by isHidden",
	"users: new user with no Password",
	"users: new user with an empty Password",
	"users: new user with a commonly guessed password",
	"users: a commonly guessed new password",
	"users: an empty new password",
	"users: reset of one's own password",
	"users: an administrator resets another's password",
	"users: a user with no password changes their own",
	"users: a disabled account signs in",
	"users: an administrator is disabled",
	"users: the last administrator deletes themselves",
	"users: an administrator names the zero id",
	"users: a Policy body without every field",
	"users: a Policy that changes a field that is not enforced",
	"users: a Policy that names a channel field",
	"users: failed sign-ins and the lockout fields",
	"users: a rename with only a Name",
	"users: the configured administrator is deleted or renamed",
	"users: the lists the user pages read",
}

func replayed(name string) bool {
	return strings.HasPrefix(name, replayedElsewhere) || strings.HasPrefix(name, replayedByTheCorsCheck) ||
		strings.HasPrefix(name, replayedByTheSubtitleDefaultCheck) || strings.HasPrefix(name, replayedByTheQuickConnectCheck) ||
		slices.Contains(heldByTheUsersChecks, name)
}

// CheckRequests fails if an entry names a request that reqs does not have.
func (a *Allowlist) CheckRequests(reqs []Request) error {
	known := map[string]bool{}
	for _, r := range reqs {
		known[r.Name] = true
	}
	for _, e := range a.entries {
		for _, name := range e.Requests {
			if !known[name] && !replayed(name) {
				return fmt.Errorf("compat/differences.yaml: the entry for %s names the request %q, which requests.yaml does not have", e.Path, name)
			}
		}
	}
	return nil
}

// Filter returns the differences of a request that no entry for it lists. An
// entry matches the whole path, with [] standing for any index, or the path
// exactly ([1] names just that element).
func (a *Allowlist) Filter(request string, diffs []Difference) []Difference {
	var unlisted []Difference
	for _, d := range diffs {
		path := indexPattern.ReplaceAllString(d.Path, "[]")
		listed := false
		for i, e := range a.entries {
			if slices.Contains(e.Requests, request) && (e.Path == path || e.Path == d.Path) {
				// Used even when the pin fails: the entry is still needed,
				// its list is what is wrong.
				a.used[i][request] = true
				listed = listed || pins(e, d)
			}
		}
		if !listed {
			unlisted = append(unlisted, d)
		}
	}
	return unlisted
}

// Failed records a request that failed before its differences were filtered.
func (a *Allowlist) Failed(request string) { a.failed[request] = true }

// Unused returns each entry and request pair that matched nothing so far:
// differences that are gone. The Entry returned names that one request.
func (a *Allowlist) Unused() []Entry {
	var out []Entry
	for i, e := range a.entries {
		for _, name := range e.Requests {
			// A request that failed before its differences were filtered (an
			// error, no rows) says nothing about whether its entries still apply.
			if !a.failed[name] && !a.used[i][name] && !replayed(name) {
				e.Requests = []string{name}
				out = append(out, e)
			}
		}
	}
	return out
}

// pins says whether d is the one answer an (ids) entry pins, or e pins nothing.
func pins(e Entry, d Difference) bool {
	if e.Jellyscale == nil {
		return true
	}
	got, _ := d.Jellyscale.([]string)
	return slices.Equal(got, e.Jellyscale)
}

func openStatus(status int) bool    { return status >= 200 && status <= 399 }
func refusalStatus(status int) bool { return status == 401 || status == 403 }

// CheckNoToken decides a no-token request from the two statuses alone. Both must be an answer to a request
// with no token (2xx, 3xx, 401 or 403): a 404 or a 5xx is an error of either server. The same status passes.
// Jellyscale answering 401 where Jellyfin opens the request passes only with an entry for that request on
// the path (status), and that is all an entry excuses. Jellyscale answering what Jellyfin refuses never
// passes, whatever the entries say: jellyscale is never looser than Jellyfin.
func (a *Allowlist) CheckNoToken(request string, jellyfin, jellyscale int) error {
	for _, s := range []int{jellyfin, jellyscale} {
		if !openStatus(s) && !refusalStatus(s) {
			return fmt.Errorf("%s: with no token jellyfin answered %d and jellyscale %d; neither is an answer to a request with no token", request, jellyfin, jellyscale)
		}
	}
	switch {
	case jellyfin == jellyscale:
		return nil
	case refusalStatus(jellyfin) && openStatus(jellyscale):
		return fmt.Errorf("%s: with no token jellyfin answered %d and jellyscale %d; jellyscale is never looser than Jellyfin", request, jellyfin, jellyscale)
	case openStatus(jellyfin) && jellyscale == 401:
		found := false
		for i, e := range a.entries {
			if e.Path == "(status)" && slices.Contains(e.Requests, request) {
				a.used[i][request] = true
				found = true
			}
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("%s: with no token jellyfin answered %d and jellyscale %d, and compat/differences.yaml has no entry for it on the path (status) that excuses it", request, jellyfin, jellyscale)
}
