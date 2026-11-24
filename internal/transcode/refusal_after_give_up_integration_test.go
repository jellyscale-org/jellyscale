//go:build integration

package transcode

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/db"
	"jellyscale/internal/playback"
	"jellyscale/internal/testenv"
)

// JS-173: a viewer is never refused for the cores of a job that has just been given up.
//
// Two sightings: lease_keeper_integration_test.go:313 (PlaybackInfo refused with "the most any worker has free is
// 0.0" right after a held worker's job was given up, 1 run of 3 in JS-167's CI-shaped run) and
// fenced_put_integration_test.go:188 (queue run 45045804305: "no transcode worker is running" after a stopped
// worker's job was given up). Each of those tests asked as an anonymous viewer, after waiting for the job's row
// to end, so the moment it asked at was whatever the race made it. These checks force the moment.
//
// The worker is a real process at production's lease (10 s) and heartbeat (3 s), with cores for one re-encode.
// The moment is the answer of its write that ends the job's row: testenv.DBProxy.OnStatement runs a step after
// Postgres has run the statement and before the worker is answered, so the row is no longer running and nothing
// the worker does after the write has happened. The question PlaybackInfo asks (Jobs.Refusal) is asked there,
// once, never again, of a replica of its own, as three viewers (the given-up job's own, another viewer of the
// same film and output, a viewer of another film). The viewer whose segment request waits on the job has its
// replica held, so that its request still waits and queues no replacement meanwhile: what is checked is the
// worker's end of the job alone. A hold on the replica could hide that a replacement is queued at once, which
// the checks of the replacement below (TestAGivenUpJobsReplacementStillHoldsTheCoresAgainstAnotherFilm) look at.
// What a lease of 10 s hides is nothing: the checks run at it; the stop past it is 11 s and no assertion reads a duration.
//
// The three askers each have a replica of their own (a pool behind a proxy of its own) and ask together: each
// holds its first read of the workers where it is until all three have made it (a meeting), so no asker is
// answered from an order another one's wait made. What the order is made of is held in Postgres or at a proxy
// and never decided by who is slow: in the stopped worker's check the worker's refresh of its row's seen_at is
// kept at its proxy until the three have read the workers (HoldStatement), where a lock on the row would
// have kept a fix that makes the job's end write mark the worker seen from ever ending the row.
//
// Wrong builds the checks fail: counting a cancelled job's cores (the cores check, and the replacement checks
// for the other film's viewer), not counting a worker that has just ended its job (the sighting checks'
// NoWorkerRunning, NoWorkerWithFreeCores or not), counting every lock-held worker whatever its seen_at
// (TestAViewerIsRefusedForNoWorkerRunningWhileTheOnlyWorkerIsStoppedPastTheLease), and not counting jobs or
// workers at all (the two guards).

// isTheJobEnd matches the write with which a worker records how a job ended (worker.go).
func isTheJobEnd(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "UPDATE transcode_jobs SET state = $2") && strings.Contains(s, "ended_at = now()")
}

// isTheJobEndNth matches the nth such write on a connection.
func isTheJobEndNth(nth int) func([]byte) bool {
	var seen atomic.Int32
	return func(sent []byte) bool { return isTheJobEnd(sent) && int(seen.Add(1)) == nth }
}

// isTheKeeperCancel matches the lease keeper's write of a job it gave up as stalled.
func isTheKeeperCancel(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "UPDATE transcode_jobs SET state = 'cancelled', error = NULLIF($2, '')")
}

// isTheCountOfWorkers matches the statements that read the workers' rows and when they were seen: the counting of
// workers and of their cores, whatever the SQL around it.
func isTheCountOfWorkers(sent []byte) bool {
	s := string(sent)
	return strings.Contains(s, "transcode_nodes") && strings.Contains(s, "seen_at")
}

// isTheSeenRefresh matches the worker's refresh of its row's seen_at (liveness.go).
func isTheSeenRefresh(sent []byte) bool {
	return strings.Contains(string(sent), "UPDATE transcode_nodes SET seen_at = now() WHERE id")
}

