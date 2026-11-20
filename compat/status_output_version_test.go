package compat

import (
	"regexp"
	"testing"
)

// Every number docs/status.md gives as OutputVersion is the constant in internal/playback/spec.go.
func TestStatusGivesTheOutputVersionTheCodeHas(t *testing.T) {
	m := regexp.MustCompile(`OutputVersion\s+= (\d+)`).FindStringSubmatch(docText(t, "internal/playback/spec.go"))
	if m == nil {
		t.Fatal("no OutputVersion constant in internal/playback/spec.go")
	}
	got := regexp.MustCompile("OutputVersion` is (\\d+)").FindAllStringSubmatch(docText(t, "docs/status.md"), -1)
	if len(got) == 0 {
		t.Fatal("docs/status.md never says what OutputVersion is")
	}
	for _, g := range got {
		if g[1] != m[1] {
			t.Errorf("docs/status.md says OutputVersion is %s; the code has %s", g[1], m[1])
		}
	}
}
