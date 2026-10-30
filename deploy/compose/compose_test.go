package compose

import (
	"os"
	"strings"
	"testing"
)

// Every role opens every library's stores at startup, so S3 credentials must
// reach all of them through the shared environment block.
func TestSharedEnvironmentCarriesS3Credentials(t *testing.T) {
	raw, err := os.ReadFile("compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	block, _, _ := strings.Cut(string(raw), "\nservices:")
	for _, name := range []string{"JELLYSCALE_S3_ACCESS_KEY", "JELLYSCALE_S3_SECRET_KEY"} {
		if !strings.Contains(block, "    "+name+": ${"+name) {
			t.Errorf("x-app environment does not pass %s", name)
		}
	}
}
