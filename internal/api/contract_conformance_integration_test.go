//go:build integration

package api

// JS-183, criterion 6. Each served v1 operation answers a valid request, and each refused one, with a body the contract allows for
// that status. The contract's schemas reject a wrong type in each answer they type, so they are not vacuous. The v1 routes the server
// serves are exactly the contract's served operations, read from what Handler registered. The contract is compiled as JSON Schema
// 2020-12 with formats asserted (santhosh-tekuri/jsonschema/v6); the oracle is the served document.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// tokenEscape writes one reference token of a JSON pointer.
var tokenEscape = strings.NewReplacer("~", "~0", "/", "~1")

// tokenUnescape reads one reference token of a JSON pointer.
var tokenUnescape = strings.NewReplacer("~1", "/", "~0", "~")

// braceEscape writes a route's parameter braces in a URI fragment.
var braceEscape = strings.NewReplacer("{", "%7B", "}", "%7D")

// parameter matches a route's parameter: the contract and the server may name it differently.
var parameter = regexp.MustCompile(`\{[^}]*\}`)

// httpMethods are the keys of a path item that are operations.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options"}

// routeKey is METHOD and path, with the case of literal segments and the names of parameters ignored.
func routeKey(method, path string) string {
	return strings.ToUpper(method) + " " + parameter.ReplaceAllString(strings.ToLower(path), "{}")
}

// nodeAt returns the object a JSON pointer names in the document.
func nodeAt(t *testing.T, document map[string]any, pointer string) map[string]any {
	t.Helper()
	var node any = document
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		fields, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s names no node of the contract", pointer)
		}
		node = fields[tokenUnescape.Replace(token)]
	}
	fields, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in the contract", pointer)
	}
	return fields
}

// settle follows the $ref objects from a pointer to the node they name, and returns that node's pointer. A response may be a
// reference to components/responses, and a schema one to components/schemas.
func settle(t *testing.T, document map[string]any, pointer string) string {
	t.Helper()
	for range 8 {
		ref, ok := nodeAt(t, document, pointer)["$ref"].(string)
		if !ok {
			return pointer
		}
		pointer = strings.TrimPrefix(ref, "#")
	}
	t.Fatalf("the $ref chain at %s is longer than 8", pointer)
	return ""
}

// answerSchema compiles the schema the contract gives one status of one operation, with its $refs resolved in the document.
func answerSchema(t *testing.T, document map[string]any, path, method, status string) *jsonschema.Schema {
	t.Helper()
	response := settle(t, document, "/paths/"+tokenEscape.Replace(path)+"/"+method+"/responses/"+status)
	schemaPointer := settle(t, document, response+"/content/"+tokenEscape.Replace("application/json")+"/schema")
	asJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"$ref": "mem:///contract.json#` + braceEscape.Replace(schemaPointer) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("mem:///contract.json", resource); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("mem:///wrapper.json", wrapper); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("mem:///wrapper.json")
	if err != nil {
		t.Fatalf("the contract gives no schema for %s %s answering %s: %v", strings.ToUpper(method), path, status, err)
	}
	return schema
}

// instanceOf decodes an answer's body as JSON, the form a schema validates.
func instanceOf(t *testing.T, raw []byte) any {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the answer is not JSON: %v: %.120q", err, raw)
	}
	return instance
}

