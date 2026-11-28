package contract_test

// JS-223, criteria 1 to 3, on the contract a client is served: the document Octave mocks against. Each check reads
// GET /jellyscale/v1/contract from a server built here, so it sees what the image carries, and needs no Postgres. A
// contract that declares sign-in, sign-out or /me differently from the criteria fails here, whatever the answers the
// server gives match.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"jellyscale/internal/api"
	"jellyscale/internal/contract"
)

const (
	signInPath  = "/jellyscale/v1/sign-in"
	signOutPath = "/jellyscale/v1/sign-out"
	mePath      = "/jellyscale/v1/me"
)

// servedDocument is the contract GET /jellyscale/v1/contract answers, byte for byte.
func servedDocument(t *testing.T) []byte {
	t.Helper()
	server := &api.Server{Name: "jellyscale", ServerID: "srv", Roles: []string{api.RoleAPI}, Log: slog.New(slog.DiscardHandler)}
	site := httptest.NewServer(server.Handler())
	t.Cleanup(site.Close)
	response, err := http.Get(site.URL + "/jellyscale/v1/contract")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /jellyscale/v1/contract: %d, want 200", response.StatusCode)
	}
	return raw
}

// documentOf reads the served contract as the generic maps the walks below use.
func documentOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("the served contract is not YAML: %v", err)
	}
	return document
}

var (
	jsonPointerEscape   = strings.NewReplacer("~", "~0", "/", "~1")
	jsonPointerUnescape = strings.NewReplacer("~1", "/", "~0", "~")
	bracesEscape        = strings.NewReplacer("{", "%7B", "}", "%7D")
)

// at returns the node a JSON pointer names, or nil when there is none.
func at(t *testing.T, document map[string]any, pointer string) any {
	t.Helper()
	var current any = document
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		fields, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = fields[jsonPointerUnescape.Replace(token)]
	}
	return current
}

// followRefs follows $ref objects from a pointer to the object they name, and returns that object's pointer.
func followRefs(t *testing.T, document map[string]any, pointer string) string {
	t.Helper()
	for range 8 {
		fields, ok := at(t, document, pointer).(map[string]any)
		if !ok {
			t.Fatalf("%s is not an object in the contract", pointer)
		}
		ref, ok := fields["$ref"].(string)
		if !ok {
			return pointer
		}
		pointer = strings.TrimPrefix(ref, "#")
	}
	t.Fatalf("the $ref chain at %s is longer than 8", pointer)
	return ""
}

func responsePointer(path, method, status string) string {
	return "/paths/" + jsonPointerEscape.Replace(path) + "/" + method + "/responses/" + status
}

// responseOf is the answer one status of one operation declares, with its $ref followed.
func responseOf(t *testing.T, document map[string]any, path, method, status string) map[string]any {
	t.Helper()
	fields, _ := at(t, document, followRefs(t, document, responsePointer(path, method, status))).(map[string]any)
	return fields
}

// mediaOf is the JSON media object of one answer, the one a body's schema and examples sit on.
func mediaOf(t *testing.T, document map[string]any, path, method, status string) string {
	t.Helper()
	return followRefs(t, document, responsePointer(path, method, status)) + "/content/" + jsonPointerEscape.Replace("application/json")
}

// requestMediaOf is the JSON media object of an operation's request body.
func requestMediaOf(t *testing.T, document map[string]any, path, method string) string {
	t.Helper()
	body := "/paths/" + jsonPointerEscape.Replace(path) + "/" + method + "/requestBody"
	return followRefs(t, document, body) + "/content/" + jsonPointerEscape.Replace("application/json")
}

