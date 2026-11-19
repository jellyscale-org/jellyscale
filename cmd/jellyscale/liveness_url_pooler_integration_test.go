//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
)

// JS-030, the liveness URL behind a real pooler: the main URL goes through
// PgBouncer in session mode, the liveness URL straight to Postgres, and every
// role still works. The containers are made here and removed with docker rm -fv:
// a Postgres of its own (the test Postgres of the other tests is not reachable
// from a container, and a pooler needs one it can reach) and PgBouncer, both on
// ports Docker assigned.

type poolerStack struct {
	direct, pooled string // Database URLs, as the host sees them.
	poolerAddr     string // host:port of PgBouncer.
	network        string
	poolerName     string
	poolerIP       string
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func hostPort(t *testing.T, container, port string) string {
	t.Helper()
	out := docker(t, "port", container, port+"/tcp")
	line := strings.Split(out, "\n")[0]
	return line[strings.LastIndex(line, ":")+1:]
}

func startPooler(t *testing.T, mode string) *poolerStack {
	t.Helper()
	tag := uuid.NewString()[:8]
	s := &poolerStack{network: "js030-net-" + tag, poolerName: "js030-pgbouncer-" + tag}
	pg := "js030-pg-" + tag
	docker(t, "network", "create", s.network)
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-fv", pg, s.poolerName).Run() //nolint:errcheck
		exec.Command("docker", "network", "rm", s.network).Run()    //nolint:errcheck
	})
	docker(t, "run", "-d", "--rm", "--name", pg, "--network", s.network, "--network-alias", "pg", "-e", "POSTGRES_PASSWORD=it",
		"-p", "127.0.0.1::5432", "postgres:18-alpine", "-c", "max_connections=500")
	docker(t, "run", "-d", "--rm", "--name", s.poolerName, "--network", s.network, "-p", "127.0.0.1::5432",
		"-e", "DB_HOST=pg", "-e", "DB_USER=postgres", "-e", "DB_PASSWORD=it", "-e", "DB_NAME=postgres", "-e", "POOL_MODE="+mode,
		"-e", "AUTH_TYPE=scram-sha-256", "-e", "MAX_CLIENT_CONN=1000", "-e", "DEFAULT_POOL_SIZE=300", "-e", "ADMIN_USERS=postgres",
		"-e", "SERVER_RESET_QUERY=DISCARD ALL", "edoburu/pgbouncer:latest")
	s.direct = "postgres://postgres:it@127.0.0.1:" + hostPort(t, pg, "5432") + "/postgres"
	s.poolerAddr = "127.0.0.1:" + hostPort(t, s.poolerName, "5432")
	s.pooled = "postgres://postgres:it@" + s.poolerAddr + "/postgres"
	s.poolerIP = docker(t, "inspect", "-f", "{{(index .NetworkSettings.Networks \""+s.network+"\").IPAddress}}", s.poolerName)
	deadline := time.Now().Add(90 * time.Second)
	for {
		pool, err := pgxpool.New(context.Background(), s.pooled)
		if err == nil {
			var one int
			err = pool.QueryRow(context.Background(), "SELECT 1").Scan(&one)
			pool.Close()
		}
		if err == nil {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("Postgres behind PgBouncer never answered: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// subtitleOf is the index and source id of the film's text subtitle, from PlaybackInfo.
func (c *cluster) subtitleOf(api *proc) (item, source string, index int) {
	c.t.Helper()
	code, _, body, err := c.call("POST", api, "/Items/"+c.item+"/PlaybackInfo", `{}`)
	if err != nil || code != 200 {
		c.t.Fatalf("PlaybackInfo: %d %v %s", code, err, body)
	}
	var info struct {
		MediaSources []struct {
			Id           string
			MediaStreams []struct {
				Type  string
				Index int
			}
		}
	}
	if err := json.Unmarshal(body, &info); err != nil || len(info.MediaSources) == 0 {
		c.t.Fatalf("PlaybackInfo: %v %s", err, body)
	}
	for _, s := range info.MediaSources[0].MediaStreams {
		if s.Type == "Subtitle" {
			return c.item, info.MediaSources[0].Id, s.Index
		}
	}
	c.t.Fatalf("the film has no subtitle track: %s", body)
	return
}

// The main URL behind PgBouncer in session mode and the liveness URL direct, with
// real processes of every role: a film transcodes and is served, a text subtitle
// is extracted, a library scan and a sweep run, and a worker killed mid-job is
// replaced before the lease it had at the kill would have run out (a 30 s lease);
// the only connections of the workers that bypass the pooler are the liveness ones.
func TestEveryRoleWorksWithTheMainURLBehindASessionModePoolerAndTheLivenessURLDirect(t *testing.T) {
	s := startPooler(t, "session")
	c := newCluster(t, true, "20s")
	c.slots = 1
	pool, err := db.Open(context.Background(), s.direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c.dsn, c.pool = s.direct, pool
	// A text subtitle beside the film.
	srt := "1\n00:00:01,000 --> 00:00:03,000\nHello\n\n2\n00:00:04,000 --> 00:00:06,000\nWorld\n"
	if err := os.WriteFile(filepath.Join(c.dir, "media", "Film (2020)", "Film (2020).en.srt"), []byte(srt), 0o644); err != nil {
		t.Fatal(err)
	}
	// The real binary's lease is the default 10 s and has no key to change it (and none may be
	// added for a test): the bound below is the lease as it stood at the kill, which a build
	// that waits the lease out misses at any length.
	liveURL := s.direct + "?application_name=js030-live"
	api := c.launchWith(newVersion, "api,stream", s.pooled)
	c.waitReady(api)
	a := c.launchWith(newVersion, "worker", s.pooled, livenessFlag+"="+liveURL)
	b := c.launchWith(newVersion, "worker", s.pooled, livenessFlag+"="+liveURL)
	c.waitReady(a)
	c.waitReady(b)
	c.login(api)

	// A film transcodes and its segments are served.
	p := c.startPlayback(api, api)
	if got := p.segment(0); !got.isSegment() {
		t.Fatalf("segment 0 through the pooler: %s\n%s\n%s", got, a.log, b.log)
	}
	// A text subtitle is extracted.
	item, source, index := c.subtitleOf(api)
	code, _, body, err := c.call("GET", api, fmt.Sprintf("/Videos/%s/%s/Subtitles/%d/0/Stream.vtt", item, source, index), "")
	if err != nil || code != 200 || !strings.Contains(string(body), "Hello") {
		t.Errorf("the text subtitle through the pooler: %d %v %.200s", code, err, body)
	}
	// A library scan ran (the film is listed), and a sweep.
	var sweeps int
	waitUntil(t, "a sweep to have run", func() bool {
		pool.QueryRow(context.Background(), "SELECT count(*) FROM sweeps").Scan(&sweeps) //nolint:errcheck
		return sweeps > 0
	})

	// Only the liveness connections bypass the pooler: Postgres sees, from outside the
	// pooler, the workers' liveness sessions (named by their URL) and this test's own pool,
	// and every liveness lock is held on a liveness session.
	var live, strays, throughPooler, lockedElsewhere int
	pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE application_name = 'js030-live' AND NOT (host(client_addr) = $1)),
		count(*) FILTER (WHERE application_name <> 'js030-live' AND NOT (host(client_addr) = $1)),
		count(*) FILTER (WHERE host(client_addr) = $1)
		FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()`, s.poolerIP).Scan(&live, &strays, &throughPooler) //nolint:errcheck
	pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING (pid)
		WHERE l.locktype = 'advisory' AND l.classid = 27219 AND a.application_name <> 'js030-live'`).Scan(&lockedElsewhere) //nolint:errcheck
	if live < 2 || live > 4 {
		t.Errorf("%d liveness connections reach Postgres directly, want each of 2 workers' lock and keeper (2 to 4)", live)
	}
	if own := pool.Stat().TotalConns(); int32(strays) > own {
		t.Errorf("%d other connections bypass the pooler, and the test's own pool has %d", strays, own)
	}
	if throughPooler == 0 {
		t.Errorf("no connection came through the pooler")
	}
	if lockedElsewhere != 0 {
		t.Errorf("%d liveness locks are held on a connection that is not a liveness one", lockedElsewhere)
	}

	// SIGKILL of the worker that holds the job: the other takes it before its lease, as it stood at the kill, would have run out.
	var victim, hash string
	var lease time.Time
	var next int
	if err := pool.QueryRow(context.Background(), "SELECT worker, spec_hash, lease_expires_at, next_segment FROM transcode_jobs WHERE state = 'running' AND NOT helper LIMIT 1").Scan(&victim, &hash, &lease, &next); err != nil {
		t.Fatalf("no running job: %v", err)
	}
	proc, other := a, b
	if b.workerID == victim {
		proc, other = b, a
	}
	proc.kill() // The lease was read before the kill: the reap cuts it within a second, so a read after could see the cut one.
	// The viewer asks for the next segment the dead job would have made, which that job's row still
	// covers (within next_segment + 3): the request waits for a job, and it is not a seek that
	// starts a new one at once, which would be a replacement however long the dead worker is remembered.
	served := make(chan answer, 1)
	go func() { served <- p.segment(min(next, len(p.entries)-1)) }()
	// The replacement is the claim, by the other worker, of a running row of the spec: seen while the
	// database's clock is still before the lease read before the kill (not a segment served, which
	// would come later whatever claimed it).
	var early bool
	waitUntil(t, "the other worker to run the spec", func() bool {
		var n int
		pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_jobs WHERE spec_hash = $1 AND state = 'running' AND worker = $2", hash, other.workerID).Scan(&n) //nolint:errcheck
		if n == 0 {
			return false
		}
		pool.QueryRow(context.Background(), "SELECT now() < $1", lease).Scan(&early) //nolint:errcheck
		return true
	})
	if !early {
		t.Errorf("the replacement was claimed after the dead worker's lease (%v) would have run out", lease)
	}
	if got := <-served; !got.isSegment() {
		t.Errorf("segment %d after the worker was killed: %s", min(next, len(p.entries)-1), got)
	}
	p.stop()
}

// A liveness URL behind a real transaction-mode pooler (PgBouncer 1.26.1, observed): the lock
// is taken on one of the pooler's server connections and stays in pg_locks there, also after the
// worker's own connection closes, so Postgres does not show "no lock" and a killed worker's lock
// never ends. A build either notices (a line naming --liveness-database-url) or does not, and
// then the one thing that must hold is the fallback: the worker starts and registers, the viewer
// is served again by another worker after the kill with its job never failed, and nobody runs
// two jobs for the spec. What the docs say of that is checked in internal/transcode/js030_docs_test.go.
// A simulation (a proxy that cuts the server side) would show the lock gone, which this pooler never does.
func TestALivenessURLBehindATransactionModePoolerEitherNamesTheFlagOrLeavesTheKilledWorkersJobToItsLease(t *testing.T) {
	s := startPooler(t, "transaction")
	c := newCluster(t, true, "20s")
	c.slots = 1
	pool, err := db.Open(context.Background(), s.direct)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c.dsn, c.pool = s.direct, pool
	api := c.launchWith(newVersion, "api,stream", s.direct)
	c.waitReady(api)
	a := c.launchWith(newVersion, "worker", s.direct, livenessFlag+"="+s.pooled)
	c.waitReady(a)
	c.login(api)
	p := c.startPlayback(api, api)
	if got := p.segment(0); !got.isSegment() {
		t.Fatalf("segment 0 with the liveness URL behind a transaction-mode pooler: %s\n%s", got, a.log)
	}
	b := c.launchWith(newVersion, "worker", s.direct, livenessFlag+"="+s.direct)
	c.waitReady(b)
	time.Sleep(5 * time.Second) // Looks of the other worker: it must not forget the live one.
	named := false
	for _, l := range strings.Split(a.log.String(), "\n") {
		named = named || (strings.Contains(l, livenessFlag) && strings.Contains(l, "lock"))
	}
	t.Logf("the worker named %s in a line about its lock: %v", livenessFlag, named)
	var rowsOfA int
	pool.QueryRow(context.Background(), "SELECT count(*) FROM transcode_nodes WHERE id = $1", a.workerID).Scan(&rowsOfA) //nolint:errcheck
	if rowsOfA != 1 {
		t.Fatalf("the live worker was forgotten")
	}
	// The next segment the dead job would have made: its row covers it, so the request waits for
	// a job (the lease of 10 s, the binary's default, may have to lapse first) rather than starting one as a seek.
	var next int
	pool.QueryRow(context.Background(), "SELECT next_segment FROM transcode_jobs WHERE state = 'running' AND NOT helper AND worker = $1", a.workerID).Scan(&next) //nolint:errcheck
	a.kill()
	if got := p.segment(min(next, len(p.entries)-1)); !got.isSegment() { // Bounded by the client's one-minute timeout, six leases.
		t.Errorf("the viewer was not served after the worker behind the transaction-mode pooler was killed: %s", got)
	}
	for _, j := range c.rows() {
		if j.State == "failed" {
			t.Errorf("job %d failed: %s", j.ID, j.Error)
		}
	}
	running := 0
	for _, j := range c.rows() {
		if j.State == "running" && !j.Helper && j.Hash == c.rows()[0].Hash {
			running++
		}
	}
	if running > 1 {
		t.Errorf("%d running jobs for the spec", running)
	}
	p.stop()
}
