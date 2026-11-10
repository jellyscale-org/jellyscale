//go:build integration

package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-028: one read of a film extracts every text track in it. The film below
// has three, streams 2 (English SubRip), 3 (Spanish SubRip) and 4 (ASS).

const (
	englishLine = "English line one"
	spanishLine = "Linea en espanol uno"
	assLine     = "Styled ASS line"
	assStyle    = "Style: Loud,Arial,48,&H0000FF00"
)

// textTracks makes the film (before any ffmpeg shim is in the way), points the
// rig at it with the streams the scanner stores for it, and returns its source
// and its asset store.
func (r *rig) textTracks(t *testing.T) (items.Source, store.Store) {
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
	es := write("es.srt", "1\n00:00:02,000 --> 00:00:04,000\n"+spanishLine+"\n")
	ass := write("loud.ass", "[Script Info]\nScriptType: v4.00+\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\n"+
		assStyle+",&H000000FF,&H00000000,&H00000000,1,0,0,0,100,100,0,0,1,2,0,8,10,10,10,1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:02.00,0:00:04.00,Loud,,0,0,0,,"+assLine+"\n")
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=30",
		"-i", en, "-i", es, "-i", ass,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3", "-map", "4",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "48", "-c:a", "aac", "-c:s:0", "srt", "-c:s:1", "srt", "-c:s:2", "ass",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:1", "language=spa",
		filepath.Join(film(t), "tracks.mkv"))
	r.useFile(t, "tracks.mkv")
	info, err := os.Stat(filepath.Join(film(t), "tracks.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET size = $1", info.Size()); err != nil {
		t.Fatal(err)
	}
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

// withUnextractableTrack adds to the stored streams a text track, stream 9, that
// the file does not have, which is a track ffmpeg cannot extract: it answers
// "matches no streams" with an error status, as the file's own ErrBadFile.
// (A track broken inside the file cannot be told from a broken film, since
// ffmpeg reports a bad packet on stderr for the run and not for the track.)
// It returns the source with the new streams.
func (r *rig) withUnextractableTrack(t *testing.T) items.Source {
	t.Helper()
	var streams []map[string]any
	var raw []byte
	if err := r.pool.QueryRow(context.Background(), "SELECT streams FROM media_sources").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatal(err)
	}
	for _, stream := range streams {
		if stream["Type"] == "Subtitle" && stream["Index"] == float64(2) {
			phantom := map[string]any{}
			for k, v := range stream {
				phantom[k] = v
			}
			phantom["Index"] = 9
			phantom["DisplayTitle"] = "Phantom"
			streams = append(streams, phantom)
			break
		}
	}
	if len(streams) == 0 {
		t.Fatal("the film has no SubRip stream 2 to copy")
	}
	changed, _ := json.Marshal(streams)
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET streams = $1", changed); err != nil {
		t.Fatal(err)
	}
	source, _, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// shimFFmpeg puts an ffmpeg in front of the real one that notes every run, then
// runs extra (shell). The only ffmpeg a worker runs in these tests is the
// extraction, so the count is how many times the film was opened to extract.
// It does not know how the extraction names its outputs.
func shimFFmpeg(t *testing.T, extra string) (runs func() int, runFile string) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	runFile = filepath.Join(dir, "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >>%q\n%s\nexec %q \"$@\"\n", runFile, extra, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(runFile)
		return strings.Count(string(data), "run")
	}, runFile
}

func (r *rig) pick(t *testing.T, j *Jobs, assets store.Store, source items.Source, index int) (subtitles.Track, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return FetchSubtitle(ctx, r.pool, j.Hub, assets, r.spec.Item, source, index)
}

// wantTrack checks that the track holds the text of stream index and not that
// of another, which is what a wrong mapping of outputs to tracks would give.
func wantTrack(t *testing.T, track subtitles.Track, index int) {
	t.Helper()
	text := map[int]string{2: englishLine, 3: spanishLine, 4: assLine}
	for i, line := range text {
		if has := strings.Contains(track.VTT, line); has != (i == index) {
			t.Errorf("track %d: its WebVTT has %q: %v\n%s", index, line, has, track.VTT)
		}
	}
	if index == 4 && !strings.Contains(track.ASS, assStyle) {
		t.Errorf("track 4 lost its ASS style:\n%s", track.ASS)
	}
}

func (r *rig) subtitleJobRows(t *testing.T) int {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind NOT IN ('fonts', 'chapters')")
}

