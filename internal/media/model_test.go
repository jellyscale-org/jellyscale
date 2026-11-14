package media_test

import (
	"encoding/json"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"jellyscale/compat/conform"
	"jellyscale/internal/media"
)

// JS-086: the one MediaSource, MediaStream and MediaAttachment type, in package
// internal/media (the names the task implies). Whatever a row holds, the answer
// built from it carries each field compat/schema-12.4.json requires, non-null
// and valid for its type, and no key it was not given and does not require.

func schema(t *testing.T) *conform.Schema {
	t.Helper()
	raw, err := os.ReadFile("../../compat/schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := conform.ParseSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// requiredFields is what the schema requires of a type, and the counts the criteria state.
func requiredFields(t *testing.T, s *conform.Schema, name string, count int) []string {
	t.Helper()
	fields := s.Types[name].Required
	if len(fields) != count {
		t.Fatalf("the schema requires %d fields of %s, the criteria say %d", len(fields), name, count)
	}
	return fields
}

// valid says whether value is a valid JSON value for the schema node of a required field.
func valid(node *conform.Node, value any) bool {
	switch {
	case value == nil:
		return false
	case len(node.Enum) > 0:
		s, ok := value.(string)
		return ok && slices.Contains(node.Enum, s)
	}
	switch node.Type {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == math.Trunc(n)
	case "number":
		_, ok := value.(float64)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	}
	return true
}

func roundTrip[T any](t *testing.T, in string) map[string]any {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("decoding %s: %v", in, err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("encoded %s is not an object: %v", raw, err)
	}
	return out
}

func checkRequired(t *testing.T, s *conform.Schema, typeName string, fields []string, in string, out map[string]any) {
	t.Helper()
	for _, field := range fields {
		value, ok := out[field]
		if !ok {
			t.Errorf("%s from %s lacks %s", typeName, in, field)
			continue
		}
		if !valid(s.Types[typeName].Props[field], value) {
			t.Errorf("%s from %s has %s = %v, not valid for its schema type", typeName, in, field, value)
		}
	}
}

// Rows as releases stored them, from an empty one to the full probe's, give an
// answer with all 17 fields of MediaSourceInfo that a source has, valid, and the
// values Jellyfin sends for a file played from disk where the row has none.
func TestASourceOfAnyStoredShapeCarriesTheRequiredFieldsAtValidValues(t *testing.T) {
	s := schema(t)
	fields := requiredFields(t, s, "MediaSourceInfo", 17)
	for _, in := range []string{
		`{}`,
		`{"Id":"abc"}`,
		`{"Id":"abc","Protocol":"File","Container":"mkv","SupportsTranscoding":true,"MediaStreams":[{"Index":0,"Type":"Video"}]}`,
		`{"Id":"abc","Protocol":"File","Type":"Default","Container":"mkv","SupportsDirectPlay":true,"SupportsDirectStream":true,"SupportsTranscoding":true,"Bitrate":2000000,"MediaStreams":[],"MediaAttachments":[]}`,
	} {
		out := roundTrip[media.MediaSource](t, in)
		checkRequired(t, s, "MediaSourceInfo", fields, in, out)
		for key, want := range map[string]any{"Protocol": "File", "Type": "Default", "SupportsProbing": true, "TranscodingSubProtocol": "http"} {
			if out[key] != want {
				t.Errorf("a source from %s has %s = %v, want %v", in, key, out[key], want)
			}
		}
	}
	// A source built in code and never decoded is valid too: no empty string for an enum.
	raw, err := json.Marshal(media.MediaSource{})
	if err != nil {
		t.Fatal(err)
	}
	var zero map[string]any
	if err := json.Unmarshal(raw, &zero); err != nil {
		t.Fatal(err)
	}
	checkRequired(t, s, "MediaSourceInfo", fields, "the zero MediaSource", zero)
}

// A stream carries the 10 fields of MediaStream the schema requires, and no key
// it was not given and does not require: no Width or empty Language on an audio
// stream, none lost.
func TestAStreamCarriesTheRequiredFieldsAndNoZeroKeyItWasNotGiven(t *testing.T) {
	s := schema(t)
	fields := requiredFields(t, s, "MediaStream", 10)
	for _, in := range []string{
		`{"Index":0,"Type":"Video"}`,
		`{"Index":1,"Type":"Audio","Codec":"aac","IsDefault":true}`,
		`{"Index":2,"Type":"Subtitle","Codec":"subrip","IsExternal":true,"Language":"eng"}`,
		`{"Index":3,"Type":"Subtitle","Codec":"PGSSUB","IsImageSubtitleStream":true,"Width":1920,"Height":1080}`,
		`{"Index":0,"Type":"Video","Codec":"h264","Width":160,"Height":90,"Level":41,"IsInterlaced":false,"ColorTransfer":"","BitDepth":8,"VideoRange":"SDR","VideoRangeType":"SDR"}`,
	} {
		out := roundTrip[media.MediaStream](t, in)
		checkRequired(t, s, "MediaStream", fields, in, out)
		var given map[string]any
		if err := json.Unmarshal([]byte(in), &given); err != nil {
			t.Fatal(err)
		}
		for key, value := range out {
			if _, ok := given[key]; !ok && !slices.Contains(fields, key) {
				t.Errorf("a stream from %s gained %s = %v", in, key, value)
			}
		}
		for key, value := range given {
			if got, ok := out[key]; !ok || got != value {
				t.Errorf("a stream from %s lost or changed %s: %v", in, key, got)
			}
		}
	}
}

// An attachment always has its Index, and gains nothing else.
func TestAnAttachmentCarriesItsIndex(t *testing.T) {
	s := schema(t)
	fields := requiredFields(t, s, "MediaAttachment", 1)
	for _, in := range []string{`{}`, `{"Index":5,"FileName":"Serif.ttf","MimeType":"font/ttf"}`} {
		out := roundTrip[media.MediaAttachment](t, in)
		checkRequired(t, s, "MediaAttachment", fields, in, out)
		var given map[string]any
		if err := json.Unmarshal([]byte(in), &given); err != nil {
			t.Fatal(err)
		}
		for key := range out {
			if _, ok := given[key]; !ok && !slices.Contains(fields, key) {
				t.Errorf("an attachment from %s gained %s", in, key)
			}
		}
	}
}

// Nothing knows which fields are required unless the type says so: each of the
// three is a struct with no map or interface field anywhere in it, and each
// field the schema requires is a field with its own json name and no omitempty
// (an omitted required key is the bug the fill-in covered). A map type under
// another name, or a MarshalJSON that copies a defaults table, fails here.
func TestTheTypesAreStructsWithEveryRequiredFieldAsAnAlwaysSentField(t *testing.T) {
	s := schema(t)
	for typeName, model := range map[string]reflect.Type{
		"MediaSourceInfo": reflect.TypeFor[media.MediaSource](),
		"MediaStream":     reflect.TypeFor[media.MediaStream](),
		"MediaAttachment": reflect.TypeFor[media.MediaAttachment](),
	} {
		if model.Kind() != reflect.Struct {
			t.Fatalf("%s is a %s, not a struct", model.Name(), model.Kind())
		}
		noMapOrInterface(t, model, model.Name())
		tags := map[string]string{}
		for i := range model.NumField() {
			field := model.Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			tags[name] = options
		}
		for _, required := range s.Types[typeName].Required {
			options, ok := tags[required]
			if !ok {
				t.Errorf("%s has no field sent as %q, which the schema requires of %s", model.Name(), required, typeName)
			} else if strings.Contains(options, "omitempty") || strings.Contains(options, "omitzero") {
				t.Errorf("%s sends %q with %q: a required field is left out when empty", model.Name(), required, options)
			}
		}
	}
}

func noMapOrInterface(t *testing.T, ty reflect.Type, path string) {
	t.Helper()
	switch ty.Kind() {
	case reflect.Struct:
		for i := range ty.NumField() {
			noMapOrInterface(t, ty.Field(i).Type, path+"."+ty.Field(i).Name)
		}
	case reflect.Slice, reflect.Array, reflect.Pointer:
		noMapOrInterface(t, ty.Elem(), path)
	case reflect.Map, reflect.Interface:
		t.Errorf("%s is a %s: the model holds a source, a stream or an attachment in typed fields", path, ty.Kind())
	case reflect.Invalid:
	default:
	}
}

func TestTheTypesAreStructs(t *testing.T) {
	for _, ty := range []reflect.Type{reflect.TypeFor[media.MediaSource](), reflect.TypeFor[media.MediaStream](), reflect.TypeFor[media.MediaAttachment]()} {
		if ty.Kind() != reflect.Struct {
			t.Fatalf("%s is a %s, not a struct", ty.Name(), ty.Kind())
		}
	}
}
