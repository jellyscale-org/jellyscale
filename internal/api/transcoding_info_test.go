package api

import "testing"

// A re-encode downmixes to stereo, so it reports 2 channels; copied audio keeps
// the source's, and reporting 2 for a 5.1 copy misinforms the dashboard.
func TestTranscodingInfoChannelsFollowTheAudio(t *testing.T) {
	streams := []byte(`[{"Type":"Video","Index":0,"Codec":"hevc"},{"Type":"Audio","Index":1,"Codec":"eac3","Channels":6}]`)
	for _, test := range []struct {
		spec string
		want int
	}{
		{`{"AudioIndex":1,"AudioCopy":"eac3"}`, 6},
		{`{"AudioIndex":1}`, 2},
	} {
		info, err := transcodingInfo(transcodeRow{Spec: []byte(test.spec), Streams: streams, State: "running"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := info["AudioChannels"]; got != test.want {
			t.Errorf("spec %s: AudioChannels %v, want %d", test.spec, got, test.want)
		}
	}
}
