// Package conform holds JSON responses to Jellyfin 12.4's OpenAPI schema
// (JS-070): the differential tier checks jellyscale's answers with it, and the
// e2e gates check what jellyfin-web receives, through cmd/conform.
package conform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Node is one type of the schema file, cut down from the OpenAPI document to
// what the checks read.
type Node struct {
	Type     string           `json:"type,omitempty"`
	Format   string           `json:"format,omitempty"`
	Nullable bool             `json:"nullable,omitempty"`
	Enum     []string         `json:"enum,omitempty"`
	Ref      string           `json:"ref,omitempty"`
	Items    *Node            `json:"items,omitempty"`
	Props    map[string]*Node `json:"props,omitempty"`
	Required []string         `json:"required,omitempty"`
	// Values types the values of a map; an object with neither Props nor Values takes anything.
	Values *Node `json:"values,omitempty"`

	optional bool // nullable or with a default: the property may be left out
}

// Schema is the checked-in file (compat/schema-12.4.json).
type Schema struct {
	Image   string `json:"image"`   // the container the document came from
	Version string `json:"version"` // info.version of the document
	// Types are the named schemas. A property is required in one when the document
	// lists it or when it is not nullable and has no default, which is how the
	// Kotlin SDK reads the document; Required holds both. An object written inline
	// (BaseItemDto.ImageBlurHashes) requires only what it lists.
	Types map[string]*Node `json:"types"`
	// Operations maps "METHOD /route/{template}" to the JSON type of each status it declares.
	Operations map[string]map[string]*Node `json:"operations"`

	ops []operation
}

type operation struct {
	key, method string
	template    template
}

