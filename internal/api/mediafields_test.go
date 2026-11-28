package api

import (
	"math"
	"strings"
	"testing"
)

// The fields a strict SDK client reads from every source, stream and attachment, and the
// kind it reads each as. They are always present and never null; a field a client does not
// read is absent unless the film has a value for it.
var mediaFieldKinds = map[string]map[string]string{
	"source": {
		"GenPtsInput": "boolean", "HasSegments": "boolean", "IgnoreDts": "boolean", "IgnoreIndex": "boolean",
		"IsInfiniteStream": "boolean", "IsRemote": "boolean", "ReadAtNativeFramerate": "boolean",
		"RequiresClosing": "boolean", "RequiresLooping": "boolean", "RequiresOpening": "boolean",
		"SupportsDirectPlay": "boolean", "SupportsDirectStream": "boolean", "SupportsProbing": "boolean",
		"SupportsTranscoding": "boolean",
		"Protocol":            "string", "TranscodingSubProtocol": "string", "Type": "string",
	},
	"stream": {
		"IsDefault": "boolean", "IsExternal": "boolean", "IsForced": "boolean", "IsHearingImpaired": "boolean",
		"IsInterlaced": "boolean", "IsOriginal": "boolean", "IsTextSubtitleStream": "boolean",
		"SupportsExternalStream": "boolean",
		"Index":                  "integer", "Type": "string",
	},
	"attachment": {"Index": "integer"},
}

var mediaEnums = map[string]string{
	"source.Protocol":               "File Http Rtmp Rtsp Udp Rtp Ftp",
	"source.TranscodingSubProtocol": "http hls",
	"source.Type":                   "Default Grouping Placeholder",
	"stream.Type":                   "Audio Video Subtitle EmbeddedImage Data Lyric",
}

// checkMediaFields fails unless every field a client reads is present, non-null and of
// the kind it reads it as. where names the answer and the element being checked.
func checkMediaFieldKinds(t *testing.T, kind string, obj map[string]any, where string) {
	t.Helper()
	for field, want := range mediaFieldKinds[kind] {
		value, ok := obj[field]
		if !ok {
			t.Errorf("%s: %s has no %s", where, kind, field)
			continue
		}
		switch want {
		case "boolean":
			if _, ok := value.(bool); !ok {
				t.Errorf("%s: %s.%s is %v, want a boolean", where, kind, field, value)
			}
		case "integer":
			n, ok := value.(float64)
			if !ok || n != math.Trunc(n) {
				t.Errorf("%s: %s.%s is %v, want an integer", where, kind, field, value)
			}
		case "string":
			s, ok := value.(string)
			if !ok {
				t.Errorf("%s: %s.%s is %v, want a string", where, kind, field, value)
				continue
			}
			if allowed, enum := mediaEnums[kind+"."+field]; enum && !strings.Contains(" "+allowed+" ", " "+s+" ") {
				t.Errorf("%s: %s.%s is %q, want one of %s", where, kind, field, s, allowed)
			}
		}
	}
}
