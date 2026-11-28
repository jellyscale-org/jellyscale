//go:build integration

package api

// JS-223, criterion 2. Sign-in and /me each give an example answer, and sign-in an example request, that the contract's own
// schema accepts. The design has JS-183's conformance check the examples, and this is that check extended to them. Octave
// mocks against these examples, so a wrong type in one must be refused, which the second test shows for the account.
// Sign-out answers 204 with no body, so it has no example to check.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// examplesAt returns the examples of the media type at a pointer, by name.
func examplesAt(t *testing.T, document map[string]any, pointer string) map[string]any {
	t.Helper()
	examples, _ := nodeAt(t, document, pointer)["examples"].(map[string]any)
	return examples
}

// compileSchemaAt compiles the schema at a pointer, with its $refs resolved in the document, as answerSchema does.
func compileSchemaAt(t *testing.T, document map[string]any, schemaPointer string) *jsonschema.Schema {
	t.Helper()
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
		t.Fatalf("the contract's schema at %s does not compile: %v", schemaPointer, err)
	}
	return schema
}

// exampleValue is the value an example carries, as JSON.
func exampleValue(t *testing.T, name string, example any) []byte {
	t.Helper()
	fields, _ := example.(map[string]any)
	raw, err := json.Marshal(fields["value"])
	if err != nil {
		t.Fatalf("example %s: %v", name, err)
	}
	return raw
}

func TestSignInAndMeGiveExamplesTheContractAccepts(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	cases := []struct{ name, path, method string }{
		{"sign-in", "/jellyscale/v1/sign-in", "post"},
		{"me", "/jellyscale/v1/me", "get"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			media := "/paths/" + tokenEscape.Replace(tc.path) + "/" + tc.method + "/responses/200"
			media = settle(t, document, media) + "/content/" + tokenEscape.Replace("application/json")
			examples := examplesAt(t, document, media)
			if len(examples) == 0 {
				t.Fatalf("%s %s gives no example answer for Octave to mock against", strings.ToUpper(tc.method), tc.path)
			}
			schema := compileSchemaAt(t, document, settle(t, document, media+"/schema"))
			for name, example := range examples {
				if err := schema.Validate(instanceOf(t, exampleValue(t, name, example))); err != nil {
					t.Errorf("%s %s: example %s is not accepted by its schema: %v", strings.ToUpper(tc.method), tc.path, name, err)
				}
			}
		})
	}
}

func TestSignInGivesARequestExampleTheContractAccepts(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	media := "/paths/" + tokenEscape.Replace("/jellyscale/v1/sign-in") + "/post/requestBody"
	media = settle(t, document, media) + "/content/" + tokenEscape.Replace("application/json")
	examples := examplesAt(t, document, media)
	if len(examples) == 0 {
		t.Fatalf("POST /jellyscale/v1/sign-in gives no example request")
	}
	schema := compileSchemaAt(t, document, settle(t, document, media+"/schema"))
	for name, example := range examples {
		if err := schema.Validate(instanceOf(t, exampleValue(t, name, example))); err != nil {
			t.Errorf("the example request %s is not accepted by its schema: %v", name, err)
		}
	}
}

// Negative control: the sign-in answer's schema types the account, so an administrator flag that is not a boolean is refused.
func TestTheSignInAnswerSchemaRefusesAWrongTypeInTheAccount(t *testing.T) {
	kit := newEnv(t)
	document := servedContract(t, kit)
	media := "/paths/" + tokenEscape.Replace("/jellyscale/v1/sign-in") + "/post/responses/200"
	media = settle(t, document, media) + "/content/" + tokenEscape.Replace("application/json")
	var example map[string]any
	for _, value := range examplesAt(t, document, media) {
		if err := json.Unmarshal(exampleValue(t, "sign-in", value), &example); err != nil {
			t.Fatal(err)
		}
		break
	}
	if example == nil {
		t.Fatalf("POST /jellyscale/v1/sign-in gives no example answer")
	}
	account, _ := example["account"].(map[string]any)
	account["administrator"] = "yes"
	broken, err := json.Marshal(example)
	if err != nil {
		t.Fatal(err)
	}
	schema := compileSchemaAt(t, document, settle(t, document, media+"/schema"))
	if schema.Validate(instanceOf(t, broken)) == nil {
		t.Fatalf("the sign-in schema accepts %.200q, a non-boolean administrator", broken)
	}
}