// FromOpenAPI makes the schema file from an OpenAPI document and the image it came from.
func FromOpenAPI(doc []byte, image string) ([]byte, error) {
	var d struct {
		Info  struct{ Version string }
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct{ Schema json.RawMessage }
			}
		}
		Components struct{ Schemas map[string]json.RawMessage }
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the OpenAPI document: %w", err)
	}
	if d.Info.Version == "" || len(d.Paths) == 0 {
		return nil, fmt.Errorf("the OpenAPI document has no version or no paths")
	}
	s := Schema{Image: image, Version: d.Info.Version, Types: map[string]*Node{}, Operations: map[string]map[string]*Node{}}
	for name, raw := range d.Components.Schemas {
		n, err := convert(raw)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", name, err)
		}
		for field, p := range n.Props {
			if !p.optional && !slices.Contains(n.Required, field) {
				n.Required = append(n.Required, field)
			}
		}
		slices.Sort(n.Required)
		s.Types[name] = n
	}
	for path, methods := range d.Paths {
		for method, op := range methods {
			method = strings.ToUpper(method)
			if !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "HEAD", "PATCH", "OPTIONS"}, method) {
				continue
			}
			statuses := map[string]*Node{}
			for status, resp := range op.Responses {
				for _, ct := range jsonFirst(resp.Content) {
					n, err := convert(resp.Content[ct].Schema)
					if err != nil {
						return nil, fmt.Errorf("%s %s %s: %w", method, path, status, err)
					}
					statuses[status] = n
					break
				}
			}
			s.Operations[method+" "+path] = statuses
		}
	}
	out, err := json.MarshalIndent(&s, "", " ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// jsonFirst lists the JSON media types of a response, application/json first.
func jsonFirst[V any](content map[string]V) []string {
	var types []string
	for ct := range content {
		if strings.HasPrefix(ct, "application/json") {
			types = append(types, ct)
		}
	}
	slices.SortFunc(types, func(a, b string) int {
		if (a == "application/json") != (b == "application/json") {
			if a == "application/json" {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return types
}

type rawNode struct {
	Ref                  string                     `json:"$ref"`
	AllOf                []json.RawMessage          `json:"allOf"`
	Type                 string                     `json:"type"`
	Format               string                     `json:"format"`
	Nullable             bool                       `json:"nullable"`
	Default              json.RawMessage            `json:"default"`
	Enum                 []any                      `json:"enum"`
	Items                json.RawMessage            `json:"items"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
}

// convert reads one OpenAPI schema. A property written as an allOf of one
// reference, with its own nullable, is the way the document says "that type, or null".
func convert(raw json.RawMessage) (*Node, error) {
	var r rawNode
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	n := &Node{Type: r.Type, Format: r.Format, Nullable: r.Nullable, Required: r.Required}
	n.optional = r.Nullable || r.Default != nil
	if r.Ref != "" {
		n.Ref = strings.TrimPrefix(r.Ref, "#/components/schemas/")
	}
	for _, part := range r.AllOf {
		p, err := convert(part)
		if err != nil {
			return nil, err
		}
		if p.Ref != "" && n.Ref == "" {
			n.Ref = p.Ref
		} else if p.Ref != "" || p.Type != "" {
			return nil, fmt.Errorf("allOf of more than one type")
		}
	}
	for _, e := range r.Enum {
		n.Enum = append(n.Enum, fmt.Sprint(e))
	}
	if r.Items != nil {
		items, err := convert(r.Items)
		if err != nil {
			return nil, err
		}
		n.Items = items
	}
	if len(r.Properties) > 0 {
		n.Props = map[string]*Node{}
		for name, p := range r.Properties {
			pn, err := convert(p)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			n.Props[name] = pn
		}
	}
	if ap := bytes.TrimSpace(r.AdditionalProperties); len(ap) > 0 && ap[0] == '{' && string(ap) != "{}" {
		values, err := convert(r.AdditionalProperties)
		if err != nil {
			return nil, err
		}
		n.Values = values
	}
	return n, nil
}

// ParseSchema reads the checked-in file.
func ParseSchema(file []byte) (*Schema, error) {
	var s Schema
	if err := json.Unmarshal(file, &s); err != nil {
		return nil, fmt.Errorf("the schema file: %w", err)
	}
	if s.Image == "" || s.Version == "" || len(s.Operations) == 0 {
		return nil, fmt.Errorf("the schema file records no image, version or operations")
	}
	for key := range s.Operations {
		method, path, _ := strings.Cut(key, " ")
		s.ops = append(s.ops, operation{key, method, parseTemplate(path)})
	}
	slices.SortFunc(s.ops, func(a, b operation) int { return strings.Compare(a.key, b.key) })
	return &s, nil
}

// Drift says whether a checked-in file differs from one just generated: a hand
// edit, or a Jellyfin that has changed.
func Drift(checkedIn, generated []byte) error {
	if bytes.Equal(checkedIn, generated) {
		return nil
	}
	a, b := strings.Split(string(checkedIn), "\n"), strings.Split(string(generated), "\n")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return fmt.Errorf("compat/schema-12.4.json differs from the document of the running Jellyfin at line %d (%q, generated %q); run compat/gen-schema.sh", i+1, strings.TrimSpace(a[i]), strings.TrimSpace(b[i]))
		}
	}
	return fmt.Errorf("compat/schema-12.4.json has %d lines, the document of the running Jellyfin gives %d; run compat/gen-schema.sh", len(a), len(b))
}

// Requires says whether a field of a named type is required, however the document marks it.
func (s *Schema) Requires(typeName, field string) bool {
	t := s.Types[typeName]
	return t != nil && slices.Contains(t.Required, field)
}

// Operation finds the operation of a request line, whatever the case of the path
// and the names of its parameters: a segment in braces matches any segment, and
// a literal segment is preferred to one. It returns the route as the document writes it.
func (s *Schema) Operation(method, path string) (route string, ok bool) {
	key, ok := s.operationKey(method, path)
	return key, ok
}

func (s *Schema) operationKey(method, path string) (string, bool) {
	segments := split(path)
	best, bestScore := "", -1
	for _, op := range s.ops {
		if op.method != strings.ToUpper(method) {
			continue
		}
		if score, ok := op.template.match(segments); ok && score > bestScore {
			best, bestScore = op.key, score
		}
	}
	return best, bestScore >= 0
}

// split cuts a path (a query is dropped) into its lower-case segments.
func split(path string) []string {
	path, _, _ = strings.Cut(path, "?")
	return strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
}

// A template is a route as the document writes it. A segment is a literal, a parameter
// ("{itemId}", any segment), or a literal with a parameter in it ("Stream.{routeFormat}").
type template []segment

type segment struct {
	literal string
	re      *regexp.Regexp // set for a segment with a parameter in it
	param   bool           // the whole segment is one
}

var paramPattern = regexp.MustCompile(`\{[^}]*\}`)

func parseTemplate(path string) template {
	var t template
	for _, text := range split(path) {
		switch {
		case !strings.Contains(text, "{"):
			t = append(t, segment{literal: text})
		case paramPattern.ReplaceAllString(text, "") == "":
			t = append(t, segment{param: true})
		default:
			parts := paramPattern.Split(text, -1)
			for i, part := range parts {
				parts[i] = regexp.QuoteMeta(part)
			}
			t = append(t, segment{re: regexp.MustCompile("^" + strings.Join(parts, "[^/]+") + "$")})
		}
	}
	return t
}

// match says whether the template fits a path (already split), and how closely: literal segments
// count most, leftmost most of all, so that /Items/Suggestions beats /Items/{itemId}.
func (t template) match(path []string) (int, bool) {
	if len(t) != len(path) {
		return 0, false
	}
	score := 0
	for i, seg := range t {
		weight := 0
		switch {
		case seg.param:
			if path[i] == "" {
				return 0, false
			}
		case seg.re != nil:
			if !seg.re.MatchString(path[i]) {
				return 0, false
			}
			weight = 1
		case seg.literal == path[i]:
			weight = 2
		default:
			return 0, false
		}
		score += weight << (len(t) - i)
	}
	return score, true
}
