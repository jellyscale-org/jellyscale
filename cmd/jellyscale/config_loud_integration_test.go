//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/testenv"
)

// JS-109. The real binary, one process each, on a database of its own where
// what the process wrote is looked at. A refusal takes milliseconds; the 20 s
// below only bounds a build that starts serving instead.

const refusalHang = 20 * time.Second

// emptyDatabase creates a database with no table in it and returns its URL
// and a pool on it.
func emptyDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	shared := testenv.DatabaseURL(t)
	admin, err := pgx.Connect(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("js109_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, shared)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(shared)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return u.String(), pool
}

// command is the binary with the args and, when config is not "", a config
// file of that text, on the database dsn. Its PATH holds no ffmpeg.
func command(t *testing.T, dsn string, config string, args ...string) *exec.Cmd {
	t.Helper()
	dir := t.TempDir()
	if config != "" {
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "--config", path)
	}
	cmd := exec.Command(binary(t), append(args, "--listen", freeAddr(t))...)
	cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+dsn, "PATH="+dir)
	return cmd
}

// refused runs the command, which must exit non-zero by itself, and returns
// what it said.
func refused(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() <= 0 {
			t.Errorf("want a non-zero exit, got %v\n%s", err, out.String())
		}
	case <-time.After(refusalHang):
		cmd.Process.Kill() //nolint:errcheck
		<-done
		t.Errorf("the process started and ran for %v instead of refusing:\n%s", refusalHang, out.String())
	}
	return out.String()
}

// tablesIn is how many tables the database has: none if nothing ever migrated
// it, and so nothing was written.
func tablesIn(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog', 'information_schema')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A refused --roles is refused before the process migrates, listens or
// registers, and the roles spelt right in the list are not run.
func TestARefusedRolesTouchesNothingInPostgres(t *testing.T) {
	t.Parallel()
	cases := []struct{ roles, mentions string }{
		{"wokrer", "wokrer"},
		{"api,wokrer,stream", "wokrer"},
		{"", "--roles"},
		{"api,,stream", "--roles"},
		{"api,", "--roles"},
		{",api", "--roles"},
		{" ", "--roles"},
		{"api,all", "all"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.roles), func(t *testing.T) {
			t.Parallel()
			dsn, pool := emptyDatabase(t)
			out := refused(t, command(t, dsn, `{}`, "--roles="+c.roles))
			if !strings.Contains(out, c.mentions) {
				t.Errorf("the output does not name %q:\n%s", c.mentions, out)
			}
			if strings.Contains(out, "listening") || strings.Contains(out, "worker started") {
				t.Errorf("a refused list ran a role:\n%s", out)
			}
			if n := tablesIn(t, pool); n != 0 {
				t.Errorf("the refused start left %d tables in the database (it migrated)", n)
			}
		})
	}
}

// A bad scanInterval is refused before anything is written: no migration, so
// no server id, admin, library or transcode_nodes row either.
func TestABadScanIntervalTouchesNothingInPostgres(t *testing.T) {
	t.Parallel()
	for _, roles := range []string{"api", "stream", "worker", "all"} {
		for _, bad := range []string{`"soon"`, `"2 minutes"`, `"15"`, `15`, `true`, `"0s"`, `"-1h"`} {
			t.Run(roles+"/"+bad, func(t *testing.T) {
				t.Parallel()
				dsn, pool := emptyDatabase(t)
				config := `{"adminUser": "admin", "scanInterval": ` + bad + `, "libraries": [{"name": "Films", "kind": "movies", "media": "file://` + t.TempDir() + `", "assets": "file://` + t.TempDir() + `"}]}`
				cmd := command(t, dsn, config, "--roles="+roles)
				cmd.Env = append(cmd.Env, "JELLYSCALE_ADMIN_PASSWORD=pw")
				out := refused(t, cmd)
				if !strings.Contains(out, "scanInterval") || strings.Contains(out, "unknown field") {
					t.Errorf("the output does not name scanInterval:\n%s", out)
				}
				if n := tablesIn(t, pool); n != 0 {
					t.Errorf("the refused start left %d tables in the database (it migrated)", n)
				}
			})
		}
	}
}

// A process whose roles and scanInterval are good starts: the transcode_nodes
// row it registered has exactly the roles asked for, in the order given, it
// answers /healthz unless it is a worker alone, and says "worker started" when a
// worker is among them.
func startedWith(t *testing.T, dsn, config string, args ...string) *replica {
	t.Helper()
	// The address comes from freeAddr, which closes its listener before the
	// binary binds the port a moment later; with dozens of processes and pools
	// opening connections from the same ephemeral range, another socket can
	// take it in between. That is the test's race, not the server's: start
	// again on a new port, a few times. Any other early exit fails at once.
	var r *replica
	for attempt := 1; ; attempt++ {
		r = startOnce(t, dsn, config, args...)
		if r == nil {
			if attempt >= 5 {
				t.Fatalf("--listen was taken by another socket in %d starts in a row", attempt)
			}
			continue
		}
		return r
	}
}

