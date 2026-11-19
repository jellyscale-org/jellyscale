//go:build integration

package transcode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-030, one reaper: at every cluster size one worker at a time looks for the
// dead. Real worker processes (outage_kit_integration_test.go), each behind a
// proxy for each of its connections, and the proxies' record of the statements
// sent: the reaper is the worker whose statements read pg_locks or forget a
// node, so these checks need not know how the build chooses it.
//
// Timings that the criteria give are production's: the look period is 250 ms
// (a 40th of the 10 s table's lease), the confirmGap 0.5 s, the hold 10 s, and the
// lease of a job here is 30 s (JELLYSCALE_TEST_LEASE) with its production
// heartbeat of 9 s, so that "claimed before its lease ran out" cannot be met by a
// build that waits the lease out. Nothing passes or fails on how long something
// took, except that a node is never forgotten sooner than two looks the
// confirmGap apart, which a slow machine only makes later.

var thirtySecondLease = []string{"JELLYSCALE_TEST_LEASE=30s", "JELLYSCALE_TEST_HEARTBEAT=9s"}

var reaperStatement = regexp.MustCompile(`pg_locks|DELETE FROM transcode_nodes`)

// reaperStatements is how many statements of the worker, on any of its connections, read pg_locks or forget a node.
func (w *outageWorker) reaperStatements() int {
	n := 0
	for _, p := range []interface{ Statements() []string }{w.db, w.live, w.keep} {
		for _, sql := range p.Statements() {
			if reaperStatement.MatchString(sql) {
				n++
			}
		}
	}
	return n
}

// lookers are the workers among those given whose statements for the dead ran in the window.
func lookers(window time.Duration, workers []*outageWorker) []*outageWorker {
	before := make([]int, len(workers))
	for i, w := range workers {
		before[i] = w.reaperStatements()
	}
	time.Sleep(window)
	var found []*outageWorker
	for i, w := range workers {
		if w.reaperStatements() > before[i] {
			found = append(found, w)
		}
	}
	return found
}

// twentyFiveLooks is over 20 look periods, and well over the 10 look periods of a confirmation.
func twentyFiveLooks() time.Duration { return 25 * liveTiming("lookPeriod") }

// theReaper waits until exactly one worker looks for the dead, and returns it.
func (s *stack) theReaper(t *testing.T) *outageWorker {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if got := lookers(2*time.Second, s.workers); len(got) == 1 {
			return got[0]
		} else if time.Now().After(deadline) {
			names := []string{}
			for _, w := range got {
				names = append(names, w.id)
			}
			t.Fatalf("%d of the %d workers look for the dead (%v), want one", len(got), len(s.workers), names)
		}
	}
}

func (s *stack) specN(i int) playback.Spec {
	spec := s.r.spec
	spec.VideoBitrate += int64(i) * 100_000
	return spec
}

func signalAll(workers []*outageWorker, sig syscall.Signal) {
	for _, w := range workers {
		w.cmd.Process.Signal(sig) //nolint:errcheck
	}
}

func without(all []*outageWorker, drop ...*outageWorker) []*outageWorker {
	var out []*outageWorker
	for _, w := range all {
		keep := true
		for _, d := range drop {
			keep = keep && w != d
		}
		if keep {
			out = append(out, w)
		}
	}
	return out
}

// giveAJob makes the worker the owner of a job for the spec: the others are
// stopped, their connections standing so that nobody forgets them, while the
// viewer asks, and go on once it runs.
func (s *stack) giveAJob(t *testing.T, w *outageWorker, spec playback.Spec, ps string, stopped ...*outageWorker) *viewerLoop {
	t.Helper()
	signalAll(stopped, syscall.SIGSTOP)
	defer signalAll(stopped, syscall.SIGCONT)
	loop := s.play(s.jobs, spec, ps, 0, 20)
	waitFor(t, "a job of the spec to run", 60*time.Second, func() bool { return s.jobOf(spec.Hash()) != "" })
	if got := s.jobOf(spec.Hash()); got != w.id {
		t.Fatalf("the job is worker %s's, not the one the check chose (%s)", got, w.id)
	}
	return loop
}