// schemaOf compiles the schema of one media object, with its $refs resolved in the document.
func schemaOf(t *testing.T, document map[string]any, media string) *jsonschema.Schema {
	t.Helper()
	pointer := followRefs(t, document, media+"/schema")
	asJSON, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"$ref": "mem:///contract.json#` + bracesEscape.Replace(pointer) + `"}`))
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
		t.Fatalf("the contract's schema at %s does not compile: %v", pointer, err)
	}
	return schema
}

// accepts reports whether the schema accepts a value, once it is JSON.
func accepts(t *testing.T, schema *jsonschema.Schema, value any) bool {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return schema.Validate(instance) == nil
}

// examplesOf returns the example values of one media object, by name.
func examplesOf(t *testing.T, document map[string]any, media string) map[string]any {
	t.Helper()
	fields, _ := at(t, document, media).(map[string]any)
	listed, _ := fields["examples"].(map[string]any)
	values := map[string]any{}
	for name, entry := range listed {
		example, _ := entry.(map[string]any)
		values[name] = example["value"]
	}
	return values
}

// copyOf returns a deep copy of an answer, so a check can break the copy and leave the example alone.
func copyOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// memberOf returns the object under one key of an answer, failing the test when the example has none.
func memberOf(t *testing.T, answer map[string]any, key string) map[string]any {
	t.Helper()
	fields, ok := answer[key].(map[string]any)
	if !ok {
		t.Fatalf("the example has no %s object", key)
	}
	return fields
}

// breakage changes one copy of an answer, and the contract must refuse the result.
type breakage struct {
	name  string
	apply func(t *testing.T, answer map[string]any)
}

// JS-223, criterion 1. A name a client reads from the contract is camelCase. The leaving operations keep their served
// bodies until the cutover, so their own schemas keep Jellyfin's names; PlaybackInfo keeps three of its fields, and its
// refusal members are not among them.
var (
	camelName            = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	leavingSchemas       = []string{"TrackPreferences", "TrackPreferencesPatch", "Configuration", "MediaStream", "MediaSource"}
	jellyfinPlayback     = []string{"MediaSources", "PlaySessionId", "ErrorCode"}
	leavingOperations    = []string{"/jellyscale/v1/track-preferences", "/jellyscale/v1/users/configuration", "/jellyscale/v1/items/{itemId}/playback-info"}
	todaysTokenSpellings = []string{"header Authorization", "header X-Emby-Authorization", "header X-Emby-Token", "header X-MediaBrowser-Token", "query ApiKey", "query api_key"}
)

// nameProblems lists the property names and required entries under one node that are not camelCase, except the exempt ones.
func nameProblems(node any, where string, exempt []string) []string {
	var found []string
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "examples" {
				continue
			}
			if key == "properties" {
				properties, _ := child.(map[string]any)
				for name := range properties {
					if !slices.Contains(exempt, name) && !camelName.MatchString(name) {
						found = append(found, where+" property "+name)
					}
				}
			}
			if key == "required" {
				entries, _ := child.([]any)
				for _, entry := range entries {
					name, _ := entry.(string)
					if !slices.Contains(exempt, name) && !camelName.MatchString(name) {
						found = append(found, where+" required "+name)
					}
				}
			}
			found = append(found, nameProblems(child, where+"/"+key, exempt)...)
		}
	case []any:
		for _, child := range typed {
			found = append(found, nameProblems(child, where, exempt)...)
		}
	}
	return found
}

func TestEveryNameTheClientsReadIsCamelCase(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	schemas, _ := at(t, document, "/components/schemas").(map[string]any)
	var problems []string
	for name, schema := range schemas {
		switch {
		case name == "PlaybackInfo":
			problems = append(problems, nameProblems(schema, name, jellyfinPlayback)...)
		case slices.Contains(leavingSchemas, name):
		default:
			problems = append(problems, nameProblems(schema, name, nil)...)
		}
	}
	paths, _ := document["paths"].(map[string]any)
	for path, item := range paths {
		if !slices.Contains(leavingOperations, path) {
			problems = append(problems, nameProblems(item, path, nil)...)
		}
	}
	playback, _ := schemas["PlaybackInfo"].(map[string]any)
	properties, _ := playback["properties"].(map[string]any)
	for _, member := range []string{"reason", "action"} {
		if _, ok := properties[member]; !ok {
			problems = append(problems, "PlaybackInfo has no "+member+" member")
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		t.Fatalf("%d names a client reads are not camelCase:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// JS-223, criterion 2. The sign-in answer names the account, the profile and the device, and carries the token once. The
// example Octave mocks against is accepted, and each thing the answer must name is refused when it is missing or mistyped.
func TestTheSignInAnswerNamesAnAccountProfileAndDeviceAndCarriesAToken(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	media := mediaOf(t, document, signInPath, "post", "200")
	schema := schemaOf(t, document, media)
	answers := examplesOf(t, document, media)
	if len(answers) == 0 {
		t.Fatalf("POST /jellyscale/v1/sign-in gives no example answer for Octave to mock against")
	}
	breaks := []breakage{
		{"has no token", func(t *testing.T, answer map[string]any) { delete(answer, "token") }},
		{"has no account", func(t *testing.T, answer map[string]any) { delete(answer, "account") }},
		{"has no profile", func(t *testing.T, answer map[string]any) { delete(answer, "profile") }},
		{"has no device", func(t *testing.T, answer map[string]any) { delete(answer, "device") }},
		{"names an account with no id", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "account"), "id") }},
		{"names an account with no name", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "account"), "name") }},
		{"names a profile with no id", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "profile"), "id") }},
		{"names a profile with no name", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "profile"), "name") }},
		{"names a device with no id", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "device"), "id") }},
		{"names a device with no name", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "device"), "name") }},
		{"gives the account a numeric id", func(t *testing.T, answer map[string]any) { memberOf(t, answer, "account")["id"] = 7 }},
	}
	for name, answer := range answers {
		fields, _ := answer.(map[string]any)
		if !accepts(t, schema, fields) {
			t.Errorf("example %s is refused by the sign-in answer's own schema", name)
		}
		for _, change := range breaks {
			broken := copyOf(t, fields)
			change.apply(t, broken)
			if accepts(t, schema, broken) {
				t.Errorf("example %s: the contract accepts a sign-in answer that %s", name, change.name)
			}
		}
	}
}

// JS-223, criterion 2. /me answers the account, the profile and the device of the caller, and no token.
func TestMeNamesAnAccountProfileAndDeviceAndCarriesNoToken(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	media := mediaOf(t, document, mePath, "get", "200")
	schema := schemaOf(t, document, media)
	answers := examplesOf(t, document, media)
	if len(answers) == 0 {
		t.Fatalf("GET /jellyscale/v1/me gives no example answer for Octave to mock against")
	}
	breaks := []breakage{
		{"has no account", func(t *testing.T, answer map[string]any) { delete(answer, "account") }},
		{"has no profile", func(t *testing.T, answer map[string]any) { delete(answer, "profile") }},
		{"has no device", func(t *testing.T, answer map[string]any) { delete(answer, "device") }},
		{"names an account with no id", func(t *testing.T, answer map[string]any) { delete(memberOf(t, answer, "account"), "id") }},
		{"carries a token", func(t *testing.T, answer map[string]any) { answer["token"] = "a-token" }},
	}
	for name, answer := range answers {
		fields, _ := answer.(map[string]any)
		if !accepts(t, schema, fields) {
			t.Errorf("example %s is refused by /me's own schema", name)
		}
		for _, change := range breaks {
			broken := copyOf(t, fields)
			change.apply(t, broken)
			if accepts(t, schema, broken) {
				t.Errorf("example %s: the contract accepts a /me answer that %s", name, change.name)
			}
		}
	}
}

// JS-223, criterion 2 (reviewer's minor 2 reads it too). Within each example the account, the profile and the device have
// their own ids, and no id is used as another kind anywhere in the examples. /me describes the same device, so the same
// id may repeat under the same kind.
func TestTheIdsInTheExamplesOfSignInAndMeAreDistinctAcrossKinds(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	kindOf := map[string]string{}
	for _, probe := range []struct{ path, method string }{{signInPath, "post"}, {mePath, "get"}} {
		media := mediaOf(t, document, probe.path, probe.method, "200")
		answers := examplesOf(t, document, media)
		if len(answers) == 0 {
			t.Errorf("%s %s gives no example answer", probe.method, probe.path)
		}
		for name, answer := range answers {
			fields, _ := answer.(map[string]any)
			for _, kind := range []string{"account", "profile", "device"} {
				object, _ := fields[kind].(map[string]any)
				id, ok := object["id"].(string)
				if !ok || id == "" {
					t.Errorf("%s example %s names no %s id", probe.path, name, kind)
					continue
				}
				if other, seen := kindOf[id]; seen && other != kind {
					t.Errorf("%s example %s uses the id %s as both %s and %s", probe.path, name, id, other, kind)
				}
				kindOf[id] = kind
			}
		}
	}
}

// JS-223, criterion 2 and reviewer's item 4. Sign-in answers 400, 401, 403 and 429 with exactly its own reason each, and its
// 429 carries the seconds to wait and the scope of the refusal, with the Retry-After header. The seconds are 1 to 900, so a
// client never retries into a refusal it was told the length of.
func TestSignInDeclaresItsRefusalsAndItsRetryWindow(t *testing.T) {
	raw := servedDocument(t)
	parsed, err := contract.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string][]string{
		"400": {"invalid-body"}, "401": {"credentials-not-accepted"}, "403": {"account-off"}, "429": {"too-many-tries"},
	}
	for status, want := range reasons {
		if got := parsed.DeclaredReasons("post", signInPath, status); !slices.Equal(got, want) {
			t.Errorf("sign-in declares %v for status %s, want %v", got, status, want)
		}
	}
	document := documentOf(t, raw)
	headers, _ := responseOf(t, document, signInPath, "post", "429")["headers"].(map[string]any)
	if _, ok := headers["Retry-After"]; !ok {
		t.Errorf("sign-in's 429 declares no Retry-After header")
	}
	schema := schemaOf(t, document, mediaOf(t, document, signInPath, "post", "429"))
	valid := map[string]any{"reason": "too-many-tries", "action": "retry", "retryAfterSeconds": 900, "scope": "device"}
	if !accepts(t, schema, valid) {
		t.Errorf("the 429 schema refuses a too-many-tries answer with 900 seconds and scope device")
	}
	for name, change := range map[string]func(map[string]any){
		"901 seconds":        func(answer map[string]any) { answer["retryAfterSeconds"] = 901 },
		"0 seconds":          func(answer map[string]any) { answer["retryAfterSeconds"] = 0 },
		"no seconds":         func(answer map[string]any) { delete(answer, "retryAfterSeconds") },
		"no scope":           func(answer map[string]any) { delete(answer, "scope") },
		"a scope of account": func(answer map[string]any) { answer["scope"] = "account" },
	} {
		broken := copyOf(t, valid)
		change(broken)
		if accepts(t, schema, broken) {
			t.Errorf("the 429 schema accepts a too-many-tries answer with %s", name)
		}
	}
}

// JS-223, reviewer's minor 1. The seconds and the scope belong to too-many-tries alone: every other refusal refuses them.
func TestScopeAndRetrySecondsAreMembersOfTooManyTriesOnly(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	schema := schemaOf(t, document, mediaOf(t, document, "/jellyscale/v1/track-preferences", "get", "401"))
	base := map[string]any{"reason": "unauthorized", "action": "sign-in"}
	if !accepts(t, schema, base) {
		t.Fatalf("the 401 refusal is refused as the sign-in refusal, reason unauthorized and action sign-in")
	}
	for name, extra := range map[string]map[string]any{"scope": {"scope": "name"}, "retryAfterSeconds": {"retryAfterSeconds": 5}} {
		broken := copyOf(t, base)
		for key, value := range extra {
			broken[key] = value
		}
		if accepts(t, schema, broken) {
			t.Errorf("a 401 refusal carrying %s is accepted", name)
		}
	}
}

// JS-223, criterion 2. Sign-out and /me declare their own refusal, and sign-out answers 204 with no body.
func TestSignOutAnswersNoContentAndMeAndSignOutRefuseWithoutAToken(t *testing.T) {
	raw := servedDocument(t)
	parsed, err := contract.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct{ path, method string }{{signOutPath, "post"}, {mePath, "get"}} {
		if got := parsed.DeclaredReasons(probe.method, probe.path, "401"); !slices.Equal(got, []string{"unauthorized"}) {
			t.Errorf("%s %s declares %v for status 401, want [unauthorized]", probe.method, probe.path, got)
		}
	}
	document := documentOf(t, raw)
	response := responseOf(t, document, signOutPath, "post", "204")
	if _, hasBody := response["content"]; hasBody {
		t.Errorf("sign-out's 204 declares a body; it answers no content")
	}
}

// JS-223, criterion 2. A token sent as Authorization: Bearer is declared beside the six header and query spellings the server
// reads today, and sign-out and /me accept every one of them (internal/compat/auth.go, Authorization carries both schemes).
func TestTheBearerTokenIsDeclaredBesideTodaysSpellingsOnSignOutAndMe(t *testing.T) {
	document := documentOf(t, servedDocument(t))
	schemes, _ := at(t, document, "/components/securitySchemes").(map[string]any)
	var bearer, apiKeys []string
	var spellings []string
	for name, value := range schemes {
		fields, _ := value.(map[string]any)
		switch fields["type"] {
		case "http":
			if fields["scheme"] == "bearer" {
				bearer = append(bearer, name)
			}
		case "apiKey":
			apiKeys = append(apiKeys, name)
			in, _ := fields["in"].(string)
			key, _ := fields["name"].(string)
			spellings = append(spellings, in+" "+key)
		}
	}
	if len(bearer) != 1 {
		t.Fatalf("the contract declares %d bearer schemes, want one: %v", len(bearer), bearer)
	}
	want := slices.Clone(todaysTokenSpellings)
	slices.Sort(want)
	slices.Sort(spellings)
	if !slices.Equal(spellings, want) {
		t.Errorf("the declared token spellings are %v, want %v", spellings, want)
	}
	for _, probe := range []struct{ path, method string }{{signOutPath, "post"}, {mePath, "get"}} {
		operation, _ := at(t, document, "/paths/"+jsonPointerEscape.Replace(probe.path)+"/"+probe.method).(map[string]any)
		listed := map[string]bool{}
		alternatives, _ := operation["security"].([]any)
		for _, alternative := range alternatives {
			requirement, _ := alternative.(map[string]any)
			for name := range requirement {
				listed[name] = true
			}
		}
		if !listed[bearer[0]] {
			t.Errorf("%s %s does not accept the bearer token", probe.method, probe.path)
		}
		for _, name := range apiKeys {
			if !listed[name] {
				t.Errorf("%s %s does not accept the %s spelling", probe.method, probe.path, name)
			}
		}
	}
}