// meeting lets n askers go on together: each arrives once, and wait returns when all have (or a hang is over).
type meeting struct {
	left atomic.Int32
	open chan struct{}
}

func newMeeting(n int) *meeting {
	m := &meeting{open: make(chan struct{})}
	m.left.Store(int32(n))
	return m
}

func (m *meeting) arrive() {
	if m.left.Add(-1) == 0 {
		close(m.open)
	}
}

func (m *meeting) wait() {
	select {
	case <-m.open:
	case <-time.After(keeperHang):
	}
}

// replicaOf is a replica of its own for PlaybackInfo to be asked of: a pool behind a proxy of its own, which
// sends each statement's text.
func (r *rig) replicaOf(t *testing.T) (*Jobs, *testenv.DBProxy) {
	t.Helper()
	proxy := testenv.NewDBProxy(t, r.dsn)
	pool, err := db.Open(context.Background(), proxy.DSN()+"&default_query_exec_mode=exec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Jobs{Pool: pool, Segments: r.segments, QueueTimeout: 5 * time.Second}, proxy
}

// asker is a viewer PlaybackInfo is asked for.
type asker struct {
	who          string
	spec         playback.Spec
	user, device string
}

// askers are the three viewers of the criteria. The job's own has the spec, user and device the job's viewer
// has (rig.viewer); the other two are neither.
func (r *rig) askers() []asker {
	other := r.spec
	other.Item = db.CompactID(uuid.NewString())
	other.Source = other.Item
	return []asker{
		{"the given-up job's own viewer", r.spec, r.user, "device"},
		{"another viewer of the same film and output", r.spec, uuid.NewString(), "another device"},
		{"a viewer of another film", other, uuid.NewString(), "a third device"},
	}
}

type answer struct {
	who, reason, text string
	err               error
}

func (a answer) admitted() bool { return a.err == nil && a.reason == "" && a.text == "" }

// giveUpScene is a stack with one job of the rig's spec running for a viewer, whose segment request waits on it.
type giveUpScene struct {
	t           *testing.T
	s           *stack
	r           *rig
	victim      *outageWorker // The job's worker.
	other       *outageWorker // A second worker, idle, or nil.
	job         int64
	asker       *Jobs   // A replica PlaybackInfo is asked of: not the one the viewer waits on.
	through     []*Jobs // A replica of its own for each of the askers, behind proxies.
	proxies     []*testenv.DBProxy
	viewer      atomic.Bool
	askers      []asker
	once        sync.Once
	mu          sync.Mutex
	answers     []answer
	rowAt       string // The job's state at the moment.
	waiting     bool   // The viewer's segment request was still waiting at the moment.
	viewerEnded bool
	letRefresh  func() // Lets the held refresh of the worker's seen_at go on, once armed.
}

// newGiveUpScene starts the stack and the viewer, whose request waits for the first segment of a job that runs
// with its ffmpeg. env is the workers' (a slow store, for the job whose work stalls).
func newGiveUpScene(t *testing.T, workers int, env ...string) *giveUpScene {
	t.Helper()
	slowAt(t, "2")
	s := newStack(t, workers, env...)
	sc := &giveUpScene{t: t, s: s, r: s.r, victim: s.workers[0], asker: s.r.jobs(t), askers: s.r.askers()}
	for range sc.askers {
		j, p := s.r.replicaOf(t)
		sc.through, sc.proxies = append(sc.through, j), append(sc.proxies, p)
	}
	t.Cleanup(s.replica.Release)
	if workers == 2 {
		// The job goes to the worker that is not the reaper: the reaper is what forgets the other (path 4).
		reaper := s.theReaper(t)
		sc.victim, sc.other = without(s.workers, reaper)[0], reaper
		signalAll([]*outageWorker{sc.other}, syscall.SIGSTOP) // Its connections stand, so nobody forgets it.
	}
	if len(env) > 0 {
		setPuts(t, env[0][strings.Index(env[0], "=")+1:], sc.victim, "stall all 0s") // Every Put waits for its context.
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		s.fetch(ctx, s.jobs, sc.r.spec, psid(0), 0) //nolint:errcheck
		sc.viewer.Store(true)
	}()
	waitFor(t, "the job to run on the worker the check chose", keeperHang, func() bool { return sc.r.owner(t) == sc.victim.id })
	waitFor(t, "the job's ffmpeg", keeperHang, func() bool { _, ok := sc.r.ffmpegOf(sc.victim); return ok })
	sc.job = sc.r.jobOf(t, sc.r.hash)
	if sc.other != nil {
		signalAll([]*outageWorker{sc.other}, syscall.SIGCONT)
	}
	// The viewer's replica answers nothing from here (before the first segment, which takes seconds at the shim's
	// rate), so its segment request still waits at the moment and queues no replacement meanwhile.
	s.replica.Hold()
	return sc
}

// freeTheViewer lets the viewer's replica answer again, as the checks of what its request does need.
func (sc *giveUpScene) freeTheViewer() { sc.s.replica.Release() }

// ask is the question, once, asked by the three viewers together, each of its own replica. It runs on the proxy's
// goroutine, so it only records.
func (sc *giveUpScene) ask() {
	sc.once.Do(func() {
		defer sc.releaseTheRefresh()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var state string
		sc.r.pool.QueryRow(ctx, "SELECT state FROM transcode_jobs WHERE id = $1", sc.job).Scan(&state) //nolint:errcheck
		sc.mu.Lock()
		sc.rowAt, sc.waiting = state, !sc.viewer.Load()
		sc.viewerEnded = !sc.waiting
		sc.mu.Unlock()
		m := newMeeting(len(sc.askers))
		answers := make([]answer, len(sc.askers))
		var wg sync.WaitGroup
		for i, a := range sc.askers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var once sync.Once
				arrive := func() { once.Do(m.arrive) }
				// An asker's first read of the workers waits for the other two, and then lets the held refresh go on.
				sc.proxies[i].OnStatement(isTheCountOfWorkers, nil, func() { arrive(); m.wait(); sc.releaseTheRefresh() })
				reason, text, err := sc.through[i].Refusal(ctx, a.spec, a.user, a.device)
				arrive() // An asker admitted before it counted anything does not hold the others.
				answers[i] = answer{a.who, reason, text, err}
			}()
		}
		wg.Wait()
		sc.mu.Lock()
		sc.answers = answers
		sc.mu.Unlock()
	})
}

