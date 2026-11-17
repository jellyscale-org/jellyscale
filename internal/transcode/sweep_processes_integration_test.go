//go:build integration

package transcode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
)

// JS-078: sweeps by real processes. A sweeper is the test binary run again
// (TestMain sees JELLYSCALE_TEST_SWEEPER): a worker that opens its own pool and
// stores, sweeps once and exits. Its steps are held by files in a directory the
// test owns, so the interleaving is the test's: a sweeper that has listed the
// store stops there until the test lets it go, and one can be stopped before a
// given delete to be killed there.
//
// Environment: JELLYSCALE_TEST_SEGMENTS_URI (the transcode store),
// JELLYSCALE_TEST_ASSET_URIS (comma-separated asset stores), JELLYSCALE_TEST_BUDGET,
// JELLYSCALE_TEST_SYNC (the directory), JELLYSCALE_TEST_DELAY (a sleep before it
// sweeps, to move its step against the others'), and JELLYSCALE_TEST_HOLD:
// "segments:after-list" or "assets:after-list" (after its listing of that store
// returns, it writes listed-<id> and waits for release-<id>), or
// "segments:delete:N" or "assets:delete:N" (it writes deleting-<id> and waits for
// release-<id> before its Nth delete). It writes done-<id> when its sweep returns.
//
// Two more modes are the workers of a cluster as they run: JELLYSCALE_TEST_LOOP
// (a period) runs the worker's own sweepLoop at that SweepEvery, writes
// ready-<id> as it starts and lists-<id> (how often it has listed the transcode
// store) after each listing, and ends on SIGTERM; JELLYSCALE_TEST_COLLECT makes
// a worker that never sweeps write metric-<id>, what its Collect says the
// transcode store holds ("none" when it does not say), and exit.

func sweeperProcess(id string) {
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "sweeper %s: %s: %v\n", id, what, err)
		os.Exit(1)
	}
	ctx := context.Background()
	dir := os.Getenv("JELLYSCALE_TEST_SYNC")
	dsn, err := dsnWith(os.Getenv("JELLYSCALE_TEST_DATABASE_URL"), "application_name", "sweeper-"+id)
	if err != nil {
		fail("database url", err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		fail("database", err)
	}
	mark := func(name string, text ...string) { // Whole or not there: a reader never sees half of it.
		temp := filepath.Join(dir, "."+name)
		if err := os.WriteFile(temp, []byte(strings.Join(text, "")), 0o644); err != nil {
			fail("mark "+name, err)
		}
		if err := os.Rename(temp, filepath.Join(dir, name)); err != nil {
			fail("mark "+name, err)
		}
	}
	await := func(name string) {
		for deadline := time.Now().Add(3 * time.Minute); !fileExists(filepath.Join(dir, name)); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				fail("waiting for "+name, fmt.Errorf("it never came"))
			}
		}
	}
	hold := func(spy *spyStore, mode string) {
		switch {
		case mode == "after-list":
			var once sync.Once
			spy.afterList = func() {
				once.Do(func() {
					mark("listed-" + id)
					await("release-" + id)
				})
			}
		case strings.HasPrefix(mode, "delete:"):
			n, _ := strconv.Atoi(strings.TrimPrefix(mode, "delete:"))
			var deletes atomic.Int64
			spy.beforeDelete = func(string) {
				if int(deletes.Add(1)) == n {
					mark("deleting-" + id)
					await("release-" + id)
				}
			}
		}
	}
	which, mode, _ := strings.Cut(os.Getenv("JELLYSCALE_TEST_HOLD"), ":")
	segments, err := store.Open(os.Getenv("JELLYSCALE_TEST_SEGMENTS_URI"))
	if err != nil {
		fail("transcode store", err)
	}
	segmentSpy := &spyStore{Store: segments}
	if which == "segments" {
		hold(segmentSpy, mode)
	}
	var media store.Cache
	for _, uri := range strings.Split(os.Getenv("JELLYSCALE_TEST_ASSET_URIS"), ",") {
		if uri == "" {
			continue
		}
		opened, err := store.Open(uri)
		if err != nil {
			fail("asset store", err)
		}
		spy := &spyStore{Store: opened}
		if which == "assets" {
			hold(spy, mode)
		}
		media.Put(uri, spy)
	}
	budget, _ := strconv.ParseInt(os.Getenv("JELLYSCALE_TEST_BUDGET"), 10, 64)
	w := &Worker{ID: id, Pool: pool, Segments: segmentSpy, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)).With("sweeper", id),
		Media: media.Open, Retention: 24 * time.Hour, Budget: budget}
	if os.Getenv("JELLYSCALE_TEST_COLLECT") != "" {
		text := "none"
		if value, reported, err := metricBytes(w); err != nil {
			fail("collect", err)
		} else if reported {
			text = strconv.FormatFloat(value, 'f', -1, 64)
		}
		mark("metric-"+id, text)
		os.Exit(0)
	}
	if period, err := time.ParseDuration(os.Getenv("JELLYSCALE_TEST_LOOP")); err == nil {
		w.SweepEvery = period
		spy := segmentSpy
		spy.afterList = func() { mark("lists-"+id, strconv.Itoa(spy.listCount())) }
		stop, cancel := signal.NotifyContext(ctx, syscall.SIGTERM)
		defer cancel()
		mark("ready-"+id, "")
		w.sweepLoop(stop)
		os.Exit(0)
	}
	if d, err := time.ParseDuration(os.Getenv("JELLYSCALE_TEST_DELAY")); err == nil {
		time.Sleep(d)
	}
	if err := w.Sweep(ctx); err != nil {
		fail("sweep", err)
	}
	mark("done-" + id)
	os.Exit(0)
}

