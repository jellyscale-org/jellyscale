//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/testenv"
)

// JS-082: a worker claims only jobs of an output version it makes. These tests
// run real processes of two builds of the binary, the same source with
// OutputVersion changed (go build -overlay replaces the constant; the server
// gains no flag, environment variable or config key for it), on one Postgres
// and one transcode store, and ask them for segments through the HTTP API as a
// player does.
//
// They name one thing the task implies and the build must add: the column
// transcode_jobs.output_version (integer, NOT NULL, DEFAULT 4).
//
// Timing. The viewer's wait is the production queueTimeout of 6 seconds
// (internal/transcode.queueTimeout, unexported). Nothing here asserts that
// something was fast: where a check waits for something to happen it waits at
// most a minute, which only bounds a hang, and the one lower bound (a refusal
// comes no sooner than queueTimeout) cannot fail on a slow machine. The one
// place a start must beat queueTimeout is a worker process that starts while a
// viewer's request waits; on a runner that takes more than about 5 seconds to
// start a process, that check fails, and the fix is the runner.

const (
	queueTimeout = 6 * time.Second // As internal/transcode.queueTimeout.
	oldVersion   = 4               // The last version before rows recorded one.
	newVersion   = 5
	hang         = time.Minute
)

// versionBuilds are the two binaries, built once.
var (
	buildsOnce sync.Once
	builds     map[int]string
	buildsErr  error
	filmFile   string
)

// buildWith builds the binary with OutputVersion set to version, by replacing
// internal/playback/spec.go in the build alone.
func buildWith(dir string, version int) (string, error) {
	spec, err := os.ReadFile("../../internal/playback/spec.go")
	if err != nil {
		return "", err
	}
	pattern := regexp.MustCompile(`(?m)^(\s*OutputVersion\s*=\s*)\d+`)
	if !pattern.Match(spec) {
		return "", errors.New("internal/playback/spec.go has no OutputVersion constant to change")
	}
	changed := filepath.Join(dir, fmt.Sprintf("spec-%d.go", version))
	if err := os.WriteFile(changed, pattern.ReplaceAll(spec, []byte("${1}"+strconv.Itoa(version))), 0o644); err != nil {
		return "", err
	}
	original, err := filepath.Abs("../../internal/playback/spec.go")
	if err != nil {
		return "", err
	}
	overlay, _ := json.Marshal(map[string]map[string]string{"Replace": {original: changed}})
	overlayFile := filepath.Join(dir, fmt.Sprintf("overlay-%d.json", version))
	if err := os.WriteFile(overlayFile, overlay, 0o644); err != nil {
		return "", err
	}
	binary := filepath.Join(dir, fmt.Sprintf("jellyscale-v%d", version))
	out, err := exec.Command("go", "build", "-overlay", overlayFile, "-o", binary, ".").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build with OutputVersion %d: %v\n%s", version, err, out)
	}
	return binary, nil
}

// syncBuffer is a log that a process and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// proc is one running jellyscale process.
type proc struct {
	cmd      *exec.Cmd
	version  int
	roles    string
	addr     string // Of a process with api or stream.
	workerID string // Of a process with a worker.
	log      *syncBuffer
	done     chan struct{}
}

func (p *proc) name() string { return fmt.Sprintf("v%d %s", p.version, p.roles) }

// stop ends the process as an orchestrator does, with SIGTERM, and kills it if
// it has not gone after the hang bound.
func (p *proc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	select {
	case <-p.done:
	case <-time.After(hang):
		p.cmd.Process.Kill() //nolint:errcheck
		<-p.done
	}
}

type cluster struct {
	t       *testing.T
	dsn     string
	pool    *pgxpool.Pool
	dir     string
	lead    string
	slow    bool
	slots   int    // transcodeSlots of the next process started; 0 is the default.
	extra   string // Config keys of the next process started, each with a trailing comma (JS-087: transcodeCores).
	procs   []*proc
	workers map[string]int // Worker ID to the version of its build.
	item    string
	token   string
}