// jobOf is the worker of the spec's running job, or "".
func (s *stack) jobOf(hash string) string {
	var worker string
	s.r.pool.QueryRow(context.Background(), "SELECT COALESCE(worker, '') FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' AND NOT helper ORDER BY id DESC LIMIT 1", hash).Scan(&worker) //nolint:errcheck
	return worker
}

// leaseOf is the lease of the spec's running job at this moment.
func (s *stack) leaseOf(t *testing.T, hash string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.r.pool.QueryRow(context.Background(), "SELECT lease_expires_at FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' AND NOT helper ORDER BY id DESC LIMIT 1", hash).Scan(&at); err != nil {
		t.Fatalf("no running job of %s: %v", hash, err)
	}
	return at
}

// forgottenAt waits until the worker's row is gone and says when the test saw it.
func (s *stack) forgottenAt(t *testing.T, id string) time.Time {
	t.Helper()
	waitFor(t, "worker "+id+" to be forgotten", 60*time.Second, func() bool {
		return s.r.count(t, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id) == 0
	})
	return time.Now()
}

// replacedBefore waits until a running job of the spec belongs to a worker that is not among dead, and says
// whether the database's clock was before the lease then.
func (s *stack) replacedBefore(t *testing.T, hash string, dead []*outageWorker, lease time.Time) (early bool, who string) {
	t.Helper()
	waitFor(t, "a live worker to run the spec "+hash, 90*time.Second, func() bool {
		who = s.jobOf(hash)
		for _, d := range dead {
			if who == d.id {
				return false
			}
		}
		return who != ""
	})
	if err := s.r.pool.QueryRow(context.Background(), "SELECT now() < $1", lease).Scan(&early); err != nil {
		t.Fatal(err)
	}
	return early, who
}

func killAll(workers ...*outageWorker) time.Time {
	at := time.Now()
	for _, w := range workers {
		w.cmd.Process.Kill() //nolint:errcheck
	}
	for _, w := range workers {
		<-w.reaped
	}
	return at
}

// With 3 workers and with 60, one worker looks for the dead over 25 look
// periods and the other workers send no statement that reads pg_locks or forgets
// a node, on any of their connections: the count does not grow with the fleet.
func TestOneWorkerAloneLooksForTheDeadWhetherThereAre3Workers60(t *testing.T) {
	for _, n := range []int{3, 60} {
		t.Run(fmt.Sprintf("%d workers", n), func(t *testing.T) {
			s := newStack(t, n)
			time.Sleep(3 * time.Second) // The role settles.
			got := lookers(twentyFiveLooks(), s.workers)
			if len(got) != 1 {
				ids := []string{}
				for _, w := range got {
					ids = append(ids, w.id)
				}
				t.Errorf("%d of %d workers sent statements that read pg_locks or forget a node over 25 look periods, want exactly 1: %v", len(got), n, ids)
			}
		})
	}
}

// The reaper killed while it holds a job a viewer waits on: exactly one of the
// others takes the role; it forgets the dead worker on today's conditions (not
// before two looks the confirmGap apart), and the job is claimed before its
// lease, as it stood at the kill, would have run out.
func TestTheReaperKilledWhileItHoldsAJobIsReplacedByExactlyOneOtherAndItsJobIsClaimedBeforeItsLease(t *testing.T) {
	slowAt(t, "0.5")
	s := newStack(t, 3, thirtySecondLease...)
	reaper := s.theReaper(t)
	spec := s.specN(0)
	loop := s.giveAJob(t, reaper, spec, psid(0), without(s.workers, reaper)...)
	waitFor(t, "the viewer to have a segment", 60*time.Second, func() bool { return loop.got.Load() >= 1 })
	survivors := without(s.workers, reaper)

	// Read before the kill: the reap cuts the lease within a second of it, so a read after could see the cut one.
	// At most a heartbeat earlier than the real lease, so the bound is stricter, never falsely failed.
	lease := s.leaseOf(t, spec.Hash())
	killed := killAll(reaper)
	forgot := s.forgottenAt(t, reaper.id)
	if gap := forgot.Sub(killed); gap < liveTiming("confirmGap") {
		t.Errorf("the dead reaper was forgotten %v after the kill, sooner than two looks the confirmGap (%v) apart", gap, liveTiming("confirmGap"))
	}
	early, who := s.replacedBefore(t, spec.Hash(), []*outageWorker{reaper}, lease)
	if !early {
		t.Errorf("worker %s took the reaper's job after the lease (%v) it had at the kill would have run out", who, lease)
	}
	before := loop.got.Load()
	waitFor(t, "the viewer to be served again", 90*time.Second, func() bool { return loop.got.Load() > before })

	time.Sleep(3 * time.Second) // The role settles.
	if got := lookers(twentyFiveLooks(), survivors); len(got) != 1 {
		t.Errorf("%d of the 2 surviving workers look for the dead over 25 look periods, want exactly 1", len(got))
	}
}