type sweeperProc struct {
	cmd  *exec.Cmd
	id   string
	dir  string
	done chan struct{}
	err  error // What Wait said, once done is closed.
}

// startSweeper starts a sweeper; the test ends it.
func startSweeper(t *testing.T, dir, id string, env ...string) *sweeperProc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_SWEEPER="+id, "JELLYSCALE_TEST_SYNC="+dir)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &sweeperProc{cmd: cmd, id: id, dir: dir, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-p.done }) //nolint:errcheck
	return p
}

// readFile is what a sweeper wrote, or "" when it has not.
func (p *sweeperProc) readFile(name string) string {
	data, _ := os.ReadFile(filepath.Join(p.dir, name))
	return string(data)
}

func (p *sweeperProc) mark(name string) {
	os.WriteFile(filepath.Join(p.dir, name), nil, 0o644) //nolint:errcheck
}

// signal closes when the sweeper has written the file.
func (p *sweeperProc) signal(name string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for deadline := time.Now().Add(3 * time.Minute); !fileExists(filepath.Join(p.dir, name)) && time.Now().Before(deadline); {
			select {
			case <-p.done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return ch
}

func (p *sweeperProc) await(t *testing.T, name string) {
	t.Helper()
	<-p.signal(name)
	if !fileExists(filepath.Join(p.dir, name)) {
		t.Fatalf("sweeper %s ended (%v) or timed out before it wrote %s", p.id, p.err, name)
	}
}

func (p *sweeperProc) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("sweeper %s did not end", p.id)
	}
	if p.err != nil {
		t.Errorf("sweeper %s: %v", p.id, p.err)
	}
}

func (r *rig) sweeperEnv(tg target, budget int64, hold string) []string {
	env := []string{"JELLYSCALE_TEST_SEGMENTS_URI=" + tg.uri, "JELLYSCALE_TEST_BUDGET=" + strconv.FormatInt(budget, 10)}
	if hold != "" {
		env = append(env, "JELLYSCALE_TEST_HOLD="+hold)
	}
	return env
}

