//go:build integration

package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// A worker draws the track its job names, extracting it from the source on the
// way, and a track it can't read fails the job with the reason and leaves no
// ffmpeg behind.
func TestWorkerBurnsTheTrackOrFailsTheJob(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.useBurnFilm(t)
	r.worker(t, nil)
	j := r.jobs(t)

	// Segment 6 is 24 to 28 seconds, inside the cue at the bottom.
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	init, err := r.fetchAt(t, j, psid(0), -1, 6)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := r.fetch(t, j, psid(0), 6)
	if err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "burnt.mp4")
	if err := os.WriteFile(joined, append(init, segment...), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stream 1 is audio: there is no such track to draw.
	bad := 1
	r.spec.Subtitle = &bad
	r.hash = r.spec.Hash()
	began := time.Now()
	_, err = r.fetch(t, j, psid(1), 0)
	var failed FailedError
	if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "subtitle track 1") || time.Since(began) > 20*time.Second {
		t.Fatalf("after %s: %v", time.Since(began), err)
	}
	waitFor(t, "no ffmpeg to be left", 10*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })

	testenv.Font(t)
	frames := brightByHalf(t, joined)
	if len(frames) < 3 {
		t.Fatalf("%d frames", len(frames))
	}
	for second, half := range frames[:3] {
		if half[1] < 20 || half[0] > 3 {
			t.Errorf("second %d of the segment: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
	}
}

// useBurnFilm makes a black film with the burn track as its stream 2.
func (r *rig) useBurnFilm(t *testing.T) {
	t.Helper()
	dir := film(t)
	track := filepath.Join(t.TempDir(), "track.ass")
	if err := os.WriteFile(track, []byte(burnTrack), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40", "-i", track,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", "-c:s", "ass",
		filepath.Join(dir, "black.mkv"))
	r.useFile(t, "black.mkv")
}

// A worker keeps its lease while it extracts a track, which can take minutes,
// and gives the extraction up, with its slot, when the worker is stopped.
func TestWorkerHoldsItsLeaseWhileExtracting(t *testing.T) {
	r := newRig(t, nil)
	r.useBurnFilm(t)
	real := testenv.FFmpeg(t)
	shimDir := t.TempDir()
	begun := filepath.Join(shimDir, "begun")
	// Waits only for the extraction, which is the run that writes track files.
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *track.vtt*) touch %q; while true; do sleep 0.1 >/dev/null 2>&1; done;; esac\nexec %q \"$@\"\n", begun, real)
	if err := os.WriteFile(filepath.Join(shimDir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stop := r.worker(t, nil)
	j := r.jobs(t)
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	go r.fetch(t, j, psid(0), 0) //nolint:errcheck
	waitFor(t, "the extraction to start", 20*time.Second, func() bool { _, err := os.Stat(begun); return err == nil })
	// A claim gives the lease and the startup time; a heartbeat gives the lease
	// alone, which is shorter. One must come while the extraction runs.
	waitFor(t, "a heartbeat during the extraction", 10*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND lease_expires_at < now() + interval '5 seconds'") == 1
	})
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopping the worker waited for the extraction")
	}
	waitFor(t, "no ffmpeg to be left", 10*time.Second, func() bool { return len(r.ffmpegProcesses()) == 0 })
}

// A scratch directory given as a relative path works: ffmpeg runs inside the
// job's directory when it burns, so the paths it is given can't be relative.
func TestBurnWithRelativeScratch(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.useBurnFilm(t)
	// Beside the scratch directory, so the path needs no "..", which would
	// resolve by luck when doubled.
	t.Chdir(filepath.Dir(r.scratch))
	r.worker(t, func(w *Worker) { w.Scratch = filepath.Base(r.scratch) })
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatal(err)
	}
}

// A read of the track that outlasts the time after which the sweep takes a
// scratch directory for abandoned doesn't cost the job its directory.
func TestBurnSurvivesALongRead(t *testing.T) {
	r := newRig(t, nil)
	r.useBurnFilm(t)
	real := testenv.FFmpeg(t)
	shimDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *track.vtt*) sleep 3 >/dev/null 2>&1;; esac\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(filepath.Join(shimDir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := scratchIdle
	scratchIdle = time.Second
	t.Cleanup(func() { scratchIdle = old })
	var worker *Worker
	r.worker(t, func(w *Worker) { worker = w })
	stopSweeping := make(chan struct{})
	t.Cleanup(func() { close(stopSweeping) })
	go func() {
		for {
			select {
			case <-stopSweeping:
				return
			case <-time.After(100 * time.Millisecond):
				worker.clearScratch()
			}
		}
	}()
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatal(err)
	}
}

