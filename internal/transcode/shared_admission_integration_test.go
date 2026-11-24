//go:build integration

package transcode

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"jellyscale/internal/playback"
)

// JS-161 (D-044, D-046, D-050): admission by what the workers with room keep at real time, alone or together, and the
// rule that a viewer already watching at real time is never taken under it by a newcomer. Rows stand for workers as
// the cluster sees them: transcode_nodes.cores is the worker's capacity in reference cores (what the worker declared or assumed), and a new column, transcode_nodes.no_helpers, says the worker's jobs ask for no helpers
// (transcodeHelpers false), which a replica must read to know whether a play can be shared. The names the build adds:
// that column, playback.CoresHeldByWatchedFilm, and a refusal's words that carry the cost and the most any worker has.
//
// A 4K tone mapped re-encode costs 3.2 cores. A worker throttled to 0.85 times real time on it alone has 0.85 * 3.2 =
// 2.72 reference cores. Two such workers kept it at 1.0x with helpers (docs/transcoding.md, JS-162 and JS-168), a
// worker alone does not. What lies between (how much slower than the cost the pair may be) is the design's, taken
// from the 2+2 and 2+6 measurements; these checks use the measured 2.72 + 2.72 and the far-off 0.6 + 0.6, and leave
// the margin between to the build.
//
// Mutants each must fail: sum the free capacities with no helpers-on test (the helpers-off case); admit a lone worker
// by its own capacity times 1.2 (the lone case); count a helper as free for a newcomer, as today (the helper-held
// cases); stop the helper in ensure before the room is judged (a refused play cancels a helper); let the small worker
// claim a job it does not hold when a worker that holds it is free (the placement case).

const sharedCapacity = 2.72

// addHelperlessNode is a worker whose jobs ask for no helpers.
func (r *rig) addHelperlessNode(t *testing.T, id string, cores float64) {
	t.Helper()
	r.insertNodeRow(t, id, playback.OutputVersion, fmt.Sprintf("INSERT INTO transcode_nodes (id, roles, cores, no_helpers, output_version) VALUES ($1, 'worker', %v, true, $2)", cores))
}

func (r *rig) refusal(t *testing.T, spec playback.Spec, device string) (reason, text string) {
	t.Helper()
	reason, text, err := r.jobs(t).Refusal(context.Background(), spec, r.user, device)
	if err != nil {
		t.Fatal(err)
	}
	return reason, text
}

// A play that no worker keeps at real time alone, and that the workers with room keep together, is admitted; the same
// play is refused, with its reason, when the workers are too slow even together, when there is one, when no worker
// asks for helpers, and when the other worker's cores are held by another viewer's job. Today the first is refused.
func TestAPlayThatTwoWorkersHoldTogetherIsAdmittedAndOneThatTheyDoNotIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		nodes func(t *testing.T, r *rig)
		admit bool
	}{
		{"two throttled workers, helpers on", func(t *testing.T, r *rig) {
			r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
			r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
		}, true},
		{"one throttled worker alone", func(t *testing.T, r *rig) { r.addNode(t, "a", sharedCapacity, playback.OutputVersion) }, false},
		{"two workers far too slow, together", func(t *testing.T, r *rig) {
			r.addNode(t, "a", 0.6, playback.OutputVersion)
			r.addNode(t, "b", 0.6, playback.OutputVersion)
		}, false},
		{"two throttled workers, helpers off on both", func(t *testing.T, r *rig) {
			r.addHelperlessNode(t, "a", sharedCapacity)
			r.addHelperlessNode(t, "b", sharedCapacity)
		}, false},
		{"the other worker's cores held by another viewer's job", func(t *testing.T, r *rig) {
			r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
			r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
			r.running(t, "hdr4k", "b")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			tc.nodes(t, r)
			spec := r.costSpec(t, "hdr4k")
			reason, text := r.refusal(t, spec, "device")
			if tc.admit {
				if reason != "" {
					t.Fatalf("refused (%s): %s", reason, text)
				}
				return
			}
			if reason == "" {
				t.Fatal("a 3.2 core play that these workers do not keep at real time is admitted")
			}
			// The refused line gives the cost and the most any worker had, in reference cores.
			if !strings.Contains(text, "3.2") {
				t.Errorf("the refusal %q does not give the job's cost, 3.2", text)
			}
			if tc.name == "one throttled worker alone" && !strings.Contains(text, "2.7") {
				t.Errorf("the refusal %q does not give the most any worker had, 2.72", text)
			}
		})
	}
}