// holdTheRefresh keeps the victim's next refresh of its row's seen_at at its proxy until the three askers have
// read the workers, which is production's order for a worker that was stopped: the end of its job's row first,
// its refresh at the next beat. Armed only at the stop, so the worker was seen until then.
func (sc *giveUpScene) holdTheRefresh() {
	release := sc.victim.db.HoldStatement(isTheSeenRefresh)
	sc.mu.Lock()
	sc.letRefresh = release
	sc.mu.Unlock()
	sc.t.Cleanup(release)
}

func (sc *giveUpScene) releaseTheRefresh() {
	sc.mu.Lock()
	release := sc.letRefresh
	sc.mu.Unlock()
	if release != nil {
		release()
	}
}

// judge says what the question was answered at the moment. wantRowEnded is false for a moment that is a lease
// lapsed with the row never written (a killed worker), true where a worker ended the row. all asks the three
// viewers; else only the first, the job's own, was to be admitted.
func (sc *giveUpScene) judge(wantRowEnded, all bool) {
	t := sc.t
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.answers) == 0 {
		t.Fatal("the question was never asked: the moment did not come")
	}
	if sc.viewerEnded {
		t.Fatalf("harness error: the viewer's segment request was served before the moment, so it did not wait on the job")
	}
	if wantRowEnded && sc.rowAt == "running" {
		t.Fatalf("harness error: the job's row was still running at the moment the question was asked")
	}
	for i, a := range sc.answers {
		t.Logf("at the moment (job %d %s, the viewer's segment request still waiting: %v) %s: reason %q, text %q, err %v", sc.job, sc.rowAt, sc.waiting, a.who, a.reason, a.text, a.err)
		if i > 0 && !all {
			continue
		}
		if !a.admitted() {
			t.Errorf("%s was refused right after the job was given up: reason %q (%s) %v", a.who, a.reason, a.text, a.err)
		}
	}
}

