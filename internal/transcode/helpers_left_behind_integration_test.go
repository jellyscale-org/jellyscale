//go:build integration

package transcode

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"jellyscale/internal/playback"
)

// JS-167 (D-046): a helper whose viewers have all seeked away stops within
// seconds, and its worker is free for the film's new place. The kit is in
// helpers_left_behind_kit_integration_test.go.
//
// The film is 23 segments of 4 seconds. Chunks are 4 segments, so a viewer at
// segment 0 has its helper at 4, and one at 12 has its own job in the chunk of
// 12 and its helper at 16. A forward seek is to segment 20, past the helper's
// chunk and the segments a request would join it for (catchUpSegments, 3), and
// past the lead; a backward one is from 12 to 0.
//
// Mutants the checks must fail, each a line of a right build: a running helper
// that never looks at the viewers (only at its limit) fails the forward and
// backward ends; a helper whose end is cancelled but whose ffmpeg is left to
// the heartbeat fails the cores check; one that looks only for "no viewer in my
// window" and not for "the viewer's limit is behind my start" fails the backward
// ends; one that ends on a failed read fails the slow-read check; one that
// counts the end as done or failed fails the counter check.

// A helper short of its limit, or paused at it, whose only viewer seeks forward
// beyond it is over within the bound: cancelled, not failed, ffmpeg gone, cores
// free. Today a helper short of its limit looks only when it gets to the limit,
// and then lifts it to the film's end (limitFor with -1).
func TestAHelperWhoseViewerSeekedForwardBeyondItIsEnded(t *testing.T) {
	for _, tc := range []struct {
		state string
		lead  time.Duration
	}{{"running", 60 * time.Second}, {"paused", 20 * time.Second}} {
		t.Run(tc.state, func(t *testing.T) {
			slow(t)
			r := newRig(t, nil)
			c := r.cluster(t, clusterOpts{n: 3, lead: tc.lead})
			sc := r.forwardScene(t, c, tc.state)
			sk := r.seek(t, c.j, sc.s, psid(0), 20)
			c.wantOver(t, sc.helper, sk.at, "after a forward seek to segment 20")
			sk.answered(t)
		})
	}
}

// Two helpers on two workers, for chunks of one stream, with the viewer gone: each
// is over, each is counted once under a reason of its own and never as failed, and
// each is logged once, by the worker that ran it, with the reason and the job.
func TestTwoHelpersWhoseViewerSeekedAwayAreEachEndedCountedAndLogged(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// Three slots: the main job and a helper on each of the other two, which is exactly two helpers (JS-168 keeps no
	// slot for a viewer who has not asked, so a fourth worker would run a third that may have stored nothing at the seek).
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
	s := r.mainStream()
	c.viewerAt(t, psid(0), 0)
	var live []hrow
	defer func() {
		if t.Failed() {
			t.Logf("jobs of the spec: %+v", r.rowsOf(t, s.hash))
		}
	}()
	waitFor(t, "two helpers or more running on other workers, the first still short of the next one's chunk", 80*time.Second, func() bool {
		live = nil
		for _, h := range r.helpersOf(t, s.hash) {
			if h.State == "running" && h.Next > h.Start {
				live = append(live, h)
			}
		}
		return len(live) >= 2 && live[0].Next <= 6
	})
	before := endedCounts(t, c.ws[c.ids[0]])
	sk := r.seek(t, c.j, s, psid(0), 20)
	for i, h := range live {
		c.wantOver(t, h, sk.at, "helper "+strconv.Itoa(i+1)+" of "+strconv.Itoa(len(live)))
	}
	sk.answered(t)

	after := endedCounts(t, c.ws[c.ids[0]])
	if after["failed"] != before["failed"] {
		t.Errorf("a helper whose viewers left was counted as failed: %v then %v", before, after)
	}
	// Under its own reason and not also as done: a helper that ends by reaching another job's chunk is done, and
	// none here is two segments from one (the seek comes with the first at 6 or less, at 4 seconds a segment).
	if after["done"] != before["done"] {
		t.Errorf("a helper whose viewers left was also counted as done: %v then %v", before, after)
	}
	reasons := newReasons(before, after)
	if len(reasons) != 1 {
		t.Fatalf("%d helpers left behind added %v to jellyscale_transcode_jobs_ended_total (before %v, after %v), want %d under one reason of its own", len(live), reasons, before, after, len(live))
	}
	for reason, n := range reasons {
		if int(n) != len(live) {
			t.Errorf("%d helpers left behind added %v under %q, want %d", len(live), n, reason, len(live))
		}
		for _, h := range live {
			c.wantLoggedOnce(t, h, reason)
		}
		if text := doc(t, "docs/running.md"); !strings.Contains(text, "`"+reason+"`") {
			t.Errorf("docs/running.md does not name the reason `%s` where it lists the ended-jobs counter's values", reason)
		}
	}
}

