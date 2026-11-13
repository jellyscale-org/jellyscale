package conform_test

// The checker that holds every JSON response to Jellyfin 12.4's OpenAPI schema
// (JS-070). Both tiers use it: the differential runner and, through
// compat/conform/cmd/conform, the e2e gates. These tests name the package the
// task implies; none compiles yet.
//
//	conform.FromOpenAPI(doc []byte, image string) ([]byte, error)
//	    the checked-in schema file (compat/schema-12.4.json) from an OpenAPI document;
//	    it records the image and the document's info.version
//	conform.ParseSchema(file []byte) (*Schema, error)    Schema.Image, Schema.Version
//	conform.Drift(checkedIn, generated []byte) error     nil only when they are the same
//	conform.LoadRules(path string) (Rules, error)         compat/conform.yaml: aliases and unchecked routes
//	conform.NewChecker(s *Schema, r Rules) *Checker
//	(*Checker).Check(request, method, path string, status int, contentType string, body []byte) []Violation
//	    request labels the answer in a report (a request name, "sign-in"); path may carry a query
//	Violation{Request, Route, JSONPath, Field, Message string} and String()
//	(*Checker).Counts() (responses, routes int)           JSON responses checked, and the routes they were on
//	(*Checker).Summary(label string) (string, error)      "<label>: checked N JSON responses on M routes"; error when N is 0
//	conform.CheckJSONL(r io.Reader, c *Checker) []Violation   lines {"request","method","path","status","contentType","body"}