// Two workers sweeping at once remove what one sweep would: with three
// unwatched specs and a budget that holds two, the oldest goes and no more.
// Sweeper B is held after its listing, which is the whole store; A sweeps
// meanwhile, completely, or waits on a lock B holds (its session, by name), and
// then B goes on from a listing taken before A removed anything. A build that
// plans from that listing and the viewers' rows now removes the next spec too.
// The mutant: a sweep that does not look at the store again, or takes no lock,
// after it has been waiting. A sleep before A's start (JELLYSCALE_TEST_DELAY) must not matter.
func TestTwoWorkersSweepingTheTranscodeStoreAtOnceRemoveWhatOneWould(t *testing.T) {
	for _, tg := range targets(t) {
		for _, delay := range []string{"0s", "700ms"} {
			t.Run(tg.name+"/A starts after "+delay, func(t *testing.T) {
				tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
				st := tg.open(t)
				r := newRig(t, st)
				a, b, c := hex32("a"), hex32("b"), hex32("c")
				three := []int64{1000, 1000, 1000}
				r.seedSpec(t, st, c, three, time.Hour)
				r.seedSpec(t, st, a, three, 3*time.Hour)
				r.seedSpec(t, st, b, three, 2*time.Hour)
				dir := t.TempDir()
				second := startSweeper(t, dir, "B", r.sweeperEnv(tg, 6000, "segments:after-list")...)
				second.await(t, "listed-B")
				first := startSweeper(t, dir, "A", append(r.sweeperEnv(tg, 6000, ""), "JELLYSCALE_TEST_DELAY="+delay)...)
				r.untilEither(t, "sweeper A to finish or to wait on B's lock", first.signal("done-A"), "sweeper-A")
				second.mark("release-B")
				first.wait(t)
				second.wait(t)
				if left := keysUnder(t, st, "segments/"+a); len(left) != 0 {
					t.Errorf("the oldest spec keeps %v", left)
				}
				for _, hash := range []string{b, c} {
					if n := len(keysUnder(t, st, "segments/"+hash)); n != 3 {
						t.Errorf("spec %.1s has %d of 3 objects: two sweeps removed more than one would", hash, n)
					}
				}
			})
		}
	}
}

// A sweeper killed partway through a spec's removal leaves a spec the next
// sweep finishes removing, and the others whole. The sweeper is stopped before
// its second delete, so it has deleted one object and no more, and is killed
// there.
func TestASpecLeftHalfRemovedByAKilledSweeperIsFinishedByTheNextSweep(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			r := newRig(t, st)
			a, b, c := hex32("a"), hex32("b"), hex32("c")
			three := []int64{1000, 1000, 1000}
			r.seedSpec(t, st, a, three, 3*time.Hour)
			r.seedSpec(t, st, b, three, 2*time.Hour)
			r.seedSpec(t, st, c, three, time.Hour)
			killed := startSweeper(t, t.TempDir(), "K", r.sweeperEnv(tg, 6000, "segments:delete:2")...)
			killed.await(t, "deleting-K")
			if err := killed.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-killed.done
			left := len(keysUnder(t, st, "segments/"+a))
			if left == 0 || left == 3 {
				t.Fatalf("the killed sweeper left %d of 3 objects of the oldest spec, want a spec removed in part", left)
			}
			log, _ := newLogs()
			w := r.sweeper(st, 6000, log)
			if err := w.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if left := keysUnder(t, st, "segments/"+a); len(left) != 0 {
				t.Errorf("the next sweep left %v of the half-removed spec", left)
			}
			for _, hash := range []string{b, c} {
				if n := len(keysUnder(t, st, "segments/"+hash)); n != 3 {
					t.Errorf("spec %.1s has %d of 3 objects after the next sweep", hash, n)
				}
			}
			if v, ok := storeBytes(t, w); !ok || v != 6000 {
				t.Errorf("the metric is %v (reported: %v), the store holds 6000 bytes", v, ok)
			}
		})
	}
}