// A play admitted as shared runs its viewer's job on one of the two workers, and a helper of it is claimed by the
// other, which holds nothing of the spec (D-052). Today the job is admitted by neither worker's claim.
func TestASharedPlaysJobIsClaimedByOneWorkerAndItsHelperByTheOther(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
	r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
	spec := r.costSpec(t, "hdr4k")
	r.wantOffered(t, spec)
	if _, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), psid(0), 0, 0); err != nil {
		t.Fatal(err)
	}
	wa := &Worker{Pool: r.pool, ID: "a", Cores: sharedCapacity}
	wb := &Worker{Pool: r.pool, ID: "b", Cores: sharedCapacity}
	first, ok := r.claimAs(t, wa)
	taker, other, otherW := wa, "b", wb
	if !ok {
		if first, ok = r.claimAs(t, wb); !ok {
			t.Fatal("neither worker claimed the viewer's job of an admitted shared play")
		}
		taker, other, otherW = wb, "a", wa
	}
	if _, again := r.claimAs(t, otherW); again {
		t.Error("both workers claimed something of one viewer's job")
	}
	r.watchedBy(t, spec, "device")
	if done, err := taker.insertHelper(context.Background(), claimed{ID: first, Hash: spec.Hash(), Spec: spec}, 3, 12); err != nil || !done {
		t.Fatalf("the helper was not queued: %v, %v", done, err)
	}
	helper, ok := r.claimAs(t, otherW)
	if !ok {
		t.Fatalf("the other worker, %s, did not claim the helper", other)
	}
	if !r.isHelper(t, helper) {
		t.Error("what the other worker claimed is not the helper")
	}
}

// With workers of unequal speed a job goes to the worker that holds it at real time, whichever worker looks at the
// queue first, from the start and after a seek: 10 of 10 each. This one passes today, because a job dearer than a
// worker's cores is not claimed by it; it holds the rule that shared admission must not lift for a worker that holds
// the job alone (the process-level check in cmd/jellyscale is the one that fails today, through the 3.2 floor).
func TestAJobGoesToTheWorkerThatHoldsItWhicheverLooksFirst(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "big", 4, playback.OutputVersion)
	r.addNode(t, "small", sharedCapacity, playback.OutputVersion)
	big := &Worker{Pool: r.pool, ID: "big", Cores: 4}
	small := &Worker{Pool: r.pool, ID: "small", Cores: sharedCapacity}
	for i := range 20 {
		spec := r.costSpec(t, "hdr4k")
		id := r.addJob(t, spec, "queued", "", false, 0)
		if i%2 == 1 {
			r.startsAt(t, id, 12) // After a seek.
		}
		order := []*Worker{big, small}
		if (i/2)%2 == 1 {
			order = []*Worker{small, big}
		}
		for _, w := range order {
			got, ok := r.claimAs(t, w)
			if ok && w == small {
				t.Fatalf("round %d: the worker that does not hold the job claimed it", i)
			}
			if ok && got != id {
				t.Fatalf("round %d: claimed job %d, want %d", i, got, id)
			}
		}
		if worker := mustScalar(t, r, fmt.Sprintf("SELECT COALESCE(worker, '') FROM transcode_jobs WHERE id = %d", id)); worker != "big" {
			t.Fatalf("round %d: the job is on %q, want the worker that holds it", i, worker)
		}
		r.exec(t, "DELETE FROM transcode_jobs")
	}
}

// sharedPlay is a viewer's 4K tone mapped job running on worker a (2.72 cores, which it does not hold alone) with
// its helper on worker b (also 2.72), a viewer watching it from "device-1".
func (r *rig) sharedPlay(t *testing.T) (spec playback.Spec, viewerJob, helper int64, watcher string) {
	t.Helper()
	r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
	r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
	spec, viewerJob = r.running(t, "hdr4k", "a")
	watcher = r.watchedBy(t, spec, "device-1")
	helper = r.addJob(t, spec, "running", "b", true, 0)
	return spec, viewerJob, helper, watcher
}

