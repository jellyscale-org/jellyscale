package playback

import (
	"slices"
	"testing"
)

// JS-161 (D-046, D-050): a play refused because its cores are held by a film another viewer is watching has a reason of
// its own, apart from NoWorkerWithFreeCores, which the log, the decision line and the refusals counter carry and
// JS-164 reads as demand. Names the build must add: CoresHeldByWatchedFilm.

func TestTheHelperHeldRefusalHasAReasonOfItsOwn(t *testing.T) {
	if CoresHeldByWatchedFilm == NoWorkerWithFreeCores || CoresHeldByWatchedFilm == "" {
		t.Fatalf("the reason is %q, which is not its own", CoresHeldByWatchedFilm)
	}
	if !slices.Contains(Reasons(), CoresHeldByWatchedFilm) {
		t.Error("Reasons() does not list it, so the decision line's counters and docs/running.md's list leave it out")
	}
}
