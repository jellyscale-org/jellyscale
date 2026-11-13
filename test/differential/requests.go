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
	// Rows says the request is meant to return items on the fixtures, so an
	// empty list from either server fails: a route or fixture broken on both
	// would otherwise agree.
	Rows bool `yaml:"rows"`
	// Play names a PlaybackInfo request. Before this one, the device plays what it
	// answers: it fetches the transcode's playlists and first segment and reports
	// the start of playback, so that a session has something to say.
	Play string `yaml:"play"`
	// Only keeps the differences under that key of each array element (the report
	// of everything else a session says belongs to the tasks that build it).
	Only string `yaml:"only"`
	// Status is the status both servers must answer; 0 means any 2xx.
	Status int `yaml:"status"`
	// Kotlin is the model of org.jellyfin.sdk.model.api that jellyscale's answer is
	// decoded as by jellyfin-sdk-kotlin's own serializers ("BaseItemDto", "SessionInfoDto[]").
	Kotlin string `yaml:"kotlin"`
	// DecodeOnly says both servers are asked and held to the schema, and jellyscale's answer is
	// decoded, but the two answers are not compared: they differ in ways other tasks build away
	// (the streams a scan lists), and the request is here for what the SDK decodes.
	DecodeOnly bool `yaml:"decode_only"`
}

// CheckAnswer says why one server's answer is not what r expects, or nil.
func CheckAnswer(r Request, status int, doc any) error {
	if r.Status == 0 && (status < 200 || status > 299) {
		return fmt.Errorf("answered %d, want 2xx", status)
	}
	if r.Status != 0 && status != r.Status {
		return fmt.Errorf("answered %d, want %d", status, r.Status)
	}
	if !r.Rows || status >= 300 {
		return nil
	}
	var list []any
	switch doc := doc.(type) {
	case map[string]any:
		list, _ = doc["Items"].([]any)
	case []any:
		list = doc
	}
	for _, row := range list {
		if row != "" { // an empty body reaches here as one blank line
			return nil
		}
	}
	return fmt.Errorf("answered no rows")
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