// A new viewer whose job fits only by stopping a helper that a watched job needs to stay at real time is refused
// within seconds with the reason of its own, whatever the newcomer costs, and the helper is not stopped, neither by
// the refusal nor by a stream replica's segment request that queues the job (the same rule for both). Once the first
// viewer stops, the same play is admitted on its first PlaybackInfo. Today the newcomer is admitted and the helper
// stopped.
func TestANewViewerWhoFitsOnlyByStoppingAWatchedFilmsHelperIsRefusedAndTheHelperStays(t *testing.T) {
	// In this state (the viewer's 3.2 job on a, its helper on b, both at 2.72) stopping the helper makes a 1080p SDR
	// newcomer fit on b, so the reason is the held film's. It does not make a 3.2 newcomer fit: b alone has 2.72, and a
	// has 2.72 - 3.2 for a partner. That is plain shortage, NoWorkerWithFreeCores, which JS-164 reads as demand and the
	// helper-held reason must not relabel (the 4K state that does fit by stopping is in the next test).
	for kind, want := range map[string]string{"sdr1080": playback.CoresHeldByWatchedFilm, "hdr4k": playback.NoWorkerWithFreeCores} {
		t.Run(kind, func(t *testing.T) {
			r := newRig(t, nil)
			_, viewer, helper, watcher := r.sharedPlay(t)
			newcomer := r.costSpec(t, kind)
			reason, text := r.refusal(t, newcomer, "device-2")
			if reason != want {
				t.Fatalf("the newcomer's reason is %q (%s), want %s", reason, text, want)
			}
			if want == playback.CoresHeldByWatchedFilm && !strings.Contains(strings.ToLower(text), "watch") {
				t.Errorf("the words %q do not say the cores are held by a film another viewer is watching", text)
			}
			// A stream replica's request for the same play: refused, and no helper stopped or delayed.
			_, _, _ = r.jobs(t).ensure(context.Background(), newcomer, newcomer.Hash(), psid(1), 0, 0)
			for name, id := range map[string]int64{"the helper": helper, "the viewer's job": viewer} {
				if got := r.state(t, id); got != "running" {
					t.Errorf("%s is %s after the newcomer was refused", name, got)
				}
			}
			if n := r.count(t, fmt.Sprintf("SELECT count(*) FROM transcode_jobs WHERE spec_hash = '%s' AND state IN ('queued','running')", newcomer.Hash())); n != 0 {
				t.Errorf("a refused play left %d live jobs", n)
			}
			// The first viewer stops: the same play is admitted at its next PlaybackInfo.
			if err := r.jobs(t).Stop(context.Background(), r.user, watcher, "", ""); err != nil {
				t.Fatal(err)
			}
			if reason, text := r.refusal(t, newcomer, "device-2"); reason != "" {
				t.Errorf("after the first viewer stopped, the same play is refused (%s): %s", reason, text)
			}
		})
	}
}

