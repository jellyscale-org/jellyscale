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
