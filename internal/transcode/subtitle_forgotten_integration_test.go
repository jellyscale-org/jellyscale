//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// JS-030, subtitle leases: a worker that is forgotten (its liveness lock gone
// and its row deleted by a reap) has its running subtitle_jobs rows cut to the
// forgottenLease in the statement that deletes its row, as its transcode jobs
// are. Real worker processes (outage_kit_integration_test.go for the proxies,
// failover_integration_test.go for the plain ones), the real ffmpeg behind a shim
// that hangs the first run, and production's lease of 30 s (JELLYSCALE_TEST_LEASE)
// wherever the bound is "before the lease it had at the kill would have run out":
// a build that waits the lease out fails at any length.

const everyKindOfSubtitleRow = 5

// plantRows makes running subtitle_jobs rows of every kind for the worker, each
// with the lease: a film's text pass, a subtitle file beside the film, a .sup,
// fonts and chapter pictures.
func (r *rig) plantRows(t *testing.T, worker string, lease string) []string {
	t.Helper()
	kinds := []struct{ kind, key string }{
		{"track", "pass:" + worker}, {"track", "track:" + worker}, {"sup", "sup:" + worker}, {"fonts", "fonts:" + worker}, {"chapters", "chapters:" + worker},
	}
	var keys []string
	for i, k := range kinds {
		r.exec(t, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
			VALUES ($1, $2, 'item', 'source', $3, 'running', $4, 1, now() + $5::interval)`, k.key, k.kind, i, worker, lease)
		keys = append(keys, k.key)
	}
	return keys
}

// recordCuts installs a trigger that notes, in the statement that deletes a node's row
// (an AFTER trigger sees what the statement's other parts did), the leases of that
// node's running subtitle and transcode rows. It is removed when the test ends.
func (r *rig) recordCuts(t *testing.T) {
	t.Helper()
	drop := func() {
		for _, sql := range []string{"DROP TRIGGER IF EXISTS js030_record ON transcode_nodes", "DROP FUNCTION IF EXISTS js030_record()", "DROP TABLE IF EXISTS js030_cut"} {
			r.pool.Exec(context.Background(), sql) //nolint:errcheck
		}
	}
	t.Cleanup(drop) // Registered before anything is made, as docs/testing.md asks.
	r.exec(t, "CREATE TABLE js030_cut (worker text, key text, lease timestamptz, at timestamptz)")
	r.exec(t, `CREATE FUNCTION js030_record() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		INSERT INTO js030_cut SELECT OLD.id, key, lease_expires_at, now() FROM subtitle_jobs WHERE worker = OLD.id AND state = 'running';
		INSERT INTO js030_cut SELECT OLD.id, 'transcode:' || id, lease_expires_at, now() FROM transcode_jobs WHERE worker = OLD.id AND state = 'running';
		RETURN OLD; END $$`)
	r.exec(t, "CREATE TRIGGER js030_record AFTER DELETE ON transcode_nodes FOR EACH ROW EXECUTE FUNCTION js030_record()")
}

type cut struct {
	key         string
	lease, at   time.Time
	secondsLeft float64
}

func (r *rig) cuts(t *testing.T, worker string) map[string]cut {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), "SELECT key, lease, at, extract(epoch FROM lease - at) FROM js030_cut WHERE worker = $1", worker)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]cut{}
	for rows.Next() {
		var c cut
		if err := rows.Scan(&c.key, &c.lease, &c.at, &c.secondsLeft); err != nil {
			t.Fatal(err)
		}
		found[c.key] = c
	}
	return found
}

// A worker killed with running rows of every kind beside a transcode job: the
// statement that deletes its row leaves each at most the forgottenLease and
// never a longer lease than it had; two workers forgotten in one look are the same.
func TestAForgottenWorkersSubtitleRowsOfEveryKindAreCutInTheStatementThatDeletesItsRow(t *testing.T) {
	s := newStack(t, 4, thirtySecondLease...)
	r := s.r
	r.recordCuts(t)
	a, b := s.workers[0], s.workers[1]
	live := s.workers[2]
	for _, w := range []*outageWorker{a, b, live} {
		r.plantRows(t, w.id, "25 seconds")
	}
	// A row that already had less than the forgottenLease keeps what it had.
	r.exec(t, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
		VALUES ('short', 'track', 'item', 'source', 9, 'running', $1, 1, now() + interval '100 milliseconds')`, a.id)
	r.exec(t, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
		VALUES ('ended', 'track', 'item', 'source', 10, 'done', $1, 1, now() + interval '25 seconds')`, a.id)
	// And a transcode job of each, beside them.
	for _, w := range []*outageWorker{a, b} {
		r.exec(t, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at, output_version)
			VALUES ($1, '{}', 'o', 0, 0, 'running', $2, now() + interval '25 seconds', 5)`, "h"+w.id, w.id)
	}
	var before time.Time
	r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM subtitle_jobs WHERE key = 'short'").Scan(&before) //nolint:errcheck

	killAll(a, b) // Two workers forgotten in one look, or in two.
	s.forgottenAt(t, a.id)
	s.forgottenAt(t, b.id)
	for _, w := range []*outageWorker{a, b} {
		got := r.cuts(t, w.id)
		for _, key := range []string{"pass:", "track:", "sup:", "fonts:", "chapters:", "transcode:"} {
			var c *cut
			for k, v := range got {
				if strings.HasPrefix(k, key) {
					v := v
					c = &v
				}
			}
			if c == nil {
				t.Errorf("worker %s: no running %s row was there when its row was deleted", w.id, key)
				continue
			}
			if c.secondsLeft > liveTiming("forgottenLease").Seconds()+0.05 {
				t.Errorf("worker %s: the row %s had %.2f s of lease in the statement that deleted the worker's row, want at most %v", w.id, c.key, c.secondsLeft, liveTiming("forgottenLease"))
			}
		}
	}
	if c, ok := r.cuts(t, a.id)["short"]; ok && c.lease.After(before) {
		t.Errorf("a row with %v of lease left was given %v", before, c.lease)
	}
	// What is not the forgotten worker's keeps its lease.
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE worker = $1 AND lease_expires_at > now() + interval '15 seconds'", live.id); n != everyKindOfSubtitleRow {
		t.Errorf("a live worker has %d of its %d rows with their lease, the cut reached it", n, everyKindOfSubtitleRow)
	}
	if n := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE key = 'ended' AND lease_expires_at > now() + interval '15 seconds'"); n != 1 {
		t.Errorf("a finished row's lease was touched")
	}
}

