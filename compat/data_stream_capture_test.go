package compat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// JS-119 observed first: compat/captures/data-stream-12.4.md and its JSON record what a real Jellyfin 12.4 lists for a
// data track (an MP4 with a tmcd track after two audio and two subtitle tracks) in PlaybackInfo and in both lists of
// GET /Items/{itemId}, and that jellyfin-web 12.4's menus and media info leave it out. This holds the record to the
// criterion: the data stream with all its fields, at its own Index, which is its position, in every list, and the
// three views of jellyfin-web.
func TestTheDataStreamCaptureIsComplete(t *testing.T) {
	raw, err := os.ReadFile("captures/data-stream-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture map[string]any
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if capture["image"] != "jellyfin/jellyfin:12.4" || capture["date"] == "" {
		t.Errorf("the capture names image %v and date %v", capture["image"], capture["date"])
	}
	lists := map[string][]any{}
	for _, answer := range []string{"PlaybackInfo POST", "PlaybackInfo GET", "GET /Items/{itemId}", "GET /Users/{userId}/Items/{itemId}"} {
		reply, ok := capture[answer].(map[string]any)
		if !ok {
			t.Errorf("the capture has no answer of %s", answer)
			continue
		}
		lists[answer+" MediaSources[0].MediaStreams"], _ = reply["MediaSources"].([]any)[0].(map[string]any)["MediaStreams"].([]any)
		if strings.HasPrefix(answer, "GET /") {
			lists[answer+" MediaStreams"], _ = reply["MediaStreams"].([]any)
		}
	}
	fields := []string{"CodecTag", "Language", "TimeBase", "VideoRange", "VideoRangeType", "AudioSpatialFormat", "IsInterlaced", "BitRate", "IsDefault",
		"IsForced", "IsHearingImpaired", "IsOriginal", "Type", "Index", "IsExternal", "IsTextSubtitleStream", "SupportsExternalStream"}
	for name, streams := range lists {
		if len(streams) != 6 {
			t.Errorf("%s lists %d streams, want 6", name, len(streams))
			continue
		}
		for position, s := range streams {
			if index := s.(map[string]any)["Index"]; index != float64(position) {
				t.Errorf("%s: position %d holds Index %v", name, position, index)
			}
		}
		data := streams[5].(map[string]any)
		if data["Type"] != "Data" {
			t.Errorf("%s: the stream at 5 is %v, want Data", name, data["Type"])
		}
		for _, field := range fields {
			if _, ok := data[field]; !ok {
				t.Errorf("%s: the data stream has no %s", name, field)
			}
		}
		if len(data) != len(fields) {
			t.Errorf("%s: the data stream has %d fields, the capture lists %d", name, len(data), len(fields))
		}
	}
	web, _ := capture["web"].(map[string]any)
	for _, view := range []string{"details page, audio select", "details page, subtitle select", "details page, Media Info blocks", "player, audio menu", "player, subtitle menu"} {
		entries, ok := web[view].([]any)
		if !ok {
			t.Errorf("the capture says nothing of jellyfin-web's %s", view)
			continue
		}
		for _, entry := range entries {
			if entry == "5" || entry == "Data" {
				t.Errorf("%s lists the data stream: %v", view, entries)
			}
		}
	}
}
