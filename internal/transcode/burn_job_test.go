package transcode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A worker draws the track its job names, extracting it from the source on the
// way, and a track it can't read fails the job with the reason and leaves no
// ffmpeg behind.
func TestWorkerBurnsTheTrackOrFailsTheJob(t *testing.T) {
	needFFmpeg(t)
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
	frames := brightByHalf(t, joined)
	if len(frames) < 3 {
		t.Fatalf("%d frames", len(frames))
	}
	for second, half := range frames[:3] {
		if half[1] < 20 || half[0] > 3 {
			t.Errorf("second %d of the segment: bright pixels top %d, bottom %d; want text at the bottom", second, half[0], half[1])
		}
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
	needFFmpeg(t)
	r := newRig(t, nil)
	r.useBurnFilm(t)
	real, _ := exec.LookPath("ffmpeg")
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
	needFFmpeg(t)
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
	needFFmpeg(t)
	r := newRig(t, nil)
	r.useBurnFilm(t)
	real, _ := exec.LookPath("ffmpeg")
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
