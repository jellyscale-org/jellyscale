//go:build integration

package subtitles

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-028, second attempt: what the film's pass costs when it meets a track or a
// file ffmpeg can't read. The film has three SubRip text tracks, streams 1 to 3.

// countFFmpeg puts an ffmpeg in front of the real one that notes every run. The
// only ffmpeg FillText runs is the extraction, so the count is the number of
// times the film was opened.
func countFFmpeg(t *testing.T) (runs func() int) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >>%q\nexec %q \"$@\"\n", log, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(log)
		return strings.Count(string(data), "run")
	}
}

// threeTrackFilm makes a film whose text tracks are streams 1, 2 and 3. The
// track at position broken (1 to 3, 0 for none) holds bytes that are not UTF-8,
// which ffmpeg refuses to decode and names on stderr.
func threeTrackFilm(t *testing.T, broken int) (dir string, src items.Source) {
	t.Helper()
	testenv.FFmpeg(t)
	dir = t.TempDir()
	args := []string{"-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10:duration=3"}
	for i := 1; i <= 3; i++ {
		text := fmt.Sprintf("1\n00:00:02,000 --> 00:00:03,000\nLine %d\n", i)
		if i == broken {
			text = "1\n00:00:02,000 --> 00:00:03,000\n\xff\xfe\xfd caf\xe9\n"
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.srt", i))
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", "srt", "-i", path)
	}
	args = append(args, "-map", "0", "-map", "1", "-map", "2", "-map", "3", "-c:v", "libx264", "-preset", "ultrafast", "-c:s", "copy", filepath.Join(dir, "film.mkv"))
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	info, err := os.Stat(filepath.Join(dir, "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	streams := `[{"Index": 0, "Type": "Video"}`
	for i := 1; i <= 3; i++ {
		streams += fmt.Sprintf(`, {"Index": %d, "Type": "Subtitle", "Codec": "subrip", "IsTextSubtitleStream": true}`, i)
	}
	return dir, items.Source{ID: "src", Key: "film.mkv", ETag: "v1", Size: info.Size(), Streams: []byte(streams + "]")}
}

// A film with one text track ffmpeg refuses costs at most two reads, not one
// for the pass and one for every track: ffmpeg's stderr names the stream it
// failed on (here "sist#0:2/subrip"), so the pass is made again without it. The
// good tracks are stored and the bad one is not, wherever it lies.
func TestFillTextWithABrokenTrackReadsTheFilmAtMostTwice(t *testing.T) {
	for broken := 1; broken <= 3; broken++ {
		t.Run(fmt.Sprintf("track %d broken", broken), func(t *testing.T) {
			dir, src := threeTrackFilm(t, broken)
			runs := countFFmpeg(t)
			assets := store.NewPosix(t.TempDir())
			ctx := context.Background()
			if err := FillText(ctx, src, store.NewPosix(dir), assets, 0, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("FillText: %v", err)
			}
			if n := runs(); n > 2 {
				t.Errorf("%d reads of the film for one broken track, want at most 2", n)
			}
			for index := 1; index <= 3; index++ {
				track, found, err := Read(ctx, assets, Key(src, index))
				if err != nil || found != (index != broken) {
					t.Errorf("track %d: stored %v, err %v; want stored %v", index, found, err, index != broken)
				}
				if found && !strings.Contains(track.VTT, fmt.Sprintf("Line %d", index)) {
					t.Errorf("track %d holds another's text:\n%s", index, track.VTT)
				}
			}
		})
	}
}

// A film whose file ffmpeg can't open is read once, whatever its number of
// tracks, and stores nothing: there is no track to blame, so none is read alone.
func TestFillTextOnAFileFFmpegCannotOpenReadsItOnce(t *testing.T) {
	dir, src := threeTrackFilm(t, 0)
	if err := os.WriteFile(filepath.Join(dir, "film.mkv"), []byte("not a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs := countFFmpeg(t)
	assets := store.NewPosix(t.TempDir())
	// Whether FillText reports the file as bad is not the check; what it reads is.
	FillText(context.Background(), src, store.NewPosix(dir), assets, 0, slog.New(slog.DiscardHandler)) //nolint:errcheck
	if n := runs(); n != 1 {
		t.Errorf("%d reads of a file ffmpeg can't open, want 1", n)
	}
	for index := 1; index <= 3; index++ {
		if _, found, _ := Read(context.Background(), assets, Key(src, index)); found {
			t.Errorf("track %d stored from a file ffmpeg can't open", index)
		}
	}
}
