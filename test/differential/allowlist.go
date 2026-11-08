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
	Task     string   `yaml:"task"` // a gap names the task in tasks/prd.json that closes it
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

// CheckRequests fails if an entry names a request that reqs does not have.
func (a *Allowlist) CheckRequests(reqs []Request) error {
	known := map[string]bool{}
	for _, r := range reqs {
		known[r.Name] = true
	}
	for _, e := range a.entries {
		for _, name := range e.Requests {
			if !known[name] {
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
			if !a.failed[name] && !a.used[i][name] {
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
