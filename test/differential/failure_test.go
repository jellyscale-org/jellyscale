package differential

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The runner refuses, before it talks to any server, an allowlist that names a
// request requests.yaml does not have, and says which.
func TestRunnerFailsOnAnEntryNamingNoRequest(t *testing.T) {
	dir := t.TempDir()
	list := filepath.Join(dir, "differences.yaml")
	if err := os.WriteFile(list, []byte("- {requests: [no such request], path: Foo, kind: improvement, reason: r}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reqs, _ := filepath.Abs("requests.yaml")
	cmd := exec.Command("go", "run", "./run", "-requests", reqs, "-allowlist", list,
		"-jellyfin", "http://127.0.0.1:1", "-jellyscale", "http://127.0.0.1:1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("passed: %s", out)
	}
	if !strings.Contains(string(out), "no such request") {
		t.Fatalf("the output does not name the missing request: %s", out)
	}
}
