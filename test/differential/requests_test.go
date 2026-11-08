package differential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Allowlist entries name requests, so two requests with one name would let an
// entry written for one hide a difference in the other.
func TestTwoRequestsWithOneNameAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.yaml")
	text := "- name: films\n  path: /Items?IncludeItemTypes=Movie\n- name: films\n  path: /Items?IncludeItemTypes=Movie&SortBy=SortName\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRequests(path)
	if err == nil || !strings.Contains(err.Error(), `"films"`) {
		t.Fatalf("want an error naming the repeated request, got %v", err)
	}
}