// startOnce is one start; it returns nil when the process exited because its
// --listen address was already in use.
func startOnce(t *testing.T, dsn, config string, args ...string) *replica {
	t.Helper()
	cmd := command(t, dsn, config, args...)
	r := &replica{t: t, log: &bytes.Buffer{}, exited: make(chan struct{}), cmd: cmd}
	for i, a := range cmd.Args {
		if a == "--listen" {
			r.addr = cmd.Args[i+1]
		}
	}
	cmd.Stderr, cmd.Stdout = &lockedWriter{w: r.log}, &lockedWriter{w: r.log}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { r.status = cmd.Wait(); close(r.exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-r.exited }) //nolint:errcheck
	// The roles the args ask for, as the process should register them: "all" is
	// the three, and a list keeps its order with the spaces around a name gone.
	var asked []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--roles="); ok {
			if v == "all" {
				asked = []string{"api", "stream", "worker"}
				continue
			}
			for _, role := range strings.Split(v, ",") {
				asked = append(asked, strings.TrimSpace(role))
			}
		}
	}
	workerOnly := len(asked) == 1 && asked[0] == "worker"
	wantsWorker := slices.Contains(asked, "worker")
	pool := connect(t, dsn)
	// What the process registered: its row names the roles it runs. Not the log,
	// whose format is the server's own (slog quotes a value only when it has a
	// space, and a worker alone never logs "listening").
	registered := func() string {
		var roles string
		err := pool.QueryRow(context.Background(), "SELECT roles FROM transcode_nodes WHERE id LIKE $1", fmt.Sprintf("%d-%%", cmd.Process.Pid)).Scan(&roles)
		if err != nil {
			return ""
		}
		return roles
	}
	portTaken := false
	waitFor(t, 60*time.Second, "the process to have started", func() bool {
		select {
		case <-r.exited:
			if strings.Contains(r.log.String(), "address already in use") {
				portTaken = true
				return true
			}
			t.Fatalf("the process exited: %v\n%s", r.status, r.log)
		default:
		}
		if wantsWorker && !strings.Contains(r.log.String(), "worker started") {
			return false
		}
		return registered() != "" && (workerOnly || r.status2xx("/healthz"))
	})
	if portTaken {
		return nil
	}
	// A build that drops a listed role (runs only the first, say) is caught here
	// and not by /healthz, which any one HTTP role answers.
	if want := strings.Join(asked, ","); registered() != want {
		t.Errorf("--roles=%v: the transcode_nodes row says roles %q, want %q:\n%s", asked, registered(), want, r.log)
	}
	return r
}

func TestEveryListOfRolesAndGoodScanIntervalStartsAProcess(t *testing.T) {
	t.Parallel()
	for _, roles := range []string{"all", "api", "stream", "worker", "worker,api", " stream , api ", "api,stream,worker", "worker,stream,api"} {
		for _, config := range []string{`{}`, `{"scanInterval": "200ms"}`, `{"scanInterval": "1h"}`} {
			t.Run(roles+"/"+config, func(t *testing.T) {
				t.Parallel()
				dsn, _ := emptyDatabase(t)
				startedWith(t, dsn, config, "--roles="+roles)
			})
		}
	}
}