// The takeover itself: another worker claims the row before the lease it had at
// the kill would have run out (30 s), the request waiting is answered with its
// track, the file is read by one more ffmpeg run and not two, and what a lost
// lease costs is unchanged: one more attempt.
func TestAKilledWorkersSubtitleExtractionIsTakenOverBeforeItsLeaseRunsOut(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	begun := filepath.Join(t.TempDir(), "begun")
	hang := fmt.Sprintf("if mkdir %q.lock 2>/dev/null; then touch %q; exec sleep 600; fi", begun, begun)
	runs, _ := shimFFmpeg(t, hang)
	workers := map[string]*exec.Cmd{}
	for _, id := range []string{"w1", "w2", "w3"} {
		workers[id] = r.workerProcess(t, id, t.TempDir(), "JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_LEASE=30s", "JELLYSCALE_TEST_HEARTBEAT=9s")
	}
	j := r.jobs(t)
	type result struct {
		track subtitles.Track
		err   error
	}
	answered := make(chan result, 1)
	go func() {
		track, err := r.pick(t, j, assets, source, 3)
		answered <- result{track, err}
	}()
	waitFor(t, "the first extraction to start", 30*time.Second, func() bool { _, err := os.Stat(begun); return err == nil })
	var owner string
	var lease time.Time
	if err := r.pool.QueryRow(context.Background(), "SELECT worker, lease_expires_at FROM subtitle_jobs WHERE state = 'running' AND kind NOT IN ('fonts', 'chapters')").Scan(&owner, &lease); err != nil {
		t.Fatalf("no subtitle job is running: %v", err)
	}
	victim := workers[owner]
	victim.Process.Kill() //nolint:errcheck
	victim.Wait()         //nolint:errcheck
	// The lease is not read again after the kill: the reap cuts it within a second, and a test process that
	// was not scheduled for that long would read the cut one. The one read before the kill is at most a
	// heartbeat earlier than the real one, so the bound is stricter, never falsely failed.

	var got result
	select {
	case got = <-answered:
	case <-time.After(90 * time.Second):
		t.Fatal("the request was not answered")
	}
	var early bool
	r.pool.QueryRow(context.Background(), "SELECT now() < $1", lease).Scan(&early) //nolint:errcheck
	if got.err != nil {
		t.Fatalf("the request waiting on the killed worker's extraction: %v", got.err)
	}
	wantTrack(t, got.track, 3)
	if !early {
		t.Errorf("the request was answered after the lease the row had at the kill (%v) would have run out: the row waited it out", lease)
	}
	if n := runs(); n != 2 {
		t.Errorf("%d ffmpeg runs (the killed one and the one that finished), want 2", n)
	}
	if n := r.count(t, "SELECT max(attempts) FROM subtitle_jobs WHERE kind = 'track'"); n != 2 {
		t.Errorf("the row records %d attempts after one takeover, want 2", n)
	}
}

