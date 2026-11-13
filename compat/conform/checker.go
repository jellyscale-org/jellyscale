package conform

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Violation is one place where an answer does not fit the schema.
type Violation struct {
	Request  string // what the answer was to: a request name, "sign-in"
	Route    string // the operation, "POST /Users/AuthenticateByName"
	JSONPath string // of the object holding the field, "$.User.Policy"
	Field    string
	Message  string
}

func (v Violation) String() string {
	route := v.Route
	if route == "" {
		route = "(no route)"
	}
	return fmt.Sprintf("%s: %s: %s field %s: %s", v.Request, route, v.JSONPath, v.Field, v.Message)
}

// Checker holds answers to a Schema and counts what it checked.
type Checker struct {
	schema    *Schema
	rules     Rules
	responses int
	routes    map[string]bool
}

func NewChecker(s *Schema, r Rules) *Checker {
	return &Checker{schema: s, rules: r, routes: map[string]bool{}}
}

// Counts says how many JSON responses were checked, and on how many routes.
func (c *Checker) Counts() (responses, routes int) { return c.responses, len(c.routes) }

// Summary is the line a run prints; a run that checked nothing is an error.
func (c *Checker) Summary(label string) (string, error) {
	n, routes := c.Counts()
	if n == 0 {
		return "", fmt.Errorf("%s: checked no JSON responses", label)
	}
	return fmt.Sprintf("%s: checked %d JSON responses on %d routes", label, n, routes), nil
}

// Check holds one answer to the schema. path may carry a query. An answer that
// is not JSON and has no declared JSON type is not checked.
func (c *Checker) Check(request, method, path string, status int, contentType string, body []byte) []Violation {
	method = strings.ToUpper(method)
	isJSON := strings.Contains(strings.ToLower(contentType), "json")
	fail := func(route, field, format string, args ...any) []Violation {
		return []Violation{{Request: request, Route: route, JSONPath: "$", Field: field, Message: fmt.Sprintf(format, args...)}}
	}
	if c.rules.unchecked(method, path) {
		return nil
	}
	key, ok := c.schema.operationKey(method, path)
	if !ok {
		if target, aliased := c.rules.AliasFor(method, path); aliased {
			key, ok = c.schema.operationKey(method, target)
		}
	}
	if !ok {
		if !isJSON {
			return nil
		}
		return fail(method+" "+path, "(route)", "a JSON answer on a route that is no operation of the schema, and not in compat/conform.yaml")
	}
	node := c.schema.Operations[key][strconv.Itoa(status)]
	if node == nil {
		node = c.schema.Operations[key]["default"]
	}
	if node == nil {
		if !isJSON {
			return nil
		}
		return fail(key, "(status)", "a JSON answer with status %d, which the operation declares no JSON type for", status)
	}
	success := status >= 200 && status < 300
	if !isJSON && !success {
		return nil
	}
	c.responses++
	c.routes[key] = true
	if !isJSON {
		return fail(key, "(body)", "no JSON body (content type %q), where the schema gives one", contentType)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return fail(key, "(body)", "no JSON body, where the schema gives one: %v", err)
	}
	var out []Violation
	v := validator{schema: c.schema, request: request, route: key, out: &out}
	v.check(node, value, "$", "")
	return out
}

type validator struct {
	schema  *Schema
	request string
	route   string
	out     *[]Violation
}

func (v validator) report(path, field, format string, args ...any) {
	*v.out = append(*v.out, Violation{Request: v.request, Route: v.route, JSONPath: path, Field: field, Message: fmt.Sprintf(format, args...)})
}

// wrong places a wrong value: as a field of its object, or else as the value itself.
func (v validator) wrong(path, field, format string, args ...any) {
	if field == "" {
		v.report(path, "(value)", format, args...)
		return
	}
	v.report(path, field, format, args...)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$|^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// check holds value to node. path is the path of the object holding it, and
// field its name there ("" for the top and for array elements, whose path
// then already ends in their index).
func (v validator) check(node *Node, value any, path, field string) {
	here := path
	if field != "" {
		here = path + "." + field
	}
	if value == nil {
		if !node.Nullable {
			v.wrong(path, field, "null where the schema does not allow it")
		}
		return
	}
	if node.Ref != "" {
		target := v.schema.Types[node.Ref]
		if target == nil {
			v.wrong(path, field, "the schema has no type %s", node.Ref)
			return
		}
		merged := *target
		merged.Nullable = node.Nullable
		node = &merged
	}
	if len(node.Enum) > 0 {
		s, isString := value.(string)
		n, isNumber := value.(json.Number)
		if !(isString && slices.Contains(node.Enum, s)) && !(isNumber && slices.Contains(node.Enum, n.String())) {
			v.wrong(path, field, "%v is not one of %s", value, strings.Join(node.Enum, ", "))
		}
		return
	}
	switch node.Type {
	case "string":
		s, ok := value.(string)
		if !ok {
			v.wrong(path, field, "%s where a string is expected", kind(value))
			return
		}
		switch node.Format {
		case "uuid":
			if !uuidPattern.MatchString(s) {
				v.wrong(path, field, "%q is not a uuid", s)
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				v.wrong(path, field, "%q is not a date-time", s)
			}
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			v.wrong(path, field, "%s where an integer is expected", kind(value))
			return
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		switch {
		case err != nil || f != math.Trunc(f):
			v.wrong(path, field, "%s is not an integer", n)
		case node.Format == "int32" && (f < math.MinInt32 || f > math.MaxInt32):
			v.wrong(path, field, "%s does not fit an int32", n)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			v.wrong(path, field, "%s where a number is expected", kind(value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			v.wrong(path, field, "%s where a boolean is expected", kind(value))
		}
	case "array":
		list, ok := value.([]any)
		if !ok {
			v.wrong(path, field, "%s where an array is expected", kind(value))
			return
		}
		if node.Items != nil {
			for i, item := range list {
				v.check(node.Items, item, fmt.Sprintf("%s[%d]", here, i), "")
			}
		}
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			v.wrong(path, field, "%s where an object is expected", kind(value))
			return
		}
		for _, name := range node.Required {
			if obj[name] == nil {
				v.report(here, name, "required, and absent or null")
			}
		}
		names := make([]string, 0, len(obj))
		for name := range obj {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch {
			case obj[name] == nil && slices.Contains(node.Required, name):
				// reported above
			case node.Props[name] != nil:
				v.check(node.Props[name], obj[name], here, name)
			case node.Values != nil:
				v.check(node.Values, obj[name], here, name)
			}
		}
	}
}

func kind(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return "null"
}

// CheckJSONL checks what the e2e gates record, one object per line:
// {"request","method","path","status","contentType","body"}.
func CheckJSONL(r io.Reader, c *Checker) []Violation {
	var out []Violation
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<28)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var line struct {
			Request, Method, Path, ContentType, Body string
			Status                                   int
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			out = append(out, Violation{Request: fmt.Sprintf("line %d", n), JSONPath: "$", Field: "(line)", Message: "not a recorded response: " + err.Error()})
			continue
		}
		out = append(out, c.Check(line.Request, line.Method, line.Path, line.Status, line.ContentType, []byte(line.Body))...)
	}
	if err := sc.Err(); err != nil {
		out = append(out, Violation{Request: "input", JSONPath: "$", Field: "(input)", Message: err.Error()})
	}
	return out
}
