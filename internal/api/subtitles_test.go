package api

import (
	"testing"

	"jellyscale/internal/playback"
)

// jellyfin-web reads DefaultSubtitleStreamIndex as a position in MediaStreams,
// so it is only given where the position holds that track.
func TestChosenSubtitleNeedsItsPosition(t *testing.T) {
	stream := func(index int, kind string) any { return map[string]any{"Index": float64(index), "Type": kind} }
	// A second audio track the list leaves out puts the subtitle at position 2, Index 3.
	source := map[string]any{"MediaStreams": []any{stream(0, "Video"), stream(1, "Audio"), stream(3, "Subtitle")}}
	three, two := playback.Number(3), playback.Number(2)
	if index, ok := chosenSubtitle(source, &three); ok {
		t.Errorf("track 3 sits at position 2, yet answered %d", index)
	}
	if index, ok := chosenSubtitle(source, &two); ok {
		t.Errorf("position 2 isn't track 2, yet answered %d", index)
	}
	source["MediaStreams"] = []any{stream(0, "Video"), stream(1, "Audio"), stream(2, "Subtitle")}
	if index, ok := chosenSubtitle(source, &two); !ok || index != 2 {
		t.Errorf("track at its own position: %d %v", index, ok)
	}
}
