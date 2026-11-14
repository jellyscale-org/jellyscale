//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/testenv"
)

// These tests run the real binary, one process per replica, on a database of
// their own: another test's libraries in the shared one are what made JS-031's
// parallel tests flake, so a check that read them would flake too.

// What a bound on waiting means: only a hang is caught by it. Where a test lets
// the grace pass, it waits on what the worker did (its scan log lines), and
// where the worker has nothing to log (an empty list) it waits a fixed time
// that a slower machine can only make less than enough, never fail the check
// on: the check is that nothing changed, and a tick that has not yet run
// changes nothing.
const libraryHang = 90 * time.Second

type libCluster struct {
	t     *testing.T
	bin   string
	dsn   string
	pool  *pgxpool.Pool
	media string
}

// newLibCluster builds the binary and creates an empty database for the test.
func newLibCluster(t *testing.T) *libCluster {
	t.Helper()
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "jellyscale")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	shared := testenv.DatabaseURL(t)
	admin, err := pgx.Connect(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("js102_%d", time.Now().UnixNano())
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
	return &libCluster{t: t, bin: bin, dsn: u.String(), pool: pool, media: t.TempDir()}
}

// folder is a library folder, with n one-second films in it.
func (c *libCluster) folder(name string, n int) string {
	c.t.Helper()
	dir := filepath.Join(c.media, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.t.Fatal(err)
	}
	for i := range n {
		out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i",
			fmt.Sprintf("testsrc2=size=160x90:rate=10:duration=%d", i+1), "-c:v", "libx264", "-preset", "ultrafast",
			filepath.Join(dir, fmt.Sprintf("%s %c (2020).mkv", name, 'A'+i))).CombinedOutput()
		if err != nil {
			c.t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	return dir
}

// entry is a library's line in a config.
func entry(name, dir string, chapterImages bool) map[string]any {
	return map[string]any{"name": name, "kind": "movies", "media": "file://" + dir, "assets": "file://" + dir, "chapterImages": chapterImages}
}

type process struct {
	c     *libCluster
	cmd   *exec.Cmd
	port  int
	token string
	mu    sync.Mutex
	log   bytes.Buffer
	done  chan struct{}
}

func (p *process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.Write(b)
}

func (p *process) logged() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.String()
}

func (p *process) count(text string) int { return strings.Count(p.logged(), text) }

// start runs the binary with the roles. config is the file's text; "" is no file.
// It returns once the process is serving. That is not proof that a worker has
// synced: where it logs "worker started" relative to library.Sync is the
// build's to choose, so a test that reads what Sync wrote uses syncedStart.
func (c *libCluster) start(roles, config string) *process {
	c.t.Helper()
	p := c.launch(roles, config)
	p.ready()
	return p
}

func (c *libCluster) launch(roles, config string) *process {
	c.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		c.t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	args := []string{"--roles", roles, "--listen", fmt.Sprintf("127.0.0.1:%d", port)}
	if config != "" {
		path := filepath.Join(c.t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			c.t.Fatal(err)
		}
		args = append(args, "--config", path)
	}
	p := &process{c: c, port: port, done: make(chan struct{})}
	p.cmd = exec.Command(c.bin, args...)
	p.cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+c.dsn, "JELLYSCALE_ADMIN_PASSWORD=pw-js102")
	p.cmd.Stderr, p.cmd.Stdout = p, p
	if err := p.cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	go func() { p.cmd.Wait(); close(p.done) }()
	c.t.Cleanup(p.stop)
	return p
}

func (p *process) stop() {
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
	}
}

// ready waits for the process to have started: a worker logs that, an api or
// stream replica answers /healthz. It says nothing about when Sync ran.
func (p *process) ready() {
	p.c.t.Helper()
	deadline := time.Now().Add(libraryHang)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			p.c.t.Fatalf("a process exited at start:\n%s", p.logged())
		default:
		}
		if strings.Contains(p.logged(), "worker started") {
			return
		}
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", p.port)); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 204 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.c.t.Fatalf("a process did not start:\n%s", p.logged())
}

