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
// reason and the capacity's source as its only labels (D-062: capacity="declared" or "assumed", and a worker that
// declared nothing says in the line that its capacity is assumed and names transcodeCores). A job at 1.2 times real time or more logs and counts nothing. Names the build adds: the log
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
		source string // "declared" unless the worker declares nothing.
		setup  func(r *rig, w *Worker)
	}{
		{"a lone worker", "lone_worker", "declared", func(r *rig, w *Worker) {}},
		{"transcodeHelpers off", "helpers_off", "declared", func(r *rig, w *Worker) { w.NoHelpers = true }},
		{"no worker with the helper's cost free", "no_room", "declared", func(r *rig, w *Worker) {
			r.addNode(t, "other", 0.1, playback.OutputVersion) // A live worker that has not the 0.8 a helper costs.
		}},
		// Nothing declared, neither cores nor slots (the rig gives every worker Slots: 2, which is a declaration): the processors
		// at the reference speed are its capacity, and the line says it is assumed.
		{"a lone worker that declared nothing", "lone_worker", "assumed", func(r *rig, w *Worker) { w.Cores, w.Slots = 0, 0 }},
		{"transcodeHelpers off on a worker that declared nothing", "helpers_off", "assumed", func(r *rig, w *Worker) { w.Cores, w.Slots, w.NoHelpers = 0, 0, true }},
		{"a job at 1.2 times real time or more", "", "declared", func(r *rig, w *Worker) { w.HelpBelow = 0.05 }},
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
					if tc.reason != "" && !hasLabels(line, "reason", tc.reason, "capacity", tc.source) {
						t.Errorf("a series other than reason=%q capacity=%q (the only labels): %s", tc.reason, tc.source, line)
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
			if got := value(text, `jellyscale_transcode_unhelped_jobs_total{capacity="`+tc.source+`",reason="`+tc.reason+`"}`); got != 1 {
				t.Errorf("the counter for %s, %s, is %v, want 1", tc.reason, tc.source, got)
			}
			// What an operator reads: a worker that declared nothing says its capacity is assumed and which setting to use;
			// one that declared it does not say it is assumed.
			assumed := strings.Contains(lines[0], "assumed") && strings.Contains(lines[0], "transcodeCores")
			if tc.source == "assumed" && !assumed {
				t.Errorf("the line of a worker that declared nothing does not say its capacity is assumed and name transcodeCores: %s", lines[0])
			}
			if tc.source == "declared" && strings.Contains(lines[0], "assumed") {
				t.Errorf("the line of a worker that declared its capacity says it is assumed: %s", lines[0])
			}
		})
	}
}

// hasLabels says whether the exposition line has exactly the labels given as name, value pairs, in any order.
func hasLabels(line string, pairs ...string) bool {
	open, end := strings.Index(line, "{"), strings.Index(line, "}")
	if open < 0 || end < open {
		return false
	}
	have := strings.Split(line[open+1:end], ",")
	if len(have) != len(pairs)/2 {
		return false
	}
	for i := 0; i < len(pairs); i += 2 {
		found := false
		for _, h := range have {
			found = found || h == pairs[i]+`="`+pairs[i+1]+`"`
		}
		if !found {
			return false
		}
	}
	return true
}