// newCluster is an empty cluster on the test Postgres: its tables emptied, its
// own transcode store. slow makes the workers' ffmpeg read at the film's speed,
// so that a job is slower than 1.2 times real time and asks for helpers.
func newCluster(t *testing.T, slow bool, lead string) *cluster {
	t.Helper()
	ensureBuilds(t)
	c := &cluster{t: t, dsn: testenv.DatabaseURL(t), dir: t.TempDir(), lead: lead, slow: slow, workers: map[string]int{}}
	// db.Open migrates, so the test passes alone on a fresh database too.
	pool, err := db.Open(context.Background(), c.dsn)
	if err != nil {
		t.Fatal(err)
	}
	c.pool = pool
	t.Cleanup(func() {
		for i := len(c.procs) - 1; i >= 0; i-- {
			c.procs[i].stop()
		}
		pool.Close()
	})
	if _, err := pool.Exec(context.Background(), "TRUNCATE libraries, users, transcode_jobs, transcode_viewers, transcode_nodes, subtitle_jobs, chapter_images CASCADE"); err != nil {
		t.Fatal(err)
	}
	movie := filepath.Join(c.dir, "media", "Film (2020)")
	if err := os.MkdirAll(movie, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filmFile, filepath.Join(movie, "Film (2020).mkv")); err != nil {
		t.Fatal(err)
	}
	// The real ffmpeg, behind a shim that paces the segments' runs when asked.
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(c.dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pace := ""
	if slow {
		pace = "-readrate 1"
	}
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n*\"-f hls\"*) exec %s %s \"$@\";;\nesac\nexec %s \"$@\"\n", real, pace, real)
	if err := os.WriteFile(filepath.Join(c.dir, "bin", "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Replicas have no ffmpeg on their PATH, as in production.
	if err := os.MkdirAll(filepath.Join(c.dir, "nobin"), 0o755); err != nil {
		t.Fatal(err)
	}
	return c
}

// logs is every process's output, for a failure's message.
func (c *cluster) logs() string {
	var all strings.Builder
	for _, p := range c.procs {
		fmt.Fprintf(&all, "--- %s\n%s\n", p.name(), p.log.String())
	}
	return all.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// start runs a process of the build with the version, with the roles. It
// returns when the process is serving (a replica) or registered (a worker).
func (c *cluster) start(version int, roles string) *proc {
	c.t.Helper()
	p := c.launch(version, roles)
	c.waitReady(p)
	return p
}

// launch starts the process without waiting for it.
func (c *cluster) launch(version int, roles string) *proc {
	c.t.Helper()
	n := len(c.procs)
	slots := ""
	if c.slots > 0 {
		slots = fmt.Sprintf(`"transcodeSlots": %d,`, c.slots)
	}
	slots += c.extra
	config := fmt.Sprintf(`{"name": "versions", "adminUser": "admin", "scanInterval": "1h", %s
 "transcodeStore": "file://%s/segments", "transcodeScratch": "%s/scratch-%d", "transcodeIdle": "60s", "transcodeLead": %q,
 "libraries": [{"name": "Movies", "kind": "movies", "media": "file://%s/media", "assets": "file://%s/assets"}]}`,
		slots, c.dir, c.dir, n, c.lead, c.dir, c.dir)
	configFile := filepath.Join(c.dir, fmt.Sprintf("config-%d.json", n))
	if err := os.WriteFile(configFile, []byte(config), 0o644); err != nil {
		c.t.Fatal(err)
	}
	p := &proc{version: version, roles: roles, log: &syncBuffer{}, done: make(chan struct{})}
	args := []string{"--roles=" + roles, "--config", configFile}
	if strings.Contains(roles, "api") || strings.Contains(roles, "stream") {
		p.addr = freeAddr(c.t)
		args = append(args, "--listen", p.addr)
	}
	p.cmd = exec.Command(builds[version], args...)
	// Replicas have no ffmpeg on their PATH, as in production; workers get the shim.
	path := filepath.Join(c.dir, "bin") + ":" + os.Getenv("PATH")
	if p.addr != "" {
		path = filepath.Join(c.dir, "nobin")
	}
	p.cmd.Env = append(os.Environ(), "JELLYSCALE_DATABASE_URL="+c.dsn, "JELLYSCALE_ADMIN_PASSWORD=r-pass", "PATH="+path)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	go func() {
		p.cmd.Wait() //nolint:errcheck
		close(p.done)
	}()
	c.procs = append(c.procs, p)
	return p
}

var workerStarted = regexp.MustCompile(`worker started.*worker=(\S+)`)

func (c *cluster) waitReady(p *proc) {
	c.t.Helper()
	deadline := time.Now().Add(hang)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			c.t.Fatalf("%s exited at startup:\n%s", p.name(), p.log.String())
		default:
		}
		ready := true
		if p.addr != "" {
			resp, err := http.Get("http://" + p.addr + "/healthz")
			ready = err == nil && resp.StatusCode == 204
			if err == nil {
				resp.Body.Close()
			}
		}
		if strings.Contains(p.roles, "worker") {
			m := workerStarted.FindStringSubmatch(p.log.String())
			ready = false
			if m != nil {
				p.workerID = m[1]
				c.workers[p.workerID] = p.version
				var slots int
				c.pool.QueryRow(context.Background(), "SELECT COALESCE(max(slots), 0) FROM transcode_nodes WHERE id = $1", p.workerID).Scan(&slots) //nolint:errcheck
				ready = slots > 0
			}
		}
		if ready {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("%s did not come up:\n%s", p.name(), p.log.String())
}

// call is an HTTP request to a replica, with the token once there is one.
func (c *cluster) call(method string, p *proc, path string, body string, header ...string) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(method, "http://"+p.addr+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("X-Emby-Token", c.token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{Timeout: hang}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data, err
}

// login signs in at an api replica and finds the film, once the scan has
// listed it.
func (c *cluster) login(api *proc) {
	c.t.Helper()
	if c.token != "" {
		return
	}
	code, _, body, err := c.call("POST", api, "/Users/AuthenticateByName", `{"Username":"admin","Pw":"r-pass"}`,
		"Authorization", `MediaBrowser Client="c", Device="d", DeviceId="i", Version="1"`)
	if err != nil || code != 200 {
		c.t.Fatalf("sign in: %d %v %s", code, err, body)
	}
	var auth struct{ AccessToken string }
	if err := json.Unmarshal(body, &auth); err != nil {
		c.t.Fatal(err)
	}
	c.token = auth.AccessToken
	deadline := time.Now().Add(hang)
	for time.Now().Before(deadline) {
		_, _, body, _ := c.call("GET", api, "/Items?Recursive=true&IncludeItemTypes=Movie&Limit=1", "")
		var items struct{ Items []struct{ Id string } }
		if json.Unmarshal(body, &items) == nil && len(items.Items) == 1 {
			c.item = items.Items[0].Id
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatalf("the film was not listed; the logs:\n%s", c.logs())
}

// playing is one viewer's playback: a PlaybackInfo at an api replica, and the
// HLS playlists of a stream replica.
type playing struct {
	c       *cluster
	api     *proc
	stream  *proc
	psid    string
	base    string
	entries []string // The main playlist's lines that are not tags.
}

// startPlayback is what a player does before its first segment.
func (c *cluster) startPlayback(api, stream *proc) *playing {
	c.t.Helper()
	return c.startPlaybackAt(api, stream, 2147483647)
}

// startPlaybackAt is startPlayback with a streaming bitrate cap. A different
// cap is a different spec (the video bitrate is in the hash), so a viewer of
// each cap has a job of their own.
func (c *cluster) startPlaybackAt(api, stream *proc, maxBitrate int) *playing {
	c.t.Helper()
	c.login(api)
	profile, err := os.ReadFile("../../compat/captures/transcode-12.4.json")
	if err != nil {
		c.t.Fatal(err)
	}
	var capture struct{ DeviceProfile json.RawMessage }
	if err := json.Unmarshal(profile, &capture); err != nil {
		c.t.Fatal(err)
	}
	code, _, body, err := c.call("POST", api, "/Items/"+c.item+"/PlaybackInfo",
		fmt.Sprintf(`{"DeviceProfile":%s,"MaxStreamingBitrate":%d,"AudioStreamIndex":"1"}`, capture.DeviceProfile, maxBitrate))
	if err != nil || code != 200 {
		c.t.Fatalf("PlaybackInfo at %s: %d %v %s", api.name(), code, err, body)
	}
	var info struct {
		PlaySessionId string
		MediaSources  []struct{ TranscodingUrl string }
		ErrorCode     string
	}
	if err := json.Unmarshal(body, &info); err != nil || len(info.MediaSources) == 0 || info.MediaSources[0].TranscodingUrl == "" {
		c.t.Fatalf("PlaybackInfo at %s offers no transcode (%q): %s", api.name(), info.ErrorCode, body)
	}
	url := info.MediaSources[0].TranscodingUrl
	p := &playing{c: c, api: api, stream: stream, psid: info.PlaySessionId, base: url[:strings.Index(url, "/master.m3u8")]}
	_, _, master, err := c.call("GET", stream, url, "")
	if err != nil {
		c.t.Fatal(err)
	}
	var main string
	for _, line := range strings.Split(string(master), "\n") {
		if strings.HasPrefix(line, "main.m3u8") {
			main = line
		}
	}
	if main == "" {
		c.t.Fatalf("the master playlist at %s names no main playlist: %s", stream.name(), master)
	}
	_, _, playlist, err := c.call("GET", stream, p.base+"/"+main, "")
	if err != nil {
		c.t.Fatal(err)
	}
	for _, line := range strings.Split(string(playlist), "\n") {
		if strings.HasPrefix(line, "hls1/main/") {
			p.entries = append(p.entries, line)
		}
	}
	if len(p.entries) < 40 {
		c.t.Fatalf("the film has %d segments, the tests want at least 40: %s", len(p.entries), playlist)
	}
	return p
}

// answer is what a request for a segment got.
type answer struct {
	code       int
	retryAfter string
	body       []byte
	took       time.Duration
	err        error
}

func (a answer) isSegment() bool {
	return a.code == 200 && len(a.body) > 1000 && bytes.Contains(a.body[:16], []byte("styp"))
}

func (a answer) String() string {
	return fmt.Sprintf("%d (%d bytes, Retry-After %q, %v) %v", a.code, len(a.body), a.retryAfter, a.took.Round(time.Millisecond), a.err)
}

func (p *playing) segment(n int) answer {
	began := time.Now()
	code, header, body, err := p.c.call("GET", p.stream, p.base+"/"+p.entries[n], "")
	return answer{code, header.Get("Retry-After"), body, time.Since(began), err}
}

// stop ends the playback as jellyfin-web does when it leaves.
func (p *playing) stop() {
	p.c.call("DELETE", p.stream, "/Videos/ActiveEncodings?deviceId=i&PlaySessionId="+p.psid, "") //nolint:errcheck
}

// row is a transcode_jobs row.
type row struct {
	ID                         int64
	Hash, Owner, State, Worker string
	Helper                     bool
	Version                    int
	Error                      string
	Start                      int
}

func (c *cluster) rows() []row {
	c.t.Helper()
	r, err := c.pool.Query(context.Background(), `SELECT id, spec_hash, owner, state, COALESCE(worker, ''), helper, output_version, COALESCE(error, ''), start_segment
		FROM transcode_jobs ORDER BY id`)
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Close()
	var rows []row
	for r.Next() {
		var j row
		if err := r.Scan(&j.ID, &j.Hash, &j.Owner, &j.State, &j.Worker, &j.Helper, &j.Version, &j.Error, &j.Start); err != nil {
			c.t.Fatal(err)
		}
		rows = append(rows, j)
	}
	return rows
}

// rowOf is the viewer's own job.
func (c *cluster) rowOf(p *playing) (row, bool) {
	for _, j := range c.rows() {
		if j.Owner == p.psid && !j.Helper {
			return j, true
		}
	}
	return row{}, false
}

// checkRan fails the test for every job a worker of another version ran.
func (c *cluster) checkRan() {
	c.t.Helper()
	for _, j := range c.rows() {
		if j.Worker == "" {
			continue
		}
		version, known := c.workers[j.Worker]
		if !known {
			c.t.Errorf("job %d was run by worker %s, which this test did not start", j.ID, j.Worker)
		} else if version != j.Version {
			c.t.Errorf("job %d is version %d and was run by a worker of version %d", j.ID, j.Version, version)
		}
	}
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(hang)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("gave up after %v waiting for %s", hang, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// storedUnder says whether the transcode store holds anything under the spec's
// hash.
func (c *cluster) storedUnder(hash string) bool {
	entries, err := os.ReadDir(filepath.Join(c.dir, "segments", "segments", hash))
	return err == nil && len(entries) > 0
}

// ensureBuilds makes the film and the two binaries once for the package, in a
// directory that outlives any one test (TMPDIR, which test/integration.sh
// removes), so a test that starts a cluster passes in any order.
func ensureBuilds(t *testing.T) {
	t.Helper()
	testenv.FFmpeg(t)
	buildsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "jellyscale-builds-")
		if err != nil {
			buildsErr = err
			return
		}
		// A film no client plays as it is (MPEG-4 video), so that every play is a
		// re-encode, which a helper can help.
		filmFile = filepath.Join(dir, "film.mkv")
		out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=240",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=240",
			"-c:v", "mpeg4", "-q:v", "6", "-g", "96", "-c:a", "aac", filmFile).CombinedOutput()
		if err != nil {
			buildsErr = fmt.Errorf("the film: %v\n%s", err, out)
			return
		}
		built := map[int]string{}
		for _, v := range []int{oldVersion, newVersion} {
			if built[v], err = buildWith(dir, v); err != nil {
				buildsErr = err
				return
			}
		}
		builds = built
	})
	if buildsErr != nil {
		t.Fatal(buildsErr)
	}
}

func TestOutputVersionAcrossProcesses(t *testing.T) {
	testenv.DatabaseURL(t)
	ensureBuilds(t)

	// Viewers on both replicas get their segments, and each job's version is
	// that of the replica that queued it and of the worker that ran it.
	t.Run("a mixed cluster serves viewers on both replicas and each job is run by a worker of its replica's version", func(t *testing.T) {
		c := newCluster(t, false, "20s")
		w4, w5 := c.start(oldVersion, "worker"), c.start(newVersion, "worker")
		r4, r5 := c.start(oldVersion, "api,stream"), c.start(newVersion, "api,stream")
		_, _ = w4, w5
		c.login(r4)
		var wg sync.WaitGroup
		type played struct {
			p       *playing
			replica *proc
			answers []answer
		}
		var all []*played
		for i := 0; i < 4; i++ {
			replica := []*proc{r4, r5}[i%2]
			all = append(all, &played{p: c.startPlayback(replica, replica), replica: replica})
		}
		for i, pl := range all {
			wg.Add(1)
			go func() {
				defer wg.Done()
				pl.answers = append(pl.answers, pl.p.segment(i*8), pl.p.segment(i*8+1))
			}()
		}
		wg.Wait()
		for _, pl := range all {
			for _, a := range pl.answers {
				if !a.isSegment() {
					t.Errorf("a viewer on %s got %s", pl.replica.name(), a)
				}
			}
			j, ok := c.rowOf(pl.p)
			switch {
			case !ok:
				t.Errorf("a viewer on %s has no job row", pl.replica.name())
			case j.Version != pl.replica.version:
				t.Errorf("the job of a viewer on %s records version %d", pl.replica.name(), j.Version)
			}
			pl.p.stop()
		}
		seen := map[int]bool{}
		for _, j := range c.rows() {
			seen[j.Version] = true
		}
		if !seen[oldVersion] || !seen[newVersion] {
			t.Errorf("the job rows' versions are %v, want both %d and %d", seen, oldVersion, newVersion)
		}
		c.checkRan()
	})

	// The wait for a worker of the job's own version, with only another
	// version's worker running and a slot free on it.
	t.Run("a viewer's job no worker of its version takes is refused after queueTimeout and stores nothing", func(t *testing.T) {
		c := newCluster(t, false, "20s")
		c.start(oldVersion, "worker")
		r5 := c.start(newVersion, "api,stream")
		p := c.startPlayback(r5, r5)
		a := p.segment(0)
		if a.code != 503 || a.retryAfter == "" {
			t.Fatalf("the segment of a job no worker of its version takes got %s, want 503 with Retry-After", a)
		}
		if a.took < queueTimeout-100*time.Millisecond {
			t.Errorf("refused after %v, before queueTimeout (%v)", a.took, queueTimeout)
		}
		j, ok := c.rowOf(p)
		if !ok {
			t.Fatal("the viewer's request left no job row")
		}
		if j.State != "cancelled" || j.Worker != "" || j.Version != newVersion {
			t.Errorf("the job ended %s with worker %q and version %d, want cancelled, no worker, version %d", j.State, j.Worker, j.Version, newVersion)
		}
		if c.storedUnder(j.Hash) {
			t.Errorf("an object is stored under hash %s of a job no worker of its version ran", j.Hash)
		}
		// The log names the cause: another version's worker had a slot.
		var line string
		for _, l := range strings.Split(r5.log.String(), "\n") {
			if strings.Contains(l, "spec="+j.Hash) && strings.Contains(l, "level=ERROR") {
				line = l
			}
		}
		if line == "" {
			t.Fatalf("the stream replica logged no error for spec %s:\n%s", j.Hash, r5.log.String())
		}
		for _, wrong := range []string{"slot", "no worker", "no transcode worker"} {
			if strings.Contains(strings.ToLower(line), wrong) {
				t.Errorf("the log line says %q, which is not the cause (a worker of another version had a free slot): %s", wrong, line)
			}
		}
		if !strings.Contains(strings.ToLower(line), "version") {
			t.Errorf("the log line does not name the cause, a worker of another output version: %s", line)
		}
		c.checkRan()
	})

	t.Run("a worker of the job's version that starts within queueTimeout claims the waiting job and the request gets the segment", func(t *testing.T) {
		c := newCluster(t, false, "20s")
		c.start(oldVersion, "worker")
		r5 := c.start(newVersion, "api,stream")
		p := c.startPlayback(r5, r5)
		answered := make(chan answer, 1)
		go func() { answered <- p.segment(0) }()
		waitUntil(t, "the job to be left queued (no worker of its version runs, and no other worker may take it)", func() bool { j, ok := c.rowOf(p); return ok && j.State == "queued" })
		if j, _ := c.rowOf(p); j.Worker != "" {
			t.Fatalf("the job has worker %q while no worker of its version runs", j.Worker)
		}
		w5 := c.start(newVersion, "worker")
		a := <-answered
		if !a.isSegment() {
			t.Fatalf("the request got %s, want the segment: a worker of the job's version started while it waited", a)
		}
		j, _ := c.rowOf(p)
		if j.Worker != w5.workerID || j.Version != newVersion {
			t.Errorf("the job's worker is %q with version %d, want the new worker %q with %d", j.Worker, j.Version, w5.workerID, newVersion)
		}
		p.stop()
		c.checkRan()
	})

	// A pre-release replica writes no version, and a row of none is version 4.
	t.Run("a job written with no version is run by a worker of version 4 and not by one of a later version", func(t *testing.T) {
		c := newCluster(t, false, "20s")
		w4 := c.start(oldVersion, "worker")
		r4 := c.start(oldVersion, "api,stream")
		first := c.startPlayback(r4, r4)
		if a := first.segment(0); !a.isSegment() {
			t.Fatalf("the first play got %s", a)
		}
		first.stop()
		w4.stop()
		c.start(newVersion, "worker")
		// What a replica built before the column inserts: no output_version.
		var id int64
		err := c.pool.QueryRow(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment)
			SELECT spec_hash, spec, 'old-replica', 30, 30 FROM transcode_jobs WHERE NOT helper ORDER BY id LIMIT 1 RETURNING id`).Scan(&id)
		if err != nil {
			t.Fatalf("inserting a job with no version: %v", err)
		}
		time.Sleep(3 * time.Second) // Less than queueTimeout; the new worker polls every second.
		var state, worker string
		var version int
		if err := c.pool.QueryRow(context.Background(), "SELECT state, COALESCE(worker, ''), output_version FROM transcode_jobs WHERE id = $1", id).Scan(&state, &worker, &version); err != nil {
			t.Fatal(err)
		}
		if version != oldVersion || state != "queued" || worker != "" {
			t.Fatalf("the job with no version is version %d, %s, worker %q after the worker of version %d had 3 seconds: want 4, queued, none", version, state, worker, newVersion)
		}
		w4b := c.start(oldVersion, "worker")
		waitUntil(t, "a worker of version 4 to claim the job", func() bool {
			c.pool.QueryRow(context.Background(), "SELECT state, COALESCE(worker, '') FROM transcode_jobs WHERE id = $1", id).Scan(&state, &worker) //nolint:errcheck
			return worker != ""
		})
		if worker != w4b.workerID {
			t.Errorf("the job was claimed by %q, want the worker of version 4, %q", worker, w4b.workerID)
		}
	})

	// A slow job's helper.
	t.Run("a slow job's helper is made by a worker of the job's version or by none", func(t *testing.T) {
		c := newCluster(t, true, "60s")
		w4 := c.start(oldVersion, "worker")
		w5 := c.start(newVersion, "worker") // Idle, with slots to spare.
		r4 := c.start(oldVersion, "api,stream")
		p := c.startPlayback(r4, r4)
		stop := c.keepPlaying(p)
		defer stop()
		// Long enough for a job's speed to be measured and a helper queued and
		// claimed: what a worker does is the answer, so this waits, and
		// asserts nothing about how long it took.
		deadline := time.Now().Add(30 * time.Second)
		helpers := 0
		for time.Now().Before(deadline) {
			for _, j := range c.rows() {
				if j.Worker == w5.workerID {
					t.Fatalf("the idle worker of version %d made job %d (helper %v) of a spec of version %d", newVersion, j.ID, j.Helper, j.Version)
				}
				if j.Helper {
					helpers++
					if j.Version != oldVersion {
						t.Errorf("helper %d records version %d, the job it helps is %d", j.ID, j.Version, oldVersion)
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Logf("helper rows seen: %d (none is allowed beside another version's workers; one on the worker of its version is)", helpers)
		_ = w4
		c.checkRan()
	})

	t.Run("in a cluster of one version a helper still starts on another worker", func(t *testing.T) {
		c := newCluster(t, true, "60s")
		c.start(newVersion, "worker")
		c.start(newVersion, "worker")
		r5 := c.start(newVersion, "api,stream")
		p := c.startPlayback(r5, r5)
		stop := c.keepPlaying(p)
		defer stop()
		var helper row
		waitUntil(t, "a helper to be claimed by a worker that does not run the viewer's job", func() bool {
			rows := c.rows()
			var viewers string
			for _, j := range rows {
				if !j.Helper && j.Worker != "" {
					viewers = j.Worker
				}
			}
			for _, j := range rows {
				if j.Helper && j.Worker != "" && j.Worker != viewers && viewers != "" {
					helper = j
					return true
				}
			}
			return false
		})
		if helper.Version != newVersion {
			t.Errorf("the helper records version %d, want %d", helper.Version, newVersion)
		}
		c.checkRan()
	})

	// Slot accounting counts only the version in question. w4 has three slots
	// and an idle w5 of the other version is beside it: a version 4 viewer's
	// helper runs on w4, and a second and a third version 4 viewer, each of a
	// spec of its own, still get a segment, the third by preempting the helper.
	// Counting w5's idle slots instead leaves no preempt, a job no worker may
	// take, and a 503 after queueTimeout.
	t.Run("an idle worker of another version does not count as a slot for a viewer: a helper is preempted for the next viewer", func(t *testing.T) {
		c := newCluster(t, true, "60s")
		c.slots = 3
		w4 := c.start(oldVersion, "worker")
		c.slots = 0
		c.start(newVersion, "worker") // Idle, with its default slots.
		r4 := c.start(oldVersion, "api,stream")
		first := c.startPlayback(r4, r4)
		stop := c.keepPlaying(first)
		defer stop()
		var helper row
		waitUntil(t, "a helper of the first viewer's spec to run on w4", func() bool {
			for _, j := range c.rows() {
				if j.Helper && j.State == "running" && j.Worker == w4.workerID {
					helper = j
					return true
				}
			}
			return false
		})
		second := c.startPlaybackAt(r4, r4, 1_500_000)
		if a := second.segment(0); !a.isSegment() {
			t.Fatalf("the second viewer, w4's third slot, got %s", a)
		}
		third := c.startPlaybackAt(r4, r4, 1_000_000)
		if a := third.segment(0); !a.isSegment() {
			t.Fatalf("the third viewer got %s, want a segment: its slot is the helper's, which a viewer preempts (the idle worker of version %d is no slot for it)", a, newVersion)
		}
		for _, j := range c.rows() {
			if j.ID == helper.ID && (j.State != "cancelled" || j.Error != "a new viewer needed its slot") {
				t.Errorf("the helper %d is %s (%q), want cancelled with 'a new viewer needed its slot'", j.ID, j.State, j.Error)
			}
		}
		second.stop()
		third.stop()
		c.checkRan()
	})

	// Walking the documented upgrade order: workers, then stream, then api,
	// the old workers last. At each step viewers through every running
	// pair of replicas get a segment.
	t.Run("walking the upgrade order every viewer gets a segment and each job is run by a worker of its version", func(t *testing.T) {
		c := newCluster(t, false, "20s")
		plays := 0
		step := func(name string, pairs ...[2]*proc) {
			for _, pair := range pairs {
				api, stream := pair[0], pair[1]
				p := c.startPlayback(api, stream)
				// A segment of its own, six apart, past the lead (5 segments)
				// of every earlier play.
				a := p.segment(plays * 6)
				plays++
				if !a.isSegment() {
					t.Errorf("%s: a viewer on api %s and stream %s got %s, want a segment", name, api.name(), stream.name(), a)
				}
				j, ok := c.rowOf(p)
				switch {
				case !ok:
					t.Errorf("%s: no job row for the viewer on %s", name, stream.name())
				case j.Version != stream.version:
					t.Errorf("%s: the job of a viewer on stream %s records version %d", name, stream.name(), j.Version)
				case c.workers[j.Worker] != j.Version:
					t.Errorf("%s: a job of version %d was run by %q, a worker of version %d", name, j.Version, j.Worker, c.workers[j.Worker])
				}
				p.stop()
			}
		}
		w4 := c.start(oldVersion, "worker")
		s4, a4 := c.start(oldVersion, "stream"), c.start(oldVersion, "api")
		step("before the upgrade", [2]*proc{a4, s4})
		w5 := c.start(newVersion, "worker")
		_ = w5
		step("new workers started", [2]*proc{a4, s4})
		s5 := c.start(newVersion, "stream")
		step("new stream started beside the old", [2]*proc{a4, s4}, [2]*proc{a4, s5})
		s4.stop()
		step("old stream stopped", [2]*proc{a4, s5})
		a5 := c.start(newVersion, "api")
		step("new api started beside the old", [2]*proc{a4, s5}, [2]*proc{a5, s5})
		a4.stop()
		step("old api stopped", [2]*proc{a5, s5})
		w4.stop()
		step("old workers stopped", [2]*proc{a5, s5})
		c.checkRan()
	})
}

// keepPlaying is a viewer that asks for segment after segment, the next when
// the last has come, as a player does, until the returned function is called.
func (c *cluster) keepPlaying(p *playing) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for n := 0; n < len(p.entries); n++ {
			select {
			case <-done:
				return
			default:
			}
			if a := p.segment(n); a.err != nil || a.code != 200 {
				return
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		p.stop()
	}
}
