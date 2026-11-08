//go:build integration

package transcode

import (
	"testing"
	"time"
)

// A helper fills the read-ahead of the viewers it serves, not of one far past
// it: a second viewer at segment 20 has a job of its own, and the chunks
// between the first viewer's lead and it are nobody's to encode.
func TestHelpersDoNotFillTheGapToAFarViewer(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// 24 seconds is 6 segments: segments 0 to 5 may be made after asking for 0.
	r.workers(t, 3, 2, 2, r.ownStore(t), "JELLYSCALE_TEST_LEAD_SECONDS=24")
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(0))
	if _, err := r.fetch(t, j, psid(1), 20); err != nil {
		t.Fatal(err)
	}
	r.keepWatching(t, j, psid(1))
	time.Sleep(25 * time.Second)
	for n := 7; n < 19; n++ { // Past the first viewer's lead, short of the second's own jobs.
		if r.stored(n) {
			t.Fatalf("segment %d is stored with viewers at segments 0 and 20 and a lead of 6 segments: %+v", n, r.jobsOf(t, r.hash))
		}
	}
}
