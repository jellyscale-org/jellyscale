//go:build integration

package subtitles

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"jellyscale/internal/store"
)

// JS-028, third attempt: tracks the file lacks (listed by a scan, then the file
// was replaced). ffmpeg names only the first of them, so they are dropped one
// run at a time; none of them, nor an undecodable track beside them, may cost
// the good tracks.
func TestFillTextServesGoodTracksWhateverTheFileLacksOrCannotDecode(t *testing.T) {
	for _, c := range []struct {
		name            string
		broken          int
		extra, wantGood []int
	}{
		{"one missing", 0, []int{7}, []int{1, 2, 3}},
		{"two missing", 0, []int{7, 8}, []int{1, 2, 3}},
		{"three missing", 0, []int{9, 8, 7}, []int{1, 2, 3}},
		{"two missing and one undecodable", 3, []int{7, 8}, []int{1, 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, src := threeTrackFilm(t, c.broken)
			streams := strings.TrimSuffix(string(src.Streams), "]")
			for _, i := range c.extra {
				streams += fmt.Sprintf(`, {"Index": %d, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true}`, i)
			}
			src.Streams = []byte(streams + "]")
			assets := store.NewPosix(t.TempDir())
			ctx := context.Background()
			if err := FillText(ctx, src, store.NewPosix(dir), assets, 0, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("FillText: %v", err)
			}
			for _, index := range append([]int{1, 2, 3}, c.extra...) {
				want := false
				for _, g := range c.wantGood {
					want = want || g == index
				}
				track, found, err := Read(ctx, assets, Key(src, index))
				if err != nil || found != want {
					t.Errorf("track %d: stored %v, err %v; want stored %v", index, found, err, want)
				}
				if found && !strings.Contains(track.VTT, fmt.Sprintf("Line %d", index)) {
					t.Errorf("track %d holds another's text:\n%s", index, track.VTT)
				}
			}
		})
	}
}
