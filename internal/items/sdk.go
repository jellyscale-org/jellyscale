package items

// Clients built on jellyfin-sdk-kotlin (Findroid, Android TV) decode strictly:
// a response without one of these fields fails to decode, and the client
// stops (Findroid's film page spun forever on 2026-11-11). They are the
// non-null fields with no default of MediaSourceInfo and MediaStream in
// jellyfin-sdk-kotlin 1.9.4, the release Findroid pins, with the value
// Jellyfin sends for a file it plays from disk. A stored DTO that lacks one
// (scanned before it was written, or left out because jellyfin-web never
// reads it) gets it when it is served.
var sourceDefaults = map[string]any{
	"Protocol":               "File",
	"Type":                   "Default",
	"IsRemote":               false,
	"ReadAtNativeFramerate":  false,
	"IgnoreDts":              false,
	"IgnoreIndex":            false,
	"GenPtsInput":            false,
	"SupportsTranscoding":    false,
	"SupportsDirectStream":   false,
	"SupportsDirectPlay":     false,
	"IsInfiniteStream":       false,
	"RequiresOpening":        false,
	"RequiresClosing":        false,
	"RequiresLooping":        false,
	"SupportsProbing":        true,
	"TranscodingSubProtocol": "http",
	"HasSegments":            false,
}

var streamDefaults = map[string]any{
	"IsInterlaced":           false,
	"IsDefault":              false,
	"IsForced":               false,
	"IsHearingImpaired":      false,
	"IsOriginal":             false,
	"IsExternal":             false,
	"IsTextSubtitleStream":   false,
	"SupportsExternalStream": false,
}

// WithSDKFields fills in, on each media source and each of its streams, the
// fields the SDK requires that the map lacks. Fields it has are kept.
func WithSDKFields(source map[string]any) {
	fill(source, sourceDefaults)
	streams(source)
}

// withItemSDKFields does the same for an item DTO's sources and its own
// MediaStreams list.
func withItemSDKFields(dto map[string]any) {
	sources, _ := dto["MediaSources"].([]any)
	for _, entry := range sources {
		if source, ok := entry.(map[string]any); ok {
			WithSDKFields(source)
		}
	}
	streams(dto)
}

func streams(holder map[string]any) {
	switch list := holder["MediaStreams"].(type) {
	case []any: // As decoded from a stored DTO.
		for _, entry := range list {
			if stream, ok := entry.(map[string]any); ok {
				fill(stream, streamDefaults)
			}
		}
	case []map[string]any: // As PlaybackInfo builds it.
		for _, stream := range list {
			fill(stream, streamDefaults)
		}
	}
}

func fill(m, defaults map[string]any) {
	for key, value := range defaults {
		if _, ok := m[key]; !ok {
			m[key] = value
		}
	}
}