// Each way a job is given up: a viewer asking right after is admitted, as PlaybackInfo must, for what is really
// held or gone is the only reason to refuse. The first two are the two sightings of JS-173 and are asked as the
// three viewers; the others hold today and must keep holding.
func TestAViewerIsNotRefusedAtTheMomentAJobIsGivenUp(t *testing.T) {
	cases := []struct {
		name           string
		workers        int
		slowStore, all bool // slowStore: every Put of the worker waits for its context, so the job's loop does not turn.
		rowEnded       bool
		give           func(t *testing.T, sc *giveUpScene)
	}{
		{"a renewal unanswered while the worker's connections are held", 1, false, true, true, func(t *testing.T, sc *giveUpScene) {
			ended := sc.victim.db.OnStatement(isTheJobEnd, nil, sc.ask)
			sc.victim.hold()
			waitFor(t, "the worker to log that it gave the job up for its lease", keeperHang, func() bool { return len(interruptedLines(sc.victim)) > 0 })
			sc.victim.release()
			waitForClosed(t, "the worker's write that ends the job's row", ended)
			theReasonIsTheLease(t, sc.victim, 1)
		}},
		{"the worker stopped past the lease and continued", 1, false, true, true, func(t *testing.T, sc *giveUpScene) {
			// As fenced_put_integration_test.go's "stopped after the answer, a later renewal": the answer to the
			// fifth renewal is held, the worker stopped for longer than the lease (11 s against 10 s), then continued.
			// Its refresh of its row's seen_at is kept at its proxy from the stop until the three askers have read
			// the workers: production's order, where the job's end comes first and the refresh at the next beat (3 s).
			ended := sc.victim.db.OnStatement(isTheJobEnd, nil, sc.ask)
			stopped := sc.victim.keep.OnStatement(isTheRenewalNth(5), nil, func() {
				sc.holdTheRefresh()
				sc.victim.cmd.Process.Signal(syscall.SIGSTOP) //nolint:errcheck
				time.Sleep(11 * time.Second)
				sc.victim.cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
			})
			waitForClosed(t, "the worker to be stopped and continued", stopped)
			waitForClosed(t, "the worker's write that ends the job's row", ended)
		}},
		{"the job's work stalled for a lease", 1, true, false, true, func(t *testing.T, sc *giveUpScene) {
			ended := sc.victim.keep.OnStatement(isTheKeeperCancel, nil, sc.ask)
			waitForClosed(t, "the keeper's write that ends the stalled job's row", ended)
		}},
		{"the lease taken away while the worker lives, forgotten by the reaper", 2, false, false, true, func(t *testing.T, sc *giveUpScene) {
			ended := sc.victim.db.OnStatement(isTheJobEnd, nil, sc.ask)
			sc.victim.live.CutServers() // Its lock is gone and its connection stands.
			sc.victim.live.Refuse(true) // And it cannot take it again, so the reaper's forgetting stands.
			t.Cleanup(func() { sc.victim.live.Refuse(false) })
			waitForClosed(t, "the forgotten worker's write that ends the job's row", ended)
		}},
		{"the worker killed beside a second worker", 2, false, false, false, func(t *testing.T, sc *giveUpScene) {
			sc.victim.kill()
			// Nothing writes the job's row: the moment is its lease having lapsed, by Postgres's clock.
			waitFor(t, "the dead worker's job to have a lease that has lapsed", keeperHang, func() bool { return sc.r.leaseRunOut(t, sc.job) })
			sc.ask()
		}},
		{"the worker shut down beside a second worker", 2, false, false, true, func(t *testing.T, sc *giveUpScene) {
			ended := sc.victim.db.OnStatement(isTheJobEnd, nil, sc.ask)
			sc.victim.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
			waitForClosed(t, "the worker's write that ends the job's row", ended)
		}},
		{"the viewer's own Stop", 1, false, false, true, func(t *testing.T, sc *giveUpScene) {
			// Stop writes the row itself; the moment is its return, with the worker not yet answered for it.
			if err := sc.asker.Stop(context.Background(), sc.r.user, psid(0), "device", ""); err != nil {
				t.Fatal(err)
			}
			sc.ask()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var env []string
			if c.slowStore {
				env = []string{slowPutEnv(t.TempDir())}
			}
			sc := newGiveUpScene(t, c.workers, env...)
			c.give(t, sc)
			sc.judge(c.rowEnded, c.all)
		})
	}
}

