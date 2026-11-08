package differential

// JS-067: the tier compares GET /Sessions during a transcode with Jellyfin's, and the
// three fields jellyscale adds to TranscodingInfo are listed as the improvement they are
// (internal/api/transcoding_info_integration_test.go says what they mean). The request
// needs the tier to start a transcode first, which is the task's to build; this test
// only holds the shipped files to it. `test/differential/run.sh` is the check that the
// rest of TranscodingInfo agrees with Jellyfin's.

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

const sessionsRequest = "sessions during a transcode"

func TestSessionsDuringATranscodeAreComparedAndTheSpeedIsAnImprovement(t *testing.T) {
	reqs, err := LoadRequests("requests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range reqs {
		if r.Name == sessionsRequest {
			found = true
			if r.Route != "GET /Sessions" || r.Method != "GET" {
				t.Errorf("%q is %s %s, want GET /Sessions", r.Name, r.Method, r.Route)
			}
		}
	}
	if !found {
		t.Fatalf("test/differential/requests.yaml has no request named %q", sessionsRequest)
	}

	text, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var written []listed
	if err := yaml.Unmarshal(text, &written); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"EncodeSpeed", "Worker", "IsBelowRealTime"} {
		path := "[].TranscodingInfo." + field
		ok := false
		for _, w := range written {
			if w.Kind == "improvement" && slices.Contains(w.Requests, sessionsRequest) && (w.Path == path || slices.Contains(w.Paths, path)) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("compat/differences.yaml has no improvement entry for %s in %q", path, sessionsRequest)
		}
	}
}
