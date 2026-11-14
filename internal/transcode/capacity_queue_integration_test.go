//go:build integration

package transcode

import (
	"context"
	"fmt"
	"testing"

	"jellyscale/internal/playback"
)

// A queued job that no worker has claimed yet takes the cores of one worker,
// not of every worker: with two idle workers one 4K tone mapped job queued
// leaves the other for a second, and two queued leave none for a third.
func TestAQueuedJobTakesOneWorkersCoresNotEveryWorkers(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", 4, playback.OutputVersion)
	r.addNode(t, "b", 4, playback.OutputVersion)
	r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 0)
	r.wantOffered(t, r.costSpec(t, "hdr4k"))
	r.addJob(t, r.costSpec(t, "hdr4k"), "queued", "", false, 0)
	r.wantRefused(t, r.costSpec(t, "hdr4k"), []string{"cores", "3.2"})
}

// What ensure queues carries its cost, so that a reader that sums costs reads
// no spec; a row beside it that has none is costed from its spec.
func TestAJobQueuedByEnsureCarriesItsCost(t *testing.T) {
	r := newRig(t, nil)
	spec := r.costSpec(t, "hdr4k")
	id, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), psid(0), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustScalar(t, r, fmt.Sprintf("SELECT cost::float8 FROM transcode_jobs WHERE id = %d", id)); got != "3.2" {
		t.Errorf("the job's cost is %q, want 3.2", got)
	}
}
