//go:build integration

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// The extraction of a film's image tracks (JS-089) is asked for by the play and
// waited on by no request: PlaybackInfo, the master playlist and the main
// playlist (the handlers that ask, as for the fonts) are answered while the
// film's job is held, running and unfinished, never after it. A segment is
// asked for as every play does and is not part of this check: no transcode
// worker runs in this fixture, and the handler of a segment asks for nothing.
func TestNoRequestOfAPlayWaitsForTheFilmsImageTrackJob(t *testing.T) {
	f := subtitleEnv(t, "")
	f.enableTranscoding(t)
	// The job's ffmpeg waits for a file that only this test writes, so the job is
	// still running when every request has been answered, whatever the machine.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *matroska*) while [ ! -e %q ]; do sleep 0.1; done;; esac\nexec %q \"$@\"\n", gate, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	open := func() { os.WriteFile(gate, nil, 0o644) } //nolint:errcheck // Opened twice is fine.
	t.Cleanup(open)
	mustExec(t, f.pool, "TRUNCATE subtitle_jobs")

	// A request that waits for the job never ends while it is held. The hang bound
	// opens the gate so that it does, and the test then fails.
	var waited atomic.Bool
	bound := time.AfterFunc(60*time.Second, func() { waited.Store(true); open() })
	defer bound.Stop()
	f.plays(t, f.imageInfo(t, profileWith(t, false), map[string]any{"SubtitleStreamIndex": 2}))
	bound.Stop()
	if waited.Load() {
		t.Fatal("a request of the play waited for the film's image track job")
	}
	// The worker takes the job after the requests (a queued job is as unfinished); it
	// must be held at its read, not done.
	waitUntil(t, "the worker to hold the image track job at its read", func() bool {
		return scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE kind = 'images' AND state = 'running'") == 1
	})
	if done := scalar[int](t, f.pool, "SELECT count(*)::int FROM subtitle_jobs WHERE state IN ('done', 'failed')"); done != 0 {
		t.Errorf("%d subtitle jobs ended while the gate was shut", done)
	}
}
