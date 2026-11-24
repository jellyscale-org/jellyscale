package api

import (
	"encoding/json"
	"os"
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
