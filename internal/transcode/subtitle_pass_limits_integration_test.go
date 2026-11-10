//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-028, third attempt: any number of bad tracks, a full time limit for every
// run, and a mark that names the version of the file it was found in.

// twoBrokenTracksFilm is a film with English (stream 2), two tracks ffmpeg
// refuses to decode (3 and 4, bytes that are not UTF-8) and an ASS track (5).
func (r *rig) twoBrokenTracksFilm(t *testing.T) store.Store {
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
	bad2 := write("bad2.srt", "1\n00:00:02,000 --> 00:00:04,000\n\xfe\xff\xfc na\xefve\n")
	ass := write("loud.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n"+
		assStyle+",&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:02.00,0:00:04.00,Loud,,0,0,0,,"+assLine+"\n")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=30",
		"-f", "srt", "-i", en, "-f", "srt", "-i", bad, "-f", "srt", "-i", bad2, "-i", ass,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3", "-map", "4", "-map", "5",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "48", "-c:a", "aac", "-c:s", "copy",
		filepath.Join(film(t), "twobroken.mkv"))
	r.useFile(t, "twobroken.mkv")
	info, err := os.Stat(filepath.Join(film(t), "twobroken.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
	assets, err := store.Open(r.source(t).AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	return assets
}

// One run of ffmpeg logs every stream it fails on, so a film with two tracks it
// can't decode still costs the first pick at most two reads (the pass, then the
// pass without all the tracks it named), not one more per bad track. The good
// tracks are served, the bad ones hidden, and none is read again.
func TestTwoBrokenTracksCostAtMostTwoReads(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	assets := r.twoBrokenTracksFilm(t)
	runs, _ := shimFFmpeg(t, "")
	stop := r.worker(t, nil)
	defer stop()
	j := r.jobs(t)
	track, err := r.pick(t, j, assets, r.source(t), 2)
	if err != nil {
		t.Fatalf("the English track beside two broken ones: %v", err)
	}
	if !strings.Contains(track.VTT, englishLine) {
		t.Errorf("track 2 is not the English text:\n%s", track.VTT)
	}
	if n := runs(); n > 2 {
		t.Errorf("the first pick cost %d reads of the film, want at most 2", n)
	}
	track, err = r.pick(t, j, assets, r.source(t), 5)
	if err != nil || !strings.Contains(track.ASS, assStyle) {
		t.Errorf("the ASS track beside two broken ones: %v %q", err, track.ASS)
	}
	for _, index := range []int{3, 4} {
		if _, err := r.pick(t, j, assets, r.source(t), index); !errors.Is(err, ErrUnreadable) {
			t.Errorf("broken track %d: %v, want ErrUnreadable", index, err)
		}
	}
	settled := runs()
	if settled > 2 {
		t.Errorf("%d reads of the film for the picks of four tracks, want at most 2", settled)
	}
	r.forgetFailures(t)
	for _, index := range []int{3, 4, 2, 5} {
		r.pick(t, j, assets, r.source(t), index) //nolint:errcheck
	}
	if n := runs() - settled; n != 0 {
		t.Errorf("asking again after the failure memory read the film %d more times, want none", n)
	}
}

// Each run of a pass has the whole time limit, as each per-track read had. Here
// the first two runs each take 4 seconds before ffmpeg starts, under a 6 second
// limit: together they are over it, each alone is not. A shared deadline kills
// the second run and serves no track. The shorter limit could hide a run that
// is given less than the limit but still enough; the runs here are 2/3 of it, so
// a run given half is also caught.
func TestEveryRunOfAPassHasTheFullTimeLimit(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	assets := r.brokenTrackFilm(t)
	counter := t.TempDir()
	runs, _ := shimFFmpeg(t, fmt.Sprintf(
		"n=$(ls %q | wc -l); touch %q/$$; if [ \"$n\" -lt 2 ]; then sleep 4; fi", counter, counter))
	stop := r.worker(t, func(w *Worker) { w.SubtitleTimeout = 6 * time.Second })
	defer stop()
	j := r.jobs(t)
	for _, index := range []int{2, 4} {
		track, err := r.pick(t, j, assets, r.source(t), index)
		if err != nil {
			t.Fatalf("track %d, whose pass took 8 seconds in two runs of 4 under a limit of 6: %v", index, err)
		}
		wantTrack(t, track, index)
	}
	if _, err := r.pick(t, j, assets, r.source(t), 3); !errors.Is(err, ErrUnreadable) {
		t.Errorf("the broken track: %v, want ErrUnreadable", err)
	}
	if n := runs(); n != 2 {
		t.Errorf("%d reads of the film, want 2", n)
	}
}

// A pass over one version of the film marks no track of another. The pass for
// version v1 is held in its ffmpeg while the file is replaced (a scan stores
// v2's streams, none unreadable); v1's pass then finds track 3 can't be
// extracted and must not hide track 3 of v2, which nothing has tried.
func TestAPassOverAnOlderVersionMarksNothingOnTheNewer(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	assets := r.brokenTrackFilm(t)
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	shimFFmpeg(t, fmt.Sprintf("touch %q; while [ ! -e %q ]; do sleep 0.1; done", started, release))
	stop := r.worker(t, nil)
	defer stop()
	j := r.jobs(t)
	old := r.source(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.pick(t, j, assets, old, 2)
		done <- err
	}()
	waitFor(t, "the pass over v1 to start ffmpeg", 30*time.Second, func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET object_etag = 'v2'"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the pick during the pass over v1: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the pass over v1 did not end")
	}
	if subtitles.Unreadable(r.source(t), 3) {
		t.Error("track 3 of v2 is marked unreadable by the pass over v1; nothing has read v2")
	}
	// Control: the pass over the version in the library does hide its bad track.
	if _, err := r.pick(t, j, assets, r.source(t), 2); err != nil {
		t.Fatalf("a good track of v2: %v", err)
	}
	if !subtitles.Unreadable(r.source(t), 3) {
		t.Error("the pass over v2 left its broken track 3 unhidden")
	}
}
