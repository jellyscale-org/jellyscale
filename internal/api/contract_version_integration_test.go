//go:build integration

package api

// JS-183, criterion 2: the identity reports the version the server was built with. The build sets buildVersion with
// -ldflags -X; this test sets it and restores it. The image half, that a built image reports its own label, is in test/image.sh.

import (
	"net/http"
	"testing"
)

func TestServerIdentityReportsTheVersionItWasBuiltWith(t *testing.T) {
	previous := buildVersion
	buildVersion = "7.8.9-check"
	t.Cleanup(func() { buildVersion = previous })
	kit := newEnv(t)
	response, raw := kit.do(t, 0, http.MethodGet, "/jellyscale/v1/server", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /jellyscale/v1/server with no token: %d %.120q, want 200", response.StatusCode, raw)
	}
	fields := exactKeys(t, raw, []string{"name", "id", "version", "apiVersion", "capabilities"})
	if version := stringField(t, fields, "version"); version != "7.8.9-check" {
		t.Fatalf("Version %q, want the build's 7.8.9-check", version)
	}
}