// The first pick of one text track runs one ffmpeg for the film, which writes
// every text track; a later pick of another runs none and starts no job, and is
// served with no worker running at all.
func TestFirstPickOfATextTrackExtractsEveryTextTrackInOneFFmpeg(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	runs, _ := shimFFmpeg(t, "")
	stop := r.worker(t, nil)
	j := r.jobs(t)

	first, err := r.pick(t, j, assets, source, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantTrack(t, first, 3)
	if n := runs(); n != 1 {
		t.Fatalf("%d ffmpeg runs for the first pick, want 1", n)
	}
	rows := r.subtitleJobRows(t)
	stop() // No worker from here: only the store can answer.

	for _, index := range []int{2, 4, 3} {
		track, err := r.pick(t, j, assets, source, index)
		if err != nil {
			t.Fatalf("track %d with the film's first pick done and no worker: %v", index, err)
		}
		wantTrack(t, track, index)
	}
	if n := runs(); n != 1 {
		t.Errorf("%d ffmpeg runs after picking the other tracks, want still 1", n)
	}
	if got := r.subtitleJobRows(t); got != rows {
		t.Errorf("picking the other tracks started jobs: %d rows, was %d", got, rows)
	}
}

// On S3 the film is read once for all its text tracks, as the bytes the store
// gave the worker show, and picking the others reads nothing more. Not once per
// track, which is three reads for this film.
func TestTextTracksFromS3ReadTheFilmOnce(t *testing.T) {
	testenv.FFmpeg(t)
	media := s3Segments(t)
	for name := range media {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			source, _ := r.textTracks(t)
			whole, err := os.ReadFile(filepath.Join(film(t), "tracks.mkv"))
			if err != nil {
				t.Fatal(err)
			}
			if err := media[name].Put(context.Background(), "tracks.mkv", bytes.NewReader(whole)); err != nil {
				t.Fatal(err)
			}
			counted := &countingStore{Store: media[name]}
			runs, _ := shimFFmpeg(t, "")
			r.worker(t, func(w *Worker) {
				w.Media = func(string) (store.Store, error) { return counted, nil }
			})
			j := r.jobs(t)

			if _, err := r.pick(t, j, media[name], source, 2); err != nil {
				t.Fatal(err)
			}
			size := int64(len(whole))
			// A little over the film's size is the container's index and the
			// probe of its head; one read per track would be about three times.
			got := counted.read.Load()
			if got < size*9/10 || got > size*5/4 {
				t.Errorf("%d bytes read of a %d byte film (%.2f times) for the first pick, want one read of it", got, size, float64(got)/float64(size))
			}
			for _, index := range []int{3, 4} {
				track, err := r.pick(t, j, media[name], source, index)
				if err != nil {
					t.Fatal(err)
				}
				wantTrack(t, track, index)
			}
			if after := counted.read.Load(); after != got || runs() != 1 {
				t.Errorf("picking the other tracks read %d more bytes with %d ffmpeg runs in all, want none more and 1", after-got, runs())
			}
		})
	}
}

// Two viewers on separate api processes, each picking a different track of the
// same film at the same moment, with two worker processes to take the job: one
// ffmpeg opens the film. The shim sleeps a second in the run so that the second
// request arrives while the first is extracting; the sleep adds time to the
// extraction only, and a lease or heartbeat is at production length.
func TestTwoApisPickingDifferentTracksCauseOneRead(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	r.textTracks(t)
	runs, _ := shimFFmpeg(t, "sleep 1 >/dev/null 2>&1")
	r.workerProcess(t, "w1", t.TempDir(), "JELLYSCALE_TEST_SUBTITLES=1")
	r.workerProcess(t, "w2", t.TempDir(), "JELLYSCALE_TEST_SUBTITLES=1")

	at := time.Now().Add(2 * time.Second).UnixNano()
	apis := map[int]*exec.Cmd{2: r.apiProcess(t, 2, at), 3: r.apiProcess(t, 3, at)}
	outputs := map[int]*bytes.Buffer{}
	for index, cmd := range apis {
		outputs[index] = &bytes.Buffer{}
		cmd.Stdout = outputs[index]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for index, cmd := range apis {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("api asking for track %d: %v", index, err)
			}
		case <-time.After(60 * time.Second):
			cmd.Process.Kill() //nolint:errcheck
			t.Fatalf("api asking for track %d got no answer", index)
		}
		wantTrack(t, subtitles.Track{VTT: outputs[index].String()}, index)
	}
	if n := runs(); n != 1 {
		t.Fatalf("%d ffmpeg runs for two viewers picking two tracks, want 1", n)
	}
}

// apiProcess is an api replica that asks for one track at a time (unix
// nanoseconds) and prints its WebVTT; TestMain sees JELLYSCALE_TEST_API_TRACK.
func (r *rig) apiProcess(t *testing.T, index int, at int64) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_API_TRACK="+strconv.Itoa(index), "JELLYSCALE_TEST_API_AT="+strconv.FormatInt(at, 10),
		"JELLYSCALE_TEST_API_ITEM="+r.spec.Item)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// apiProcessBody is the body of such a process. Like an api replica it has a