// What a watched film holds is judged from the ledger, per watched spec, and not from a helper row alone: JS-168 saw
// the second viewer arrive with both workers holding helpers of the first (its own job had made its chunk), and with
// the helper just finished. A helper row, or a running viewer job of a worker too small for it, are two states of one
// hold. In each the newcomer is refused with the held reason, and nothing is stopped, queued or delayed. Mutants:
// "held = a helper whose spec has a running non-helper job on a worker with capacity under its cost" (fails the
// first), "held = a live helper row" (fails the second). Today each newcomer is admitted and any helper stopped.
func TestAWatchedFilmsHoldDoesNotDependOnWhichRowsAreLive(t *testing.T) {
	type state struct {
		name  string
		setup func(t *testing.T, r *rig, spec playback.Spec)
		// The reason each newcomer gets: stopping a helper makes the 4K one fit only where both workers' cores are the
		// helpers' (then two free workers hold it together).
		want map[string]string
	}
	for _, st := range []state{
		{"helpers only, on both workers (the viewer's own job has made its chunk)", func(t *testing.T, r *rig, spec playback.Spec) {
			r.addJob(t, spec, "done", "a", false, 0)
			r.addJob(t, spec, "running", "a", true, 0)
			r.addJob(t, spec, "running", "b", true, 0)
		}, map[string]string{"sdr1080": playback.CoresHeldByWatchedFilm, "hdr4k": playback.CoresHeldByWatchedFilm}},
		{"the viewer's job running, no helper row, the last helper done", func(t *testing.T, r *rig, spec playback.Spec) {
			r.addJob(t, spec, "running", "a", false, 0)
			r.addJob(t, spec, "done", "b", true, 0)
		}, map[string]string{"sdr1080": playback.CoresHeldByWatchedFilm, "hdr4k": playback.NoWorkerWithFreeCores}},
	} {
		for _, kind := range []string{"sdr1080", "hdr4k"} {
			t.Run(st.name+"/"+kind, func(t *testing.T) {
				r := newRig(t, nil)
				r.addNode(t, "a", sharedCapacity, playback.OutputVersion)
				r.addNode(t, "b", sharedCapacity, playback.OutputVersion)
				spec := r.costSpec(t, "hdr4k")
				r.watchedBy(t, spec, "device-1")
				st.setup(t, r, spec)
				before := r.stringOf(t, "SELECT string_agg(id || ':' || state, ',' ORDER BY id) FROM transcode_jobs")
				newcomer := r.costSpec(t, kind)
				if reason, text := r.refusal(t, newcomer, "device-2"); reason != st.want[kind] {
					t.Fatalf("the newcomer's reason is %q (%s), want %s", reason, text, st.want[kind])
				}
				_, _, _ = r.jobs(t).ensure(context.Background(), newcomer, newcomer.Hash(), psid(1), 0, 0)
				if after := r.stringOf(t, "SELECT string_agg(id || ':' || state, ',' ORDER BY id) FROM transcode_jobs"); after != before {
					t.Errorf("a refused newcomer changed the jobs: %s, was %s", after, before)
				}
			})
		}
	}
}

// The first viewer's own seek is never refused for that play's own helpers: its job is replaced (the old one is
// cancelled, a new one inserted at the seek), whose room is its own, and the helper stays. A build that judges the seek
// as a newcomer finds a with 2.72 free and b holding the play's helper, no partner, and refuses the watched film.
func TestTheFirstViewersOwnSeekIsNotRefusedForItsOwnHelpers(t *testing.T) {
	r := newRig(t, nil)
	spec, viewer, helper, watcher := r.sharedPlay(t)
	r.exec(t, "UPDATE transcode_jobs SET owner = $1 WHERE id = $2", watcher, viewer)
	r.exec(t, "UPDATE transcode_viewers SET job_id = $1 WHERE play_session_id = $2", viewer, watcher)
	if _, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), watcher, 90, 90); err != nil {
		t.Fatalf("the first viewer's own seek is refused: %v", err)
	}
	if got := r.state(t, helper); got != "running" {
		t.Errorf("the play's own helper is %s after its viewer's seek", got)
	}
	if got := r.state(t, viewer); got == "running" || got == "queued" {
		t.Errorf("the viewer's old job is still %s after its seek", got)
	}
	if n := r.count(t, fmt.Sprintf("SELECT count(*) FROM transcode_jobs WHERE spec_hash = '%s' AND NOT helper AND state IN ('queued','running')", spec.Hash())); n != 1 {
		t.Errorf("%d live jobs of the viewer after its seek, want its one new job", n)
	}
}

