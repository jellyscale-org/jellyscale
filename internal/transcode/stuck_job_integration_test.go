//go:build integration

package transcode

import (
	"testing"

	"jellyscale/internal/playback"
)

// A viewer's job that waits queued gets the cores of the helpers beside it, though a worker that was killed
// is still counted and looks like room (D-046 dropped the room kept for it): after a second the helpers
// that hold the live worker's cores stop, once for a failover's job, and a queued helper does the same for none.
func TestAQueuedViewersJobNoWorkerTookStopsTheHelpersThatHoldTheCores(t *testing.T) {
	r := newRig(t, nil)
	cost := r.spec.Cost()
	r.addNode(t, "a-killed", cost, playback.OutputVersion)
	r.addNode(t, "b", cost, playback.OutputVersion)
	helper := r.addJob(t, r.spec, "running", "b", true, 0)
	j := &Jobs{Pool: r.pool}

	queuedHelper := r.addJob(t, r.spec, "queued", "", true, 4)
	if err := j.makeRoom(t.Context(), queuedHelper); err != nil {
		t.Fatal(err)
	}
	if got := r.state(t, helper); got != "running" {
		t.Fatalf("a queued helper stopped a running one: %s", got)
	}

	viewers := r.addJob(t, r.spec, "queued", "", false, 4)
	if err := j.makeRoom(t.Context(), viewers); err != nil {
		t.Fatal(err)
	}
	if got := r.state(t, helper); got != "cancelled" {
		t.Fatalf("the helper holding the only live worker's cores was left running beside a viewer's job no worker took: %s", got)
	}
	if got := r.state(t, viewers); got != "queued" {
		t.Fatalf("the viewer's job was changed: %s", got)
	}
}
