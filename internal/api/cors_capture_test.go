package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// corsCapture is compat/captures/cors-12.4.json, which test/differential/capture_cors.py writes.
type corsCapture struct {
	Image         string   `json:"image"`
	ServerVersion string   `json:"server_version"`
	Origins       []string `json:"origins"`
	Rows          []struct {
		Request  string            `json:"request"`
		Method   string            `json:"method"`
		Path     string            `json:"path"`
		Origin   string            `json:"origin"`
		Sent     map[string]string `json:"sent"`
		Token    bool              `json:"token"`
		Status   int               `json:"status"`
		Headers  map[string]string `json:"headers"`
		BodySize int               `json:"body_bytes"`
	} `json:"rows"`
}

func readCorsCapture(t *testing.T) corsCapture {
	t.Helper()
	raw, err := os.ReadFile("../../compat/captures/cors-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture corsCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return capture
}

// The capture holds every request the task names, each with two different origins and with none.
func TestTheCorsCaptureHoldsEachRequestFromTwoOriginsAndFromNone(t *testing.T) {
	capture := readCorsCapture(t)
	if capture.Image != "jellyfin/jellyfin:12.4" || len(capture.ServerVersion) < 4 || capture.ServerVersion[:4] != "12.4" {
		t.Errorf("the capture is of %s %s, want jellyfin/jellyfin:12.4", capture.Image, capture.ServerVersion)
	}
	if len(capture.Origins) != 2 || capture.Origins[0] == capture.Origins[1] {
		t.Errorf("origins %v, want two different ones", capture.Origins)
	}
	want := []string{"open route", "viewer route with a token", "viewer route with no token", "ranged film", "preflight of the sign-in",
		"preflight asking range", "preflight asking all six headers", "preflight asking DELETE", "preflight asking PATCH", "preflight asking a header outside the six",
		"GET of an unserved path", "preflight of an unserved path"}
	origins := map[string]map[string]bool{}
	for _, row := range capture.Rows {
		if origins[row.Request] == nil {
			origins[row.Request] = map[string]bool{}
		}
		origins[row.Request][row.Origin] = true
		if row.Status == 0 {
			t.Errorf("%s from %q has no status", row.Request, row.Origin)
		}
	}
	for _, request := range want {
		if got := origins[request]; len(got) != 3 || !got[""] || !got[capture.Origins[0]] || !got[capture.Origins[1]] {
			t.Errorf("the capture has %v for %q, want both origins and none", got, request)
		}
	}
}

// The capture shows that Jellyfin's answer to the ranged film exposes no header, while the task has a page on another
// origin read Content-Range, Accept-Ranges and Content-Length: so jellyscale differs, and compat/differences.yaml lists
// that difference, under the request "cors: ranged film", with its reason. (Without this entry the replay of
// internal/api/cors_integration_test.go would name it as unlisted.)
func TestDifferencesYamlListsThatJellyscaleExposesTheRangeHeadersJellyfinDoesNot(t *testing.T) {
	capture := readCorsCapture(t)
	for _, row := range capture.Rows {
		if row.Request == "ranged film" && row.Headers["access-control-expose-headers"] != "" {
			t.Fatalf("Jellyfin exposes %q: this check's premise is wrong", row.Headers["access-control-expose-headers"])
		}
	}
	raw, err := os.ReadFile("../../compat/differences.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range strings.Split(string(raw), "\n- ") {
		if strings.Contains(entry, `"cors: ranged film"`) && strings.Contains(entry, "header access-control-expose-headers") && strings.Contains(entry, "reason:") {
			return
		}
	}
	t.Error(`compat/differences.yaml has no entry for "cors: ranged film" with the path "header access-control-expose-headers" and a reason`)
}
