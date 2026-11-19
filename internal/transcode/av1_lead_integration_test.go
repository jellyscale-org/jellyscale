//go:build integration

package transcode

import (
	"testing"
	"time"
)

// An AV1 fallback is one ffmpeg for each segment, so the lead (invariant on read-ahead) holds between two runs as well
// as in one: the job stops making segments at the lead beyond the furthest the viewers asked for, starts no ffmpeg
// for the next while it holds, and goes on when a viewer asks for more.
func TestAnAV1FallbackJobHoldsAtItsLeadBetweenItsRuns(t *testing.T) {
	name := av1Film(t, "lead.mkv", "25")
	_, source := av1Source(t, name)
	layout := source.Layout(true)
	r := newRig(t, nil)
	r.av1Fallback(t, name)
	r.worker(t, func(w *Worker) { w.Lead = 12 * time.Second })
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	stopWatching := r.keepWatching(t, j, psid(0))
	defer stopWatching()
	first := limitFor(layout, 0, 12*time.Second)
	waitFor(t, "the job to hold at its lead", 60*time.Second, func() bool { return r.next(t) == first })
	// Longer than the job takes to make a segment of this film.
	time.Sleep(3 * time.Second)
	if got := r.next(t); got != first || r.stored(first) || len(r.ffmpegProcesses()) != 0 {
		t.Fatalf("the job did not hold at segment %d: next %d, segment %d stored %v, %d ffmpeg running", first, got, first, r.stored(first), len(r.ffmpegProcesses()))
	}
	// A request moves the bound: the job makes the segment asked for and what the lead allows after it.
	if _, err := r.fetch(t, j, psid(0), first); err != nil {
		t.Fatal(err)
	}
	second := limitFor(layout, first, 12*time.Second)
	waitFor(t, "the job to hold at its new lead", 60*time.Second, func() bool { return r.next(t) == second })
	if second < layout.Count() && r.stored(second) {
		t.Errorf("segment %d is past the lead and was stored", second)
	}
	if n := r.count(t, "SELECT count(*) FROM transcode_jobs"); n != 1 {
		t.Errorf("%d jobs, want the one that held", n)
	}
}
