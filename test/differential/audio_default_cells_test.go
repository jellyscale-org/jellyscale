package differential

import "testing"

// JS-121: compat/differences.yaml also lists the cells of compat/captures/audio-default-12.4.json where jellyscale
// answers another DefaultAudioStreamIndex than Jellyfin 12.4, under names that start "audio default: ". They are replayed by
// internal/api/audio_default_integration_test.go, not asked of two servers by this tier, so this tier neither wants a
// request of requests.yaml for them nor reports them unused.
func TestCellsOfTheAudioDefaultCaptureAreNeitherRequestsNorUnused(t *testing.T) {
	a, err := ParseAllowlist("- {requests: ['audio default: Audio Short De | deu | PlayDefaultAudioTrack false'], path: 'MediaSources[].DefaultAudioStreamIndex', kind: improvement, reason: 'a track tagged de is German here and matched by no saved language there'}\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRequests([]Request{{Name: "search"}}); err != nil {
		t.Errorf("a cell of the capture is refused as no request: %v", err)
	}
	if got := a.Unused(); len(got) != 0 {
		t.Errorf("a cell of the capture is reported unused: %+v", got)
	}
	// Any other name that is no request still fails.
	b, err := ParseAllowlist("- {requests: ['no such request'], path: 'Items[].Name', kind: improvement, reason: r}\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CheckRequests([]Request{{Name: "search"}}); err == nil {
		t.Error("a name that is no request passed")
	}
}
