// Package contract holds the published contract of jellyscale's own API.
package contract

import (
	_ "embed"
	"errors"
	"fmt"

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

// Operation is one method of one path. State is declared, preview or stable (D-134).
type Operation struct {
	State      string `yaml:"x-state"`
	Capability string `yaml:"x-capability"`
}

// Reason is one entry of the closed list of reasons a client may be told.
type Reason struct {
	Kind   string `yaml:"kind"`
	Action string `yaml:"action"`
}

// Capability is one thing a client may build against, with the state its design supports.
type Capability struct {
	ID    string `yaml:"id"`
	State string `yaml:"state"`
}

// Contract is a parsed document. Raw is what the server serves to clients.
type Contract struct {
	OpenAPI      string                          `yaml:"openapi"`
	Paths        map[string]map[string]Operation `yaml:"paths"`
	Reasons      map[string]Reason               `yaml:"x-reasons"`
	Capabilities map[string]Capability           `yaml:"x-capabilities"`
	Raw          []byte                          `yaml:"-"`
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