// waitForClosed waits for a hook's channel, which is closed once its step has run.
func waitForClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(keeperHang):
		t.Fatalf("timed out after %v waiting for %s", keeperHang, what)
	}
}

// The question is asked at the moment the held worker's write has ended the job's row, and the viewer's segment
// request, still waiting on its replica, queues its replacement while the question is being answered: after the
// question has seen that no job of the spec is live, before it counts the cores. Each of the three askers has a
// replica of its own that holds its first read of the workers until all three have made it (a meeting, so that
// whichever asker is slow is not answered after the replacement existed for it alone); then the viewer's replica
// answers, and the three go on once the replacement is in the database. The viewer's request queues it by itself,
// the test waits for that state and for nothing else.
// A replacement is a job of the viewer's own spec: it is the job the same film's viewers join and the job's own
// viewer is not kept waiting behind. A viewer of another film is rightly refused for the cores it holds, and is
// as rightly admitted by a build that answers all three from one snapshot taken before it existed: both are
// consistent orders, so that answer is logged and not asserted here (the next check asserts it once the
// replacement is queued before the question).
func TestAViewerIsNotRefusedWhenTheGivenUpJobsReplacementIsQueuedAsTheQuestionIsAsked(t *testing.T) {
	sc := newGiveUpScene(t, 1)
	r, old := sc.r, sc.job
	var mu sync.Mutex
	got := map[string]answer{}
	var fired [3]atomic.Bool
	var queued atomic.Bool
	asking := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		m := newMeeting(len(sc.askers))
		var wg sync.WaitGroup
		for i, a := range sc.askers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var once sync.Once
				arrive := func() { once.Do(m.arrive) }
				sc.proxies[i].OnStatement(isTheCountOfWorkers, func() {
					fired[i].Store(true)
					arrive()
					m.wait()
					sc.freeTheViewer() // Only now: the question has seen no live job of the spec.
					for deadline := time.Now().Add(keeperHang); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						var n int
						r.pool.QueryRow(ctx, "SELECT count(*) FROM transcode_jobs WHERE id > $1 AND spec_hash = $2 AND state IN ('queued', 'running')", old, r.hash).Scan(&n) //nolint:errcheck
						if n > 0 {
							queued.Store(true)
							return
						}
					}
				}, nil)
				reason, text, err := sc.through[i].Refusal(ctx, a.spec, a.user, a.device)
				arrive()
				mu.Lock()
				got[a.who] = answer{a.who, reason, text, err}
				mu.Unlock()
			}()
		}
		wg.Wait()
	}
	ended := sc.victim.db.OnStatement(isTheJobEnd, nil, asking)
	sc.victim.hold()
	waitFor(t, "the worker to log that it gave the job up for its lease", keeperHang, func() bool { return len(interruptedLines(sc.victim)) > 0 })
	sc.victim.release()
	waitForClosed(t, "the question asked at the moment of the worker's write that ends the job's row", ended)
	// The viewer of another film is always counted for, so its first read of the workers is where the viewer was freed.
	if !fired[2].Load() || !queued.Load() {
		t.Fatalf("harness error: the viewer of another film read the workers: %v, the replacement was queued as it did: %v", fired[2].Load(), queued.Load())
	}
	for i, a := range sc.askers {
		an := got[a.who]
		t.Logf("with the replacement queued as the question was asked, %s: reason %q, text %q, err %v", a.who, an.reason, an.text, an.err)
		if i < 2 && !an.admitted() {
			t.Errorf("%s was refused for the cores of a replacement it is, or would join: reason %q (%s) %v", a.who, an.reason, an.text, an.err)
		}
	}
}

