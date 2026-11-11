//go:build integration

package transcode

import (
	"context"
	"testing"

	"jellyscale/internal/library"
	"jellyscale/internal/testenv"
)

// Learning where the cue showing at a cut began costs the store a handful of
// small reads, however many cues the film has: reading each cue's packet to
// classify it (at the scan or in a run) would cost one Open for every cue of
// every image track, which the byte counts of the other checks do not show.
// A 256 s film with a cue every 4 seconds (64 cues) is read for its points
// once, and then asked for the picture at many cuts.
func TestTheCuePointsAndAPictureAtACutCostAHandfulOfReads(t *testing.T) {
	var cues []testenv.Cue
	for k := range 64 {
		cue := top
		if k%2 == 1 {
			cue = bottom
		}
		cues = append(cues, cue(float64(4*k+1), float64(4*k+3)))
	}
	media := t.TempDir()
	size := cueFilm{codec: "copy", seconds: 256, cues: cues}.write(t, media, "film")
	m := newMeter(media, 0)
	ctx := context.Background()
	found, err := library.ReadImageCues(ctx, m, "film.mkv", size)
	if err != nil {
		t.Fatal(err)
	}
	track, ok := found[2]
	if !ok || len(track.Points) < 64 {
		t.Fatalf("the film's Cues gave %d points for the image track (found %v)", len(track.Points), ok)
	}
	if opens := m.opens.Load(); opens > 8 {
		t.Errorf("reading the cue points opened the film %d times, for a film of 64 cues: it grows with them", opens)
	}
	// A run opens at or before the start of the picture showing at its cut, and
	// at its cut if none is (ffmpeg's Cues put a keyframe at least every 5 s of
	// the file, so it is never far before).
	for _, tc := range []struct {
		cut, latest float64
	}{{2, 1}, {3.5, 3.5}, {130.5, 129}, {131.5, 131.5}, {250.2, 249}, {255.9, 255.9}} {
		before := m.opens.Load()
		open, err := track.OpenAt(ctx, m, "film.mkv", size, tc.cut)
		if err != nil {
			t.Fatal(err)
		}
		if open > tc.latest || open < tc.latest-6 {
			t.Errorf("at %v s: a run opens at %v, want between %v and %v", tc.cut, open, tc.latest-6, tc.latest)
		}
		if opens := m.opens.Load() - before; opens > 4 {
			t.Errorf("the picture at %v s cost %d opens of the film", tc.cut, opens)
		}
	}
}
