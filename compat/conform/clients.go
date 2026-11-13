package conform

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientNames and PathNames are the cells of compat/clients.yaml: each client
// has requests on each of its main paths.
var (
	ClientNames = []string{"Findroid", "Android TV", "Swiftfin", "Streamyfin"}
	PathNames   = []string{"startup", "sign-in", "home", "browse", "detail", "playback", "progress", "stop"}
)

type Clients []Client

// Client is one SDK client: the version or commit its requests were read at.
type Client struct {
	Name    string                     `yaml:"name"`
	Version string                     `yaml:"version"`
	Paths   map[string][]ClientRequest `yaml:"paths"`
}

// ClientRequest is one request a client sends. Route is the template as in
// compat/endpoints.yaml; Query maps each name as the client's SDK spells it to
// sample values; Auth is "mediabrowser" (Authorization: MediaBrowser ...), "emby"
// (X-Emby-Authorization) or "none"; From is "source" or "traffic".
type ClientRequest struct {
	Method string              `yaml:"method"`
	Route  string              `yaml:"route"`
	Query  map[string][]string `yaml:"query"`
	Body   string              `yaml:"body"`
	Auth   string              `yaml:"auth"`
	From   string              `yaml:"from"`
}

func LoadClients(path string) (Clients, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c struct{ Clients Clients }
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c.Clients, nil
}

func (c Clients) Find(name string) *Client {
	for i := range c {
		if c[i].Name == name {
			return &c[i]
		}
	}
	return nil
}

// Request is the client's request on a path, or nil when the cell has none.
func (c *Client) Request(path, method, route string) *ClientRequest {
	for i, r := range c.Paths[path] {
		if r.Method == method && strings.EqualFold(r.Route, route) {
			return &c.Paths[path][i]
		}
	}
	return nil
}
