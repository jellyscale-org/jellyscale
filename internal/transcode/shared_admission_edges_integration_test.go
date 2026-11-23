//go:build integration

package transcode

import (
	"fmt"
	"testing"

	"jellyscale/internal/playback"
)

// JS-161 (D-046): edges of shared admission that the checks of shared_admission_integration_test.go leave open. A job that
// is not cut into chunks gets no helper, so it is never admitted as shared; the slowest pair of the task's range (0.80 of
// the 3.2 price, 2.56 cores each) is admitted; and a play is hosted only by a worker that asks for helpers, whichever
// worker looks at the queue first.

// hevcFallback is an HEVC fallback re-encode of a 1080p SDR source, which costs 3.2 cores like a 4K tone mapped one and
// is not cut into chunks.
func (r *rig) hevcFallback(t *testing.T) playback.Spec {
	t.Helper()
	spec := r.costSpec(t, "sdr1080")
	spec.Recode, spec.Width, spec.Height, spec.SourceHeight = "hevc", 1920, 1080, 1080
	if spec.Cost() != 3.2 {
		t.Fatalf("the HEVC fallback costs %v, the table says 3.2", spec.Cost())
	}
	return spec
}

func TestAFallbackThatIsNotCutIntoChunksIsNeverAdmittedAsShared(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
	r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
	if reason, text := r.refusal(t, r.hevcFallback(t), "device"); reason != playback.NoWorkerWithFreeCores {
		t.Errorf("reason %q (%s), want %s: no helper could take part of it", reason, text, playback.NoWorkerWithFreeCores)
	}
}

func TestTheSlowestPairOfTheTasksRangeIsAdmitted(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", 2.56, playback.OutputVersion)
	r.addNode(t, "b", 2.56, playback.OutputVersion)
	if reason, text := r.refusal(t, r.costSpec(t, "hdr4k"), "device"); reason != "" {
		t.Errorf("refused (%s): %s", reason, text)
	}
}

func TestAShareHostedPlayIsClaimedOnlyByAWorkerThatAsksForHelpers(t *testing.T) {
	r := newRig(t, nil)
	r.addHelperlessNode(t, "a", sharedCapacity)
	r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
	spec := r.costSpec(t, "hdr4k")
	r.wantOffered(t, spec)
	for i := range 6 {
		id := r.addJob(t, spec, "queued", "", false, 0)
		if i%2 == 1 {
			r.startsAt(t, id, 12)
		}
		helperless := &Worker{Pool: r.pool, ID: "a", Cores: sharedCapacity, NoHelpers: true}
		asking := &Worker{Pool: r.pool, ID: "b", Cores: sharedCapacity}
		order := []*Worker{helperless, asking}
		if i%4 >= 2 {
			order = []*Worker{asking, helperless}
		}
		for _, w := range order {
			if _, ok := r.claimAs(t, w); ok && w == helperless {
				t.Fatalf("round %d: a worker that asks for no helpers took the job of a shared play", i)
			}
		}
		if worker := mustScalar(t, r, fmt.Sprintf("SELECT COALESCE(worker, '') FROM transcode_jobs WHERE id = %d", id)); worker != "b" {
			t.Fatalf("round %d: the job is on %q, want the worker that asks for helpers", i, worker)
		}
		r.exec(t, "DELETE FROM transcode_jobs")
	}
}
