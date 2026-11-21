//go:build integration

package transcode

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// In one play of a film, several seeks each get their segment the same way: a short one that starts a new
// job, one past half, one as far in as the dogfood's (1183 seconds) and one into the film's last segment.
// The play goes on between them as a viewer's does, on one worker and one stream replica.
func TestSeveralSeeksInOnePlayEachGetTheirSegment(t *testing.T) {
	films := newSeekFilms(t)
	a := seekRow(t, "dtshdma-8")
	severalSeeks(t, films, a, films.film(t, a))
}

// severalSeeks plays the film of the audio at media from its start and seeks as the test above says, with a burnt image subtitle.
func severalSeeks(t *testing.T, films *seekFilms, a seekAudio, media string) {
	s := films.rig(t, media, a)
	s.apply(a, false, true)
	const last = 452 // 453 segments: 452 of 4 seconds and one of 2
	targets := []int{12, seekSegment, 1183/4 + 1, last}
	// One run of the whole film from its start, from which each seek's neighbours are kept.
	s.fresh(t)
	whole := s.rig.worker(t, func(w *Worker) { w.Slots, w.NoHelpers = 2, true })
	s.playThrough(t, s.jobs(t), psid(0), 0, last+1)
	whole()
	refs := map[int]string{}
	for _, n := range targets {
		refs[n] = t.TempDir()
		s.storedTo(t, refs[n], max(n-1, 0), min(n+2, last+1))
	}
	s.fresh(t)
	stop := s.rig.worker(t, func(w *Worker) { w.Slots, w.NoHelpers, w.Lead = 2, true, 8*time.Second })
	defer stop()
	j := s.jobs(t)
	if _, err := s.fetchAt(t, j, psid(0), 0, 0); err != nil {
		t.Fatalf("segment 0: %v", err)
	}
	for _, n := range targets {
		t.Run("segment "+strconv.Itoa(n), func(t *testing.T) {
			_, err := s.fetchAt(t, j, psid(0), n, n)
			got := s.got(t, n, err)
			var mine []job
			for _, row := range got.jobs {
				if row.Start == n {
					mine = append(mine, row)
				}
				if row.State == "failed" {
					t.Errorf("a job of the play failed: %+v", row)
				}
			}
			got.jobs = mine
			checkSegment(t, got, refs[n], n)
		})
	}
}