// A track that is a subtitle file beside the film is drawn like one inside it,
// read from its file, and a job for a file that has since changed fails.
func TestWorkerBurnsAnExternalTrack(t *testing.T) {
	testenv.FFmpeg(t)
	testenv.Font(t)
	r := newRig(t, nil)
	dir := film(t)
	if err := os.WriteFile(filepath.Join(dir, "plain.ass"), []byte(burnTrack), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", filepath.Join(dir, "plain.mkv"))
	r.useFile(t, "plain.mkv")
	if _, err := r.pool.Exec(context.Background(), `UPDATE media_sources SET streams = streams || $1::jsonb`,
		`[{"Index": 2, "Type": "Subtitle", "Codec": "ass", "IsExternal": true, "IsTextSubtitleStream": true, "Path": "plain.ass", "Fingerprint": "f1"}]`); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	j := r.jobs(t)

	// Segment 6 is 24 to 28 seconds, inside the cue at the bottom.
	index := 2
	r.spec.Subtitle, r.spec.SubtitleFingerprint = &index, "f1"
	r.hash = r.spec.Hash()
	init, err := r.fetchAt(t, j, psid(0), -1, 6)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := r.fetch(t, j, psid(0), 6)
	if err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "burnt.mp4")
	if err := os.WriteFile(joined, append(init, segment...), 0o644); err != nil {
		t.Fatal(err)
	}
	frames := brightByHalf(t, joined)
	if len(frames) < 3 {
		t.Fatalf("%d frames", len(frames))
	}
	for second, half := range frames[:3] {
		if half[1] < 20 || half[0] > 3 {
			t.Errorf("second %d of the segment: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
	}

	// The file was edited and scanned since this spec was made.
	r.spec.SubtitleFingerprint = "an older version"
	r.hash = r.spec.Hash()
	var failed FailedError
	if _, err := r.fetch(t, j, psid(1), 0); !errors.As(err, &failed) || !strings.Contains(failed.Reason, "subtitle file has changed") {
		t.Fatalf("a stale file: %v", err)
	}
}

// A burnt-in track is drawn with the fonts attached to the film, which a fonts
// job takes out of the film for the burn-in job to wait on, as jellyfin-web
// draws a track it renders itself. fontconfig is given no fonts, so the only
// font libass can draw with is the attached one: the text is in the picture
// when the film has the font and the job puts it in place, and not when it has
// none. Without the burn-in's call to put the film's fonts in place, the first
// film shows no text.
func TestBurnDrawsWithTheFontsOfTheFilm(t *testing.T) {
	testenv.FFmpeg(t)
	testenv.Font(t)
	font := strings.TrimSpace(run(t, "fc-match", "-f", "%{file}", "serif"))
	family := strings.TrimSpace(run(t, "fc-match", "-f", "%{family[0]}", "serif"))
	family, _, _ = strings.Cut(family, ",")
	conf := filepath.Join(t.TempDir(), "fonts.conf")
	if err := os.WriteFile(conf, []byte(`<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig></fontconfig>`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FONTCONFIG_FILE", conf)
	track := filepath.Join(t.TempDir(), "track.ass")
	if err := os.WriteFile(track, []byte(strings.ReplaceAll(burnTrack, "Arial", family)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, attached := range []bool{true, false} {
		name := fmt.Sprintf("fonts-%v.mkv", attached)
		args := []string{"-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24:duration=40",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40", "-i", track,
			"-map", "0:v", "-map", "1:a", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", "-c:s", "ass"}
		if attached {
			args = append(args, "-attach", font, "-metadata:s:t:0", "mimetype=font/ttf")
		}
		run(t, "ffmpeg", append(args, filepath.Join(film(t), name))...)
		r := newRig(t, nil)
		r.useFile(t, name)
		stop := r.worker(t, nil)
		index := 2
		r.spec.Subtitle = &index
		r.hash = r.spec.Hash()
		j := r.jobs(t)
		init, err := r.fetchAt(t, j, psid(0), -1, 6)
		if err != nil {
			t.Fatalf("attached %v: %v", attached, err)
		}
		segment, err := r.fetch(t, j, psid(0), 6)
		if err != nil {
			t.Fatalf("attached %v: %v", attached, err)
		}
		joined := filepath.Join(t.TempDir(), "burnt.mp4")
		if err := os.WriteFile(joined, append(init, segment...), 0o644); err != nil {
			t.Fatal(err)
		}
		frames := brightByHalf(t, joined)
		if len(frames) < 3 {
			t.Fatalf("%d frames", len(frames))
		}
		for second, half := range frames[:3] {
			switch {
			case attached && (half[1] < 20 || half[0] > 3):
				t.Errorf("with the font attached, second %d: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
			case !attached && half[0]+half[1] > 0:
				t.Errorf("with no font anywhere, second %d has %d bright pixels", second, half[0]+half[1])
			}
		}
		stop() // The next film has its own rig, and this worker would take its jobs.
		if got, want := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'fonts'"), map[bool]int{true: 1, false: 0}[attached]; got != want {
			t.Errorf("attached %v: %d fonts jobs, want %d", attached, got, want)
		}
	}
}

// burnWithText burns track 2 of the rig's film for its segment 6 (24 to 28
// seconds) and returns the bright pixels of the top and bottom halves of each
// second. A track failure fails the test.
func (r *rig) burnWithText(t *testing.T) [][2]int {
	t.Helper()
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	j := r.jobs(t)
	init, err := r.fetchAt(t, j, psid(0), -1, 6)
	if err != nil {
		t.Fatal(err)
	}
	segment, err := r.fetch(t, j, psid(0), 6)
	if err != nil {
		t.Fatal(err)
	}
	joined := filepath.Join(t.TempDir(), "burnt.mp4")
	if err := os.WriteFile(joined, append(init, segment...), 0o644); err != nil {
		t.Fatal(err)
	}
	frames := brightByHalf(t, joined)
	if len(frames) < 3 {
		t.Fatalf("%d frames", len(frames))
	}
	return frames
}

// An ASS film with a font of its own attached, so that burning it has fonts to
// wait for, and the track's own text at the bottom from 20 s.
func (r *rig) useFilmWithFont(t *testing.T, codec, track string, attach bool) {
	t.Helper()
	testenv.Font(t)
	file := filepath.Join(t.TempDir(), "track."+codec)
	if err := os.WriteFile(file, []byte(track), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:size=640x360:rate=24:duration=40",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=40", "-i", file,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", "-c:s", map[string]string{"ass": "ass", "srt": "srt"}[codec]}
	if attach {
		font := strings.TrimSpace(run(t, "fc-match", "-f", "%{file}", "serif"))
		args = append(args, "-attach", font, "-metadata:s:t:0", "mimetype=font/ttf")
	}
	run(t, "ffmpeg", append(args, filepath.Join(film(t), "fonts.mkv"))...)
	r.useFile(t, "fonts.mkv")
}

const bottomCue = "1\n00:00:20,000 --> 00:00:40,000\nBOTTOM LINE OF SUBTITLE TEXT\n"

// A SubRip track names no font, so burning it does not wait for the film's
// fonts: no fonts job is made, however many fonts the film has.
func TestSubRipBurnDoesNotWaitForFonts(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.useFilmWithFont(t, "srt", bottomCue, true)
	r.worker(t, nil)
	for second, half := range r.burnWithText(t)[:3] {
		if half[1] < 5 || half[0] > 3 {
			t.Errorf("second %d: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
	}
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'fonts'"); n != 0 {
		t.Errorf("a SubRip burn made %d fonts jobs", n)
	}
}

// Segments are kept under a hash that doesn't name the fonts, so a burn drawn
// without fonts that failed for a reason that may pass (here the fonts job has
// failed on the store) would be served to every viewer for a day. The burn
// fails instead, and a later request, once the failure is forgotten, burns with
// the fonts.
func TestAssBurnFailsWhenTheFontsJobFailedTransiently(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.useFilmWithFont(t, "ass", strings.ReplaceAll(burnTrack, "0:00:06.00,0:00:10.00,Top", "0:00:20.00,0:00:40.00,Bottom"), true)
	source, _, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, error, ended_at)
		VALUES ($1, 'fonts', $2, $3, 0, 'failed', 'the store is gone', now())`, subtitles.FontsKey(source.ID, source.ETag), r.spec.Item, source.ID); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	if _, err := r.fetchAt(t, r.jobs(t), psid(0), -1, 6); err == nil {
		t.Fatal("a burn without the film's fonts was served")
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed' AND error LIKE 'fonts of the film:%'"); n != 1 {
		t.Fatalf("%d jobs failed on the fonts", n)
	}
	if _, found, _ := subtitles.ReadFonts(context.Background(), assets, source); found {
		t.Fatal("the failed fonts job left a result")
	}
	// Once the failures are forgotten the fonts are asked for again.
	for _, table := range []string{"subtitle_jobs", "transcode_jobs"} {
		if _, err := r.pool.Exec(context.Background(), "UPDATE "+table+" SET ended_at = now() - interval '1 hour'"); err != nil {
			t.Fatal(err)
		}
	}
	for second, half := range r.burnWithText(t)[:3] {
		if half[1] < 5 || half[0] > 3 {
			t.Errorf("second %d: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
	}
	if fonts, found, err := subtitles.ReadFonts(context.Background(), assets, source); err != nil || !found || len(fonts) != 1 {
		t.Errorf("fonts after the second burn: %v %v %v", fonts, found, err)
	}
}

// A film whose fonts ffmpeg rejects (an error exit on the data) is remembered
// as having none for as long as it is this version of the film, so that
// burning it again does not run the job again, and the burn draws without
// them. A failure that says nothing about the film (ffmpeg killed by a signal)
// is not remembered, and fails the burn (see
// TestAssBurnFailsWhenTheFontsJobFailedTransiently): the fonts are tried again
// at the next request.
func TestFontsThatCanNeverBeReadAreNotTriedAgain(t *testing.T) {
	for name, c := range map[string]struct {
		script     string
		remembered bool
		fonts      int // How many fonts are kept.
	}{
		"an error exit":      {"echo 'Invalid data found when processing input' >&2; exit 1", true, 0},
		"killed by a signal": {"kill -9 $$", false, 0},
		// ffmpeg warns about plenty of files it reads fine.
		"a warning and exit 0": {"echo 'Warning: unusual attachment' >&2", true, 1},
	} {
		t.Run(name, func(t *testing.T) {
			real := testenv.FFmpeg(t)
			shimDir := t.TempDir()
			shim := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *dump_attachment*) %s;; esac\nexec %q \"$@\"\n", c.script, real)
			if err := os.WriteFile(filepath.Join(shimDir, "ffmpeg"), []byte(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			r := newRig(t, nil)
			r.useFilmWithFont(t, "ass", strings.ReplaceAll(burnTrack, "0:00:06.00,0:00:10.00,Top", "0:00:20.00,0:00:40.00,Bottom"), true)
			source, _, err := r.items.Source(context.Background(), r.spec.Item, "")
			if err != nil {
				t.Fatal(err)
			}
			assets, err := store.Open(source.AssetStoreURI)
			if err != nil {
				t.Fatal(err)
			}
			r.worker(t, nil)
			if c.remembered {
				for second, half := range r.burnWithText(t)[:3] {
					if half[1] < 5 || half[0] > 3 {
						t.Errorf("second %d: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
					}
				}
			} else {
				index := 2
				r.spec.Subtitle = &index
				r.hash = r.spec.Hash()
				if _, err := r.fetchAt(t, r.jobs(t), psid(0), -1, 6); err == nil {
					t.Fatal("a burn without fonts that a signal cut short was served")
				}
			}
			fonts, found, err := subtitles.ReadFonts(context.Background(), assets, source)
			if err != nil || found != c.remembered || len(fonts) != c.fonts {
				t.Fatalf("fonts remembered: %v (%v), want %v; %d kept, want %d", found, err, c.remembered, len(fonts), c.fonts)
			}
			// Long after the job's failure is forgotten, asking again queues a job
			// only if the film was not remembered.
			if _, err := r.pool.Exec(context.Background(), "UPDATE subtitle_jobs SET ended_at = now() - interval '1 hour'"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, err = FetchFonts(ctx, r.pool, &Hub{Pool: r.pool, Log: r.log}, assets, r.spec.Item, source)
			if remembered := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'fonts' AND ended_at < now() - interval '30 minutes'") == 1; remembered != c.remembered {
				t.Errorf("the job stayed as it was: %v, want %v (%v)", remembered, c.remembered, err)
			}
		})
	}
}

// Segments are kept under a hash that doesn't name the fonts, so a burn drawn
// without fonts that were only out of reach for a while would be served to
// every viewer for a day. Here the fonts job is done but a font it lists is
// missing from the store: the burn runs the fonts job again, waits for it and
// is drawn with the restored font on the first request, because a viewer who
// is burning in never asks for the font itself.
func TestAssBurnRestoresALostFont(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.useFilmWithFont(t, "ass", strings.ReplaceAll(burnTrack, "0:00:06.00,0:00:10.00,Top", "0:00:20.00,0:00:40.00,Bottom"), true)
	source, _, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	var index int
	for i := range subtitles.Fonts(source) {
		index = i
	}
	manifest := fmt.Sprintf(`{"%d": "%s"}`, index, strings.Repeat("0", 64))
	if err := assets.Put(context.Background(), subtitles.FontsKey(source.ID, source.ETag), strings.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	two := 2
	r.spec.Subtitle = &two
	r.hash = r.spec.Hash()
	if _, err := r.fetchAt(t, r.jobs(t), psid(0), -1, 6); err != nil {
		t.Fatalf("the burn with a lost font: %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); n != 0 {
		t.Errorf("%d jobs failed", n)
	}
	ctx := context.Background()
	fonts, found, err := subtitles.ReadFonts(ctx, assets, source)
	if err != nil || !found || !subtitles.Exists(ctx, assets, subtitles.FontKey(fonts[index])) {
		t.Errorf("the lost font was not restored: %v %v", found, err)
	}
}

// A font cut short in the store is not a lost one to the store, but libass
// would draw with whatever part of it parses, and the burn is kept for every
// viewer. The burn checks each font against its hash, takes a bad one out, has
// the fonts job put a whole one back and burns with that. fontconfig is given
// no fonts, so text in the picture means the attached font was whole.
func TestAssBurnRestoresACutFont(t *testing.T) {
	testenv.FFmpeg(t)
	conf := filepath.Join(t.TempDir(), "fonts.conf")
	if err := os.WriteFile(conf, []byte(`<?xml version="1.0"?><!DOCTYPE fontconfig SYSTEM "fonts.dtd"><fontconfig></fontconfig>`), 0o644); err != nil {
		t.Fatal(err)
	}
	family := strings.TrimSpace(run(t, "fc-match", "-f", "%{family[0]}", "serif"))
	family, _, _ = strings.Cut(family, ",")
	r := newRig(t, nil)
	r.useFilmWithFont(t, "ass", strings.ReplaceAll(strings.ReplaceAll(burnTrack, "Arial", family), "0:00:06.00,0:00:10.00,Top", "0:00:20.00,0:00:40.00,Bottom"), true)
	t.Setenv("FONTCONFIG_FILE", conf) // After the film is made, which needs the system's fonts.
	r.worker(t, nil)
	ctx := context.Background()
	source, _, err := r.items.Source(ctx, r.spec.Item, "")
	if err != nil {
		t.Fatal(err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := FetchFonts(ctx, r.pool, &Hub{Pool: r.pool, Log: r.log}, assets, r.spec.Item, source)
	if err != nil || len(hashes) != 1 {
		t.Fatalf("fonts: %v %v", hashes, err)
	}
	var sum string
	for _, sum = range hashes {
	}
	key := subtitles.FontKey(sum)
	body, err := assets.Open(ctx, key, 0)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := io.ReadAll(body)
	body.Close()
	if err != nil || len(whole) < 2 {
		t.Fatalf("font: %d bytes, %v", len(whole), err)
	}
	if err := assets.Put(ctx, key, bytes.NewReader(whole[:len(whole)/2])); err != nil {
		t.Fatal(err)
	}
	for second, half := range r.burnWithText(t)[:3] {
		if half[1] < 20 || half[0] > 3 {
			t.Errorf("second %d: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
	}
	body, err = assets.Open(ctx, key, 0)
	if err != nil {
		t.Fatalf("the font was not put back: %v", err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || !bytes.Equal(got, whole) {
		t.Errorf("the font in the store is %d bytes, want %d (%v)", len(got), len(whole), err)
	}
}
