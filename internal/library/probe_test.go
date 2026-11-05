package library

import (
	"encoding/json"
	"testing"
)

// Two tracks in one language must read differently in a client's menu.
func TestSubtitleLabelsTellTracksApart(t *testing.T) {
	for _, c := range []struct {
		language, codec     string
		isDefault, isForced bool
		want                string
	}{
		{"eng", "subrip", true, false, "eng - Default - SUBRIP"},
		{"eng", "subrip", false, true, "eng - Forced - SUBRIP"},
		{"", "ass", false, false, "ASS"},
		{"fra", "PGSSUB", true, true, "fra - Default - Forced - PGSSUB"},
	} {
		if got := displaySubtitle(c.language, c.codec, c.isDefault, c.isForced); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}

// A Dolby Vision file is mapped by its base layer, which profile 5 doesn't
// have as HDR10 or HLG: the probe passes that on, from the side data ffprobe
// writes.
func TestDolbyVisionBaseLayerCompatibility(t *testing.T) {
	var parsed ffprobeOutput
	raw := `{"streams":[{"side_data_list":[{"side_data_type":"Mastering display metadata"},` +
		`{"side_data_type":"DOVI configuration record","dv_profile":5,"dv_bl_signal_compatibility_id":0}]}]}`
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	side := parsed.Streams[0].SideData
	if bl, ok := dolbyVisionBlCompatibility(side); !ok || bl != 0 {
		t.Errorf("profile 5: %d %v", bl, ok)
	}
	if got := rangeType("smpte2084", side); got != "DOVI" {
		t.Errorf("range %q", got)
	}
	if _, ok := dolbyVisionBlCompatibility(side[:1]); ok {
		t.Error("a file without Dolby Vision side data has a base layer compatibility")
	}
}

// Chapters read as Jellyfin's DTO has them: titled as the file says, "Chapter N"
// (N counting from 1 over the file's list) when it says nothing, in time order,
// and never with an ImageTag.
func TestChaptersAreNamedAndOrdered(t *testing.T) {
	var parsed ffprobeOutput
	raw := `{"chapters":[
		{"start_time":"20.000000","tags":{"title":"Middle"}},
		{"start_time":"0.000000","tags":{"title":"  Opening "}},
		{"start_time":"45.500000","tags":{"title":" "}},
		{"start_time":"N/A","tags":{"title":"Broken"}},
		{"start_time":"50.000000"}]}`
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	got := chaptersOf(parsed)
	want := []struct {
		name  string
		ticks int64
	}{{"Opening", 0}, {"Middle", 200_000_000}, {"Chapter 3", 455_000_000}, {"Chapter 5", 500_000_000}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i, w := range want {
		if got[i]["Name"] != w.name || got[i]["StartPositionTicks"] != w.ticks {
			t.Errorf("chapter %d: %v, want %s at %d", i, got[i], w.name, w.ticks)
		}
		if _, ok := got[i]["ImageTag"]; ok {
			t.Errorf("chapter %d has an ImageTag", i)
		}
	}
	if none := chaptersOf(ffprobeOutput{}); none == nil || len(none) != 0 {
		t.Errorf("no chapters: %#v", none)
	}
}
