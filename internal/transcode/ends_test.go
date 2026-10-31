package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/library"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

// useFile points the rig's item at a file in the film directory, with the
// duration and streams the scanner would store for it.
func (r *rig) useFile(t *testing.T, name string) playback.Source {
	t.Helper()
	probe, err := library.ProbeInput(context.Background(), filepath.Join(film(t), name))
	if err != nil {
		t.Fatal(err)
	}
	streams, _ := json.Marshal(probe.Streams)
	if _, err := r.pool.Exec(context.Background(), "UPDATE items SET run_time_ticks = $1", probe.RunTimeTicks); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = $1, streams = $2", name, streams); err != nil {
		t.Fatal(err)
	}
	parsed, err := playback.ParseStreams(streams)
	if err != nil {
		t.Fatal(err)
	}
	return playback.Source{RunTimeTicks: probe.RunTimeTicks, Streams: parsed}
}

// ffmpeg's hls muxer cuts segments only while video lasts, so a file whose
// audio outruns its video must get a playlist that stops with the video.
func TestPlaylistStopsWhereTheVideoDoes(t *testing.T) {
	needFFmpeg(t)
	r := newRig(t, nil)
	run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=28",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "48", "-c:a", "aac", filepath.Join(film(t), "long-audio.mkv"))
	source := r.useFile(t, "long-audio.mkv")
	count := playback.Segments(source.Ticks())
	if count != 5 {
		t.Fatalf("playlist has %d segments for 20 s of video, want 5", count)
	}
	r.worker(t, nil)
	j := r.jobs(t)
	for _, n := range []int{0, count - 1} { // From the start, and a restart at the last one.
		if _, err := r.fetch(t, j, psid(n), n); err != nil {
			t.Fatalf("segment %d of %d: %v", n, count, err)
		}
	}
	waitFor(t, "the jobs to end", 30*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
	})
	if failed := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); failed != 0 {
		t.Fatalf("%d jobs failed", failed)
	}
	if body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, count), 0); err == nil {
		body.Close()
		t.Fatalf("ffmpeg made segment %d, so the playlist is short", count)
	}
}

// A job whose input ends early (a read that failed midway looks like the end
// of the file to ffmpeg) must not store the segment it cut short, and must
// fail, so viewers never get a bad segment.
func TestInputThatEndsEarlyFailsTheJob(t *testing.T) {
	needFFmpeg(t)
	r := newRig(t, nil)
	whole, err := os.ReadFile(filepath.Join(film(t), "film.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(film(t), "cut.mkv"), whole[:len(whole)*2/5], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(context.Background(), "UPDATE media_sources SET store_key = 'cut.mkv'"); err != nil {
		t.Fatal(err)
	}
	r.worker(t, nil)
	if _, err := r.fetch(t, r.jobs(t), psid(0), 0); err != nil {
		t.Fatalf("segment 0 is whole and should be served: %v", err)
	}
	waitFor(t, "the job to fail", 30*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'") == 1 })
	next := r.count(t, "SELECT next_segment FROM transcode_jobs")
	if body, err := r.segments.Open(context.Background(), SegmentKey(r.hash, next), 0); err == nil {
		body.Close()
		t.Fatalf("segment %d, cut short by the end of the input, was stored", next)
	}
	if reason := mustScalar(t, r, "SELECT error FROM transcode_jobs"); !strings.Contains(reason, "input ended early") {
		t.Fatalf("the reason is %q", reason)
	}
}

// A replica that can't read what the worker wrote gets told why.
func TestUnreadableStoreIsNamed(t *testing.T) {
	needFFmpeg(t)
	r := newRig(t, nil)
	r.worker(t, nil) // Writes to the rig's store.
	j := r.jobs(t)
	j.Segments = store.NewPosix(t.TempDir()) // This replica looks somewhere else.
	_, err := r.fetch(t, j, psid(0), 0)
	var failed FailedError
	if !errors.As(err, &failed) || !strings.Contains(failed.Reason, "transcodeStore must be the same") {
		t.Fatalf("got %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'failed'"); n != 1 {
		t.Fatalf("%d failed jobs, want the unreadable one marked so PlaybackInfo stops offering it", n)
	}
}
