package conform

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Rules are compat/conform.yaml: what the schema cannot say by itself.
type Rules struct {
	// Aliases are routes Jellyfin 12.4 dropped from its document that jellyscale
	// still serves; an answer on one is checked as the route it stands for.
	Aliases []Alias `yaml:"aliases"`
	// Unchecked are routes whose JSON answer the schema does not type; each says why.
	Unchecked []Unchecked `yaml:"unchecked"`
}

type Alias struct {
	Method    string `yaml:"method"`
	Route     string `yaml:"route"`
	StandsFor string `yaml:"stands_for"`
}

type Unchecked struct {
	Method string `yaml:"method"`
	Route  string `yaml:"route"`
	Reason string `yaml:"reason"`
}

func LoadRules(path string) (Rules, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Rules{}, err
	}
	var r Rules
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return Rules{}, fmt.Errorf("%s: %w", path, err)
	}
	for _, u := range r.Unchecked {
		if strings.TrimSpace(u.Reason) == "" {
			return Rules{}, fmt.Errorf("%s: the unchecked route %s %s has no reason", path, u.Method, u.Route)
		}
	}
	return r, nil
}

// AliasFor is the route a request line stands for, if an alias covers it.
func (r Rules) AliasFor(method, path string) (target string, ok bool) {
	for _, a := range r.Aliases {
		if strings.EqualFold(a.Method, method) && sameRoute(a.Route, path) {
			return a.StandsFor, true
		}
	}
	return "", false
}

func (r Rules) unchecked(method, path string) bool {
	for _, u := range r.Unchecked {
		if strings.EqualFold(u.Method, method) && sameRoute(u.Route, path) {
			return true
		}
	}
	return false
}

// sameRoute says whether a request path fits a route template, whatever the case.
func sameRoute(route, path string) bool {
	_, ok := parseTemplate(route).match(split(path))
	return ok
}
