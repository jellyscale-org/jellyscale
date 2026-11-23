//go:build integration

package transcode

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-161 (BL-005): a running re-encode under 1.2 times real time that gets no helper says why once per job and reason,
// never once per heartbeat (JS-162 saw 141 to 304 refusals per play), logs it and counts it on /metrics once, with the
// reason as the only label. A job at 1.2 times real time or more logs and counts nothing. Names the build adds: the log
// message "slow job gets no helper" with the attribute reason, and jellyscale_transcode_unhelped_jobs_total{reason}
// with the reasons lone_worker, helpers_off and no_room (a job that is not cut into chunks, an HEVC or AV1 fallback,
// is the fourth, not_chunked: the process-level check of docs/transcoding.md's measurements covers it).
//
// The rig's heartbeat is 200 ms, which gives a job many more beats than production's 3 seconds in the same time: a
// count of one over so many beats shows a rule that is once per job and not once per beat, and a production heartbeat
// could only give fewer.

type jobLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *jobLogs) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *jobLogs) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestASlowJobThatGetsNoHelperSaysWhyOncePerJobAndReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string // "" says nothing is logged or counted.
		setup  func(r *rig, w *Worker)
	}{
		{"a lone worker", "lone_worker", func(r *rig, w *Worker) {}},
		{"transcodeHelpers off", "helpers_off", func(r *rig, w *Worker) { w.NoHelpers = true }},
		{"no worker with the helper's cost free", "no_room", func(r *rig, w *Worker) {
			r.addNode(t, "other", 0.1, playback.OutputVersion) // A live worker that has not the 0.8 a helper costs.
		}},
		{"a job at 1.2 times real time or more", "", func(r *rig, w *Worker) { w.HelpBelow = 0.05 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slow(t) // Real time: the job is under 1.2x.
			r := newRig(t, nil)
			var logs jobLogs
			var w *Worker
			r.worker(t, func(worker *Worker) {
				w, worker.Cores, worker.Idle = worker, 1, 30*time.Second
				worker.Log = slog.New(slog.NewTextHandler(&logs, nil))
				tc.setup(r, worker)
			})
			j := r.jobs(t)
			if _, err := r.fetch(t, j, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			stop := r.keepWatching(t, j, psid(0))
			defer stop()
			waitFor(t, "the job's speed to show", 40*time.Second, func() bool {
				return value(scrape(t, w), `jellyscale_transcode_job_speed_ratio{job="`+r.jobID(t)+`",kind="reencode"}`) > 0
			})
			time.Sleep(8 * time.Second) // Dozens of heartbeats.
			var lines []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "slow job gets no helper") {
					lines = append(lines, line)
				}
			}
			text := scrape(t, w)
			counted := 0.0
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "jellyscale_transcode_unhelped_jobs_total{") {
					counted += value(line+"\n", strings.Fields(line)[0])
					if tc.reason != "" && !strings.HasPrefix(line, `jellyscale_transcode_unhelped_jobs_total{reason="`+tc.reason+`"}`) {
						t.Errorf("a series other than reason=%q (the only label): %s", tc.reason, line)
					}
				}
			}
			if tc.reason == "" {
				if len(lines) != 0 || counted != 0 {
					t.Errorf("a job that needs no help logged %d lines and counted %v: %v", len(lines), counted, lines)
				}
				return
			}
			if len(lines) != 1 || !strings.Contains(lines[0], tc.reason) {
				t.Errorf("%d log lines, want one naming %s: %v", len(lines), tc.reason, lines)
			}
			if got := value(text, `jellyscale_transcode_unhelped_jobs_total{reason="`+tc.reason+`"}`); got != 1 {
				t.Errorf("the counter for %s is %v, want 1", tc.reason, got)
			}
		})
	}
}