// pool and a hub of its own and runs no ffmpeg.
func apiProcessBody(trackEnv string) {
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "api: %s: %v\n", what, err)
		os.Exit(1)
	}
	index, err := strconv.Atoi(trackEnv)
	if err != nil {
		fail("track", err)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, os.Getenv("JELLYSCALE_TEST_DATABASE_URL"))
	if err != nil {
		fail("database", err)
	}
	hub := &Hub{Pool: pool, Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	go hub.Run(ctx)
	item := os.Getenv("JELLYSCALE_TEST_API_ITEM")
	source, found, err := (&items.Repo{Pool: pool, ServerID: "srv"}).Source(ctx, item, "")
	if err != nil || !found {
		fail("source", fmt.Errorf("%v %v", found, err))
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		fail("assets", err)
	}
	if at, err := strconv.ParseInt(os.Getenv("JELLYSCALE_TEST_API_AT"), 10, 64); err == nil {
		time.Sleep(time.Until(time.Unix(0, at)))
	}
	track, err := FetchSubtitle(ctx, pool, hub, assets, item, source, index)
	if err != nil {
		fail("FetchSubtitle", err)
	}
	fmt.Print(track.VTT)
	os.Exit(0)
}

// A track ffmpeg can't extract is hidden on its own: asking for it answers
// ErrUnreadable and the film's other text tracks are still served, in one pass
// (once the bad one is out of it, a pick of any of them leaves the others in the
// store, which no worker is needed for). Either track may be asked for first.
func TestATrackFFmpegCannotExtractIsHiddenAndTheOthersAreServed(t *testing.T) {
	for _, badFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("bad track first %v", badFirst), func(t *testing.T) {
			testenv.FFmpeg(t)
			r := newRig(t, nil)
			_, assets := r.textTracks(t)
			source := r.withUnextractableTrack(t)
			runs, _ := shimFFmpeg(t, "")
			stop := r.worker(t, nil)
			j := r.jobs(t)

			askBad := func() {
				t.Helper()
				if _, err := r.pick(t, j, assets, source, 9); !errors.Is(err, ErrUnreadable) {
					t.Fatalf("the track ffmpeg cannot extract: %v, want ErrUnreadable", err)
				}
			}
			if badFirst {
				askBad()
			}
			track, err := r.pick(t, j, assets, source, 2)
			if err != nil {
				t.Fatalf("a good track beside the bad one: %v", err)
			}
			wantTrack(t, track, 2)
			if !badFirst {
				askBad()
			}
			after := runs()
			askBad() // Remembered: a bad track isn't read again for every request.
			if runs() != after {
				t.Errorf("asking for the bad track again ran ffmpeg %d more times", runs()-after)
			}
			stop()
			for _, index := range []int{3, 4} {
				track, err := r.pick(t, j, assets, source, index)
				if err != nil {
					t.Fatalf("track %d, which the bad track must not have cost: %v", index, err)
				}
				wantTrack(t, track, index)
			}
		})
	}
}

// A worker that dies mid-extraction leaves the job to another worker, which
// takes it when the lease lapses (10 seconds at production length) and
// extracts every track; the tracks are then served with no worker. Real
// processes, the SIGKILL of the one holding the job.
func TestAWorkerKilledMidExtractionLeavesTheJobToAnother(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	begun := filepath.Join(t.TempDir(), "begun")
	// The first ffmpeg to start hangs, as an extraction on a dying host does.
	hang := fmt.Sprintf("if mkdir %q.lock 2>/dev/null; then touch %q; exec sleep 600; fi", begun, begun)
	runs, _ := shimFFmpeg(t, hang)
	workers := map[string]*exec.Cmd{}
	for _, id := range []string{"w1", "w2"} {
		workers[id] = r.workerProcess(t, id, t.TempDir(), "JELLYSCALE_TEST_SUBTITLES=1")
	}
	j := r.jobs(t)

	type result struct {
		track subtitles.Track
		err   error
	}
	answered := make(chan result, 1)
	go func() {
		track, err := r.pick(t, j, assets, source, 3)
		answered <- result{track, err}
	}()
	waitFor(t, "the first extraction to start", 30*time.Second, func() bool { _, err := os.Stat(begun); return err == nil })
	var owner string
	if err := r.pool.QueryRow(context.Background(), "SELECT worker FROM subtitle_jobs WHERE state = 'running' AND kind NOT IN ('fonts', 'chapters')").Scan(&owner); err != nil {
		t.Fatalf("no subtitle job is running: %v", err)
	}
	victim, ok := workers[owner]
	if !ok {
		t.Fatalf("the job is held by %q, not one of the workers", owner)
	}
	victim.Process.Kill() //nolint:errcheck
	killed := time.Now()
	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("after the worker holding the job was killed: %v", got.err)
		}
		wantTrack(t, got.track, 3)
		if took := time.Since(killed); took > 40*time.Second {
			t.Errorf("the other worker took %s to finish", took)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the job was not taken over by the other worker")
	}
	if n := runs(); n != 2 {
		t.Errorf("%d ffmpeg runs (the killed one and the one that finished), want 2", n)
	}
	for id, cmd := range workers {
		cmd.Process.Kill() //nolint:errcheck
		_ = id
	}
	for _, index := range []int{2, 4} {
		track, err := r.pick(t, j, assets, source, index)
		if err != nil {
			t.Fatalf("track %d after the takeover, with no worker: %v", index, err)
		}
		wantTrack(t, track, index)
	}
}
