//go:build integration

package transcode

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A request that waits for a segment is not charged the time Postgres was away:
// two outages each longer than WaitTimeout, with the request inside Segment
// throughout, and it is still answered with the segment stored afterwards.
func TestSegmentWaitIsNotChargedForPostgresBeingAway(t *testing.T) {
	s := newStack(t, 0) // No worker: the job stays queued until the test stores the segment.
	j := s.jobs
	j.WaitTimeout = 3 * time.Second
	j.QueueTimeout = 5 * time.Minute
	r := s.r
	type answer struct {
		body []byte
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		body, err := s.fetch(context.Background(), j, r.spec, psid(0), 0)
		done <- answer{body, err}
	}()
	waitFor(t, "a queued job", 10*time.Second, func() bool { return r.count(t, "SELECT count(*) FROM transcode_jobs") == 1 })

	for range 2 {
		s.replica.Away()
		s.replica.Sever()
		time.Sleep(4 * time.Second)
		s.replica.Back()
		time.Sleep(time.Second)
	}
	want := []byte("segment")
	if err := r.segments.Put(context.Background(), SegmentKey(r.hash, 0), bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.err != nil || !bytes.Equal(a.body, want) {
			t.Fatalf("got %q, %v after two outages longer than WaitTimeout, want the segment", a.body, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the request was not answered")
	}
}