// Each served operation, asked a valid request and each refused one: the answer's status is the one named, and its body is one the
// contract allows for that status.
func TestEveryServedV1AnswerIsAllowedByTheContract(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	item := "/jellyscale/v1/items/" + kit.item + "/playback-info"
	const itemTemplate = "/jellyscale/v1/items/{itemId}/playback-info"
	cases := []struct {
		name, method, path, template, token string
		body                                any
		status                              int
	}{
		{"server, no token", http.MethodGet, "/jellyscale/v1/server", "/jellyscale/v1/server", "", nil, http.StatusOK},
		{"track preferences", http.MethodGet, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", kit.token, nil, http.StatusOK},
		{"track preferences saved", http.MethodPatch, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", kit.token, map[string]any{"KeepAudioLanguages": []string{"deu"}}, http.StatusOK},
		{"track preferences bad body", http.MethodPatch, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", kit.token, json.RawMessage(`{"HideOtherAudio":"yes"}`), http.StatusBadRequest},
		{"configuration saved", http.MethodPatch, "/jellyscale/v1/users/configuration", "/jellyscale/v1/users/configuration", kit.token, map[string]any{"AudioLanguagePreference": "deu"}, http.StatusOK},
		{"configuration bad body", http.MethodPatch, "/jellyscale/v1/users/configuration", "/jellyscale/v1/users/configuration", kit.token, json.RawMessage(`{"SubtitleMode":"Nope"}`), http.StatusBadRequest},
		{"playback info", http.MethodPost, item, itemTemplate, kit.token, map[string]any{}, http.StatusOK},
		{"playback info, another's id", http.MethodPost, item, itemTemplate, kit.token, map[string]any{"UserId": kit.other}, http.StatusForbidden},
		{"track preferences, no token", http.MethodGet, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", "", nil, http.StatusUnauthorized},
		{"declared, not served", http.MethodPost, "/jellyscale/v1/sign-in", "/jellyscale/v1/sign-in", "", map[string]any{}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			if response.StatusCode != tc.status {
				t.Fatalf("%s %s: %d %.120q, want %d", tc.method, tc.path, response.StatusCode, raw, tc.status)
			}
			schema := answerSchema(t, document, tc.template, strings.ToLower(tc.method), strconv.Itoa(tc.status))
			if err := schema.Validate(instanceOf(t, raw)); err != nil {
				t.Fatalf("%s %s answered %d with a body the contract does not allow: %v: %.200q", tc.method, tc.path, tc.status, err, raw)
			}
		})
	}
}

// The schemas are not vacuous. Each case takes an answer the server gives, sets one typed field to a wrong type or adds a key the
// schema does not give, and the schema must reject it. Without this, a schema that accepts any object passes every answer above.
func TestTheContractRejectsAWrongTypeInEachAnswerItTypes(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	cases := []struct {
		name, method, path, template, token string
		body                                any
		status                              int
		breakAnswer                         func(answer map[string]any) bool
	}{
		{"track preferences HideOtherAudio", http.MethodGet, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", kit.token, nil, http.StatusOK,
			func(answer map[string]any) bool { answer["HideOtherAudio"] = "yes"; return true }},
		{"configuration AudioLanguagePreference", http.MethodPatch, "/jellyscale/v1/users/configuration", "/jellyscale/v1/users/configuration", kit.token,
			map[string]any{"AudioLanguagePreference": "deu"}, http.StatusOK, func(answer map[string]any) bool { answer["AudioLanguagePreference"] = 5; return true }},
		{"identity ApiVersion", http.MethodGet, "/jellyscale/v1/server", "/jellyscale/v1/server", "", nil, http.StatusOK,
			func(answer map[string]any) bool { answer["ApiVersion"] = 3; return true }},
		{"playback info Hidden", http.MethodPost, "/jellyscale/v1/items/" + kit.item + "/playback-info", "/jellyscale/v1/items/{itemId}/playback-info", kit.token,
			map[string]any{}, http.StatusOK, breakAnAudioTrack},
		{"sign-in refusal an extra key", http.MethodGet, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", "", nil, http.StatusUnauthorized,
			func(answer map[string]any) bool { answer["Extra"] = true; return true }},
		{"sign-in refusal a number Reason", http.MethodGet, "/jellyscale/v1/track-preferences", "/jellyscale/v1/track-preferences", "", nil, http.StatusUnauthorized,
			func(answer map[string]any) bool { answer["Reason"] = 7; return true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := kit.do(t, 0, tc.method, tc.path, tc.token, tc.body)
			if response.StatusCode != tc.status {
				t.Fatalf("%s %s: %d %.120q, want %d", tc.method, tc.path, response.StatusCode, raw, tc.status)
			}
			var answer map[string]any
			if err := json.Unmarshal(raw, &answer); err != nil {
				t.Fatalf("the answer is not a JSON object: %v", err)
			}
			if !tc.breakAnswer(answer) {
				t.Fatalf("the answer %.200q has no field to break", raw)
			}
			broken, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			schema := answerSchema(t, document, tc.template, strings.ToLower(tc.method), strconv.Itoa(tc.status))
			if schema.Validate(instanceOf(t, broken)) == nil {
				t.Fatalf("the contract accepts %.200q for %s %s answering %d", broken, tc.method, tc.path, tc.status)
			}
		})
	}
}

// breakAnAudioTrack sets Hidden of the first audio track of the first source to a string.
func breakAnAudioTrack(answer map[string]any) bool {
	sources, _ := answer["MediaSources"].([]any)
	if len(sources) == 0 {
		return false
	}
	source, _ := sources[0].(map[string]any)
	streams, _ := source["MediaStreams"].([]any)
	for _, entry := range streams {
		stream, _ := entry.(map[string]any)
		if stream["Type"] == "Audio" {
			stream["Hidden"] = "yes"
			return true
		}
	}
	return false
}

// The v1 routes the server serves are the contract's operations that are not declared. The served set is what Handler registered,
// so a route registered outside Routes() is seen as well as one in it, and a pattern under /jellyscale/v1/ with no method fails.
func TestTheServedV1RoutesAreTheContractsServedOperations(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	served := map[string]bool{}
	for _, pattern := range kit.apis[0].ServedPatterns() {
		method, path, found := strings.Cut(pattern, " ")
		if !found {
			method, path = "", pattern
		}
		if !strings.HasPrefix(strings.ToLower(path), "/jellyscale/v1/") {
			continue
		}
		if method == "" {
			t.Errorf("%q is registered with no method: a route under /jellyscale/v1/ names one", pattern)
			continue
		}
		served[routeKey(method, path)] = true
	}
	paths, _ := document["paths"].(map[string]any)
	described := map[string]bool{}
	for path, item := range paths {
		if !strings.HasPrefix(strings.ToLower(path), "/jellyscale/v1/") {
			continue
		}
		methods, _ := item.(map[string]any)
		for method, value := range methods {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			fields, _ := value.(map[string]any)
			state, _ := fields["x-state"].(string)
			key := routeKey(method, path)
			described[key] = true
			switch {
			case state == "declared" && served[key]:
				t.Errorf("%s is declared and served", key)
			case state != "declared" && !served[key]:
				t.Errorf("%s is %q in the contract and not served", key, state)
			}
		}
	}
	for key := range served {
		if !described[key] {
			t.Errorf("%s is served and the contract does not describe it", key)
		}
	}
}