// Shared admission takes its margin from the measured pairs (JS-162, JS-168: two workers at 0.79 to 0.91 times real time
// alone held the play at 1.0 together) and not from adding free capacities: playback caps its reading at 1.0, so the runs
// show no headroom to extrapolate. Each worker that counts must have 0.65 of the job's price free, 2.08 of the 3.2: capacity
// is the price a worker keeps at 1.2 times real time, so 0.65 is the 0.78 of real time that each of two workers made the job
// at alone (D-066, JS-177; JS-161 read this as 0.78 of the price, 2.5 here, and refused a pair that holds the play); workers
// below it do not count, whatever they sum to. Mutant: "sum of free capacities of helper-on workers >= cost" admits every
// refused row here.
func TestSharedAdmissionNeedsEachWorkerAtTheSlowestMeasuredSpeed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []float64
		admit bool
	}{
		{"the measured pair", []float64{2.72, 2.72}, true},
		{"two workers just above 0.65 of the price", []float64{2.1, 2.1}, true},
		{"two workers at 0.75 of the price, which a literal 0.78 refused", []float64{2.4, 2.4}, true},
		{"two workers just below 0.65 of the price, though they sum to 4.1", []float64{2.05, 2.05}, false},
		{"two workers at half the cost, summing to 3.2", []float64{1.6, 1.6}, false},
		{"three workers at a third, summing to 3.3", []float64{1.1, 1.1, 1.1}, false},
		{"a worker below it beside two above", []float64{1.6, 2.72, 2.72}, true},
		{"one above beside two below", []float64{2.72, 1.6, 1.6}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			for i, c := range tc.nodes {
				r.addNode(t, fmt.Sprintf("n%d", i), c, playback.OutputVersion)
			}
			reason, text := r.refusal(t, r.costSpec(t, "hdr4k"), "device")
			if tc.admit && reason != "" {
				t.Errorf("refused (%s): %s", reason, text)
			}
			if !tc.admit && reason != playback.NoWorkerWithFreeCores {
				t.Errorf("reason %q (%s), want %s", reason, text, playback.NoWorkerWithFreeCores)
			}
		})
	}
}

// A helper of a job that its own worker holds at real time alone is still stopped for a new viewer, as today.
func TestAHelperOfAJobItsWorkerHoldsAloneIsStillStoppedForANewViewer(t *testing.T) {
	r := newRig(t, nil)
	r.addNode(t, "a", 4, playback.OutputVersion)
	r.addNode(t, "b", 4, playback.OutputVersion)
	spec, _ := r.running(t, "hdr4k", "a")
	r.watchedBy(t, spec, "device-1")
	helper := r.addJob(t, spec, "running", "b", true, 0)
	newcomer := r.costSpec(t, "hdr4k")
	if reason, text := r.refusal(t, newcomer, "device-2"); reason != "" {
		t.Fatalf("refused (%s): %s", reason, text)
	}
	if _, _, err := r.jobs(t).ensure(context.Background(), newcomer, newcomer.Hash(), psid(1), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := r.state(t, helper); got != "cancelled" {
		t.Errorf("the helper is %s, want cancelled for the newcomer", got)
	}
}

// A newcomer whose job fits on some worker without stopping a held helper is admitted there with no helper stopped:
// a worker too small for a helper that has the newcomer's 1080p SDR cost free.
func TestANewcomerThatFitsWithoutStoppingAHelperStopsNone(t *testing.T) {
	r := newRig(t, nil)
	_, _, helper, _ := r.sharedPlay(t)
	r.addNode(t, "c", 1, playback.OutputVersion)
	newcomer := r.costSpec(t, "sdr1080")
	if reason, text := r.refusal(t, newcomer, "device-2"); reason != "" {
		t.Fatalf("refused (%s): %s", reason, text)
	}
	if _, _, err := r.jobs(t).ensure(context.Background(), newcomer, newcomer.Hash(), psid(1), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := r.state(t, helper); got != "running" {
		t.Errorf("the helper is %s though the newcomer fits on worker c", got)
	}
}

// A viewer who joins a re-encode already running (same film, same output) starts no new job and is never refused by
// the rule.
func TestAViewerJoiningARunningReencodeIsNeverRefusedByTheHelperRule(t *testing.T) {
	r := newRig(t, nil)
	spec, _, _, _ := r.sharedPlay(t)
	before := r.count(t, "SELECT count(*) FROM transcode_jobs")
	if reason, text := r.refusal(t, spec, "device-2"); reason != "" {
		t.Fatalf("a second viewer of the same output is refused (%s): %s", reason, text)
	}
	if _, _, err := r.jobs(t).ensure(context.Background(), spec, spec.Hash(), psid(1), 0, 0); err != nil {
		t.Fatal(err)
	}
	if after := r.count(t, "SELECT count(*) FROM transcode_jobs"); after != before {
		t.Errorf("the joiner started %d new jobs", after-before)
	}
}