// get asks an api replica, signed in as the admin the first worker's config made.
func (p *process) get(path string, into any) {
	p.c.t.Helper()
	if p.token == "" {
		body, _ := json.Marshal(map[string]string{"Username": "admin", "Pw": "pw-js102"})
		req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/Users/AuthenticateByName", p.port), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", `MediaBrowser Client="c", Device="d", DeviceId="i", Version="1"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			p.c.t.Fatal(err)
		}
		defer resp.Body.Close()
		var auth struct{ AccessToken string }
		if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil || auth.AccessToken == "" {
			p.c.t.Fatalf("no token (status %d): %v", resp.StatusCode, err)
		}
		p.token = auth.AccessToken
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", p.port, path), nil)
	req.Header.Set("X-Emby-Token", p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		p.c.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		p.c.t.Fatal(err)
	}
}

// views is what /UserViews lists, by name with its id.
func (p *process) views() map[string]string {
	p.c.t.Helper()
	var out struct{ Items []struct{ Name, Id string } }
	p.get("/UserViews", &out)
	views := map[string]string{}
	for _, item := range out.Items {
		views[item.Name] = item.Id
	}
	return views
}

// films is how many films the replica lists in a library, or in all of them for "".
func (p *process) films(libraryID string) int {
	p.c.t.Helper()
	path := "/Items?Recursive=true&IncludeItemTypes=Movie"
	if libraryID != "" {
		path += "&ParentId=" + libraryID
	}
	var out struct{ TotalRecordCount int }
	p.get(path, &out)
	return out.TotalRecordCount
}

func (c *libCluster) text(sql string, args ...any) []string {
	c.t.Helper()
	rows, err := c.pool.Query(context.Background(), sql, args...)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (c *libCluster) count(sql string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// libraryRows is every column of every library row, which is what "writes
// nothing to the libraries" is measured against.
func (c *libCluster) libraryRows() []string {
	return c.text("SELECT to_jsonb(l)::text FROM libraries l ORDER BY id")
}

func (c *libCluster) exec(sql string, args ...any) {
	c.t.Helper()
	if _, err := c.pool.Exec(context.Background(), sql, args...); err != nil {
		c.t.Fatal(err)
	}
}

// seedWatchState gives the admin a row on every item, so a removal shows in a count.
func (c *libCluster) seedWatchState() {
	c.t.Helper()
	c.exec(`INSERT INTO user_item_data (user_id, item_id, played, favorite, play_count, position_ticks, last_played_at)
		SELECT (SELECT id FROM users WHERE name = 'admin'), id, true, true, 2, 1000, now() FROM items ON CONFLICT DO NOTHING`)
}

func configOf(libraries []map[string]any, extra string) string {
	raw, _ := json.Marshal(libraries)
	return fmt.Sprintf(`{"adminUser": "admin", "libraries": %s%s}`, raw, extra)
}

// waitScans waits until the process has logged n more finished scans than it had.
func (p *process) waitScans(n int) {
	p.c.t.Helper()
	want := p.count("scan finished") + n
	deadline := time.Now().Add(libraryHang)
	for p.count("scan finished") < want {
		if time.Now().After(deadline) {
			p.c.t.Fatalf("waiting for %d scans:\n%s", n, p.logged())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncedStart starts a worker with a non-empty list and returns once it has
// finished a scan of each of its n libraries. The scan loop runs on what Sync
// returned, so Sync has written by then wherever the build calls it.
func (c *libCluster) syncedStart(roles, config string, n int) *process {
	c.t.Helper()
	p := c.start(roles, config)
	p.waitScans(n - p.count("scan finished"))
	return p
}

// seeded starts a worker over three libraries, lets it scan them, and stops it:
// Alpha and Beta have films and Gamma has none. Gamma is then hidden, as if an
// earlier config had left it out, 8 days ago, which is past the default grace.
func (c *libCluster) seeded() (alpha, beta, gamma string) {
	c.t.Helper()
	alpha, beta, gamma = c.folder("Alpha", 2), c.folder("Beta", 3), c.folder("Gamma", 0)
	w := c.start("worker", configOf([]map[string]any{entry("Alpha", alpha, false), entry("Beta", beta, false), entry("Gamma", gamma, false)}, ""))
	w.waitScans(3)
	w.stop()
	c.seedWatchState()
	c.exec("UPDATE libraries SET gone_at = now() - interval '8 days' WHERE name = 'Gamma'")
	if got := c.count("SELECT count(*) FROM items"); got != 5 {
		c.t.Fatalf("%d items scanned", got)
	}
	return
}

// A process without the worker role writes nothing to the libraries, whatever
// its config lists: no file, no key, an empty list, a subset of the worker's
// list (with options that differ), a library under another name for a store
// the worker lists, and one the database lacks. Gamma is hidden, and a list
// that has it must not show it again.
func TestAProcessWithoutTheWorkerRoleWritesNothingToTheLibraries(t *testing.T) {
	c := newLibCluster(t)
	alpha, _, gamma := c.seeded()
	other := c.folder("Delta", 0)
	before := c.libraryRows()
	items, state := c.count("SELECT count(*) FROM items"), c.count("SELECT count(*) FROM user_item_data")
	if len(before) != 3 || items == 0 || state == 0 {
		t.Fatalf("seed: %d libraries, %d items, %d watch rows", len(before), items, state)
	}
	lists := []struct{ name, config string }{
		{"no config file", ""},
		{"no libraries key", `{}`},
		{"an empty list", `{"libraries": []}`},
		{"a subset", configOf([]map[string]any{entry("Alpha", other, true), entry("Gamma", gamma, true)}, "")},
		{"another name for a store the worker lists", configOf([]map[string]any{entry("Alpha renamed", alpha, true)}, "")},
		{"a library the database lacks", configOf([]map[string]any{entry("Delta", other, true)}, "")},
	}
	for _, roles := range []string{"api", "stream", "api,stream"} {
		for _, list := range lists {
			t.Run(roles+"/"+list.name, func(t *testing.T) {
				p := c.start(roles, list.config)
				// A replica is asked something, so that it is known to be serving.
				time.Sleep(300 * time.Millisecond)
				p.stop()
				if after := c.libraryRows(); strings.Join(after, "\n") != strings.Join(before, "\n") {
					t.Errorf("the libraries changed:\nbefore %v\nafter  %v", before, after)
				}
				if n := c.count("SELECT count(*) FROM items"); n != items {
					t.Errorf("%d items, were %d", n, items)
				}
				if n := c.count("SELECT count(*) FROM user_item_data"); n != state {
					t.Errorf("%d watch rows, were %d", n, state)
				}
				if strings.Contains(strings.ToLower(p.logged()), "no libraries") {
					t.Errorf("a process without the worker role logs about an empty list:\n%s", p.logged())
				}
			})
		}
	}
}

// With a worker listing two libraries, an api replica with an empty list and a
// stream replica with a subset, started after it and at the same instant, leave
// every library and item in what the api replica lists, with no worker restart;
// and they still do after the grace has passed and the worker has ticked.
func TestReplicasWithOtherListsStartedBesideAWorkerHideNothingEvenAfterTheGrace(t *testing.T) {
	c := newLibCluster(t)
	alpha, beta := c.folder("Alpha", 2), c.folder("Beta", 3)
	// A grace of a second and a tick of 200 ms are what the production 7 days
	// and 15 minutes are made to look like here; they could hide a removal that
	// needs the grace to be longer than a scan takes, which the worker's own
	// scans (each a few hundred ms) already exceed.
	worker := c.start("worker", configOf([]map[string]any{entry("Alpha", alpha, false), entry("Beta", beta, false)}, `, "missingGrace": "1s", "scanInterval": "200ms"`))
	worker.waitScans(2)
	c.seedWatchState()
	libraries, items, state := c.libraryRows(), c.count("SELECT count(*) FROM items"), c.count("SELECT count(*) FROM user_item_data")
	if len(libraries) != 2 || items != 5 || state != 5 {
		t.Fatalf("seed: %d libraries, %d items, %d watch rows", len(libraries), items, state)
	}

	var api, stream *process
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); api = c.launch("api", `{"libraries": []}`) }()
	go func() {
		defer wg.Done()
		stream = c.launch("stream", configOf([]map[string]any{entry("Alpha", alpha, false)}, ""))
	}()
	wg.Wait()
	api.ready()
	stream.ready()

	check := func(when string) {
		t.Helper()
		views := api.views()
		if len(views) != 2 || views["Alpha"] == "" || views["Beta"] == "" {
			t.Fatalf("%s: /UserViews lists %v, want Alpha and Beta", when, views)
		}
		if n := api.films(views["Alpha"]); n != 2 {
			t.Errorf("%s: Alpha lists %d films, want 2", when, n)
		}
		if n := api.films(views["Beta"]); n != 3 {
			t.Errorf("%s: Beta lists %d films, want 3", when, n)
		}
		if n := api.films(""); n != 5 {
			t.Errorf("%s: %d films in all, want 5", when, n)
		}
		if n := c.count("SELECT count(*) FROM libraries WHERE gone_at IS NOT NULL"); n != 0 {
			t.Errorf("%s: %d libraries have a grace running", when, n)
		}
		if n := c.count("SELECT count(*) FROM items"); n != items {
			t.Errorf("%s: %d items, were %d", when, n, items)
		}
		if n := c.count("SELECT count(*) FROM user_item_data"); n != state {
			t.Errorf("%s: %d watch rows, were %d", when, n, state)
		}
	}
	check("after the replicas started")

	// Past the grace, then a whole scan loop of the worker's after it.
	time.Sleep(1500 * time.Millisecond)
	worker.waitScans(4)
	check("after the grace and a worker tick")
	if got := c.libraryRows(); len(got) != len(libraries) {
		t.Errorf("libraries now %v, were %v", got, libraries)
	}
}

// A worker alone or beside others still syncs a non-empty list as before:
// it adds a library, keeps a renamed one's id, hides one left out, shows it
// again when put back, and removes it once the grace has passed since the first
// worker start without it, which a later start does not move. (These pass
// before the change; they hold the behaviour the change must keep.)
func TestAWorkerStillSyncsANonEmptyList(t *testing.T) {
	for _, roles := range []string{"worker", "api,worker", "all"} {
		t.Run(roles, func(t *testing.T) {
			c := newLibCluster(t)
			a, b := c.folder("Alpha", 1), c.folder("Beta", 0)
			added := c.folder("Fresh", 0)
			w := c.syncedStart(roles, configOf([]map[string]any{entry("Alpha", a, false), entry("Beta", b, false)}, ""), 2)
			w.stop()
			ids := map[string]string{}
			for _, row := range c.text("SELECT name || '|' || id FROM libraries") {
				name, id, _ := strings.Cut(row, "|")
				ids[name] = id
			}
			if len(ids) != 2 {
				t.Fatalf("libraries %v", ids)
			}

			// Alpha renamed, Beta left out, Fresh new.
			w = c.syncedStart(roles, configOf([]map[string]any{entry("Alpha two", a, false), entry("Fresh", added, false)}, ""), 2)
			w.stop()
			if got := c.text("SELECT name FROM libraries WHERE id = $1", ids["Alpha"]); len(got) != 1 || got[0] != "Alpha two" {
				t.Errorf("the renamed library: %v", got)
			}
			if n := c.count("SELECT count(*) FROM libraries WHERE name = 'Fresh' AND gone_at IS NULL"); n != 1 {
				t.Errorf("the new library is not added and shown")
			}
			hiddenAt := c.text("SELECT gone_at::text FROM libraries WHERE id = $1", ids["Beta"])
			if len(hiddenAt) != 1 || hiddenAt[0] == "" {
				t.Fatalf("the library left out is not hidden: %v", hiddenAt)
			}

			// Put back: shown again.
			w = c.syncedStart(roles, configOf([]map[string]any{entry("Alpha two", a, false), entry("Beta", b, false), entry("Fresh", added, false)}, ""), 3)
			w.stop()
			if n := c.count("SELECT count(*) FROM libraries WHERE id = $1 AND gone_at IS NULL", ids["Beta"]); n != 1 {
				t.Errorf("the library put back is not shown again")
			}

			// Left out again, twice: the second start keeps the first's time.
			without := configOf([]map[string]any{entry("Alpha two", a, false), entry("Fresh", added, false)}, "")
			c.syncedStart(roles, without, 2).stop()
			first := c.text("SELECT gone_at::text FROM libraries WHERE id = $1", ids["Beta"])
			c.syncedStart(roles, without, 2).stop()
			if again := c.text("SELECT gone_at::text FROM libraries WHERE id = $1", ids["Beta"]); len(first) != 1 || first[0] == "" || len(again) != 1 || again[0] != first[0] {
				t.Errorf("a later worker start moved the grace: %v then %v", first, again)
			}

			// A worker with a short grace removes it on a tick.
			c.start(roles, configOf([]map[string]any{entry("Alpha two", a, false), entry("Fresh", added, false)}, `, "missingGrace": "1s", "scanInterval": "200ms"`))
			deadline := time.Now().Add(libraryHang)
			for c.count("SELECT count(*) FROM libraries WHERE id = $1", ids["Beta"]) != 0 {
				if time.Now().After(deadline) {
					t.Fatal("the library left out was not removed after the grace")
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

// A worker whose config lists no libraries changes none: all stay in
// /UserViews with their items and watch state, a library an earlier config
// hid stays hidden with the grace it had (it is past the default grace here,
// and is not removed either), and the worker says so once, not on every tick.
func TestAWorkerListingNoLibrariesChangesNothingAndSaysSoOnce(t *testing.T) {
	lists := []struct{ name, config string }{
		{"no config file", ""},
		{"no libraries key", `{"adminUser": "admin", "missingGrace": "1s", "scanInterval": "200ms"}`},
		{"an empty list", `{"adminUser": "admin", "libraries": [], "missingGrace": "1s", "scanInterval": "200ms"}`},
	}
	for _, roles := range []string{"worker", "all"} {
		for _, list := range lists {
			t.Run(roles+"/"+list.name, func(t *testing.T) {
				c := newLibCluster(t)
				c.seeded()
				before := c.libraryRows()
				items, state := c.count("SELECT count(*) FROM items"), c.count("SELECT count(*) FROM user_item_data")
				if len(before) != 3 || items == 0 || state == 0 {
					t.Fatalf("seed: %d libraries, %d items, %d watch rows", len(before), items, state)
				}
				w := c.start(roles, list.config)
				// The tick runs at the start; with the 200 ms ticks of the configs
				// that have a file, several more follow. There is nothing a
				// worker with no libraries logs per tick, so a fixed wait is all
				// that lets one run (see libraryHang).
				time.Sleep(3 * time.Second)
				if after := c.libraryRows(); strings.Join(after, "\n") != strings.Join(before, "\n") {
					t.Errorf("the libraries changed:\nbefore %v\nafter  %v", before, after)
				}
				if n := c.count("SELECT count(*) FROM items"); n != items {
					t.Errorf("%d items, were %d", n, items)
				}
				if n := c.count("SELECT count(*) FROM user_item_data"); n != state {
					t.Errorf("%d watch rows, were %d", n, state)
				}
				if roles == "all" {
					views := w.views()
					if len(views) != 2 || views["Alpha"] == "" || views["Beta"] == "" {
						t.Errorf("/UserViews lists %v, want Alpha and Beta (Gamma stays hidden)", views)
					}
					if n := w.films(""); n != 5 {
						t.Errorf("%d films listed, want 5", n)
					}
				}
				// One line, naming both halves of what the criterion asks it to say.
				var lines []string
				for _, line := range strings.Split(w.logged(), "\n") {
					lower := strings.ToLower(line)
					if strings.Contains(lower, "no libraries") && strings.Contains(lower, "kept") {
						lines = append(lines, line)
					}
				}
				if len(lines) != 1 {
					t.Errorf("%d log lines say the config lists no libraries and the database's were kept, want 1:\n%s", len(lines), w.logged())
				}
			})
		}
	}
}
