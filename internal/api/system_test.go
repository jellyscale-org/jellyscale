package api

import (
	"net/http/httptest"
	"testing"
)

// Clients built on jellyfin-sdk-kotlin (Findroid, the Android TV app) score a
// server whose ProductName is not exactly "Jellyfin Server" as bad and drop it,
// so the app says "Server not found" though the request succeeded.
func TestPublicInfoCallsItselfJellyfinServer(t *testing.T) {
	s := &Server{Name: "jellyscale"}
	info := s.publicInfo(httptest.NewRequest("GET", "/System/Info/Public", nil))
	if got := info["ProductName"]; got != "Jellyfin Server" {
		t.Fatalf("ProductName = %v, want Jellyfin Server", got)
	}
	if got := info["ServerName"]; got != "jellyscale" {
		t.Fatalf("ServerName = %v, want the configured name", got)
	}
}