// helperJob is a running or finished helper job of the spec that has made something.
func (r *rig) helperJob(t *testing.T) (job, bool) {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT id, COALESCE(worker, ''), start_segment, next_segment, state
		FROM transcode_jobs WHERE spec_hash = $1 AND helper AND next_segment > start_segment ORDER BY id`, r.hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.Worker, &j.Start, &j.Next, &j.State); err != nil {
			t.Fatal(err)
		}
		return j, true
	}
	return job{}, false
}

// The same seek through a helper's chunk gets its segment: the viewer seeks past half, a helper starts on the chunk after the
// viewer's job, and the viewer then seeks into that chunk. The helper's own job makes the segment (it began above segment 0 and
// stored it), and no helper job of the play ends failed: a viewer's job covering for a failed helper does not count.
// reference is the run from the start's segments from seekSegment-1 to seekSegment+seekReach+2.
func aSeekThroughAHelper(t *testing.T, s *scannedFilm, reference string) {
	s.fresh(t)
	dir := s.ownStore(t)
	slowAt(t, "4") // Slower than the helpers' bound, set high below, so that helpers are wanted.
	s.workers(t, 3, 2, 6, dir, "JELLYSCALE_TEST_HELP_BELOW=1000")
	j := s.jobs(t)
	if _, err := s.fetchAt(t, j, psid(0), seekSegment, seekSegment); err != nil {
		t.Fatalf("the seek's segment: %v", err)
	}
	s.keepWatching(t, j, psid(0))
	var helper job
	waitFor(t, "a helper to make its first segment", 90*time.Second, func() bool {
		var ok bool
		helper, ok = s.helperJob(t)
		return ok
	})
	if helper.Start <= seekSegment {
		t.Fatalf("a helper began at %d, not beyond the viewer's job at %d", helper.Start, seekSegment)
	}
	n := helper.Next + 1
	if n > seekSegment+seekReach {
		t.Fatalf("the helper's chunk is at %d, beyond the %d segments the run from the start is held for", n, seekReach)
	}
	body, err := s.fetchAt(t, j, psid(0), n, seekSegment)
	if err != nil || !isMP4Segment(body) {
		t.Fatalf("the seek into the helper's chunk, segment %d: %v", n, err)
	}
	rows := s.jobsOf(t, s.hash)
	var maker *job
	for i, row := range rows {
		if row.Start > 0 && row.Start <= n && n < row.Next && row.Worker != "" {
			maker = &rows[i]
		}
	}
	if maker == nil || !s.isHelper(t, maker.ID) {
		t.Errorf("segment %d was not made by a helper job that began above segment 0: jobs %+v", n, rows)
	}
	if failed := s.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND helper AND state = 'failed'", s.hash); failed != 0 {
		t.Errorf("%d helper jobs ended failed: %+v", failed, rows)
	}
	checkSplice(t, s.got(t, n, nil), reference, n)
}

func (r *rig) isHelper(t *testing.T, id int64) bool {
	t.Helper()
	return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id = $1 AND helper", id) == 1
}

// The same seek through a failover gets its segment: with two real worker processes, the one making the seek's job is killed,
// the replacement job begins above segment 0 on the other worker, stores the next missing segment and does not fail, and the
// viewer gets no answer of 400 or above. reference is as for aSeekThroughAHelper.
func aSeekThroughAFailover(t *testing.T, s *scannedFilm, reference string) {
	s.fresh(t)
	dir := s.ownStore(t)
	slowAt(t, "2")
	first := s.workerProcess(t, "worker-a", dir)
	second := s.workerProcess(t, "worker-b", dir)
	j := s.jobs(t)
	if _, err := s.fetchAt(t, j, psid(0), seekSegment, seekSegment); err != nil {
		t.Fatalf("the seek's segment: %v", err)
	}
	// The worker of the job that began at the seek's segment, not of the latest running job (rig.owner): a helper's job for
	// a chunk beyond it may already be the latest, and killing its worker leaves the seek's job to the one that is not killed.
	var owner string
	if err := s.pool.QueryRow(context.Background(), "SELECT COALESCE(worker, '') FROM transcode_jobs WHERE spec_hash = $1 AND start_segment = $2 AND state = 'running' ORDER BY id DESC LIMIT 1", s.hash, seekSegment).Scan(&owner); err != nil {
		t.Fatalf("no running job began at the seek's segment: %v", err)
	}
	victim, other := first, "worker-b"
	if owner == "worker-b" {
		victim, other = second, "worker-a"
	}
	if owner == "" {
		t.Fatal("no worker owns the seek's job")
	}
	victim.Process.Kill() //nolint:errcheck
	for n := seekSegment + 1; n <= seekSegment+3; n++ {
		if _, err := s.fetchAt(t, j, psid(0), n, seekSegment); err != nil {
			t.Fatalf("segment %d after the kill: %v", n, err)
		}
	}
	var replacement *job
	rows := s.jobsOf(t, s.hash)
	for i, row := range rows {
		if row.Worker == other && row.Start > seekSegment {
			replacement = &rows[i]
		}
	}
	if replacement == nil || replacement.State == "failed" {
		t.Fatalf("no replacement job above segment %d on %s that did not fail: %+v", seekSegment, other, rows)
	}
	for _, row := range rows {
		if row.State == "failed" {
			t.Errorf("a job failed: %+v", row)
		}
	}
	if replacement.Start > seekSegment+seekReach {
		t.Fatalf("the replacement began at %d, beyond the %d segments the run from the start is held for", replacement.Start, seekReach)
	}
	checkSplice(t, s.got(t, replacement.Start, nil), reference, replacement.Start)
}

// The film read through a store that has no path (an S3 bucket: ffmpeg is given a loopback URL, as in
// production), where the probe of the audio and the seek's reads are ranged reads of a URL.
func TestASeekPastHalfIntoAFilmReadFromAStoreWithNoPathGetsItsSegment(t *testing.T) {
	films := newSeekFilms(t)
	for _, name := range []string{"aac-2", "truehd-6", "dtshdma-8"} {
		t.Run(name, func(t *testing.T) {
			var a seekAudio
			for _, row := range seekAudios() {
				if row.name == name {
					a = row
				}
			}
			media := films.film(t, a)
			s := films.rig(t, media, a)
			s.apply(a, false, true)
			reference := s.fromTheStart(t, seekSegment-1, seekSegment+2, t.TempDir())
			s.fresh(t)
			stop := s.worker(t, newMeter(media, 0))
			defer stop()
			_, err := s.fetchAt(t, s.jobs(t), psid(0), seekSegment, seekSegment)
			checkSegment(t, s.got(t, seekSegment, err), reference, seekSegment)
		})
	}
}