// The reaper and another worker killed together: both are forgotten and both jobs taken over.
func TestTheReaperAndAnotherWorkerKilledTogetherAreBothForgottenAndBothJobsTakenOver(t *testing.T) {
	slowAt(t, "0.5")
	s := newStack(t, 4, thirtySecondLease...)
	reaper := s.theReaper(t)
	specA, specB := s.specN(0), s.specN(1)
	a := s.giveAJob(t, reaper, specA, psid(0), without(s.workers, reaper)...)
	waitFor(t, "the first viewer to have a segment", 60*time.Second, func() bool { return a.got.Load() >= 1 })
	b := s.play(s.jobs, specB, psid(1), 0, 20)
	waitFor(t, "the second job to run", 60*time.Second, func() bool { return s.jobOf(specB.Hash()) != "" })
	other := s.worker(s.jobOf(specB.Hash()))
	if other == nil || other == reaper {
		t.Fatalf("the second job is on %v: the check needs it on a worker that is not the reaper", other)
	}
	waitFor(t, "the second viewer to have a segment", 60*time.Second, func() bool { return b.got.Load() >= 1 })

	leaseA, leaseB := s.leaseOf(t, specA.Hash()), s.leaseOf(t, specB.Hash()) // Before the kill: the reap cuts them after it.
	killed := killAll(reaper, other)
	for _, w := range []*outageWorker{reaper, other} {
		if gap := s.forgottenAt(t, w.id).Sub(killed); gap < liveTiming("confirmGap") {
			t.Errorf("worker %s was forgotten %v after the kill, sooner than the confirmGap", w.id, gap)
		}
	}
	dead := []*outageWorker{reaper, other}
	for _, c := range []struct {
		spec  playback.Spec
		lease time.Time
	}{{specA, leaseA}, {specB, leaseB}} {
		if early, who := s.replacedBefore(t, c.spec.Hash(), dead, c.lease); !early {
			t.Errorf("worker %s took the job of %s after the lease (%v) it had at the kill would have run out", who, c.spec.Hash(), c.lease)
		}
	}
}

