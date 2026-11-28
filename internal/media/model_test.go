package media_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"jellyscale/internal/media"
)

// JS-086: the one MediaSource, MediaStream and MediaAttachment type, in package
// internal/media. Whatever a row holds, the answer built from it carries every field a
// strict client reads, non-null and of the kind it reads it as, and no key it was not
// given and does not need.

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

func checkRequired(t *testing.T, typeName string, in string, out map[string]any) {
	t.Helper()
	for field, want := range media.ClientFields[typeName] {
		value, ok := out[field]
		if !ok {
			t.Errorf("%s from %s lacks %s", typeName, in, field)
			continue
		}
		switch want {
		case "bool":
			if _, ok := value.(bool); !ok {
				t.Errorf("%s from %s has %s = %v, want a boolean", typeName, in, field, value)
			}
		case "integer":
			n, ok := value.(float64)
			if !ok || n != math.Trunc(n) {
				t.Errorf("%s from %s has %s = %v, want an integer", typeName, in, field, value)
			}
		default:
			s, ok := value.(string)
			if !ok {
				t.Errorf("%s from %s has %s = %v, want a string", typeName, in, field, value)
			} else if !strings.Contains(" "+want+" ", " "+s+" ") {
				t.Errorf("%s from %s has %s = %q, want one of %s", typeName, in, field, s, want)
			}
		}
	}
}

// Rows as releases stored them, from an empty one to the full probe's, give an
// answer with every field a strict client reads from a source, valid, and the
// values a file played from disk has where the row has none.
func TestASourceOfAnyStoredShapeCarriesTheRequiredFieldsAtValidValues(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`{"Id":"abc"}`,
		`{"Id":"abc","Protocol":"File","Container":"mkv","SupportsTranscoding":true,"MediaStreams":[{"Index":0,"Type":"Video"}]}`,
		`{"Id":"abc","Protocol":"File","Type":"Default","Container":"mkv","SupportsDirectPlay":true,"SupportsDirectStream":true,"SupportsTranscoding":true,"Bitrate":2000000,"MediaStreams":[],"MediaAttachments":[]}`,
	} {
		out := roundTrip[media.MediaSource](t, in)
		checkRequired(t, "source", in, out)
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
	checkRequired(t, "source", "the zero MediaSource", zero)
}

// A stream carries every field a strict client reads, and no key
// it was not given and does not need: no Width or empty Language on an audio
// stream, none lost.
func TestAStreamCarriesTheRequiredFieldsAndNoZeroKeyItWasNotGiven(t *testing.T) {
	for _, in := range []string{
		`{"Index":0,"Type":"Video"}`,
		`{"Index":1,"Type":"Audio","Codec":"aac","IsDefault":true}`,
		`{"Index":2,"Type":"Subtitle","Codec":"subrip","IsExternal":true,"Language":"eng"}`,
		`{"Index":3,"Type":"Subtitle","Codec":"PGSSUB","IsImageSubtitleStream":true,"Width":1920,"Height":1080}`,
		`{"Index":0,"Type":"Video","Codec":"h264","Width":160,"Height":90,"Level":41,"IsInterlaced":false,"ColorTransfer":"","BitDepth":8,"VideoRange":"SDR","VideoRangeType":"SDR"}`,
	} {
		out := roundTrip[media.MediaStream](t, in)
		checkRequired(t, "stream", in, out)
		var given map[string]any
		if err := json.Unmarshal([]byte(in), &given); err != nil {
			t.Fatal(err)
		}
		for key, value := range out {
			if _, ok := given[key]; !ok && media.ClientFields["stream"][key] == "" {
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
	for _, in := range []string{`{}`, `{"Index":5,"FileName":"Serif.ttf","MimeType":"font/ttf"}`} {
		out := roundTrip[media.MediaAttachment](t, in)
		checkRequired(t, "attachment", in, out)
		var given map[string]any
		if err := json.Unmarshal([]byte(in), &given); err != nil {
			t.Fatal(err)
		}
		for key := range out {
			if _, ok := given[key]; !ok && media.ClientFields["attachment"][key] == "" {
				t.Errorf("an attachment from %s gained %s", in, key)
			}
		}
	}
}