// ...and until the next sweep, the half-removed spec still plays: a viewer who
// asks for it gets valid segments (the init segment, one that is gone and one
// that is not), from a job the request starts where one is gone. The spec is
// made by a real job, then its viewer is stopped, and the sweeper is killed
// after its first delete.
func TestASpecLeftHalfRemovedByAKilledSweeperStillPlays(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			st := tg.open(t)
			r := newRig(t, st)
			r.worker(t, nil)
			first := r.jobs(t)
			if _, err := r.fetch(t, first, psid(0), 0); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the film's job to end", time.Minute, func() bool {
				return r.count(t, "SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") == 0
			})
			if err := first.Stop(context.Background(), r.user, psid(0), "device", ""); err != nil {
				t.Fatal(err)
			}
			whole := len(keysUnder(t, st, "segments/"+r.hash))
			killed := startSweeper(t, t.TempDir(), "K", r.sweeperEnv(tg, 1, "segments:delete:2")...)
			killed.await(t, "deleting-K")
			if err := killed.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-killed.done
			if left := len(keysUnder(t, st, "segments/"+r.hash)); left == 0 || left >= whole {
				t.Fatalf("the killed sweeper left %d of %d objects", left, whole)
			}
			j := r.jobs(t)
			for _, n := range []int{-1, 0, 3, 9} {
				data, err := r.fetchAt(t, j, psid(1), n, 0)
				switch {
				case err != nil:
					t.Errorf("segment %d of the half-removed spec: %v", n, err)
				case n < 0 && !strings.Contains(string(data[:min(len(data), 64)]), "ftyp"):
					t.Errorf("the init segment is %d bytes that are not one", len(data))
				case n >= 0 && !isMP4Segment(data):
					t.Errorf("segment %d is %d bytes that are not a segment", n, len(data))
				}
			}
		})
	}
}

// Two workers sweeping the asset stores at once end with the objects one sweep
// would leave: the removed and changed films' assets gone, every current asset
// there, and no error from either. B is held after listing the store.
func TestTwoWorkersSweepingTheAssetStoresAtOnceEndWithTheObjectsOfOne(t *testing.T) {
	for _, tg := range targets(t) {
		t.Run(tg.name, func(t *testing.T) {
			tg := tg.fresh(t) // Its own prefix: no subtest sees another's leftovers.
			e := newAssetEnv(t, tg)
			shared := fontSum("shared")
			kept := e.addFilm(e.lib, "kept", "k1", streams("fp"))
			keptV := e.putVersion(kept, "k1", streams("fp"), shared, fontSum("k"))
			e.current(kept, keptV)
			removed := e.addFilm(e.lib, "removed", "r1", streams("fp"))
			removedV := e.putVersion(removed, "r1", streams("fp"), shared, fontSum("r"))
			e.remove(removed)
			changed := e.addFilm(e.lib, "changed", "c2", streams("fp"))
			oldV := e.putVersion(changed, "c1", streams("fp"), fontSum("old"))
			newV := e.putVersion(changed, "c2", streams("fp"), fontSum("new"))
			e.current(changed, newV)

			dir := t.TempDir()
			env := []string{"JELLYSCALE_TEST_SEGMENTS_URI=file://" + t.TempDir(), "JELLYSCALE_TEST_ASSET_URIS=" + tg.uri}
			second := startSweeper(t, dir, "B", append(env, "JELLYSCALE_TEST_HOLD=assets:after-list")...)
			second.await(t, "listed-B")
			first := startSweeper(t, dir, "A", env...)
			e.r.untilEither(t, "sweeper A to finish or to wait on B's lock", first.signal("done-A"), "sweeper-A")
			second.mark("release-B")
			first.wait(t)
			second.wait(t)

			e.expect(keptV.all(), true, "a current version")
			e.expect(newV.all(), true, "the current version of a changed film")
			e.expect(removedV.text, false, "a removed film's text")
			e.expect(removedV.sup, false, "a removed film's .sup")
			e.expect(removedV.chapters, false, "a removed film's chapter pictures")
			e.expect([]string{removedV.manifest, removedV.image, oldV.manifest, oldV.image}, false, "a manifest and image of a removed film and of a previous version")
			e.expect(oldV.text, false, "the previous version's text")
			e.expect([]string{"fonts/" + shared}, true, "a font a kept film names")
		})
	}
}

// collectMetric starts workers of their own that never sweep, at once, and
// returns what each one's Collect says the transcode store holds.
func (r *rig) collectMetric(t *testing.T, dir string, tg target, round, workers int) []string {
	t.Helper()
	procs := make([]*sweeperProc, workers)
	for i := range procs {
		procs[i] = startSweeper(t, dir, fmt.Sprintf("collect-%d-%d", round, i), append(r.sweeperEnv(tg, 0, ""), "JELLYSCALE_TEST_COLLECT=1")...)
	}
	values := make([]string, workers)
	for i, p := range procs {
		p.wait(t)
		values[i] = p.readFile("metric-" + p.id)
	}
	return values
}

