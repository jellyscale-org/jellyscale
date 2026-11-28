// Package contract holds the published contract of jellyscale's own API.
package contract

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var embedded []byte

// Document is the contract this build publishes, byte for byte. The image check compares it with what the server serves.
func Document() []byte { return embedded }

// Default is the parsed contract of this build.
func Default() *Contract {
	parsed, err := Parse(embedded)
	if err != nil {
		panic(err)
	}
	return parsed
}

// Response is one answer an operation declares. A response may be a $ref to components/responses, but either way it
// names the reasons a server may send with that status, including reasons in a 200 body.
type Response struct {
	Ref     string   `yaml:"$ref"`
	Reasons []string `yaml:"x-reasons"`
}

// Operation is one method of one path. State is declared, preview or stable (D-134).
type Operation struct {
	State      string              `yaml:"x-state"`
	Capability string              `yaml:"x-capability"`
	Responses  map[string]Response `yaml:"responses"`
}

// Components holds the responses operations refer to by $ref.
type Components struct {
	Responses map[string]Response `yaml:"responses"`
}

// Reason is one entry of the closed list of reasons a client may be told.
type Reason struct {
	Kind   string `yaml:"kind"`
	Action string `yaml:"action"`
	// Locked marks a transport reason, or one of a stable capability: it is added within a version, never reworded.
	Locked bool `yaml:"locked"`
}

// Capability is one thing a client may build against, with the state its design supports.
type Capability struct {
	ID      string `yaml:"id"`
	State   string `yaml:"state"`
	Retires bool   `yaml:"x-retires-at-cutover"`
}

// Contract is a parsed document. Raw is what the server serves to clients.
type Contract struct {
	OpenAPI      string                          `yaml:"openapi"`
	Paths        map[string]map[string]Operation `yaml:"paths"`
	Components   Components                      `yaml:"components"`
	Reasons      map[string]Reason               `yaml:"x-reasons"`
	Capabilities map[string]Capability           `yaml:"x-capabilities"`
	Raw          []byte                          `yaml:"-"`
}

// ResponseReasons follows a response's $ref and returns the reasons it declares.
func (c *Contract) ResponseReasons(response Response) []string {
	reasons := slices.Clone(response.Reasons)
	for range 8 {
		if response.Ref == "" {
			return reasons
		}
		name := strings.TrimPrefix(response.Ref, "#/components/responses/")
		found, ok := c.Components.Responses[name]
		if !ok {
			return reasons
		}
		for _, reason := range found.Reasons {
			if !slices.Contains(reasons, reason) {
				reasons = append(reasons, reason)
			}
		}
		response = found
	}
	return reasons
}

// DeclaredReasons is the closed set of reasons one status of one operation may carry.
func (c *Contract) DeclaredReasons(method, path, status string) []string {
	methods, ok := c.Paths[path]
	if !ok {
		return nil
	}
	operation, ok := methods[strings.ToLower(method)]
	if !ok {
		return nil
	}
	response, ok := operation.Responses[status]
	if !ok {
		return nil
	}
	return c.ResponseReasons(response)
}

// Parse reads a contract. A replica that serves no v1 route may hold one with no paths at all, but every document needs
// an OpenAPI version and the closed list of reasons.
func Parse(document []byte) (*Contract, error) {
	var parsed Contract
	if err := yaml.Unmarshal(document, &parsed); err != nil {
		return nil, fmt.Errorf("the contract is not YAML: %w", err)
	}
	if parsed.OpenAPI == "" || len(parsed.Reasons) == 0 {
		return nil, errors.New("the contract lacks an openapi version or reasons")
	}
	parsed.Raw = document
	return &parsed, nil
}