// A row whose lease is lost on its third claim fails with today's error and is
// remembered as it is today, and that does not wait for the lease: the worker
// that held it was killed.
func TestAKilledWorkersRowOnItsThirdClaimFailsWithTodaysErrorBeforeItsLease(t *testing.T) {
	s := newStack(t, 3, withEnv(thirtySecondLease, "JELLYSCALE_TEST_SUBTITLES=1")...)
	r := s.r
	victim := s.workers[0]
	r.exec(t, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
		VALUES ('third', 'track', 'item', 'source', 3, 'running', $1, 3, now() + interval '25 seconds')`, victim.id)
	var lease time.Time
	r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM subtitle_jobs WHERE key = 'third'").Scan(&lease) //nolint:errcheck
	killAll(victim)
	waitFor(t, "the row to fail", 90*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE key = 'third' AND state = 'failed'") == 1
	})
	var early bool
	r.pool.QueryRow(context.Background(), "SELECT now() < $1", lease).Scan(&early) //nolint:errcheck
	if !early {
		t.Errorf("the row failed only after its lease (%v) ran out", lease)
	}
	var reason string
	var permanent bool
	r.pool.QueryRow(context.Background(), "SELECT error, permanent FROM subtitle_jobs WHERE key = 'third'").Scan(&reason, &permanent) //nolint:errcheck
	if reason != "the worker stopped renewing its lease" || !permanent {
		t.Errorf("the row failed with %q (permanent %v), want today's error, permanent", reason, permanent)
	}
}

// No lease is cut for a worker that is not forgotten: its lock only ended and
// was taken again, every worker through an outage shorter than a lease less a
// heartbeat, a worker stopped with its connections standing. A row planted for
// the worker is never renewed by it, so a cut shows in its lease exactly.
func TestNoSubtitleLeaseIsCutForAWorkerThatIsNotForgotten(t *testing.T) {
	cases := map[string]func(t *testing.T, s *stack, w *outageWorker){
		"its lock only ended and was taken again": func(t *testing.T, s *stack, w *outageWorker) {
			old := s.lockPids()[w.id]
			s.r.terminate(t, w.id)
			waitFor(t, "the worker to hold its lock again", 30*time.Second, func() bool { p := s.lockPids()[w.id]; return p != 0 && p != old })
			time.Sleep(3 * time.Second)
		},
		"every worker through an outage shorter than a lease less a heartbeat": func(t *testing.T, s *stack, w *outageWorker) {
			s.away(true, false, s.workers...)
			time.Sleep(3 * time.Second)
			s.back(false, s.workers...)
			s.allLocked(30*time.Second, s.workers...)
			time.Sleep(2 * time.Second)
		},
		"stopped with its connections standing": func(t *testing.T, s *stack, w *outageWorker) {
			w.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
			time.Sleep(4 * time.Second)
			w.cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
			time.Sleep(2 * time.Second)
		},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStack(t, 3, thirtySecondLease...)
			w := s.workers[0]
			keys := s.r.plantRows(t, w.id, "25 seconds")
			leases := map[string]time.Time{}
			for _, key := range keys {
				var at time.Time
				s.r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM subtitle_jobs WHERE key = $1", key).Scan(&at) //nolint:errcheck
				leases[key] = at
			}
			event(t, s, w)
			if s.r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", w.id) != 1 {
				t.Fatalf("the worker was forgotten: the case is not what it claims to be")
			}
			for _, key := range keys {
				var at time.Time
				var worker string
				s.r.pool.QueryRow(context.Background(), "SELECT lease_expires_at, COALESCE(worker, '') FROM subtitle_jobs WHERE key = $1", key).Scan(&at, &worker) //nolint:errcheck
				if !at.Equal(leases[key]) || worker != w.id {
					t.Errorf("row %s: its lease was %v and is %v, its worker %s and is %s", key, leases[key], at, w.id, worker)
				}
			}
		})
	}
}

// A stopped worker's extraction is taken over only once the lease it last
// renewed has run out.
func TestAStoppedWorkersSubtitleExtractionIsTakenOverOnlyOnceItsLastLeaseHasRunOut(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.textTracks(t)
	begun := filepath.Join(t.TempDir(), "begun")
	hang := fmt.Sprintf("if mkdir %q.lock 2>/dev/null; then touch %q; exec sleep 600; fi", begun, begun)
	shimFFmpeg(t, hang)
	procs := map[string]int{}
	for _, id := range []string{"w1", "w2"} {
		c := r.workerProcess(t, id, t.TempDir(), "JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_LEASE=12s", "JELLYSCALE_TEST_HEARTBEAT=3600ms")
		procs[id] = c.Process.Pid
	}
	j := r.jobs(t)
	go r.pick(t, j, assets, source, 3) //nolint:errcheck
	waitFor(t, "the first extraction to start", 30*time.Second, func() bool { _, err := os.Stat(begun); return err == nil })
	var owner string
	if err := r.pool.QueryRow(context.Background(), "SELECT worker FROM subtitle_jobs WHERE state = 'running' AND kind NOT IN ('fonts', 'chapters')").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)                                           // A renewal has been sent since the claim.
	syscall.Kill(procs[owner], syscall.SIGSTOP)                       //nolint:errcheck
	t.Cleanup(func() { syscall.Kill(procs[owner], syscall.SIGCONT) }) //nolint:errcheck
	var lease time.Time
	r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM subtitle_jobs WHERE worker = $1 AND state = 'running'", owner).Scan(&lease) //nolint:errcheck
	waitFor(t, "another worker to take the row over", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE kind = 'track' AND worker IS NOT NULL AND worker <> $1", owner) == 1
	})
	var after bool
	r.pool.QueryRow(context.Background(), "SELECT now() >= $1", lease).Scan(&after) //nolint:errcheck
	if !after {
		t.Errorf("the stopped worker's row was taken over before the lease it last renewed (%v) ran out", lease)
	}
}

// A worker forgotten while it lives (its lock gone with its connection standing)
// ends the ffmpeg of its extraction before another worker's ffmpeg for the row
// starts, moves no cut lease later even when a renewal it had already sent reaches
// Postgres after the reap, and writes nothing to the row once another worker has
// claimed it. The interleaving is forced: the next renewal of the extraction's row is
// kept from Postgres, the lock is cut, and the renewal is let go once the row is gone.
func TestAWorkerForgottenWhileItLivesEndsItsExtractionAndItsLateRenewalMovesNoCutLeaseLater(t *testing.T) {
	overlaps := subtitleShim(t)
	s := newStack(t, 3, "JELLYSCALE_TEST_SUBTITLES=1", "JELLYSCALE_TEST_LEASE=10s", "JELLYSCALE_TEST_HEARTBEAT=3s")
	r := s.r
	source, assets := r.textTracks(t)
	type result struct {
		track subtitles.Track
		err   error
	}
	answered := make(chan result, 1)
	go func() {
		track, err := r.pick(t, s.jobs, assets, source, 3)
		answered <- result{track, err}
	}()
	waitFor(t, "an extraction to run", 60*time.Second, func() bool {
		return r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE state = 'running' AND kind = 'track'") == 1
	})
	var owner string
	r.pool.QueryRow(context.Background(), "SELECT worker FROM subtitle_jobs WHERE state = 'running' AND kind = 'track'").Scan(&owner) //nolint:errcheck
	victim := s.worker(owner)
	if victim == nil {
		t.Fatalf("the extraction is run by %q, not one of the workers", owner)
	}
	// The renewal the check lets go late: the next lease renewal of the extraction's row
	// is kept from Postgres, on the keeper's connection, until the worker is forgotten.
	// The proxy asks the matcher as the statement arrives, under its own lock, so the
	// close below is the moment this statement, and no other, is held.
	var key string
	if err := r.pool.QueryRow(context.Background(), "SELECT key FROM subtitle_jobs WHERE state = 'running' AND kind = 'track'").Scan(&key); err != nil || key == "" {
		t.Fatalf("the running extraction's row has no key to match its renewal by: %q, %v", key, err)
	}
	held := make(chan struct{})
	releaseRenewal := victim.keep.HoldStatement(func(sent []byte) bool {
		if !bytes.Contains(sent, []byte("UPDATE subtitle_jobs s SET lease_expires_at")) || !bytes.Contains(sent, []byte(key)) {
			return false
		}
		close(held)
		return true
	})
	t.Cleanup(releaseRenewal)
	waitFor(t, "a lease renewal of the extraction's row to be held from Postgres", 30*time.Second, func() bool {
		select {
		case <-held:
			return true
		default:
			return false
		}
	})
	victim.live.CutServers() // The lock is gone and the connection stands,
	victim.live.Refuse(true) // and it cannot lock again until the check lets it.
	t.Cleanup(func() { victim.live.Refuse(false) })
	s.forgottenAt(t, owner)
	var cutLease time.Time
	r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM subtitle_jobs WHERE kind = 'track'").Scan(&cutLease) //nolint:errcheck
	// The renewal reaches Postgres after the reap.
	releaseRenewal()

	// The cut lease never moves later while the row is still the victim's.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var lease time.Time
		var worker string
		if err := r.pool.QueryRow(context.Background(), "SELECT lease_expires_at, COALESCE(worker, '') FROM subtitle_jobs WHERE kind = 'track'").Scan(&lease, &worker); err != nil {
			t.Fatal(err)
		}
		if worker != owner {
			break
		}
		if lease.After(cutLease.Add(100 * time.Millisecond)) {
			t.Fatalf("the cut lease (%v) was moved later, to %v, by a renewal of the forgotten worker", cutLease, lease)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got result
	select {
	case got = <-answered:
	case <-time.After(90 * time.Second):
		t.Fatal("the request waiting on the extraction was not answered")
	}
	if got.err != nil {
		t.Fatalf("the request: %v", got.err)
	}
	wantTrack(t, got.track, 3)
	if o := overlaps(); len(o) > 0 {
		t.Errorf("an ffmpeg for the row started while the forgotten worker's was still running: %v", o)
	}
	var worker, state string
	var attempts int
	r.pool.QueryRow(context.Background(), "SELECT COALESCE(worker, ''), state, attempts FROM subtitle_jobs WHERE kind = 'track'").Scan(&worker, &state, &attempts) //nolint:errcheck
	if worker == owner || state != "done" || attempts != 2 {
		t.Errorf("the row ended %s on %q with %d attempts: the forgotten worker wrote to it or the takeover was not counted once", state, worker, attempts)
	}
}

// subtitleShim hangs the first ffmpeg run and notes any run that starts while an
// earlier one is alive; the hung run is the one a forgotten worker must end.
func subtitleShim(t *testing.T) (overlaps func() []string) {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	// The films the rig makes with lavfi are no extraction: they run as they are.
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in *lavfi*) exec %[2]q "$@";; esac
for p in $(cat %[1]s/pids 2>/dev/null); do kill -0 "$p" 2>/dev/null && echo "$p was alive when $$ started" >> %[1]s/overlap; done
echo $$ >> %[1]s/pids
if mkdir %[1]s/first 2>/dev/null; then exec sleep 600; fi
exec %[2]q "$@"
`, dir, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, _ := os.ReadFile(filepath.Join(dir, "overlap"))
		return strings.Fields(strings.ReplaceAll(string(raw), " was alive when ", "|"))
	}
}

// withEnv is the environment of base and then extra, in a slice of its own.
func withEnv(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}
