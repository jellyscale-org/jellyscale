package transcode

import (
	"testing"
	"time"

	"jellyscale/internal/playback"
)

func TestLimitFor(t *testing.T) {
	encoded := playback.EncodedLayout(90 * ticksPerSecond) // 22 segments of 4 s and a last of 2 s.
	// Segments of 4 to 12 s, as a copy has: the lead is film time, not a count.
	copied := playback.Layout{Starts: []int64{0, 4, 16, 20, 24, 36, 48, 52}, End: 56}
	for i := range copied.Starts {
		copied.Starts[i] *= ticksPerSecond
	}
	copied.End *= ticksPerSecond
	for _, c := range []struct {
		name   string
		layout playback.Layout
		from   int
		lead   time.Duration
		want   int
	}{
		{"the default lead is 15 segments of 4 s", encoded, 0, 60 * time.Second, 15},
		{"counted from the segment asked for", encoded, 5, 60 * time.Second, 20},
		{"the film ends within the lead", encoded, 10, 60 * time.Second, 23},
		{"a lead of 12 s is 3 segments of 4 s", encoded, 2, 12 * time.Second, 5},
		{"long copied segments use up the lead sooner", copied, 0, 20 * time.Second, 3},
		{"short copied segments use it up later", copied, 1, 20 * time.Second, 4},
		{"a lead shorter than a segment still lets the next one in", copied, 0, time.Second, 1},
	} {
		if got := limitFor(c.layout, c.from, c.lead); got != c.want {
			t.Errorf("%s: limit %d, want %d", c.name, got, c.want)
		}
	}
}