// What is really held still refuses: once the given-up job's viewer has caused a replacement to be queued, it
// holds the worker's only cores, and a viewer of another film is told so, with the cores as the reason, while
// the viewers of the replacement's own spec join it. A build that stops counting jobs fails this one.
func TestAGivenUpJobsReplacementStillHoldsTheCoresAgainstAnotherFilm(t *testing.T) {
	sc := newGiveUpScene(t, 1)
	old := sc.job
	// The viewer's replica answers: its request, finding the job ended, queues the replacement.
	sc.freeTheViewer()
	sc.victim.hold()
	waitFor(t, "the worker to log that it gave the job up for its lease", keeperHang, func() bool { return len(interruptedLines(sc.victim)) > 0 })
	sc.victim.release()
	waitFor(t, "the viewer's replacement of the given-up job to be queued or running", keeperHang, func() bool {
		return sc.r.count(t, "SELECT count(*) FROM transcode_jobs WHERE id > $1 AND spec_hash = $2 AND state IN ('queued', 'running')", old, sc.r.hash) > 0
	})
	for i, a := range sc.askers {
		reason, text, err := sc.asker.Refusal(context.Background(), a.spec, a.user, a.device)
		t.Logf("with the replacement queued or running, %s: reason %q, text %q, err %v", a.who, reason, text, err)
		switch {
		case i < 2 && (err != nil || reason != ""):
			// The replacement is the job the own viewer would have, and the one the same film's viewers join.
			t.Errorf("%s was refused though a live job of its spec is there to join: reason %q (%s) %v", a.who, reason, text, err)
		case i == 2 && (err != nil || reason != playback.NoWorkerWithFreeCores):
			t.Errorf("with the replacement holding the worker's only cores, %s was answered reason %q (%s) %v, want %q", a.who, reason, text, err, playback.NoWorkerWithFreeCores)
		}
	}
}

// Nor is a viewer admitted when the only worker is gone: no worker running is the reason, whatever was given up.
func TestAViewerIsRefusedForNoWorkerRunningWhenTheOnlyWorkerWasKilled(t *testing.T) {
	sc := newGiveUpScene(t, 1)
	sc.victim.kill()
	waitFor(t, "the dead worker's job to have a lease that has lapsed", keeperHang, func() bool { return sc.r.leaseRunOut(t, sc.job) })
	for _, a := range sc.askers {
		reason, text, err := sc.asker.Refusal(context.Background(), a.spec, a.user, a.device)
		if err != nil || reason != playback.NoWorkerRunning {
			t.Errorf("with the only worker killed, %s was answered reason %q (%s) %v, want %q", a.who, reason, text, err, playback.NoWorkerRunning)
		}
	}
}

