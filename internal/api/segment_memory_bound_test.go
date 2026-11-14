package api

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-077. What the stream role holds for each viewer who is mid-segment, and what docs/running.md says
// about it. TestStreamMemoryDoesNotGrowWithViewersOrSegments (integration tier) fails above this bound;
// the docs state the same figure, for the requests and limits of a deployment (JS-056).
const (
	memoryViewers          = 16
	memorySegmentSize      = 16 << 20 // A copied segment of a 4K film: tens of MB.
	segmentMemoryPerViewer = 1 << 20  // At most a sixteenth of the segment, and the same for a segment of any size.
)

// docs/running.md says the stream role's memory is a fixed base plus a figure for each concurrent viewer,
// that the figure equals the bound the memory test asserts (the process's heap), that it does not grow with
// the film's bitrate or the segment's length, and that the kernel's send buffer per connection, bounded by
// tcp_wmem, comes on top of the heap: a viewer who stalls leaves the bytes already written there, a few MiB
// each, which JS-056's limits must count (a build streaming 16 stalled viewers held about 4 MiB each outside the heap).
func TestRunningDocsStateTheStreamRolesMemory(t *testing.T) {
	raw, err := os.ReadFile("../../docs/running.md")
	if err != nil {
		t.Fatal(err)
	}
	var paragraph string
	for _, p := range strings.Split(string(raw), "\n\n") {
		lower := strings.ToLower(p)
		if strings.Contains(lower, "stream") && strings.Contains(lower, "memory") && strings.Contains(lower, "per concurrent viewer") {
			paragraph = p
		}
	}
	if paragraph == "" {
		t.Fatal("docs/running.md has no paragraph that gives the stream role's memory: it must say \"stream\", \"memory\" and a figure \"per concurrent viewer\"")
	}
	base := regexp.MustCompile(`(?i)(\d+) MiB (?:of )?(?:fixed )?base|base of (\d+) MiB`).FindStringSubmatch(paragraph)
	if base == nil {
		t.Errorf("the paragraph gives no fixed base: write \"a base of N MiB\" (or \"N MiB base\"):\n%s", paragraph)
	}
	each := regexp.MustCompile(`(?i)(\d+(?:\.\d+)?) (KiB|MiB) per concurrent viewer`).FindStringSubmatch(paragraph)
	if each == nil {
		t.Fatalf("the paragraph gives no figure like \"1 MiB per concurrent viewer\":\n%s", paragraph)
	}
	figure, _ := strconv.ParseFloat(each[1], 64)
	if each[2] == "MiB" {
		figure *= 1 << 20
	} else {
		figure *= 1 << 10
	}
	if int64(figure) != segmentMemoryPerViewer {
		t.Errorf("docs/running.md says %s %s per concurrent viewer (%d bytes), and the memory test asserts %d bytes", each[1], each[2], int64(figure), segmentMemoryPerViewer)
	}
	lower := strings.ToLower(paragraph)
	for _, phrase := range []string{"heap", "send buffer", "tcp_wmem", "on top"} {
		if !strings.Contains(lower, phrase) {
			t.Errorf("the figure is the process's heap; the paragraph must also say the kernel's send buffer per connection (bounded by tcp_wmem) comes on top of it (missing %q):\n%s", phrase, paragraph)
		}
	}
	for _, phrase := range []string{"does not grow with", "bitrate", "length"} {
		if !strings.Contains(lower, phrase) {
			t.Errorf("the paragraph must say the figure does not grow with the film's bitrate or the segment's length (missing %q):\n%s", phrase, paragraph)
		}
	}
}
