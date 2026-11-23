package differential

import "testing"

// JS-041: compat/differences.yaml also lists the cells of compat/captures/subtitles-default-12.4.json where jellyscale
// answers another DefaultSubtitleStreamIndex than Jellyfin 12.4, under names that start "subtitle default: ". They are
// replayed by internal/api/subtitles_default_integration_test.go, not asked of two servers by this tier, so this tier neither
// wants a request of requests.yaml for them nor reports them unused.
func TestCellsOfTheSubtitleDefaultCaptureAreNeitherRequestsNorUnused(t *testing.T) {
	a, err := ParseAllowlist("- {requests: ['subtitle default: A5 | Default | eng'], path: 'MediaSources[].DefaultSubtitleStreamIndex', kind: improvement, reason: 'the track a viewer saved the language of is started here'}\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRequests([]Request{{Name: "search"}}); err != nil {
		t.Errorf("a cell of the capture is refused as no request: %v", err)
	}
	if got := a.Unused(); len(got) != 0 {
		t.Errorf("a cell of the capture is reported unused: %+v", got)
	}
	// Any other name that is no request still fails, and the audio default's cells keep their own prefix.
	b, err := ParseAllowlist("- {requests: ['no such request'], path: 'Items[].Name', kind: improvement, reason: r}\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CheckRequests([]Request{{Name: "search"}}); err == nil {
		t.Error("a name that is no request passed")
	}
}