import (
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

// A small document with the shapes Jellyfin 12.4's has: a property is required
// when the schema lists it OR when it is not nullable and has no default (the
// Kotlin SDK reads both as required); enums and formats sit beside allOf refs.
const doc = `{
 "openapi": "3.0.1",
 "info": {"title": "Jellyfin API", "version": "12.4.0", "x-jellyfin-version": "12.4.0"},
 "paths": {
  "/Things/{thingId}": {"get": {"operationId": "GetThing", "parameters": [{"name": "thingId", "in": "path", "required": true, "schema": {"type": "string", "format": "uuid"}}],
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}, "404": {"description": "none"}}}},
  "/Things": {"get": {"operationId": "GetThings", "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Thing"}}}}}}},
             "post": {"operationId": "AddThing", "responses": {"204": {"description": "no content"}}}},
  "/Plain": {"get": {"operationId": "GetPlain", "responses": {"200": {"description": "ok", "content": {"text/plain": {"schema": {"type": "string"}}}}}}}
 },
 "components": {"schemas": {
  "Kind": {"enum": ["Small", "Large"], "type": "string"},
  "Tag": {"type": "object", "properties": {"Label": {"type": "string"}}},
  "Thing": {"type": "object", "required": ["Name"], "properties": {
    "Name": {"type": "string", "nullable": true},
    "Id": {"type": "string", "format": "uuid"},
    "Born": {"type": "string", "format": "date-time"},
    "Count": {"type": "integer", "format": "int32"},
    "Ratio": {"type": "number", "format": "double"},
    "Flag": {"type": "boolean", "default": false},
    "Kind": {"enum": ["Small", "Large"], "allOf": [{"$ref": "#/components/schemas/Kind"}]},
    "Note": {"type": "string", "nullable": true},
    "Tags": {"type": "array", "items": {"$ref": "#/components/schemas/Tag"}, "nullable": true},
    "Parts": {"type": "array", "items": {"$ref": "#/components/schemas/Part"}},
    "Inner": {"allOf": [{"$ref": "#/components/schemas/Part"}], "nullable": true}}},
  "Part": {"type": "object", "properties": {"Size": {"type": "integer", "format": "int64"}, "Mode": {"type": "string", "enum": ["a", "b"]}}}
 }}}`

const goodThing = `{"Name":"x","Id":"8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11","Born":"2026-11-12T10:00:00.0000000Z","Count":3,"Ratio":1.5,"Kind":"Small","Parts":[{"Size":1,"Mode":"a"},{"Size":2,"Mode":"b"}],"Inner":{"Size":5,"Mode":"a"}}`

func checker(t *testing.T, rules string) *conform.Checker {
	t.Helper()
	file, err := conform.FromOpenAPI([]byte(doc), "jellyfin/jellyfin:12.4")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	r, err := conform.LoadRules(writeRules(t, rules))
	if err != nil {
		t.Fatal(err)
	}
	return conform.NewChecker(s, r)
}

func writeRules(t *testing.T, text string) string {
	t.Helper()
	path := t.TempDir() + "/conform.yaml"
	if err := writeFile(path, text); err != nil {
		t.Fatal(err)
	}
	return path
}

func get(c *conform.Checker, path string, body string) []conform.Violation {
	return c.Check("req", "GET", path, 200, "application/json; charset=utf-8", []byte(body))
}

func wantViolation(t *testing.T, vs []conform.Violation, route, jsonPath, field string) {
	t.Helper()
	for _, v := range vs {
		if v.Route == route && v.JSONPath == jsonPath && v.Field == field {
			s := v.String()
			for _, part := range []string{"req", route, jsonPath, field} {
				if !strings.Contains(s, part) {
					t.Errorf("violation %q does not name %q", s, part)
				}
			}
			return
		}
	}
	t.Errorf("want a violation of %s at %s field %s, got %v", route, jsonPath, field, vs)
}

func TestAnAnswerThatFitsTheSchemaHasNoViolation(t *testing.T) {
	c := checker(t, "")
	if vs := get(c, "/Things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11", goodThing); len(vs) != 0 {
		t.Errorf("violations: %v", vs)
	}
}

func TestAFieldTheSchemaDoesNotListIsNotAViolation(t *testing.T) {
	c := checker(t, "")
	body := strings.Replace(goodThing, `"Count":3`, `"Count":3,"Extra":{"x":null}`, 1)
	if vs := get(c, "/Things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11", body); len(vs) != 0 {
		t.Errorf("violations: %v", vs)
	}
}

func TestARequiredFieldIsAbsentOrNullHoweverTheDocumentMarksIt(t *testing.T) {
	c := checker(t, "")
	route := "GET /Things/{thingId}"
	path := "/Things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11"
	// Name is in "required" though nullable; Born, Id and Count are required by being non-nullable with no default.
	for _, field := range []string{"Name", "Id", "Born", "Count", "Ratio", "Kind", "Parts"} {
		absent := removeKey(t, goodThing, field)
		wantViolation(t, get(c, path, absent), route, "$", field)
		null := strings.Replace(goodThing, `"`+field+`":`, `"`+field+`":null,"Was`+field+`":`, 1)
		wantViolation(t, get(c, path, null), route, "$", field)
	}
	// A nullable field with a default or no requirement may be absent or null.
	for _, field := range []string{"Flag", "Note", "Tags"} {
		if vs := get(c, path, removeKey(t, goodThing, field)); len(vs) != 0 {
			t.Errorf("%s absent: %v", field, vs)
		}
	}
	if vs := get(c, path, strings.Replace(goodThing, `"Kind":"Small"`, `"Kind":"Small","Note":null,"Tags":null,"Flag":false`, 1)); len(vs) != 0 {
		t.Errorf("nulls: %v", vs)
	}
}

func TestAViolationIsFoundAtAnyDepthAndInEveryArrayElement(t *testing.T) {
	c := checker(t, "")
	route := "GET /Things"
	// Only the third element's third level is wrong.
	good := goodThing
	bad := strings.Replace(goodThing, `{"Size":2,"Mode":"b"}`, `{"Size":"2","Mode":"b"}`, 1)
	vs := get(c, "/Things", "["+good+","+good+","+bad+"]")
	wantViolation(t, vs, route, "$[2].Parts[1]", "Size")
	if len(vs) != 1 {
		t.Errorf("want exactly the one violation, got %v", vs)
	}
	// Each wrong element is reported, not only the first.
	vs = get(c, "/Things", "["+bad+","+bad+"]")
	wantViolation(t, vs, route, "$[0].Parts[1]", "Size")
	wantViolation(t, vs, route, "$[1].Parts[1]", "Size")
	// A required field missing from an object inside an allOf-wrapped nullable ref.
	inner := strings.Replace(goodThing, `"Inner":{"Size":5,"Mode":"a"}`, `"Inner":{"Mode":"a"}`, 1)
	wantViolation(t, get(c, "/Things", "["+inner+"]"), route, "$[0].Inner", "Size")
}

func TestAValueOfTheWrongTypeIsAViolation(t *testing.T) {
	c := checker(t, "")
	path := "/Things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11"
	route := "GET /Things/{thingId}"
	for name, edit := range map[string][2]string{
		"a string for a number":       {`"Count":3`, `"Count":"3"`},
		"a fraction for an integer":   {`"Count":3`, `"Count":3.5`},
		"a number for a string":       {`"Name":"x"`, `"Name":4`},
		"a string outside its enum":   {`"Kind":"Small"`, `"Kind":"Huge"`},
		"a malformed uuid":            {`8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11`, `not-a-uuid`},
		"a malformed date-time":       {`2026-11-12T10:00:00.0000000Z`, `yesterday`},
		"an object for an array":      {`"Parts":[{"Size":1,"Mode":"a"},{"Size":2,"Mode":"b"}]`, `"Parts":{}`},
		"a bool for a number":         {`"Ratio":1.5`, `"Ratio":true`},
		"a nested string for an enum": {`"Mode":"b"}],`, `"Mode":"z"}],`},
	} {
		vs := get(c, path, strings.Replace(goodThing, edit[0], edit[1], 1))
		if len(vs) == 0 {
			t.Errorf("%s: passed", name)
		}
		for _, v := range vs {
			if v.Route != route || v.JSONPath == "" || v.Field == "" {
				t.Errorf("%s: violation %+v names no route, path or field", name, v)
			}
		}
	}
	// A whole number written as 3.0 or an exponent is an integer; an int is a number.
	for _, ok := range []string{`"Count":3.0`, `"Ratio":2`, `"Count":3e0`} {
		key := strings.SplitN(ok, ":", 2)[0]
		body := strings.Replace(removeKey(t, goodThing, strings.Trim(key, `"`)), `{`, `{`+ok+`,`, 1)
		if vs := get(c, path, body); len(vs) != 0 {
			t.Errorf("%s: %v", ok, vs)
		}
	}
}

func TestNoJSONBodyWhereTheSchemaGivesOneIsAViolation(t *testing.T) {
	c := checker(t, "")
	for name, body := range map[string]string{"empty": "", "text": "Not Found", "null": "null", "wrong top-level type": `{"a":1}`} {
		if vs := c.Check("req", "GET", "/Things", 200, "application/json", []byte(body)); len(vs) == 0 {
			t.Errorf("%s: passed", name)
		}
	}
	if vs := c.Check("req", "GET", "/Things", 200, "text/html", []byte("<html>")); len(vs) == 0 {
		t.Error("an HTML body on a JSON route passed")
	}
}

func TestAResponseIsMatchedWhateverTheCaseOfThePathAndTheNamesOfItsParameters(t *testing.T) {
	c := checker(t, "")
	for _, path := range []string{"/things", "/THINGS", "/Things?limit=3&userId=x"} {
		if vs := get(c, path, "[]"); len(vs) != 0 {
			t.Errorf("%s: %v", path, vs)
		}
	}
	bad := removeKey(t, goodThing, "Name")
	for _, path := range []string{"/things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11", "/THINGS/8F1D6C1E3F0A4A539B0E2B6F5F3A9C11?x=1"} {
		wantViolation(t, get(c, path, bad), "GET /Things/{thingId}", "$", "Name")
	}
}

func TestAnAliasIsCheckedAsTheRouteItStandsFor(t *testing.T) {
	rules := `
aliases:
  - {method: GET, route: "/Users/{userId}/Things", stands_for: "/Things"}
`
	c := checker(t, rules)
	bad := "[" + removeKey(t, goodThing, "Name") + "]"
	wantViolation(t, get(c, "/Users/abc/Things", bad), "GET /Things", "$[0]", "Name")
	if vs := get(c, "/Users/abc/Things", "[]"); len(vs) != 0 {
		t.Errorf("a good answer: %v", vs)
	}
	// Without the rule the route matches nothing, which fails.
	if vs := get(checker(t, ""), "/Users/abc/Things", "[]"); len(vs) == 0 {
		t.Error("an alias with no rule passed")
	}
}

func TestAJSONResponseWithNoOperationOrNoDeclaredTypeFailsUnlessListedWithAReason(t *testing.T) {
	c := checker(t, "")
	if vs := get(c, "/Unknown/Route", `{}`); len(vs) == 0 {
		t.Error("a JSON answer on a route no operation has passed")
	}
	// /Plain has an operation whose 200 declares no JSON type.
	if vs := get(c, "/Plain", `{}`); len(vs) == 0 {
		t.Error("a JSON answer on a route that declares none passed")
	}
	listed := checker(t, `
unchecked:
  - {method: GET, route: "/Plain", reason: "serves a JSON document the schema types as text"}
  - {method: GET, route: "/Unknown/Route", reason: "jellyscale's own route"}
`)
	if vs := get(listed, "/Plain", `{}`); len(vs) != 0 {
		t.Errorf("listed /Plain: %v", vs)
	}
	if vs := get(listed, "/unknown/route", `{}`); len(vs) != 0 {
		t.Errorf("listed route, other case: %v", vs)
	}
	if _, err := conform.LoadRules(writeRules(t, "unchecked:\n  - {method: GET, route: /Plain}\n")); err == nil {
		t.Error("an unchecked route with no reason was accepted")
	}
	// A status the operation does not declare is a JSON answer with no declared type.
	if vs := c.Check("req", "GET", "/Things", 418, "application/json", []byte(`{}`)); len(vs) == 0 {
		t.Error("an undeclared status passed")
	}
}

func TestAnAnswerWithoutABodyWhereNoneIsDeclaredIsNotCheckedOrCounted(t *testing.T) {
	c := checker(t, "")
	if vs := c.Check("req", "POST", "/Things", 204, "", nil); len(vs) != 0 {
		t.Errorf("204: %v", vs)
	}
	if n, _ := c.Counts(); n != 0 {
		t.Errorf("counted %d JSON responses", n)
	}
}

func TestTheRunSaysHowManyJSONResponsesItCheckedAndOnHowManyRoutes(t *testing.T) {
	c := checker(t, "")
	if _, err := c.Summary("differential"); err == nil {
		t.Error("a run that checked none passed")
	}
	get(c, "/Things", "[]")
	get(c, "/Things", "[]")
	get(c, "/Things/8f1d6c1e-3f0a-4a53-9b0e-2b6f5f3a9c11", goodThing)
	if r, routes := c.Counts(); r != 3 || routes != 2 {
		t.Errorf("counts %d responses on %d routes, want 3 on 2", r, routes)
	}
	got, err := c.Summary("e2e browse")
	if err != nil || got != "e2e browse: checked 3 JSON responses on 2 routes" {
		t.Errorf("summary %q, %v", got, err)
	}
}

func TestCheckJSONLReadsWhatTheE2EGatesRecord(t *testing.T) {
	c := checker(t, "")
	lines := `{"request":"GET /Things","method":"GET","path":"/Things?a=1","status":200,"contentType":"application/json","body":"[]"}
{"request":"GET /Things","method":"GET","path":"/Things","status":200,"contentType":"application/json","body":"[{}]"}
`
	vs := conform.CheckJSONL(strings.NewReader(lines), c)
	if len(vs) == 0 {
		t.Fatal("the bad second line passed")
	}
	if vs[0].Request != "GET /Things" {
		t.Errorf("request %q", vs[0].Request)
	}
	if r, _ := c.Counts(); r != 2 {
		t.Errorf("checked %d", r)
	}
	if vs := conform.CheckJSONL(strings.NewReader("not json\n"), checker(t, "")); len(vs) == 0 {
		t.Error("an unreadable line passed")
	}
}

func TestTheSchemaFileRecordsTheImageAndTheServerVersionItCameFrom(t *testing.T) {
	file, err := conform.FromOpenAPI([]byte(doc), "jellyfin/jellyfin:12.4")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	if s.Image != "jellyfin/jellyfin:12.4" || s.Version != "12.4.0" {
		t.Errorf("image %q, version %q", s.Image, s.Version)
	}
	again, _ := conform.FromOpenAPI([]byte(doc), "jellyfin/jellyfin:12.4")
	if string(again) != string(file) {
		t.Error("the same document gave two files")
	}
}

func TestDriftFailsOnAHandEditOrAStaleCopy(t *testing.T) {
	file, _ := conform.FromOpenAPI([]byte(doc), "jellyfin/jellyfin:12.4")
	if err := conform.Drift(file, file); err != nil {
		t.Errorf("same file: %v", err)
	}
	edited := strings.Replace(string(file), `"Count"`, `"Counts"`, 1)
	if edited == string(file) {
		t.Fatal("the edit changed nothing; the schema file does not name Count")
	}
	if err := conform.Drift([]byte(edited), file); err == nil {
		t.Error("a hand edit passed")
	}
	newer := strings.Replace(doc, `"Size": {"type": "integer"`, `"Sizes": {"type": "integer"`, 1)
	other, _ := conform.FromOpenAPI([]byte(newer), "jellyfin/jellyfin:12.4")
	if err := conform.Drift(file, other); err == nil {
		t.Error("a stale copy passed")
	}
}
