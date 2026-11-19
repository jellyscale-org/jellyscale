//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/testenv"
)

// JS-030, the liveness URL: `--liveness-database-url` carries a worker's liveness
// lock and its lease keeper, and nothing else. Real processes of the binary
// (the cluster of output_version_integration_test.go), a testenv.DBProxy for
// each URL so that Postgres, and the proxy's record of the statements sent
// through it, say which connection did what. Written before the flag existed:
// today `jellyscale` stops at start with "flag provided but not defined".
//
// Timings are production's: the lease is the default 10 s, and an interruption
// is 3 s (shorter than a lease less a heartbeat, 7 s) or 14 s (longer than a
// lease). Nothing here passes or fails on how long something took.

const livenessFlag = "--liveness-database-url"

// exec makes pgx send each statement's text, which the proxies record.
const execMode = "&default_query_exec_mode=exec"

// launchWith is cluster.launch for a build, roles, the main database URL and
// extra arguments: the one part of launch that the liveness tests change.
func (c *cluster) launchWith(version int, roles, dsn string, args ...string) *proc {
	c.t.Helper()
	n := len(c.procs)
	slots := ""
	if c.slots > 0 {
		slots = fmt.Sprintf(`"transcodeSlots": %d,`, c.slots)
	}
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", %s
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-%d", "transcodeIdle": "60s", "transcodeLead": %q,
 "libraries": [{"name": "Movies", "kind": "movies", "media": "file://%s/media", "assets": "file://%s/assets"}]}`,
		slots, c.dir, c.dir, n, c.lead, c.dir, c.dir)
	path := filepath.Join(c.dir, "bin") + ":" + os.Getenv("PATH")
	listen := strings.Contains(roles, "api") || strings.Contains(roles, "stream")
	if listen {
		path = filepath.Join(c.dir, "nobin")
	}
	p := startProc(c.t, procSpec{bin: builds[version], dsn: dsn, args: append([]string{"--roles=" + roles}, args...), config: config, listen: listen,
		user: "admin", password: "r-pass", path: path, term: true})
	p.version, p.roles = version, roles
	c.procs = append(c.procs, p)
	return p
}

// connections is how many Postgres sessions carry the application name.
func (c *cluster) connections(app string) int {
	var n int
	c.pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE application_name = $1", app).Scan(&n) //nolint:errcheck
	return n
}

// livenessBackend is the backend that holds the worker's liveness lock, and the
// application name its session carries ("" when none holds it).
func (c *cluster) livenessBackend(worker string) string {
	var app string
	c.pool.QueryRow(context.Background(), `SELECT a.application_name FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 2 AND l.classid = 27219 AND l.objid::bigint = hashtext($1)::bigint & 4294967295`, worker).Scan(&app) //nolint:errcheck
	return app
}

// mostConnections samples the sessions of an application name until stopped.
type mostConnections struct {
	most atomic.Int64
	stop func()
}

func (c *cluster) watchConnections(app string) *mostConnections {
	m := &mostConnections{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			var n int
			if c.pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE application_name = $1", app).Scan(&n) == nil && int64(n) > m.most.Load() {
				m.most.Store(int64(n))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	m.stop = func() { cancel(); <-done }
	c.t.Cleanup(m.stop)
	return m
}

// A worker started with a liveness URL connects its lock and its lease keeper
// through it and nothing else: one connection while it has no job, two while it
// has one, however often its connections end; every other statement goes
// through the main URL.
func TestAWorkerWithALivenessURLHoldsAtMostTwoConnectionsThereAndSendsNothingElseThere(t *testing.T) {
	c := newCluster(t, false, "20s") // The job pauses at its lead, 20 s ahead, and stays running while the test looks.
	c.slots = 1
	// The main URL is not in exec mode (a JSON parameter is not sent as text); a statement's text
	// reaches the proxy with its first use on a connection, which is all the check below needs.
	main := testenv.NewDBProxy(t, c.dsn)
	live := testenv.NewDBProxy(t, c.dsn)
	most := c.watchConnections(live.Name())

	api := c.start(newVersion, "api,stream")
	w := c.launchWith(newVersion, "worker", main.DSN(), livenessFlag+"="+live.DSN()+execMode)
	c.waitReady(w)

	waitUntil(t, "the worker's lock to be held on the liveness URL", func() bool { return c.livenessBackend(w.workerID) == live.Name() })
	time.Sleep(3 * time.Second) // Long enough for a keeper that connects without a job to have done it.
	if n := c.connections(live.Name()); n != 1 {
		t.Errorf("%d connections from the worker on the liveness URL while it has no job, want 1 (the lock)", n)
	}

	c.login(api)
	p := c.startPlayback(api, api)
	if a := p.segment(0); !a.isSegment() {
		t.Fatalf("the worker's first segment: %s\n%s", a, w.log)
	}
	waitUntil(t, "the keeper's connection on the liveness URL", func() bool { return c.connections(live.Name()) == 2 })

	// The lock's connection and the keeper's end, over and over, and are made again.
	for round := 1; round <= 5; round++ {
		before := live.Accepted()
		// Postgres ends the backends itself, so the old ones are gone before the new ones come.
		if n, err := live.Terminate(context.Background(), c.pool); err != nil || n == 0 {
			t.Fatalf("round %d: ending the backends: %d %v", round, n, err)
		}
		waitUntil(t, fmt.Sprintf("round %d: the lock and the keeper connected again", round), func() bool {
			return live.Accepted() >= before+2 && c.connections(live.Name()) == 2 && c.livenessBackend(w.workerID) == live.Name()
		})
	}
	if got := most.most.Load(); got > 2 {
		t.Errorf("at most %d connections from the worker were open on the liveness URL at once, want 2", got)
	}

	// What went through each URL.
	onlyLock := regexp.MustCompile(`pg_try_advisory_lock\(27219|transcode_jobs|subtitle_jobs`)
	forbidden := regexp.MustCompile(`(?i)INSERT|DELETE|pg_locks|LISTEN|pg_notify|libraries|items|users|sessions|media_sources|schema_migrations|CREATE|ALTER`)
	sawLock, sawRenewal := false, false
	for _, sql := range live.Statements() {
		switch {
		case forbidden.MatchString(sql):
			t.Errorf("the liveness URL carried a statement that is not the lock's or the keeper's: %.200s", sql)
		case !onlyLock.MatchString(sql):
			t.Errorf("the liveness URL carried a statement that names neither the lock nor a lease: %.200s", sql)
		}
		sawLock = sawLock || strings.Contains(sql, "pg_try_advisory_lock(27219")
		sawRenewal = sawRenewal || strings.Contains(sql, "UPDATE transcode_jobs")
	}
	if !sawLock || !sawRenewal {
		t.Errorf("the liveness URL carried the lock (%v) and a renewal (%v), want both", sawLock, sawRenewal)
	}
	// The recorder works: what the claims, the registration and the rest need is on the main URL.
	var registered, claimed bool
	for _, sql := range main.Statements() {
		registered = registered || strings.Contains(sql, "INSERT INTO transcode_nodes")
		claimed = claimed || strings.Contains(sql, "UPDATE transcode_jobs SET state = 'running'")
		if strings.Contains(sql, "pg_try_advisory_lock(27219") {
			t.Errorf("the liveness lock was taken on the main URL: %.200s", sql)
		}
	}
	if !registered || !claimed {
		t.Errorf("the main URL carried the registration (%v) and the claim (%v), want both", registered, claimed)
	}
	if got := c.connections(main.Name()); got < 2 {
		t.Errorf("%d connections of the worker on the main URL, want its pool and its hint listener", got)
	}
	p.stop()
}

// Without the flag a worker's lock and keeper use the main URL as before; an api
// or stream process given the flag starts and opens nothing there.
func TestWithoutTheFlagTheLockIsOnTheMainURLAndAReplicaGivenItOpensNothingThere(t *testing.T) {
	c := newCluster(t, false, "20s")
	main := testenv.NewDBProxy(t, c.dsn)
	live := testenv.NewDBProxy(t, c.dsn)

	w := c.launchWith(newVersion, "worker", main.DSN())
	c.waitReady(w)
	waitUntil(t, "the lock to be held through the main URL", func() bool { return c.livenessBackend(w.workerID) == main.Name() })
	if live.Accepted() != 0 {
		t.Errorf("a connection was made to a URL nobody was given")
	}

	for _, roles := range []string{"api", "stream", "api,stream"} {
		r := c.launchWith(newVersion, roles, c.dsn, livenessFlag+"="+live.DSN())
		c.waitReady(r)
		time.Sleep(2 * time.Second)
		if !r.alive() {
			t.Fatalf("a %s process given %s exited: %s", roles, livenessFlag, r.log)
		}
	}
	if n := live.Accepted(); n != 0 {
		t.Errorf("api and stream processes given %s made %d connections to it, want none", livenessFlag, n)
	}
}

// A value that is not a Postgres URL stops the start with an error that names the
// flag, before anything is sent to Postgres.
func TestALivenessURLThatIsNotAPostgresURLStopsTheStartNamingTheFlag(t *testing.T) {
	for _, bad := range []string{"not a url", "mysql://user@host/db", "http://127.0.0.1:1/x"} {
		t.Run(bad, func(t *testing.T) {
			c := newCluster(t, false, "20s")
			main := testenv.NewDBProxy(t, c.dsn)
			for _, roles := range []string{"worker", "api"} {
				p := c.launchWith(newVersion, roles, main.DSN(), livenessFlag+"="+bad)
				select {
				case <-p.exited:
				case <-time.After(30 * time.Second):
					t.Fatalf("a %s given the bad value %q is still running:\n%s", roles, bad, p.log)
				}
				if p.status == nil {
					t.Errorf("a %s given %q exited with status 0", roles, bad)
				}
				if strings.Contains(p.log.String(), "flag provided but not defined") {
					t.Errorf("a %s given %q stopped because the flag does not exist, not because of its value:\n%s", roles, bad, p.log)
				} else if !strings.Contains(p.log.String(), livenessFlag) {
					t.Errorf("the error of a %s given %q does not name %s:\n%s", roles, bad, livenessFlag, p.log)
				}
			}
			if n := main.Accepted(); n != 0 {
				t.Errorf("%d connections were made to Postgres before the bad liveness URL was refused, want none", n)
			}
			if len(main.Statements()) != 0 {
				t.Errorf("statements were sent before the bad liveness URL was refused: %.200s", main.Statements()[0])
			}
		})
	}
}

// A URL that parses and refuses connections stops nothing: the worker says why,
// has no row (so claims nothing), tries again, and takes jobs once it connects.
func TestALivenessURLThatRefusesConnectionsIsRetriedAndTheWorkerClaimsNothingMeanwhile(t *testing.T) {
	c := newCluster(t, false, "20s")
	live := testenv.NewDBProxy(t, c.dsn)
	live.Away()
	api := c.start(newVersion, "api,stream")
	w := c.launchWith(newVersion, "worker", c.dsn, livenessFlag+"="+live.DSN())
	w.until(hang, "the worker to say it started", func() bool { return workerStarted.MatchString(w.log.String()) })
	id := workerStarted.FindStringSubmatch(w.log.String())[1]
	w.until(hang, "the worker to say why it cannot take its lock", func() bool { return strings.Contains(w.log.String(), "taking the worker's liveness lock failed") })
	time.Sleep(3 * time.Second)
	if !w.alive() {
		t.Fatalf("the worker exited while its liveness URL refused connections:\n%s", w.log)
	}
	var rows int
	c.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_nodes WHERE id = $1", id).Scan(&rows) //nolint:errcheck
	if rows != 0 {
		t.Errorf("a worker that cannot take its lock has a row in transcode_nodes")
	}
	var jobs int
	c.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_jobs WHERE worker = $1", id).Scan(&jobs) //nolint:errcheck
	if jobs != 0 {
		t.Errorf("a worker that cannot take its lock claimed %d jobs", jobs)
	}
	live.Back()
	waitUntil(t, "the worker to register once its liveness URL answers", func() bool { return c.livenessBackend(id) == live.Name() })
	c.login(api)
	p := c.startPlayback(api, api)
	if a := p.segment(0); !a.isSegment() {
		t.Fatalf("a segment after the liveness URL answered: %s", a)
	}
	p.stop()
}

// A lock Postgres does not show while the connection stands is logged as it is
// today, and the line names the flag where the URL to fix is.
func TestALockPostgresDoesNotShowNamesTheLivenessURLFlag(t *testing.T) {
	c := newCluster(t, false, "20s")
	proxy := testenv.NewDBProxy(t, c.dsn)
	w := c.launchWith(newVersion, "worker", c.dsn, livenessFlag+"="+proxy.DSN())
	c.waitReady(w)
	waitUntil(t, "the worker to be registered", func() bool {
		var n int
		c.pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_nodes WHERE id = $1 AND locked", w.workerID).Scan(&n) //nolint:errcheck
		return n == 1
	})
	proxy.CutServers() // The server's end goes, the client's stays open: the lock is gone, the connection stands.
	waitUntil(t, "the worker to say Postgres does not show its lock", func() bool { return strings.Contains(w.log.String(), "does not show its lock") })
	var line string
	for _, l := range strings.Split(w.log.String(), "\n") {
		if strings.Contains(l, "does not show its lock") {
			line = l
		}
	}
	if !strings.Contains(line, livenessFlag) {
		t.Errorf("the line about a lock Postgres does not show does not name %s: %s", livenessFlag, line)
	}
}

// outageCluster is three workers, each with its own liveness proxy, one replica
// and a viewer of a spec of its own on each worker (a job each, one slot each).
type outageCluster struct {
	c       *cluster
	live    []*testenv.DBProxy
	workers []*proc
	plays   []*playing
}

func newOutageCluster(t *testing.T) *outageCluster {
	t.Helper()
	c := newCluster(t, false, "20s") // No helpers: a job is one ffmpeg that pauses at its lead.
	c.slots = 1
	o := &outageCluster{c: c}
	api := c.start(newVersion, "api,stream")
	for range 3 {
		live := testenv.NewDBProxy(t, c.dsn)
		w := c.launchWith(newVersion, "worker", c.dsn, livenessFlag+"="+live.DSN())
		c.waitReady(w)
		o.live, o.workers = append(o.live, live), append(o.workers, w)
	}
	c.login(api)
	for _, bitrate := range []int{2_000_000, 3_000_000, 4_000_000} {
		p := c.startPlaybackAt(api, api, bitrate)
		if a := p.segment(0); !a.isSegment() {
			t.Fatalf("segment 0 at %d: %s", bitrate, a)
		}
		o.plays = append(o.plays, p)
	}
	waitUntil(t, "every worker to hold a job", func() bool {
		held := map[string]bool{}
		for _, j := range c.rows() {
			if j.State == "running" {
				held[j.Worker] = true
			}
		}
		return len(held) == 3
	})
	return o
}

func (o *outageCluster) setAway(away bool) {
	for _, l := range o.live {
		if away {
			l.Away()
		} else {
			l.Back()
		}
	}
}

// A liveness URL that refuses every worker for less than a lease less a
// heartbeat leaves each job its row, worker and ffmpeg, and forgets nobody.
func TestALivenessURLThatRefusesEveryWorkerForLessThanALeaseLeavesEveryJobAndForgetsNoWorker(t *testing.T) {
	o := newOutageCluster(t)
	c := o.c
	before := c.rows()
	pids := ffmpegPIDs(t, c)
	if len(pids) != 3 {
		t.Fatalf("%d ffmpeg before the outage, want 3 (one for each worker)", len(pids))
	}
	stopWatching := watchNodes(c)
	o.setAway(true)
	time.Sleep(3 * time.Second) // Less than the lease (10 s) less a heartbeat (3 s).
	// Viewers keep asking meanwhile.
	for _, p := range o.plays {
		go p.segment(2)
	}
	time.Sleep(time.Second)
	o.setAway(false)
	waitUntil(t, "every worker to hold its lock again", func() bool {
		for _, w := range o.workers {
			if c.livenessBackend(w.workerID) == "" {
				return false
			}
		}
		return true
	})
	time.Sleep(2 * time.Second)
	forgotten := stopWatching()
	if len(forgotten) > 0 {
		t.Errorf("workers were forgotten during the interruption: %v", forgotten)
	}
	after := c.rows()
	if len(after) != len(before) {
		t.Errorf("%d job rows after the interruption, %d before: one was started", len(after), len(before))
	}
	byID := map[int64]row{}
	for _, j := range after {
		byID[j.ID] = j
	}
	for _, j := range before {
		if j.State == "running" && (byID[j.ID].State != "running" || byID[j.ID].Worker != j.Worker) {
			t.Errorf("job %d was running on %s and is %s on %s", j.ID, j.Worker, byID[j.ID].State, byID[j.ID].Worker)
		}
	}
	if now := ffmpegPIDs(t, c); fmt.Sprint(now) != fmt.Sprint(pids) {
		t.Errorf("the ffmpeg processes were %v before the interruption and %v after: one was stopped or started", pids, now)
	}
}

// One longer than a lease ends each job cancelled, never failed, claims none
// meanwhile, and the next request starts each again at the first segment not stored.
func TestALivenessURLThatRefusesEveryWorkerForLongerThanALeaseCancelsEveryJobAndTheNextRequestRestartsIt(t *testing.T) {
	o := newOutageCluster(t)
	c := o.c
	old := c.rows()
	last := old[len(old)-1].ID
	o.setAway(true)
	time.Sleep(14 * time.Second) // Longer than the lease (10 s).
	for _, j := range c.rows() {
		if j.ID > last && j.Worker != "" {
			t.Errorf("job %d was claimed by %s while no worker could lock", j.ID, j.Worker)
		}
		if j.State == "failed" {
			t.Errorf("job %d failed: %s", j.ID, j.Error)
		}
	}
	o.setAway(false)
	waitUntil(t, "every worker to hold its lock again", func() bool {
		for _, w := range o.workers {
			if c.livenessBackend(w.workerID) == "" {
				return false
			}
		}
		return true
	})
	waitUntil(t, "every old job to end", func() bool {
		for _, j := range c.rows() {
			if j.ID <= last && j.State == "running" {
				return false
			}
		}
		return true
	})
	for _, j := range c.rows() {
		if j.ID <= last && j.State != "cancelled" {
			t.Errorf("job %d ended %s (%s), want cancelled", j.ID, j.State, j.Error)
		}
	}
	for i, p := range o.plays {
		stored := countStored(c, old[i].Hash)
		if a := p.segment(stored); !a.isSegment() {
			t.Errorf("viewer %d: segment %d after the interruption: %s", i, stored, a)
		}
	}
	var restarted int
	for _, j := range c.rows() {
		if j.ID > last && !j.Helper {
			restarted++
			for _, o := range old {
				if o.Hash == j.Hash && j.Start > countStored(c, o.Hash) {
					t.Errorf("job %d restarted its spec at segment %d, past the first one not stored", j.ID, j.Start)
				}
			}
		}
	}
	if restarted != 3 {
		t.Errorf("%d jobs started again, want one for each of the 3 specs", restarted)
	}
}

// countStored is how many segments from 0 on the transcode store holds for the spec: the first one not stored.
func countStored(c *cluster, hash string) int {
	n := 0
	for {
		if _, err := os.Stat(filepath.Join(c.dir, "segments", "segments", hash, fmt.Sprintf("%d.mp4", n))); err != nil {
			return n
		}
		n++
	}
}

// ffmpegPIDs is the pids of the ffmpeg that write into this cluster's scratch directories: other
// tests' clusters run side by side and their ffmpeg are not this test's.
func ffmpegPIDs(t *testing.T, c *cluster) []string {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, e := range entries {
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if filepath.Base(args[0]) == "ffmpeg" && strings.Contains(string(raw), "-f\x00hls") && strings.Contains(string(raw), c.dir+"/") {
			found = append(found, e.Name())
		}
	}
	return found
}

// watchNodes samples transcode_nodes and says, when stopped, which rows were there at first and went.
func watchNodes(c *cluster) (stop func() []string) {
	ctx, cancel := context.WithCancel(context.Background())
	seen := map[string]bool{}
	gone := map[string]bool{}
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			rows, err := c.pool.Query(ctx, "SELECT id FROM transcode_nodes")
			if err == nil {
				now := map[string]bool{}
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						now[id] = true
					}
				}
				// A sample cut short (the context ended mid-read) is not the whole table.
				if rows.Err() != nil || ctx.Err() != nil {
					rows.Close()
					continue
				}
				rows.Close()
				mu.Lock()
				for id := range seen {
					if !now[id] {
						gone[id] = true
					}
				}
				for id := range now {
					seen[id] = true
				}
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return func() []string {
		cancel()
		<-done
		var out []string
		for id := range gone {
			out = append(out, id)
		}
		return out
	}
}