// messages are the log's msg fields, sorted: what the process said about its
// settings, without the times, addresses and ids that differ between runs.
func messages(log string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`msg=("(?:[^"\\]|\\.)*"|\S+)`).FindAllStringSubmatch(log, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func serverName(t *testing.T, r *replica) string {
	t.Helper()
	response, err := http.Get(r.url("/System/Info/Public"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body bytes.Buffer
	body.ReadFrom(response.Body) //nolint:errcheck
	return regexp.MustCompile(`"ServerName":"[^"]*"`).FindString(body.String())
}

// A process without --config has exactly the settings of one with {}: the same
// server name, and the same things said about its settings at the start.
func TestNoConfigFlagIsTheSameAsAnEmptyConfig(t *testing.T) {
	t.Parallel()
	for _, roles := range []string{"api", "stream", "worker"} {
		t.Run(roles, func(t *testing.T) {
			t.Parallel()
			dsnA, _ := emptyDatabase(t)
			dsnB, _ := emptyDatabase(t)
			without := startedWith(t, dsnA, "", "--roles="+roles)
			with := startedWith(t, dsnB, `{}`, "--roles="+roles)
			time.Sleep(500 * time.Millisecond) // Both have said what they say at the start.
			if a, b := messages(without.log.String()), messages(with.log.String()); strings.Join(a, "\n") != strings.Join(b, "\n") {
				t.Errorf("without --config it said\n%s\nwith {} it said\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
			}
			if roles == "api" { // Only api serves the route.
				if a, b := serverName(t, without), serverName(t, with); a == "" || a != b {
					t.Errorf("server name %q without --config, %q with {}", a, b)
				}
			}
		})
	}
}

// A worker with no config file hides and removes no library, even one whose
// grace is long past: the libraries in the database were kept.
func TestAWorkerWithoutAConfigFileHidesAndRemovesNoLibrary(t *testing.T) {
	t.Parallel()
	dsn, pool := emptyDatabase(t)
	startedWith(t, dsn, `{}`, "--roles=api") // Migrates the database.
	media := t.TempDir()
	ctx := context.Background()
	for i, gone := range []string{"NULL", "now() - interval '8 days'"} {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri, gone_at)
			VALUES (gen_random_uuid(), $1, 'movies', $2, $2, %s)`, gone), fmt.Sprintf("Lib %d", i), "file://"+media+fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() string {
		var s string
		if err := pool.QueryRow(ctx, "SELECT coalesce(string_agg(to_jsonb(l)::text, ',' ORDER BY name), '') FROM libraries l").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := rows()
	w := startedWith(t, dsn, "", "--roles=worker")
	time.Sleep(2 * time.Second) // A tick that has not yet run changes nothing: this can only pass too early, never fail.
	if after := rows(); after != before {
		t.Errorf("the libraries changed:\nbefore %s\nafter  %s", before, after)
	}
	w.sigterm()
	<-w.exited
}

// --- the drain, without --config --------------------------------------------------

// startNoConfig is a replica of the role started without --config, on dsn.
func startNoConfig(t *testing.T, role, dsn string) *replica {
	t.Helper()
	return startedWith(t, dsn, "", "--roles="+role)
}

// Without --config the grace is the documented 30 s, as with {}: readiness
// fails at once, /healthz stays up, and new requests are answered for the lead.
func TestOnSIGTERMWithoutAConfigFileReadinessFailsAndRequestsAreAnsweredForTheLead(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	for _, role := range []string{"api", "stream"} {
		t.Run(role, func(t *testing.T) {
			r := startNoConfig(t, role, dsn)
			path := "/System/Info/Public"
			if role == "stream" {
				path = film(t, pool, tokenFor(t, dsn), 512<<20)
				holdOpen(t, r, path)
			}
			r.sigterm()
			var first time.Time
			waitFor(t, 5*time.Second, "readiness to answer 503 after SIGTERM", func() bool {
				if r.code("/readyz") == 503 {
					first = time.Now()
					return true
				}
				return false
			})
			for time.Since(first) < readinessLead {
				if got := r.code("/readyz"); got != 503 {
					t.Fatalf("readiness answered %d after it had answered 503", got)
				}
				if got := r.code("/healthz"); got < 200 || got > 299 {
					t.Fatalf("/healthz answered %d during the drain", got)
				}
				if got := r.getCode(path, "Range", "bytes=0-99"); got != 200 && got != 206 {
					t.Fatalf("%v after the first 503, a new request was answered %d", time.Since(first).Round(time.Millisecond), got)
				}
				time.Sleep(100 * time.Millisecond)
			}
			// An api replica with nothing in flight is allowed to be gone by now; the lead was served.
		})
	}
}

// A response in flight that ends within the grace arrives whole, and the
// process then exits by itself. (An api replica has no long response to hold.)
func TestWithoutAConfigFileAResponseInFlightIsSentWholeAndTheProcessExits(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	r := startNoConfig(t, "stream", dsn)
	const size = 160 << 20
	path := film(t, pool, tokenFor(t, dsn), size)
	response, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get(r.url(path))
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("direct play: %v %v", response, err)
	}
	defer response.Body.Close()
	buf := make([]byte, 1<<20)
	var got int64
	for got < 8<<20 {
		n, err := response.Body.Read(buf)
		got += int64(n)
		if err != nil {
			t.Fatalf("read %d bytes: %v", got, err)
		}
	}
	r.sigterm()
	for {
		n, err := response.Body.Read(buf)
		got += int64(n)
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("the response was cut after %d of %d bytes: %v", got, int64(size), err)
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got != size {
		t.Fatalf("the response had %d bytes, want %d", got, int64(size))
	}
	select {
	case <-r.exited:
		if r.status != nil {
			t.Errorf("the replica exited with %v", r.status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the replica was still running 15 s after its last response ended")
	}
}

// A response that never ends is still being served 20 s after SIGTERM (the
// documented 30 s grace is not over) and the replica has exited by 45 s.
func TestWithoutAConfigFileAnEndlessResponseOutlastsTwentySecondsAndEndsWithinFortyFive(t *testing.T) {
	t.Parallel()
	dsn := testenv.DatabaseURL(t)
	pool := connect(t, dsn)
	for _, role := range []string{"stream"} {
		r := startNoConfig(t, role, dsn)
		path := film(t, pool, tokenFor(t, dsn), 512<<20)
		headers := holdOpen(t, r, path)
		began := time.Now()
		r.sigterm()
		select {
		case <-r.exited:
			t.Fatalf("the %s replica was gone %v after SIGTERM, with a response that never ends and a grace of 30 s", role, time.Since(began).Round(time.Second))
		case <-time.After(20 * time.Second):
		}
		_ = headers
		select {
		case <-r.exited:
		case <-time.After(25 * time.Second):
			r.cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
			t.Fatalf("the %s replica was still running 45 s after SIGTERM", role)
		}
	}
}
