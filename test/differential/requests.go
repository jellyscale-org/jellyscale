package differential

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Request is one line of requests.yaml, sent to both servers.
type Request struct {
	Name   string `yaml:"name"`
	Route  string `yaml:"route"` // the endpoint template, as in compat/endpoints.yaml
	Method string `yaml:"method"`
	// Path is the request line with the query jellyfin-web sends. {{Type/Name}}
	// stands for the id the server has given that item.
	Path string `yaml:"path"`
	Body string `yaml:"body"` // JSON, may use {{Type/Name}} too
	// Ordered says the route promises an order (a sorted listing, a playlist),
	// so the order of arrays is compared.
	Ordered bool `yaml:"ordered"`
}

func LoadRequests(path string) ([]Request, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reqs []Request
	if err := yaml.Unmarshal(text, &reqs); err != nil {
		return nil, err
	}
	// Allowlist entries name requests, so a repeated name would let an entry
	// for one request hide a difference in the other.
	seen := map[string]bool{}
	for _, r := range reqs {
		if seen[r.Name] {
			return nil, fmt.Errorf("%s: two requests are named %q", path, r.Name)
		}
		seen[r.Name] = true
	}
	return reqs, nil
}
