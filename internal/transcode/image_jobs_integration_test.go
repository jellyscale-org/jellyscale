//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"

	"jellyscale/internal/testenv"
)

// useImageFilm makes the film of imageFilm the rig's, with its track as stream 2.
func (r *rig) useImageFilm(t *testing.T, codec string) (items.Source, store.Store) {
	t.Helper()
	imageFilm(t, film(t), "image.mkv", codec)
	r.useFile(t, "image.mkv")
	source, found, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil || !found {
		t.Fatalf("source: %v %v", found, err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	return source, assets
}

// countRuns puts an ffmpeg in front of the real one that notes each run whose
// arguments match, and returns the count.
func countRuns(t *testing.T, match string) func() int {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *\"%s\"*) echo run >>%q;; esac\nexec %q \"$@\"\n", match, runs, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(runs)
		return strings.Count(string(data), "run")
	}
}

// A burn of an image track needs no subtitle job: the transcode reads the track
// from the film itself, along with the video. The segment it makes has the bar
// in it where the track says.
func TestImageSubtitleBurnsWithoutAnExtraction(t *testing.T) {
	for _, codec := range []string{"copy", "dvdsub"} {
		t.Run(codec, func(t *testing.T) {
			r := newRig(t, nil)
			r.useImageFilm(t, codec)
			extractions := countRuns(t, "track.vtt")
			r.worker(t, nil)
			j := r.jobs(t)
			index := 2
			r.spec.Subtitle = &index
			r.spec.Width, r.spec.Height = 640, 360
			r.hash = r.spec.Hash()
			// Segment 1 is 4 to 8 seconds; the top bar is up from 6.
			init, err := r.fetchAt(t, j, psid(0), -1, 1)
			if err != nil {
				t.Fatal(err)
			}
			segment, err := r.fetch(t, j, psid(0), 1)
			if err != nil {
				t.Fatal(err)
			}
			joined := filepath.Join(t.TempDir(), "burnt.mp4")
			if err := os.WriteFile(joined, append(init, segment...), 0o644); err != nil {
				t.Fatal(err)
			}
			frames := brightByHalf(t, joined)
			if len(frames) < 4 || frames[0] != [2]int{} || frames[1] != [2]int{} || frames[2][0] < 20 || frames[2][1] > 3 || frames[3][0] < 20 || frames[3][1] > 3 {
				t.Errorf("bright pixels by half in each second of segment 1: %v; want none for two seconds, then the top bar", frames)
			}
			if n := extractions(); n != 0 {
				t.Errorf("%d text extractions for an image track", n)
			}
			if n := r.count(t, "SELECT count(*) FROM subtitle_jobs"); n != 0 {
				t.Errorf("%d subtitle jobs for an image track", n)
			}
		})
	}
}

// Many requests copy a PGS track out of the film once, on whichever of two
// workers claims the job, and what lands in the asset store is the PGS stream.
func TestOneSupExtractionForManyRequests(t *testing.T) {
	r := newRig(t, nil)
	source, assets := r.useImageFilm(t, "copy")
	runs := countRuns(t, "-f sup")
	r.worker(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	keys := make([]string, len(errs))
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = FetchSup(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || keys[i] != subtitles.SupKey(source, 2) {
			t.Fatalf("request %d: %q %v", i, keys[i], err)
		}
	}
	if n := runs(); n != 1 {
		t.Fatalf("%d extractions, want 1", n)
	}
	// Cached: another ask runs nothing, though no worker is needed for it.
	if _, err := FetchSup(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2); err != nil || runs() != 1 {
		t.Fatalf("a cached track: %v, %d runs", err, runs())
	}
	local, ok := assets.LocalPath(subtitles.SupKey(source, 2))
	if !ok {
		t.Fatal("the asset store has no local path")
	}
	out := run(t, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", local)
	if strings.TrimSpace(out) != "hdmv_pgs_subtitle,1920,1080" {
		t.Errorf("the stored track is %q", out)
	}
	// The track and the .sup are two objects, so a text track's key is not theirs.
	if subtitles.SupKey(source, 2) == subtitles.Key(source, 2) {
		t.Error("one key for both")
	}
}

// A stream that is not a PGS track can't be copied out as one: the job fails
// for good, as one that finds the file unreadable does.
func TestSupExtractionOfAnotherStreamFails(t *testing.T) {
	r := newRig(t, nil)
	source, assets := r.useImageFilm(t, "dvdsub")
	r.worker(t, nil)
	j := r.jobs(t)
	// Stream 1 is the film's audio; its bytes would not be a .sup.
	began := time.Now()
	_, err := FetchSup(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 1)
	if err == nil || !errors.Is(err, ErrUnreadable) || time.Since(began) > 30*time.Second {
		t.Fatalf("after %s: %v", time.Since(began), err)
	}
}