// The role does not rest with a worker that cannot do it: with the reaper
// stopped (its connections standing), its looks unanswered, or its own lock ended
// within the hold, a worker that holds a trusted lock forgets a worker killed
// meanwhile, and its job is claimed before its lease would have run out; the
// reaper itself is not forgotten and its leases are not cut.
func TestARoleOutOfAWorkersHandsIsDoneByAnotherAndTheImpairedReaperIsNotForgotten(t *testing.T) {
	impairments := map[string]struct {
		apply func(t *testing.T, s *stack, reaper *outageWorker) (undo func())
	}{
		"stopped with its connections standing": {func(t *testing.T, s *stack, reaper *outageWorker) func() {
			reaper.cmd.Process.Signal(syscall.SIGSTOP)                   //nolint:errcheck
			return func() { reaper.cmd.Process.Signal(syscall.SIGCONT) } //nolint:errcheck
		}},
		"its looks unanswered": {func(t *testing.T, s *stack, reaper *outageWorker) func() {
			// A round trip of 2 s is past the look's bound (half a second) and inside the keeper's.
			for _, p := range []interface{ Delay(time.Duration) }{reaper.db, reaper.live, reaper.keep} {
				p.Delay(2 * time.Second)
			}
			return func() {
				for _, p := range []interface{ Delay(time.Duration) }{reaper.db, reaper.live, reaper.keep} {
					p.Delay(0)
				}
			}
		}},
		// The reaper's lock backend is ended again every 2 s until the check is over, so that its hold (10 s)
		// never runs out: a build that leaves the role with it through its hold never forgets the victim, and
		// the job is not taken before its lease. A single ending would pass such a build, the victim being
		// forgotten about a hold late and still inside the 21 s or more of lease that is left.
		"its own lock ended within the hold": {func(t *testing.T, s *stack, reaper *outageWorker) func() {
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				for {
					s.r.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_locks
						WHERE locktype = 'advisory' AND classid = `+livenessClass+` AND objsubid = 2 AND objid::bigint = hashtext($1)::bigint & 4294967295`, reaper.id) //nolint:errcheck
					select {
					case <-stop:
						return
					case <-time.After(2 * time.Second):
					}
				}
			}()
			return func() { close(stop); <-done }
		}},
	}
	for name, impairment := range impairments {
		t.Run(name, func(t *testing.T) {
			slowAt(t, "0.5")
			s := newStack(t, 3, thirtySecondLease...)
			reaper := s.theReaper(t)
			others := without(s.workers, reaper)
			victim, third := others[0], others[1]
			specR, specV := s.specN(0), s.specN(1)
			s.giveAJob(t, reaper, specR, psid(0), others...)
			v := s.giveAJob(t, victim, specV, psid(1), reaper, third)
			waitFor(t, "the victim's viewer to have a segment", 60*time.Second, func() bool { return v.got.Load() >= 1 })
			reaperLease := s.leaseOf(t, specR.Hash())
			nodes := s.watchNodes()

			undo := impairment.apply(t, s, reaper)
			defer undo()
			lease := s.leaseOf(t, specV.Hash()) // Before the kill: the reap cuts it after it.
			killAll(victim)
			s.forgottenAt(t, victim.id)
			if early, who := s.replacedBefore(t, specV.Hash(), []*outageWorker{victim}, lease); !early {
				t.Errorf("worker %s took the killed worker's job after the lease (%v) it had at the kill would have run out", who, lease)
			}
			for _, id := range nodes.forgotten() {
				if id == reaper.id {
					t.Errorf("the impaired reaper %s was forgotten", id)
				}
			}
			if now := s.leaseOf(t, specR.Hash()); now.Before(reaperLease) {
				t.Errorf("the impaired reaper's lease was cut from %v to %v", reaperLease, now)
			}
		})
	}
}

// A worker that is not the reaper still learns in time that it was forgotten:
// with its lock gone while its connection stands, or forgotten while it could
// not lock again, it finds its row gone and stops its ffmpeg before a
// replacement's starts; one whose lock only ended, its row there, goes on.
func TestAWorkerThatIsNotTheReaperLearnsItWasForgottenAndStopsItsFFmpegBeforeAReplacementStarts(t *testing.T) {
	for _, name := range []string{"its lock gone, its connection standing", "its lock gone and no new lock to be had"} {
		t.Run(name, func(t *testing.T) {
			overlaps := ffmpegShim(t, "0.5")
			s := newStack(t, 3)
			reaper := s.theReaper(t)
			others := without(s.workers, reaper)
			victim := others[0]
			spec := s.specN(0)
			loop := s.giveAJob(t, victim, spec, psid(0), reaper, others[1])
			waitFor(t, "the viewer to have a segment", 60*time.Second, func() bool { return loop.got.Load() >= 1 })
			first := s.r.count(t, "SELECT max(id) FROM transcode_jobs")

			victim.live.CutServers()
			if name != "its lock gone, its connection standing" {
				// It cannot lock again, so its row stays gone: the check can see it go.
				victim.live.Refuse(true)
				s.forgottenAt(t, victim.id)
			}
			waitFor(t, "the victim's ffmpeg to be gone", 10*time.Second, func() bool {
				_, ok := s.r.ffmpegOf(victim)
				return !ok
			})
			// The viewer's next request starts the job again on a worker that holds its lock.
			before := loop.got.Load()
			waitFor(t, "the viewer to be served by another job", 90*time.Second, func() bool {
				return loop.got.Load() > before+1 && s.r.count(t, "SELECT max(id) FROM transcode_jobs") > first
			})
			if got := overlaps(); len(got) > 0 {
				t.Errorf("an ffmpeg started while another of the spec still ran: %v", got)
			}
			if name == "its lock gone, its connection standing" {
				// It relocks, so it may rightly claim the viewer's replacement job: what matters is that
				// the job it was forgotten on no longer runs.
				if n := s.r.count(t, "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND id <= $2 AND state = 'running'", spec.Hash(), first); n != 0 {
					t.Errorf("the job the worker was forgotten on (id <= %d) still runs", first)
				}
			} else if s.jobOf(spec.Hash()) == victim.id {
				t.Errorf("the job is still the forgotten worker's")
			}
			victim.live.Refuse(false)
		})
	}

	t.Run("a worker whose lock only ended goes on with its jobs", func(t *testing.T) {
		slowAt(t, "0.5")
		s := newStack(t, 3)
		reaper := s.theReaper(t)
		others := without(s.workers, reaper)
		victim := others[0]
		spec := s.specN(0)
		loop := s.giveAJob(t, victim, spec, psid(0), reaper, others[1])
		waitFor(t, "the viewer to have a segment", 60*time.Second, func() bool { return loop.got.Load() >= 1 })
		pid, ok := s.r.ffmpegOf(victim)
		if !ok {
			t.Fatal("the victim runs no ffmpeg")
		}
		job := s.r.count(t, "SELECT max(id) FROM transcode_jobs")
		old := s.lockPids()[victim.id]
		s.r.terminate(t, victim.id)
		waitFor(t, "the victim to hold its lock again", 30*time.Second, func() bool { p := s.lockPids()[victim.id]; return p != 0 && p != old })
		time.Sleep(3 * time.Second)
		if again, ok := s.r.ffmpegOf(victim); !ok || again.Pid != pid.Pid || again.Start != pid.Start {
			t.Errorf("the worker's ffmpeg was %v and is now %v (running %v): its job did not go on", pid, again, ok)
		}
		if s.r.count(t, "SELECT max(id) FROM transcode_jobs") != job || s.jobOf(spec.Hash()) != victim.id {
			t.Errorf("a worker that only took its lock again lost its job")
		}
	})
}

// ffmpegShim puts an ffmpeg on the PATH that reads at rate times real time and
// notes, when it starts, every earlier ffmpeg of the shim that is still alive.
func ffmpegShim(t *testing.T, rate string) (overlaps func() []string) {
	t.Helper()
	slowAt(t, rate)
	next, err := exec.LookPath("ffmpeg") // The slow shim, which this one runs by its path: both exec, so the pid is the ffmpeg's.
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
for p in $(cat %[1]s/pids 2>/dev/null); do kill -0 "$p" 2>/dev/null && echo "$p was alive when $$ started" >> %[1]s/overlap; done
echo $$ >> %[1]s/pids
exec %[2]s "$@"
`, dir, next)
	if err := os.WriteFile(dir+"/ffmpeg", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, _ := os.ReadFile(dir + "/overlap")
		return strings.Fields(strings.ReplaceAll(string(raw), " was alive when ", "|"))
	}
}

// releaseBefore is main as it was when JS-030 began: the release whose worker,
// taking its lock and reaping by itself, runs beside the new ones. Its test
// binary is built from `git archive` of it, so the worker is that release's own.
const releaseBefore = "a3d5945"

// oldWorkerBinary builds the transcode package's integration test binary of the
// release before, once for the package; it runs the worker process of that
// release when JELLYSCALE_TEST_WORKER is set.
func oldWorkerBinary(t *testing.T) string {
	t.Helper()
	// test/ffmpeg61.sh's container has no git and mounts only the checkout, not the worktree's git
	// directory, so QA's CI-shaped run gives the binary, built beforehand by test/release-before.sh
	// (CGO off, so that a host's libc does not matter to the container's).
	if path := os.Getenv("JELLYSCALE_TEST_RELEASE_BEFORE"); path != "" {
		return path
	}
	oldBuild.Do(func() {
		dir, err := os.MkdirTemp("", "jellyscale-release-before-")
		if err != nil {
			oldErr = err
			return
		}
		oldDir = dir
		archive := exec.Command("sh", "-c", fmt.Sprintf("git -C ../.. archive %s | tar -x -C %s", releaseBefore, dir))
		if out, err := archive.CombinedOutput(); err != nil {
			oldErr = fmt.Errorf("git archive %s: %v\n%s", releaseBefore, err, out)
			return
		}
		build := exec.Command("go", "test", "-tags", "integration", "-c", "-o", filepath.Join(dir, "old.test"), "./internal/transcode")
		build.Dir = dir
		if out, err := build.CombinedOutput(); err != nil {
			oldErr = fmt.Errorf("building the release before: %v\n%s", err, out)
		}
	})
	if oldErr != nil {
		t.Fatal(oldErr)
	}
	t.Cleanup(func() {}) // The directory goes with the test run's TMPDIR (test/integration.sh removes it).
	return filepath.Join(oldDir, "old.test")
}

var (
	oldBuild sync.Once
	oldDir   string
	oldErr   error
)

// addOldWorker is stack.addWorker for a worker of the release before.
func (s *stack) addOldWorker(env ...string) *outageWorker {
	t := s.t
	t.Helper()
	w := &outageWorker{id: uuid.NewString(), db: testenv.NewDBProxy(t, s.r.dsn), live: testenv.NewDBProxy(t, s.r.dsn), keep: testenv.NewDBProxy(t, s.r.dsn), reaped: make(chan struct{})}
	cmd := exec.Command(oldWorkerBinary(t))
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_WORKER="+w.id, "JELLYSCALE_TEST_SEGMENTS="+s.seg, "JELLYSCALE_TEST_SCRATCH="+s.r.scratch,
		"JELLYSCALE_TEST_DATABASE_URL="+w.db.DSN()+"&default_query_exec_mode=exec", "JELLYSCALE_TEST_LIVENESS="+w.live.Addr(), "JELLYSCALE_TEST_KEEPER="+w.keep.Addr(),
		"JELLYSCALE_TEST_SLOTS=1", "JELLYSCALE_TEST_NO_HELPERS=1", "JELLYSCALE_TEST_PUTLOG="+s.putlog, "JELLYSCALE_TEST_CORES=0.81")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = &w.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.cmd = cmd
	go func() { cmd.Wait(); close(w.reaped) }() //nolint:errcheck
	t.Cleanup(func() { w.kill() })
	s.workers = append(s.workers, w)
	waitFor(t, "the old worker "+w.id+" to hold its lock and register", 30*time.Second, func() bool { return s.r.locked(w.id) })
	return w
}

// A worker of the release before, with its row and its lock as today, taking no
// part in choosing the reaper and reaping by itself, beside the new ones: it is
// never forgotten while it holds its lock, its own reaping forgets no live new
// worker, and it is forgotten on the same conditions when it is killed, its job
// claimed before its lease would have run out.
func TestAWorkerOfTheReleaseBeforeBesideTheNewOnesIsNeverForgottenWhileItLivesAndIsWhenItIsKilled(t *testing.T) {
	slowAt(t, "0.5")
	s := newStack(t, 2, thirtySecondLease...)
	old := s.addOldWorker(thirtySecondLease...)
	nodes := s.watchNodes()
	time.Sleep(40 * liveTiming("lookPeriod")) // Many looks of every worker, the old one's among them.
	if gone := nodes.forgotten(); len(gone) > 0 {
		t.Fatalf("live workers were forgotten with a worker of the release before beside the new ones: %v", gone)
	}

	// It holds a job and is killed: forgotten on today's conditions, replaced before its lease.
	spec := s.specN(0)
	loop := s.giveAJob(t, old, spec, psid(0), without(s.workers, old)...)
	waitFor(t, "the viewer to have a segment", 60*time.Second, func() bool { return loop.got.Load() >= 1 })
	lease := s.leaseOf(t, spec.Hash()) // Before the kill: the reap cuts it after it.
	killed := killAll(old)
	if gap := s.forgottenAt(t, old.id).Sub(killed); gap < liveTiming("confirmGap") {
		t.Errorf("the old worker was forgotten %v after its death, sooner than two looks the confirmGap apart", gap)
	}
	if early, who := s.replacedBefore(t, spec.Hash(), []*outageWorker{old}, lease); !early {
		t.Errorf("worker %s took the old worker's job after the lease (%v) it had at the kill would have run out", who, lease)
	}
	for _, id := range nodes.forgotten() {
		if id != old.id {
			t.Errorf("worker %s was forgotten besides the one that was killed", id)
		}
	}
}