// One helper whose viewer left adds exactly 1 under a reason of its own, which
// no label of the counter's other values or ids shares, and is logged once.
func TestOneHelperWhoseViewerSeekedAwayAddsOneToTheCounterAndOneLogLine(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	// Two workers: the viewer's job on one and exactly one helper on the other
	// (three equal workers run two helpers, JS-168).
	c := r.cluster(t, clusterOpts{n: 2, lead: 60 * time.Second})
	sc := r.forwardScene(t, c, "running")
	before := endedCounts(t, c.ws[c.ids[0]])
	sk := r.seek(t, c.j, sc.s, psid(0), 20)
	c.wantOver(t, sc.helper, sk.at, "the helper")
	after := endedCounts(t, c.ws[c.ids[0]])
	if after["failed"] != before["failed"] {
		t.Errorf("a helper whose viewer left was counted as failed: %v then %v", before, after)
	}
	// Under its own reason and not also as done: a helper that ends by reaching another job's chunk is done, and
	// none here is two segments from one (the seek comes with the first at 6 or less, at 4 seconds a segment).
	if after["done"] != before["done"] {
		t.Errorf("a helper whose viewer left was also counted as done: %v then %v", before, after)
	}
	reasons := newReasons(before, after)
	if len(reasons) != 1 {
		t.Fatalf("one helper left behind added %v to jellyscale_transcode_jobs_ended_total (before %v, after %v), want 1 under one reason of its own", reasons, before, after)
	}
	for reason, n := range reasons {
		if n != 1 {
			t.Errorf("one helper left behind added %v under %q, want 1", n, reason)
		}
		c.wantLoggedOnce(t, sc.helper, reason)
	}
	sk.answered(t)
}

// wantLoggedOnce: one line of the log of the worker that ran the job names the
// job's id and the reason the counter has, and no other worker logs one.
// (The build chooses the reason's word; the log's line must carry it, as the
// counter's label does.)
func (c *cluster) wantLoggedOnce(t *testing.T, h hrow, reason string) {
	t.Helper()
	job := regexp.MustCompile(`\bjob=` + strconv.FormatInt(h.ID, 10) + `\b`)
	for id, log := range c.logs {
		n := 0
		for _, line := range strings.Split(log.String(), "\n") {
			if job.MatchString(line) && strings.Contains(line, reason) {
				n++
			}
		}
		switch want := map[bool]int{true: 1, false: 0}[id == h.Worker]; {
		case n != want:
			t.Errorf("worker %s logged %d lines with job=%d and %q, want %d (it ran the job: %v)", id, n, h.ID, reason, want, id == h.Worker)
		}
	}
}

// A helper ended for another reason adds nothing under the new one: here its
// worker shuts down.
func TestAHelperEndedByItsWorkerShuttingDownIsNotCountedAsLeftBehind(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
	sc := r.forwardScene(t, c, "running")
	other := c.ws[c.ids[0]]
	if sc.helper.Worker == c.ids[0] {
		other = c.ws[c.ids[1]]
	}
	before := endedCounts(t, other)
	c.stops[sc.helper.Worker]()
	waitFor(t, "the helper's row to be over", 30*time.Second, func() bool { return r.rowOf(t, sc.helper.ID).State != "running" })
	if got := newReasons(before, endedCounts(t, other)); len(got) != 0 {
		t.Errorf("a helper ended by its worker shutting down was counted as %v", got)
	}
}

