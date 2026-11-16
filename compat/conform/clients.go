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
	// PlayerKinds are the requests a client's player builds, one entry of each in Client.Player.
	PlayerKinds = []string{"direct-play", "hls", "subtitle", "picture"}
)

type Clients []Client

// Client is one SDK client: the version or commit its requests were read at.
type Client struct {
	Name    string                     `yaml:"name"`
	Version string                     `yaml:"version"`
	Paths   map[string][]ClientRequest `yaml:"paths"`
	Player  []PlayerRequest            `yaml:"player"`
}

// PlayerRequest is a request a client's player or image loader builds as a URL (JS-126), with how the
// token travels. Token is "header", "query" or "none"; TokenName is the header (Authorization,
// X-Emby-Authorization, X-Emby-Token, X-MediaBrowser-Token) or the query name (ApiKey, api_key).
// URLFrom is "client" when the client builds the URL, "server" when it plays one the server gave it
// (TranscodingUrl, DeliveryUrl). File and Line are the source read, relative to the client's repository.
// Device is set only where the request was played on a device. NotRequested says the client never
// requests this kind, and then Method and Route are empty.
type PlayerRequest struct {
	Kind         string              `yaml:"kind"`
	Method       string              `yaml:"method"`
	Route        string              `yaml:"route"`
	Query        map[string][]string `yaml:"query"`
	NotRequested bool                `yaml:"not_requested"`
	Token        string              `yaml:"token"`
	TokenName    string              `yaml:"token_name"`
	URLFrom      string              `yaml:"url_from"`
	File         string              `yaml:"file"`
	Line         int                 `yaml:"line"`
	Device       string              `yaml:"device"`
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
