package differential

import "testing"

// JS-144: compat/differences.yaml also lists where jellyscale answers a cross-origin request of
// compat/captures/cors-12.4.json otherwise than Jellyfin 12.4, under names that start "cors: " and the path "(status)"
// or "header <lower-case name>". internal/api/cors_integration_test.go replays those requests, not this tier, so this
// tier neither wants a request of requests.yaml for them nor reports them unused.
func TestCellsOfTheCorsCaptureAreNeitherRequestsNorUnused(t *testing.T) {
	a, err := ParseAllowlist("- {requests: ['cors: open route'], paths: ['header access-control-allow-origin', '(status)'], kind: improvement, reason: 'the origin is echoed'}\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRequests([]Request{{Name: "search"}}); err != nil {
		t.Errorf("a request of the cors capture is refused as no request: %v", err)
	}
	if got := a.Unused(); len(got) != 0 {
		t.Errorf("a request of the cors capture is reported unused: %+v", got)
	}
}
