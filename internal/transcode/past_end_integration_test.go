//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// A job queued or taken over at or past the film's last segment has nothing
// left to make: it ends done, with no ffmpeg, and the worker goes on. Before,
// a copy job of that kind panicked in copyStart (an index past the layout) and
// took the worker and its other jobs down, and a re-encode ran an ffmpeg that
// failed, which refused the film to every viewer for failureMemory (a merge
// queue run of JS-087).
func TestAJobThatStartsPastTheEndEndsDoneWithNoFFmpeg(t *testing.T) {
	r := newRig(t, nil)
	r.worker(t, nil)
	raw, err := json.Marshal(r.spec)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, output_version)
		VALUES ($1, $2, 'viewer', 100000, 100000, $3) RETURNING id`, r.hash, raw, playback.OutputVersion).Scan(&id); err != nil {
		t.Fatal(err)
	}
	Notify(context.Background(), r.pool, jobHint)
	waitFor(t, "the job to end", 30*time.Second, func() bool { s := r.jobState(t, id); return s != "queued" && s != "running" })
	if got := r.jobState(t, id); got != "done" {
		t.Errorf("a job that starts past the film's end ended %s, want done", got)
	}
	if runs := len(r.ffmpegProcesses()); runs != 0 {
		t.Errorf("%d ffmpeg running for a job with nothing to make", runs)
	}
}