// A helper still queued at the seek (here a row made for a chunk the viewer has
// left behind, claimed at once) stores nothing and holds no cores 5 seconds after
// the later of the seek and its claim.
func TestAHelperStillQueuedWhenItsViewerSeeksAwayStoresNothing(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second, noHelpers: true})
	s := r.mainStream()
	c.viewerAt(t, psid(0), 0)
	sk := r.seek(t, c.j, s, psid(0), 20)
	waitFor(t, "the job for the seek to run", 20*time.Second, func() bool {
		for _, h := range r.rowsOf(t, s.hash) {
			if !h.Helper && h.Start >= 20 && h.State == "running" {
				return true
			}
		}
		return false
	})
	raw, _ := json.Marshal(s.spec)
	var id int64
	if err := r.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, helper, output_version, cost)
		VALUES ($1, $2, 'helper', 12, 12, true, $3, $4) RETURNING id`, s.hash, raw, playback.OutputVersion, s.spec.Cost()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	Notify(context.Background(), r.pool, jobHint)
	claimed := time.Now()
	waitFor(t, "the helper to be claimed or ended", 20*time.Second, func() bool { return r.rowOf(t, id).State != "queued" })
	if sk.at.After(claimed) {
		claimed = sk.at
	}
	by := claimed.Add(leftBehindBound)
	within(t, by, "the helper's ffmpeg is still running", func() bool { return r.ffmpegAt(12) == "" })
	within(t, by, "the helper's row is still running", func() bool { return r.rowOf(t, id).State != "running" })
	sleepUntil(by.Add(4 * time.Second))
	if got := r.storedBetween(s.hash, 12, 20); len(got) != 0 {
		t.Errorf("the helper left behind stored segments %v", got)
	}
	if row := r.rowOf(t, id); row.State != "cancelled" {
		t.Errorf("the helper ended as %q (%q), want cancelled", row.State, row.Err)
	}
	sk.answered(t)
}

// A helper that stopped has stored no segment that starts a lead or more after
// the last asked by a viewer at or before its chunk (15 here), stores nothing
// later than 5 seconds after the seek, keeps what it stored, and no helper is
// queued for the chunks left behind.
func TestAHelperLeftBehindStoresNothingMoreAndNoOneIsQueuedForItsChunks(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
	sc := r.forwardScene(t, c, "running")
	rowsBefore := len(r.rowsOf(t, sc.s.hash))
	storedBefore := r.storedBetween(sc.s.hash, 0, 20)
	sk := r.seek(t, c.j, sc.s, psid(0), 20)
	sleepUntil(sk.at.Add(leftBehindBound))
	atBound := r.storedBetween(sc.s.hash, 0, 20)
	time.Sleep(10 * time.Second)
	later := r.storedBetween(sc.s.hash, 0, 20)
	if len(later) != len(atBound) {
		t.Errorf("the helper went on storing after the bound: segments %v at 5 s, %v 10 s later", atBound, later)
	}
	for _, n := range later {
		if n >= 15 {
			t.Errorf("segment %d is stored, which starts a lead (60 s) or more after segment 0, the last asked by a viewer at or before the helper's chunk", n)
		}
	}
	have := map[int]bool{}
	for _, n := range later {
		have[n] = true
	}
	for _, n := range storedBefore {
		if !have[n] {
			t.Errorf("segment %d was stored at the seek and is not stored now", n)
		}
	}
	for _, h := range r.rowsOf(t, sc.s.hash)[rowsBefore:] {
		if h.Helper && h.Start < 20 {
			t.Errorf("a helper was queued for the chunk at %d the viewer left behind: %+v", h.Start, h)
		}
	}
	sk.answered(t)
}

// A viewer who plays on into the segments a helper makes past its own chunk
// (invariant 37) gets each one, and no helper stores a segment a lead or more
// after the furthest it asked for.
func TestAViewerWhoPlaysOnIntoAHelpersChunkGetsEverySegmentAndIsNotRunAheadOf(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 24 * time.Second})
	r.playThrough(t, c.j, psid(0), 0, 14) // Fatal on a segment that does not come.
	time.Sleep(6 * time.Second)
	// Furthest asked is 13; 24 s is 6 segments, so 19 is the first that may not be stored.
	if got := r.storedBetween(r.hash, 19, filmEnd); len(got) != 0 {
		t.Errorf("segments %v are stored with the viewer at 13 and a lead of 6 segments: %+v", got, r.rowsOf(t, r.hash))
	}
	for _, h := range r.rowsOf(t, r.hash) {
		if h.State == "failed" {
			t.Errorf("a job failed while the viewer played on: %+v", h)
		}
	}
}

// A seek into the helper's next segments joins it: nothing starts there, and it
// stores no further than a lead past what the viewer asked for.
func TestASeekIntoAHelpersNextSegmentsJoinsItAndIsNotRunAheadOf(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 20 * time.Second})
	sc := r.forwardScene(t, c, "paused")
	before := len(r.rowsOf(t, sc.s.hash))
	if _, err := r.fetch(t, c.j, psid(0), sc.helper.Next+1); err != nil {
		t.Fatalf("a seek into the helper's next segments failed: %v", err)
	}
	for _, h := range r.rowsOf(t, sc.s.hash)[before:] {
		if h.Start >= sc.helper.Start && h.Start <= sc.helper.Next+1 {
			t.Errorf("the seek started a job at %d inside a running helper's window: %+v", h.Start, h)
		}
	}
	time.Sleep(6 * time.Second)
	// Asked 6, 20 s is 5 segments: 11 is the first that may not be stored.
	if got := r.storedBetween(r.hash, 11, filmEnd); len(got) != 0 {
		t.Errorf("segments %v are stored with the viewer at 6 and a lead of 5 segments", got)
	}
}

// With two viewers of the spec, one seeking away leaves the helper storing up to
// the lead past the one who stayed; it ends within the bound of the last of them
// seeking away.
func TestAHelperWithAViewerLeftKeepsStoringAndEndsWithTheLastOne(t *testing.T) {
	t.Run("one seeks forward", func(t *testing.T) {
		slow(t)
		r := newRig(t, nil)
		c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
		c.viewerAt(t, psid(0), 0)
		c.viewerAt(t, psid(1), 0)
		h := c.helperIn(t, r.mainStream(), "running", 6)
		r.seek(t, c.j, r.mainStream(), psid(0), 20)
		stays := time.Now().Add(40 * time.Second)
		within(t, stays, "the helper to store two more segments for the viewer who stayed", func() bool {
			row := r.rowOf(t, h.ID)
			if row.State != "running" {
				t.Fatalf("the helper was ended with a viewer at its chunk left: %+v", row)
			}
			return row.Next >= h.Next+2
		})
		last := r.seek(t, c.j, r.mainStream(), psid(1), 20)
		c.wantOver(t, h, last.at, "after the last viewer seeked forward")
	})
	t.Run("one seeks back", func(t *testing.T) {
		slowAt(t, "0.5")
		r := newRig(t, nil)
		c := r.cluster(t, clusterOpts{n: 3, lead: 24 * time.Second})
		sc := r.backScene(t, c, "running", psid(0), psid(1))
		r.seek(t, c.j, sc.s, psid(0), 0)
		within(t, time.Now().Add(60*time.Second), "the helper to reach its limit for the viewer who stayed (segment 18) and wait there", func() bool {
			row := r.rowOf(t, sc.helper.ID)
			if row.State != "running" {
				t.Fatalf("the helper was ended with a viewer at segment 12 left: %+v", row)
			}
			return row.Next == 18 && r.ffmpegAt(sc.helper.Start) == "T"
		})
		time.Sleep(3 * time.Second)
		if row := r.rowOf(t, sc.helper.ID); row.State != "running" {
			t.Fatalf("the helper waiting at the stayed viewer's limit was ended: %+v", row)
		}
		last := r.seek(t, c.j, sc.s, psid(1), 0)
		c.wantOver(t, sc.helper, last.at, "after the last viewer seeked back")
	})
}

// A viewer who seeks back far behind a helper, from the middle of the film to
// the first minute, leaves it many leads ahead of anyone: over within the bound,
// running or paused, nothing stored later, and a seek that lands on the
// helper's last-stored segment is no wait for anyone. Today a helper paused far
// past a viewer who went back holds its worker for the film.
func TestAHelperWhoseViewerSeekedBackFarBehindItIsEnded(t *testing.T) {
	for _, state := range []string{"running", "paused"} {
		t.Run(state, func(t *testing.T) {
			slowAt(t, "0.5") // Half of real time, so that a helper is short of its limit for long enough to be found there.
			r := newRig(t, nil)
			c := r.cluster(t, clusterOpts{n: 3, lead: 24 * time.Second})
			sc := r.backScene(t, c, state, psid(0))
			storedBefore := r.storedBetween(sc.s.hash, 16, filmEnd)
			sk := r.seek(t, c.j, sc.s, psid(0), 0)
			c.wantOver(t, sc.helper, sk.at, "after a seek back from segment 12 to 0")
			sleepUntil(sk.at.Add(leftBehindBound))
			atBound := r.storedBetween(sc.s.hash, 16, filmEnd)
			time.Sleep(8 * time.Second)
			later := r.storedBetween(sc.s.hash, 16, filmEnd)
			if len(later) != len(atBound) {
				t.Errorf("the helper went on storing after the bound: %v then %v", atBound, later)
			}
			if len(later) < len(storedBefore) {
				t.Errorf("what the helper stored did not stay: %v then %v", storedBefore, later)
			}
			for _, n := range later {
				if n >= 18 { // 12 + 6 segments of lead.
					t.Errorf("segment %d is stored, a lead or more after segment 12, the last asked before the seek", n)
				}
			}
			for _, h := range r.rowsOf(t, sc.s.hash) {
				if h.Helper && h.ID > sc.helper.ID && h.Start >= 16 {
					t.Errorf("a helper was queued for the chunk at %d left behind: %+v", h.Start, h)
				}
			}
			sk.answered(t)
		})
	}
}

// The helper's cores are free at once: three jobs want the three slots of the
// cluster, and all three run within a bound that a heartbeat of 10 s cannot
// meet, which they can only do if every helper's slot is free before its
// worker's next heartbeat; and the workers no longer count the helpers' cores.
//
// Forced: the seek's job and two new viewers' jobs, queued as rows (no preemption is
// asked, which would stop a helper by another road), need all three slots of the
// cluster, whatever held them (the viewer's own job, which the seek cancels, and the
// helpers the viewer left behind, of which a cluster of three keeps two at most). Which
// job takes which slot does not matter. The seek is made right after a heartbeat of
// a helper's worker: its beats are on a grid of 10 s (a put's renewal does not
// move it), and a lease that moved with no segment stored beside it is one, so the lease
// cannot be read for "no beat since". The helper is slowed to half time so that it is
// far from the end of its chunk, where it ends of itself, however long the wait for the
// beat. A helper that is stopped at its next beat is 9.8 s after the seek; the bound is 7.
func TestAStoppedHelpersCoresAreFreeBeforeItsNextHeartbeat(t *testing.T) {
	slowAt(t, "0.5")
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second, heartbeat: 10 * time.Second})
	s := r.mainStream()
	c.viewerAt(t, psid(0), 0)
	helper := c.helperIn(t, s, "running", 5)
	lease := func() (at time.Time) {
		r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM transcode_jobs WHERE id = $1", helper.ID).Scan(&at) //nolint:errcheck
		return at
	}
	stored := func() int { return len(r.storedBetween(s.hash, helper.Start, helper.Start+8)) }
	l0, n0 := lease(), stored()
	for beat, by := false, time.Now().Add(40*time.Second); !beat; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(by) {
			t.Fatalf("no heartbeat of the helper's worker was seen in 40 s (its lease moved with no segment stored beside it)")
		}
		l1, n1 := lease(), stored()
		if l1.After(l0) && n1 == n0 {
			time.Sleep(60 * time.Millisecond) // A put renews the lease before its segment is stored.
			beat = stored() == n0
		}
		l0, n0 = l1, n1
	}
	if row := r.rowOf(t, helper.ID); row.State != "running" || row.Next >= helper.Start+4 {
		t.Fatalf("the helper was over or at the end of its chunk before the seek: %+v", row)
	}

	sk := r.seek(t, c.j, s, psid(0), 20)
	var queued []lbStream
	var ids []int64
	for i, bitrate := range []int64{700_000, 800_000} {
		q := r.streamOf(bitrate)
		if err := c.j.Watch(context.Background(), r.viewer(psid(10+i)), q.hash, 0); err != nil {
			t.Fatal(err)
		}
		r.keepWatchingStream(t, c.j, q.hash, psid(10+i))
		queued = append(queued, q)
		ids = append(ids, r.addJob(t, q.spec, "queued", "", false, 0))
	}
	Notify(context.Background(), r.pool, jobHint)
	by := sk.at.Add(leftBehindBound + 2*time.Second)
	for i, id := range ids {
		within(t, by, "queued job "+strconv.Itoa(i+1)+" of the new viewers to be claimed (all three slots were wanted)", func() bool { return r.rowOf(t, id).State == "running" })
	}
	within(t, by, "the seek's job to run", func() bool {
		for _, h := range r.rowsOf(t, s.hash) {
			if !h.Helper && h.Start >= 20 && h.State == "running" {
				return true
			}
		}
		return false
	})
	busy, want := 0.0, s.spec.Cost()+queued[0].spec.Cost()+queued[1].spec.Cost()
	for _, w := range c.ws {
		busy += value(scrape(t, w), "jellyscale_transcode_cores_busy")
	}
	if busy != want {
		t.Errorf("the workers count %v cores busy, want %v: the three jobs', and not a stopped helper's", busy, want)
	}
	if row := r.rowOf(t, helper.ID); row.State != "cancelled" {
		t.Errorf("the helper is %q (%q), want cancelled", row.State, row.Err)
	}
	if got := r.ffmpegAt(helper.Start); got != "" {
		t.Errorf("the helper's ffmpeg is still there (state %q)", got)
	}
	sk.answered(t)
}

// A viewer comes back to the first segment the stopped helper did not store,
// right after it ended: it gets that segment, the chunk's rest is made as it
// plays on, and no job fails.
func TestASeekToTheFirstSegmentAStoppedHelperDidNotStoreIsAnswered(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
	sc := r.forwardScene(t, c, "running")
	away := r.seek(t, c.j, sc.s, psid(0), 20)
	c.wantOver(t, sc.helper, away.at, "after the forward seek")
	away.answered(t)
	first := -1
	for n := sc.helper.Start; n < 20; n++ {
		if !r.storedOn(sc.s.hash, n) {
			first = n
			break
		}
	}
	if first < 0 {
		t.Fatalf("the stopped helper stored every segment up to 19, so it ran on after the seek: %+v", r.rowsOf(t, sc.s.hash))
	}
	end := (first/4 + 1) * 4
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, _, err := r.play(ctx, c.j, psid(0), first, end); err != nil {
		t.Fatalf("the seek back to segment %d, the first the helper did not store, and play on to the end of its chunk (%d): %v", first, end-1, err)
	}
	for _, h := range r.rowsOf(t, sc.s.hash) {
		if h.State == "failed" {
			t.Errorf("a job failed: %+v", h)
		}
	}
}

// A helper that still has a viewer is not stopped when the viewer steps back but
// stays within a lead of the helper's first segment (here 11, whose limit 17
// is past the helper's start, 16).
func TestAHelperIsNotStoppedWhenItsViewerStepsBackWithinALeadOfIt(t *testing.T) {
	slowAt(t, "0.5")
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 24 * time.Second})
	sc := r.backScene(t, c, "paused", psid(0))
	sk := r.seek(t, c.j, sc.s, psid(0), 11)
	for end := sk.at.Add(10 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if row := r.rowOf(t, sc.helper.ID); row.State != "running" {
			t.Fatalf("the helper was ended by a viewer stepping back to segment 11, within a lead of its first segment: %+v", row)
		}
	}
	sk.answered(t)
}

// A read of the viewers' rows that is slow, does not answer, or fails is no reason to
// stop a helper or lift its limit.
func TestAHelperIsNotStoppedAndItsLimitIsNotLiftedWhenTheReadOfTheViewersFails(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 20 * time.Second, proxied: true})
	sc := r.forwardScene(t, c, "paused")
	watch := func(what string, d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if row := r.rowOf(t, sc.helper.ID); row.State != "running" {
				t.Fatalf("the helper was ended during %s: %+v", what, row)
			}
			if got := r.storedBetween(sc.s.hash, 5, filmEnd); len(got) != 0 {
				t.Fatalf("the helper's limit was lifted during %s: segments %v stored with the viewer at 0 and a lead of 5 segments", what, got)
			}
		}
	}
	// A delay is paid on every packet, so a read that needs a new connection of the
	// pool (the workers share one) takes six times it: 12 s at 2 s, measured, past
	// the 10 s lease. The helper's loop makes that read on its own turn, and a loop
	// that does not turn for a lease is given up (DOMAINS 25,
	// TestAJobWhoseWorkHasNotComeRoundIsGivenUpWhileTheKeeperStillAnswers), so the
	// longest the loop can wait here is a read begun just before the hold, 6 x 0.5 s
	// of delay and then the 4 s hold: 7 s, 3 s short of the lease.
	c.proxy.Delay(500 * time.Millisecond)
	watch("reads that take half a second", 8*time.Second)
	c.proxy.Delay(0)
	c.proxy.Hold()
	watch("reads that do not answer", 4*time.Second)
	c.proxy.Release()
	watch("the minute after", 6*time.Second)
	// Reads that fail: on a failed read furthest returns 0 and "a viewer at 0", which a helper
	// that looks at the segment before it looks at the error takes for a viewer far behind it.
	// Each look's statement is cut as it is sent, for 6 seconds, so every look that comes fails.
	stop := time.After(6 * time.Second)
	for cut := true; cut; {
		tripped := c.proxy.SeverOn("FROM transcode_viewers")
		select {
		case <-tripped:
			watch("a read of the viewers that failed", 700*time.Millisecond)
		case <-stop:
			cut = false
		}
	}
	watch("the seconds after the failures", 5*time.Second)
}

// A viewer that has asked for no segment frees the helper while it is seen, as
// invariant 57 says, whatever the other viewer did; the bound returns, and the
// helper ends, once the viewer who is away is the only one left.
func TestAPlaylistOnlyViewerStillFreesAHelperWhoseOtherViewerSeekedAway(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 20 * time.Second, idle: 5 * time.Second})
	sc := r.forwardScene(t, c, "paused")
	if err := c.j.Watch(context.Background(), r.viewer(psid(1)), sc.s.hash, -1); err != nil {
		t.Fatal(err)
	}
	stop := r.keepWatching(t, c.j, psid(1))
	waitFor(t, "the free viewer to let the helper past its limit", 30*time.Second, func() bool { return r.rowOf(t, sc.helper.ID).Next > 5 })
	sk := r.seek(t, c.j, sc.s, psid(0), 20)
	at := r.rowOf(t, sc.helper.ID).Next
	for end := sk.at.Add(6 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if row := r.rowOf(t, sc.helper.ID); row.State != "running" {
			t.Fatalf("the helper was ended while a viewer that asked for no segment was seen: %+v", row)
		}
	}
	if now := r.rowOf(t, sc.helper.ID).Next; now <= at {
		t.Errorf("the helper made no segment in 6 seconds with a free viewer: next %d then %d", at, now)
	}
	stop()
	waitFor(t, "the helper to end once the free viewer is no longer seen", 30*time.Second, func() bool { return r.rowOf(t, sc.helper.ID).State != "running" })
	if row := r.rowOf(t, sc.helper.ID); row.State != "cancelled" {
		t.Errorf("the helper ended as %q (%q), want cancelled", row.State, row.Err)
	}
	sk.answered(t)
}

// A running helper reads the viewers' rows at most once per half second.
func TestARunningHelperReadsTheViewersAtMostOncePerHalfSecond(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second, proxied: true})
	r.forwardScene(t, c, "running")
	count := func() (n int) {
		for _, sql := range c.proxy.Statements() {
			if strings.Contains(sql, "FROM transcode_viewers") && strings.Contains(sql, "max(segment)") {
				n++
			}
		}
		return n
	}
	was := count()
	time.Sleep(6 * time.Second)
	// JS-168: three equal workers run two helpers for the one viewer, and each reads twice a second (invariant 57),
	// so the bound is per helper row the spec has, not for one: 12 in 6 seconds each, and two for a look in flight at each end.
	helpers := len(r.helpersOf(t, r.hash))
	if helpers < 1 {
		helpers = 1
	}
	if reads := count() - was; reads > 14*helpers {
		t.Errorf("the workers read the viewers' furthest segment %d times in 6 seconds with %d helper(s) and a main job's end, want at most 2 a second each", reads, helpers)
	}
}

// A job that is not a helper reads the viewers' rows only while it is at its
// limit: one running short of it reads none.
func TestAJobThatIsNotAHelperReadsTheViewersOnlyAtItsLimit(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 1, lead: 60 * time.Second, proxiedAll: true, noHelpers: true})
	c.viewerAt(t, psid(0), 0)
	count := func() (n int) {
		for _, sql := range c.proxy.Statements() {
			if strings.Contains(sql, "FROM transcode_viewers") && strings.Contains(sql, "max(segment)") {
				n++
			}
		}
		return n
	}
	was := count()
	time.Sleep(6 * time.Second)
	if reads := count() - was; reads != 0 {
		t.Errorf("a job short of its limit read the viewers' furthest segment %d times in 6 seconds", reads)
	}
	if rows := r.rowsOf(t, r.hash); len(rows) != 1 || rows[0].State != "running" {
		t.Errorf("want the one job running: %+v", rows)
	}
}

// With one worker, or helpers off, no helper row is queued.
func TestNoHelperIsQueuedWithOneWorkerOrWithHelpersOff(t *testing.T) {
	for name, o := range map[string]clusterOpts{
		"one worker":  {n: 1, lead: 60 * time.Second},
		"helpers off": {n: 3, lead: 60 * time.Second, noHelpers: true},
	} {
		t.Run(name, func(t *testing.T) {
			slow(t)
			r := newRig(t, nil)
			c := r.cluster(t, o)
			c.viewerAt(t, psid(0), 0)
			for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
				if helpers := r.helpersOf(t, r.hash); len(helpers) != 0 {
					t.Fatalf("a helper was queued: %+v", helpers)
				}
			}
		})
	}
}

// A viewer who plays on into the segments a helper makes past its own chunk
// (invariant 37: a request for them joins the helper, no job of its own starts) is a
// viewer the helper serves: it bounds the helper, and does not end it, and the
// helper's limit is not lifted because the viewer left the chunk's window. Today a
// helper looks at the viewers in [0, start+chunk) only, finds none once the viewer
// is at segment 8 or later, and lifts its limit to the film's end. A build that
// answers that by cancelling the helper under the viewer starts a job at 8
// and ends a helper the viewer is using: this fails it too.
func TestAViewerPlayedIntoAHelpersChunkBoundsTheHelperAndDoesNotEndIt(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 20 * time.Second})
	r.playThrough(t, c.j, psid(0), 0, 10) // Fatal on a segment that does not come.
	r.keepWatching(t, c.j, psid(0))
	helpers := r.helpersOf(t, r.hash)
	var mine *hrow
	for i, h := range helpers {
		if h.Start == 4 {
			mine = &helpers[i]
		}
	}
	if mine == nil {
		t.Fatalf("no helper was made for the chunk at 4, so the viewer did not play into one: %+v", r.rowsOf(t, r.hash))
	}
	time.Sleep(30 * time.Second)
	// The viewer is at 9 and the lead 5 segments: 14 is the first that may not be stored.
	if got := r.storedBetween(r.hash, 14, filmEnd); len(got) != 0 {
		t.Errorf("segments %v are stored with the viewer at 9 and a lead of 5 segments: %+v", got, r.rowsOf(t, r.hash))
	}
	// Bounded by the viewer, it runs on, or ends done at the chunk of a helper it
	// asked for itself (invariant 58); it is never cancelled as left behind.
	if row := r.rowOf(t, mine.ID); row.State != "running" && row.State != "done" {
		t.Errorf("the helper the viewer played into was ended as %q (%q), want it running or done at the next chunk, bounded by the viewer", row.State, row.Err)
	}
	for _, h := range r.rowsOf(t, r.hash) {
		if !h.Helper && h.Start >= 4 {
			t.Errorf("a job of its own was started at %d for a viewer who was joined to the helper: %+v", h.Start, h)
		}
		if h.State == "failed" {
			t.Errorf("a job failed: %+v", h)
		}
	}
}

// A job that is not a helper is ended and counted as before: a job viewers have
// all seeked away from (TestAJobLeftBehindBySeeksGivesBackItsSlot's scene) adds 1 to done and
// nothing under any reason that is not done, failed or interrupted. Helpers are off, so
// the only job that ends is the one left behind.
func TestAJobThatIsNotAHelperIsStillCountedAsDone(t *testing.T) {
	slow(t) // A fast job at the film's end would end of itself and be counted too.
	r := newRig(t, nil)
	var w *Worker
	r.worker(t, func(x *Worker) {
		w = x
		x.Lead = 12 * time.Second
		x.Slots = 3
		x.NoHelpers = true
	})
	j := r.jobs(t)
	if _, err := r.fetch(t, j, psid(0), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(t, j, psid(1), 1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to stop at its lead", 20*time.Second, func() bool { return r.paused() && r.next(t) == 4 })
	before := endedCounts(t, w)
	for i, n := range []int{12, 18} {
		if _, err := r.fetch(t, j, psid(i), n); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the job left behind to end", 20*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE start_segment = 0 AND state = 'running'") == 0
	})
	after := endedCounts(t, w)
	if got := newReasons(before, after); len(got) != 0 {
		t.Errorf("a job that is not a helper was counted as %v: no reason of a helper's is for it", got)
	}
	if after["done"] != before["done"]+1 {
		t.Errorf("the job left behind was counted as done %v times, want 1 (before %v, after %v)", after["done"]-before["done"], before, after)
	}
}

// A helper ended by a new viewer's preemption adds nothing under the reason of a
// helper whose viewers left: it is interrupted, as before.
func TestAPreemptedHelperIsNotCountedAsLeftBehind(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 20 * time.Second})
	sc := r.forwardScene(t, c, "paused")
	before := endedCounts(t, c.ws[c.ids[0]])
	// Three more viewers of three more streams: the third finds no slot but the helper's.
	for i, bitrate := range []int64{800_000, 900_000, 1_000_000} {
		s := r.streamOf(bitrate)
		if _, err := r.firstSegment(c.j, s.spec, s.hash, psid(10+i)); err != nil {
			t.Fatalf("new viewer %d: %v", i, err)
		}
		r.keepWatchingStream(t, c.j, s.hash, psid(10+i))
	}
	waitFor(t, "the helper to be stopped for a new viewer", 20*time.Second, func() bool { return r.rowOf(t, sc.helper.ID).State == "cancelled" })
	if row := r.rowOf(t, sc.helper.ID); !strings.Contains(row.Err, "new viewer") {
		t.Fatalf("the helper was not ended by the preemption: %+v", row)
	}
	after := endedCounts(t, c.ws[c.ids[0]])
	if got := newReasons(before, after); len(got) != 0 {
		t.Errorf("a helper ended by a new viewer's preemption was counted as %v", got)
	}
	if after["interrupted"] <= before["interrupted"] {
		t.Errorf("the preempted helper was not counted as interrupted: %v then %v", before, after)
	}
}

// A helper that reaches a chunk another job has ends, and is done, as before: the
// commonest end of a helper, and no helper whose viewers left. Its viewer stays at
// segment 0 within a lead of it all the while.
func TestAHelperThatReachesAnotherJobsChunkIsDoneAndNotCountedAsLeftBehind(t *testing.T) {
	slow(t)
	r := newRig(t, nil)
	c := r.cluster(t, clusterOpts{n: 3, lead: 60 * time.Second})
	sc := r.forwardScene(t, c, "running")
	before := endedCounts(t, c.ws[c.ids[0]])
	waitFor(t, "the helper to reach the next chunk, which another job has, and end", 100*time.Second, func() bool {
		return r.rowOf(t, sc.helper.ID).State != "running"
	})
	if row := r.rowOf(t, sc.helper.ID); row.State != "done" {
		t.Fatalf("the helper ended as %q (%q), want done at the chunk another job has: %+v", row.State, row.Err, r.rowsOf(t, sc.s.hash))
	}
	after := endedCounts(t, c.ws[c.ids[0]])
	if got := newReasons(before, after); len(got) != 0 {
		t.Errorf("a helper that reached another job's chunk was counted as %v", got)
	}
	if after["done"] <= before["done"] {
		t.Errorf("the helper that reached another job's chunk was not counted as done: %v then %v", before, after)
	}
}
