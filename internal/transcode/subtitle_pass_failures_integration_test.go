//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-028, second attempt: the pass and the tracks and files ffmpeg can't read.

// brokenTrackFilm is textTracks with stream 3 holding bytes that are not UTF-8,
// a track ffmpeg refuses to decode. Streams 2 (English) and 4 (ASS) are good.
func (r *rig) brokenTrackFilm(t *testing.T) store.Store {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	en := write("en.srt", "1\n00:00:02,000 --> 00:00:04,000\n"+englishLine+"\n")
	bad := write("bad.srt", "1\n00:00:02,000 --> 00:00:04,000\n\xff\xfe\xfd caf\xe9\n")
	ass := write("loud.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n"+
		assStyle+",&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:02.00,0:00:04.00,Loud,,0,0,0,,"+assLine+"\n")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=30",
		"-f", "srt", "-i", en, "-f", "srt", "-i", bad, "-i", ass,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3", "-map", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "48", "-c:a", "aac", "-c:s", "copy",
		filepath.Join(film(t), "broken.mkv"))
	r.useFile(t, "broken.mkv")
	info, err := os.Stat(filepath.Join(film(t), "broken.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	source := r.source(t)
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	return assets
}

// source is the film's source as an api replica reads it for each request.
func (r *rig) source(t *testing.T) items.Source {
	t.Helper()
	source, found, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil || !found {
		t.Fatalf("source: %v %v", found, err)
	}
	return source
}

// forgetFailures makes every subtitle job older than the 30 seconds of failure
// memory (and a finished one as old as it), as a viewer who comes back later
// finds them.
func (r *rig) forgetFailures(t *testing.T) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET ended_at = ended_at - interval '1 hour' WHERE ended_at IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
}

// A text track inside the film that ffmpeg refuses to decode costs the first
// pick of any text track at most two reads of the film (the pass, then the pass
// again without the track ffmpeg named), the good tracks are served, and the
// bad one is hidden for good: asking again, even after the 30 seconds of failure
// memory, reads nothing, until the file changes (a new ETag), when the film is
// read afresh.
func TestABrokenTrackCostsAtMostTwoReadsAndIsNotReadAgain(t *testing.T) {
	for _, badFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("bad track first %v", badFirst), func(t *testing.T) {
			testenv.FFmpeg(t)
			r := newRig(t, nil)
			assets := r.brokenTrackFilm(t)
			runs, _ := shimFFmpeg(t, "")
			stop := r.worker(t, nil)
			defer stop()
			j := r.jobs(t)
			askBad := func() {
				t.Helper()
				if _, err := r.pick(t, j, assets, r.source(t), 3); !errors.Is(err, ErrUnreadable) {
					t.Fatalf("the broken track: %v, want ErrUnreadable", err)
				}
			}
			askGood := func(index int) {
				t.Helper()
				track, err := r.pick(t, j, assets, r.source(t), index)
				if err != nil {
					t.Fatalf("track %d beside the broken one: %v", index, err)
				}
				wantTrack(t, track, index)
			}
			if badFirst {
				askBad()
			} else {
				askGood(2)
			}
			if n := runs(); n > 2 {
				t.Fatalf("the first pick cost %d reads of the film, want at most 2", n)
			}
			askGood(2)
			askGood(4)
			askBad()
			settled := runs()
			if settled > 2 {
				t.Errorf("%d reads of the film for the picks of three tracks, want at most 2", settled)
			}

			r.forgetFailures(t)
			askBad()
			askGood(2)
			askGood(4)
			if n := runs() - settled; n != 0 {
				t.Errorf("asking again after the failure memory read the film %d more times, want none: a track that can't be extracted stays hidden", n)
			}

			// The file changed: its tracks are made afresh, at most in two reads.
			if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET object_etag = 'changed'"); err != nil {
				t.Fatal(err)
			}
			askGood(2)
			if n := runs() - settled; n < 1 || n > 2 {
				t.Errorf("a changed file cost %d reads, want 1 or 2", n)
			}
		})
	}
}

// A film whose file ffmpeg can't open costs one read for any number of asks
// within the failure memory, for whichever of its tracks are asked: the pass is
// the same job for them all, and there is no track to blame, so none is read
// alone.
func TestAFilmFFmpegCannotOpenCostsOneReadWhateverTracksAreAsked(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	if err := os.WriteFile(filepath.Join(film(t), "tracks.mkv"), []byte("not a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, _ := shimFFmpeg(t, "")
	stop := r.worker(t, nil)
	defer stop()
	j := r.jobs(t)
	for _, index := range []int{2, 3, 4, 2, 3} {
		if _, err := r.pick(t, j, assets, source, index); err == nil {
			t.Fatalf("track %d of a file ffmpeg can't open was served", index)
		}
	}
	if n := runs(); n != 1 {
		t.Errorf("%d reads of a file ffmpeg can't open for five asks, want 1", n)
	}
}

// A pass that fails for a reason outside the file (here ffmpeg is held past the
// worker's time limit) is not the tracks' fault: once the failure memory has
// passed, the next ask reads the film again and the film's good tracks are
// served and its broken one is hidden, as they would have been.
func TestAPassThatTimesOutIsNotRememberedAsTheTracksFault(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	assets := r.brokenTrackFilm(t)
	// The first ffmpeg to start hangs. This stands for a slow store: the shorter
	// time limit below could hide a pass that gives up too soon, which this test does
	// not look at.
	begun := filepath.Join(t.TempDir(), "begun")
	runs, _ := shimFFmpeg(t, fmt.Sprintf("if mkdir %q.lock 2>/dev/null; then touch %q; exec sleep 600; fi", begun, begun))
	stop := r.worker(t, func(w *Worker) { w.SubtitleTimeout = 3 * time.Second })
	defer stop()
	j := r.jobs(t)
	_, err := r.pick(t, j, assets, r.source(t), 2)
	if err == nil || errors.Is(err, ErrUnreadable) {
		t.Fatalf("the pick during a read that timed out: %v, want a failure that is not ErrUnreadable", err)
	}
	r.forgetFailures(t)
	track, err := r.pick(t, j, assets, r.source(t), 2)
	if err != nil {
		t.Fatalf("a good track after the timed-out pass: %v", err)
	}
	wantTrack(t, track, 2)
	if _, err := r.pick(t, j, assets, r.source(t), 3); !errors.Is(err, ErrUnreadable) {
		t.Errorf("the broken track: %v, want ErrUnreadable", err)
	}
	if n := runs(); n > 3 {
		t.Errorf("%d reads of the film (one timed out), want at most 3", n)
	}
}