// Every worker reports the same value, and after a sweep it is the bytes the
// store holds: the cluster's number, as the queue's depth is, so a worker that
// has not swept reports what a sweeping one found. The workers are processes
// of their own (a size kept in a variable of one process shows as "none" or 0
// in the others): one runs the sweep loop at a short period, and each round
// three others, which never sweep, are asked at once. Refreshed each period: a
// later write shows in every worker without anything restarted. The bound on
// the wait is for a hang (periods of 300 ms).
func TestTheStoreSizeMetricIsTheSameOnEveryWorkerAndEqualsTheBytesHeld(t *testing.T) {
	for _, budget := range []int64{0, 1 << 40} {
		for _, tg := range targets(t) {
			t.Run(fmt.Sprintf("%s/budget %d", tg.name, budget), func(t *testing.T) {
				tg := tg.fresh(t)
				st := tg.open(t)
				r := newRig(t, st)
				r.seedSpec(t, st, hex32("a"), []int64{1000, 2000, 3000}, time.Hour)
				r.seedSpec(t, st, hex32("b"), []int64{500}, 2*time.Hour)
				dir := t.TempDir()
				looping := startSweeper(t, dir, "loop", append(r.sweeperEnv(tg, budget, ""), "JELLYSCALE_TEST_LOOP=300ms")...)
				looping.await(t, "ready-loop")
				round := 0
				everyWorkerSays := func(want string) {
					t.Helper()
					var last []string
					for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
						round++
						if last = r.collectMetric(t, dir, tg, round, 3); last[0] == want && last[1] == want && last[2] == want {
							return
						}
					}
					t.Errorf("workers that never sweep report %q, the store holds %s bytes", last, want)
				}
				everyWorkerSays("6500")
				// More is stored; a period later every worker shows it.
				putSized(t, st, SegmentKey(hex32("b"), 0), 1500)
				everyWorkerSays("8000")
			})
		}
	}
}

// How often the store is listed per sweep period does not grow with the number
// of workers: six worker processes, each running its own sweepLoop at the same
// period, list the store about as often as one does. Each process counts its
// own listings (lists-<id>); the window is six periods after all have started.
// The bound is loose (two and two over one worker's count) because the counts
// are of a window on a loaded machine; a worker that lists on its own tick
// lists six times as often, which no jitter reaches. Mutant: the loop as it is
// today, with a listing in each worker's every tick.
func TestTheStoreIsListedNoMoreOftenForSixWorkersThanForOne(t *testing.T) {
	const period = 500 * time.Millisecond
	count := func(workers int) int {
		tg := target{name: "posix", uri: "file://" + t.TempDir()}
		st := tg.open(t)
		r := newRig(t, st)
		r.seedSpec(t, st, hex32("a"), []int64{1000, 1000}, time.Hour)
		dir := t.TempDir()
		procs := make([]*sweeperProc, workers)
		for i := range procs {
			procs[i] = startSweeper(t, dir, fmt.Sprintf("w%d", i), append(r.sweeperEnv(tg, 1<<40, ""), "JELLYSCALE_TEST_LOOP="+period.String())...)
		}
		for _, p := range procs {
			p.await(t, "ready-"+p.id)
		}
		listings := func() int {
			total := 0
			for _, p := range procs {
				n, _ := strconv.Atoi(p.readFile("lists-" + p.id))
				total += n
			}
			return total
		}
		before := listings()
		time.Sleep(6 * period)
		during := listings() - before
		for _, p := range procs {
			p.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
		}
		for _, p := range procs {
			p.wait(t)
		}
		return during
	}
	one, six := count(1), count(6)
	t.Logf("listings over six periods: %d for one worker, %d for six", one, six)
	if one == 0 {
		t.Fatal("a worker that sweeps listed the store no time")
	}
	if six > 2*one+2 {
		t.Errorf("six workers listed the store %d times where one did %d", six, one)
	}
}
