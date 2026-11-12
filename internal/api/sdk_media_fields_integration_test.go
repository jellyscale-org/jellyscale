//go:build integration

package api

import (
	"encoding/json"
	"testing"

	"jellyscale/internal/db"
)

// The non-null fields with no default of MediaSourceInfo and MediaStream in
// jellyfin-sdk-kotlin 1.9.4, which Findroid pins: a response without one fails
// to decode, and Findroid's film page spun forever on 2026-11-11.
var (
	sdkSourceFields = []string{"Protocol", "Type", "IsRemote", "ReadAtNativeFramerate", "IgnoreDts", "IgnoreIndex", "GenPtsInput",
		"SupportsTranscoding", "SupportsDirectStream", "SupportsDirectPlay", "IsInfiniteStream", "RequiresOpening", "RequiresClosing",
		"RequiresLooping", "SupportsProbing", "TranscodingSubProtocol", "HasSegments"}
	sdkStreamFields = []string{"IsInterlaced", "IsDefault", "IsForced", "IsHearingImpaired", "IsOriginal", "Type", "Index",
		"IsExternal", "IsTextSubtitleStream", "SupportsExternalStream"}
)

func TestItemAndPlaybackInfoCarryEveryFieldTheKotlinSDKRequires(t *testing.T) {
	e := newEnv(t)
	id := db.CompactID(e.item)
	check := func(what string, body []byte, sourcesKey string) {
		t.Helper()
		var reply map[string]any
		if err := json.Unmarshal(body, &reply); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		sources, _ := reply[sourcesKey].([]any)
		if len(sources) == 0 {
			t.Fatalf("%s has no media sources: %s", what, body)
		}
		missing := func(m map[string]any, fields []string, where string) {
			for _, field := range fields {
				if _, ok := m[field]; !ok {
					t.Errorf("%s: %s lacks %s", what, where, field)
				}
			}
		}
		lists := [][]any{}
		for _, entry := range sources {
			source := entry.(map[string]any)
			missing(source, sdkSourceFields, "a media source")
			list, _ := source["MediaStreams"].([]any)
			lists = append(lists, list)
		}
		if list, ok := reply["MediaStreams"].([]any); ok {
			lists = append(lists, list)
		}
		for _, list := range lists {
			if len(list) == 0 {
				t.Errorf("%s: a stream list is empty", what)
			}
			for _, entry := range list {
				missing(entry.(map[string]any), sdkStreamFields, "a stream")
			}
		}
	}

	response, body := e.do(t, 0, "GET", "/Items/"+id+"?userId="+db.CompactID(e.user), e.token, nil)
	if response.StatusCode != 200 {
		t.Fatalf("item: %d", response.StatusCode)
	}
	check("GET /Items/{id}", body, "MediaSources")

	response, body = e.do(t, 0, "POST", "/Items/"+id+"/PlaybackInfo", e.token, map[string]any{"UserId": e.user, "DeviceProfile": map[string]any{}})
	if response.StatusCode != 200 {
		t.Fatalf("PlaybackInfo: %d", response.StatusCode)
	}
	check("PlaybackInfo", body, "MediaSources")
}