// A worker that is stopped past the lease and not continued (a frozen machine) keeps its connections and its lock,
// and is not a worker a viewer can be admitted to: nothing has seen it for longer than the count lasts, and a
// stream would never start. No worker running is the reason, for the job's own viewer, another of the same film
// and a viewer of another film. A build that counts every worker whose lock is held, whatever its seen_at, fails
// this one while the sighting check passes it.
func TestAViewerIsRefusedForNoWorkerRunningWhileTheOnlyWorkerIsStoppedPastTheLease(t *testing.T) {
	sc := newGiveUpScene(t, 1)
	var got []answer
	stopped := sc.victim.keep.OnStatement(isTheRenewalNth(5), nil, func() {
		sc.victim.cmd.Process.Signal(syscall.SIGSTOP)                       //nolint:errcheck
		t.Cleanup(func() { sc.victim.cmd.Process.Signal(syscall.SIGCONT) }) //nolint:errcheck
		time.Sleep(11 * time.Second)
		for _, a := range sc.askers {
			reason, text, err := sc.asker.Refusal(context.Background(), a.spec, a.user, a.device)
			got = append(got, answer{a.who, reason, text, err})
		}
		sc.victim.cmd.Process.Signal(syscall.SIGCONT) //nolint:errcheck
	})
	waitForClosed(t, "the worker to be stopped past its lease, asked of, and continued", stopped)
	if len(got) != len(sc.askers) {
		t.Fatalf("harness error: %d viewers asked, want %d", len(got), len(sc.askers))
	}
	for _, a := range got {
		t.Logf("while the only worker is stopped past the lease, %s: reason %q, text %q, err %v", a.who, a.reason, a.text, a.err)
		if a.err != nil || a.reason != playback.NoWorkerRunning {
			t.Errorf("%s was answered reason %q (%s) %v, want %q: the worker has not been seen for longer than the lease", a.who, a.reason, a.text, a.err, playback.NoWorkerRunning)
		}
	}
}

// One worker gives up two jobs at once, and a job that costs both together is admitted at the moment the second
// row is ended. The worker has cores for two re-encodes (0.8 each); the job asked is a 4K-sized one (1.6).
func TestAViewerWhoseJobCostsTwoGivenUpJobsTogetherIsNotRefused(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1, "JELLYSCALE_TEST_CORES=1.7", "JELLYSCALE_TEST_SLOTS=2")
	r, w := s.r, s.workers[0]
	t.Cleanup(w.release)
	t.Cleanup(s.replica.Release)
	specA, specB := r.spec, r.spec
	specB.VideoBitrate = 1_200_000
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.fetch(ctx, s.jobs, specA, psid(0), 0) //nolint:errcheck
	go s.fetch(ctx, s.jobs, specB, psid(1), 0) //nolint:errcheck
	waitFor(t, "both jobs to run on the worker", keeperHang, func() bool {
		return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state = 'running' AND worker = $1", w.id) == 2
	})
	big := r.spec
	big.Item = db.CompactID(uuid.NewString())
	big.Source, big.Height, big.Width = big.Item, 2160, 3840
	if cost := big.Cost(); cost < 1.59 || cost > 1.61 {
		t.Fatalf("the job asked costs %.2f cores, want the 1.6 of both together", cost)
	}
	asker := r.jobs(t)
	var ask sync.Once
	var reason, text string
	var err error
	var running int
	// The two ends go out on connections of their own, so the second to arrive can be answered before the first has
	// committed; it is kept from Postgres until the first has, so that the second is the write that leaves no job running.
	firstEnded := func() {
		for end := time.Now().Add(keeperHang); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			var left int
			r.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_jobs WHERE state = 'running'").Scan(&left) //nolint:errcheck
			if left <= 1 {
				return
			}
		}
	}
	ended := w.db.OnStatement(isTheJobEndNth(2), firstEnded, func() {
		ask.Do(func() {
			r.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_jobs WHERE state = 'running'").Scan(&running) //nolint:errcheck
			reason, text, err = asker.Refusal(context.Background(), big, uuid.NewString(), "a device")
		})
	})
	s.replica.Hold()
	w.hold()
	waitFor(t, "the worker to give both jobs up", keeperHang, func() bool { return len(interruptedLines(w)) >= 2 })
	w.release()
	waitForClosed(t, "the worker's write that ends the second job's row", ended)
	if running != 0 {
		t.Fatalf("harness error: %d jobs still ran at the moment", running)
	}
	if err != nil || reason != "" {
		t.Errorf("a job costing both given-up jobs together was refused right after the second ended: reason %q (%s) %v", reason, text, err)
	}
}
