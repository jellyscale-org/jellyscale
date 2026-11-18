//go:build integration

package image

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

// JS-124: the checks that need a Docker daemon. Every container is a disposable
// one on a port Docker assigns; none touches a service already on the machine.

func root(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s%s (this test needs Docker)", strings.Join(args, " "), err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// registry starts a disposable registry on a Docker-assigned loopback port and
// returns host:port.
func registry(t *testing.T) string {
	t.Helper()
	docker(t, "pull", "registry:2")
	id := docker(t, "run", "-d", "-p", "127.0.0.1::5000", "registry:2")
	t.Cleanup(func() { exec.Command("docker", "rm", "-fv", id).Run() }) //nolint:errcheck
	addr := strings.TrimSpace(docker(t, "port", id, "5000/tcp"))
	addr = addr[strings.LastIndex(addr, ":")-len("127.0.0.1"):]
	if i := strings.Index(addr, "\n"); i >= 0 {
		addr = addr[:i]
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		if r, err := http.Get("http://" + addr + "/v2/"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry on %s did not answer", addr)
		}
	}
}

// localImage imports an image whose one file is content, so two images differ.
func localImage(t *testing.T, content string) string {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	w.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: int64(len(content))}) //nolint:errcheck
	w.Write([]byte(content))                                                      //nolint:errcheck
	w.Close()
	name := "jellyscale-js124/" + uuid.NewString()
	cmd := exec.Command("docker", "import", "-", name)
	cmd.Stdin = &buf
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker import: %v\n%s", err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", name).Run() }) //nolint:errcheck
	return name
}

var manifestTypes = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
}, ", ")

// digest is what the registry says a tag names, "" when it names nothing.
func digest(t *testing.T, addr, tag string) string {
	t.Helper()
	req, _ := http.NewRequest("HEAD", "http://"+addr+"/v2/jellyscale/manifests/"+tag, nil)
	req.Header.Set("Accept", manifestTypes)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode == 404 {
		return ""
	}
	if r.StatusCode != 200 {
		t.Fatalf("manifest %s: %d", tag, r.StatusCode)
	}
	return r.Header.Get("Docker-Content-Digest")
}

func tagsOf(t *testing.T, addr string) []string {
	t.Helper()
	r, err := http.Get("http://" + addr + "/v2/jellyscale/tags/list")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode == 404 {
		return nil
	}
	var body struct{ Tags []string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	sort.Strings(body.Tags)
	return body.Tags
}

const sha = "0123456789abcdef0123456789abcdef01234567"

// publish runs test/publish.sh the way the workflow does: the repository, the
// two local images, and GitHub's variables for the commit and the ref.
func publish(t *testing.T, repo, server, fixtures, refType, ref string) (int, string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root(t), "test", "publish.sh")); err != nil {
		t.Fatalf("test/publish.sh does not exist: the workflow's push step is this script, which every check of publishing runs")
	}
	cmd := exec.Command("bash", "test/publish.sh", repo, server, fixtures)
	cmd.Dir = root(t)
	cmd.Env = append(os.Environ(), "GITHUB_SHA="+sha, "GITHUB_REF_TYPE="+refType, "GITHUB_REF_NAME="+ref)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("test/publish.sh: %v", err)
	return -1, ""
}

func wantTags(t *testing.T, addr string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := tagsOf(t, addr); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the registry holds tags %v, want exactly %v", got, want)
	}
}

func TestPublishingAMainCommitPushesTwoTagsAndAMainRerunKeepsThem(t *testing.T) {
	addr := registry(t)
	repo := addr + "/jellyscale"
	a, af := localImage(t, "a"), localImage(t, "af")
	if code, out := publish(t, repo, a, af, "branch", "main"); code != 0 {
		t.Fatalf("publishing a push to main: exit %d\n%s", code, out)
	}
	wantTags(t, addr, "main-0123456789ab", "main-0123456789ab-fixtures")
	first, firstF := digest(t, addr, "main-0123456789ab"), digest(t, addr, "main-0123456789ab-fixtures")
	if first == "" || first == firstF {
		t.Fatalf("digests %q and %q: the two tags must name two images", first, firstF)
	}
	// A rerun (here with other images, as a rebuild of the same commit would give) keeps what the tag named.
	b, bf := localImage(t, "b"), localImage(t, "bf")
	if code, out := publish(t, repo, b, bf, "branch", "main"); code != 0 {
		t.Fatalf("a rerun of the same commit: exit %d\n%s", code, out)
	}
	if got, gotF := digest(t, addr, "main-0123456789ab"), digest(t, addr, "main-0123456789ab-fixtures"); got != first || gotF != firstF {
		t.Errorf("a rerun moved a main tag: %s -> %s, %s -> %s", first, got, firstF, gotF)
	}
}

func TestPublishingAVersionTagAddsTheVersionAndNeverMovesItAfterwards(t *testing.T) {
	addr := registry(t)
	repo := addr + "/jellyscale"
	a, af := localImage(t, "a"), localImage(t, "af")
	if code, out := publish(t, repo, a, af, "tag", "v1.2.3"); code != 0 {
		t.Fatalf("publishing v1.2.3: exit %d\n%s", code, out)
	}
	wantTags(t, addr, "1.2.3", "1.2.3-fixtures", "main-0123456789ab", "main-0123456789ab-fixtures")
	before := map[string]string{}
	for _, tag := range tagsOf(t, addr) {
		before[tag] = digest(t, addr, tag)
	}
	b, bf := localImage(t, "b"), localImage(t, "bf")
	code, out := publish(t, repo, b, bf, "tag", "v1.2.3")
	if code == 0 {
		t.Fatalf("publishing 1.2.3 a second time exited 0\n%s", out)
	}
	if !strings.Contains(out, "1.2.3") {
		t.Errorf("the refusal does not name the tag:\n%s", out)
	}
	for tag, d := range before {
		if got := digest(t, addr, tag); got != d {
			t.Errorf("tag %s moved from %s to %s", tag, d, got)
		}
	}
	wantTags(t, addr, "1.2.3", "1.2.3-fixtures", "main-0123456789ab", "main-0123456789ab-fixtures")
}

func TestAVersionWhoseFixturesTagExistsPushesNothing(t *testing.T) {
	addr := registry(t)
	repo := addr + "/jellyscale"
	pre := localImage(t, "pre")
	docker(t, "tag", pre, repo+":1.2.3-fixtures")
	docker(t, "push", repo+":1.2.3-fixtures")
	a, af := localImage(t, "a"), localImage(t, "af")
	code, out := publish(t, repo, a, af, "tag", "v1.2.3")
	if code == 0 || !strings.Contains(out, "1.2.3-fixtures") {
		t.Errorf("exit %d, output %q: want a failure naming 1.2.3-fixtures", code, out)
	}
	wantTags(t, addr, "1.2.3-fixtures") // Nothing was pushed before the failure, not even the free tags.
}

func TestOnlyExactVersionTagsAndMainArePublished(t *testing.T) {
	addr := registry(t)
	repo := addr + "/jellyscale"
	a, af := localImage(t, "a"), localImage(t, "af")
	for _, c := range [][2]string{
		{"tag", "v1.2"}, {"tag", "v1.2.3-rc1"}, {"tag", "v1.2.3.4"}, {"tag", "1.2.3"}, {"tag", "latest"}, {"tag", "v1.2.x"}, {"tag", "v01.2.3x"},
		{"branch", "feature"}, {"branch", "latest"}, {"branch", ""},
	} {
		if code, out := publish(t, repo, a, af, c[0], c[1]); code == 0 {
			t.Errorf("%s %q was published\n%s", c[0], c[1], out)
		}
	}
	wantTags(t, addr)
	// A short or non-hex commit makes no main-<12 hex>.
	cmd := exec.Command("bash", "test/publish.sh", repo, a, af)
	cmd.Dir = root(t)
	if _, err := os.Stat(filepath.Join(root(t), "test", "publish.sh")); err != nil {
		t.Fatal("test/publish.sh does not exist")
	}
	cmd.Env = append(os.Environ(), "GITHUB_SHA=abc", "GITHUB_REF_TYPE=branch", "GITHUB_REF_NAME=main")
	if err := cmd.Run(); err == nil {
		t.Error("a 3-character GITHUB_SHA was published")
	}
	wantTags(t, addr)
}

func TestARegistryThatCannotBeAskedFailsTheRunAndNeverCountsAsTagAbsent(t *testing.T) {
	a, af := localImage(t, "a"), localImage(t, "af")
	// Nothing listens.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	if code, out := publish(t, closed+"/jellyscale", a, af, "tag", "v1.2.3"); code == 0 {
		t.Errorf("a registry that is not there: exit 0\n%s", out)
	}
	// Registries that answer every question with an error, recording any write. ghcr.io answers an
	// anonymous caller 403 DENIED for a package that does not exist (observed 2026-11-16, unverified for
	// a token with write scope): an answer of that kind is "cannot be asked", never "tag absent", so the
	// first publish of a package, if ghcr.io answers it so, is an owner step and not a silent push.
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"500", 500, "boom"},
		{"503", 503, "unavailable"},
		{"429", 429, "slow down"},
		{"401 DENIED", 401, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`},
		{"403 DENIED", 403, `{"errors":[{"code":"DENIED","message":"requested access to the resource is denied"}]}`},
	} {
		var mu sync.Mutex
		var writes []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if r.Method != "GET" && r.Method != "HEAD" {
				writes = append(writes, r.Method+" "+r.URL.Path)
			}
			mu.Unlock()
			http.Error(w, c.body, c.status)
		}))
		host := strings.TrimPrefix(srv.URL, "http://")
		for _, ref := range [][2]string{{"tag", "v1.2.3"}, {"branch", "main"}} {
			if code, out := publish(t, host+"/jellyscale", a, af, ref[0], ref[1]); code == 0 {
				t.Errorf("a registry answering %s, %s %s: exit 0\n%s", c.name, ref[0], ref[1], out)
			}
		}
		mu.Lock()
		if len(writes) > 0 {
			t.Errorf("a registry answering %s: an image was pushed to it: %v", c.name, writes)
		}
		mu.Unlock()
		srv.Close()
	}
}

// A registry that says the manifest and the repository are unknown (404, as registry:2 does for an empty
// repository) has the tag absent: the publish goes on and tries to write. The stub records the write and
// refuses it with a status docker push does not try again (403 DENIED: moby's push gives up at once on a denied
// write, while it tries a 5xx again five times with a wait between, which took 100 s on CI where the same
// stub's 500 took 1.5 s on a developer's machine), so the run fails and is shown to have tried, once for each of the image's two blobs.
// The publish's own output is logged, which is where a push that retried says so.
func TestARegistryThatAnswersNotFoundHasTheTagAbsentAndIsWrittenTo(t *testing.T) {
	a, af := localImage(t, "a"), localImage(t, "af")
	for _, code := range []string{"MANIFEST_UNKNOWN", "NAME_UNKNOWN"} {
		var mu sync.Mutex
		var writes []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" || r.Method == "HEAD" {
				if strings.HasSuffix(r.URL.Path, "/v2/") {
					w.WriteHeader(200)
					return
				}
				w.WriteHeader(404)
				w.Write([]byte(`{"errors":[{"code":"` + code + `"}]}`)) //nolint:errcheck
				return
			}
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"errors":[{"code":"DENIED","message":"no writes here"}]}`)) //nolint:errcheck
		}))
		host := strings.TrimPrefix(srv.URL, "http://")
		began := time.Now()
		exit, out := publish(t, host+"/jellyscale", a, af, "tag", "v1.2.3")
		// One line with a time, which the CI job log keeps for a run that passed (test/report.sh timings).
		t.Logf("%s: publish exited %d after %v, and docker push said it would retry %d times", code, exit, time.Since(began).Round(time.Second), strings.Count(out, "Retrying in"))
		t.Logf("%s: publish output:\n%s", code, out)
		if strings.Contains(out, "Retrying in") {
			t.Errorf("a registry answering 404 %s and refusing every write was written to again: docker retried a refusal it must not retry:\n%s", code, out)
		}
		if exit == 0 {
			t.Errorf("a registry answering 404 %s and refusing every write: the publish exited 0", code)
		}
		mu.Lock()
		if len(writes) == 0 {
			t.Errorf("a registry answering 404 %s counted as unaskable: nothing was written, so the first publish of a package would never happen", code)
		}
		// The image has two blobs (its layer and its config), each uploaded once at the same time; a push that
		// tried again would write more than that.
		if len(writes) > 2 {
			t.Errorf("a registry answering 404 %s was written to %d times, want at most one write for each of the image's two blobs: %v", code, len(writes), writes)
		}
		mu.Unlock()
		srv.Close()
	}
}

// --- the compose file, as a client has it.

func composeIn(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash(composePath)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("docker", append([]string{"compose", "-f", "compose.yaml"}, args...)...)
	cmd.Dir = dir
	var clean []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "JELLYSCALE_") && !strings.HasPrefix(e, "COMPOSE_") {
			clean = append(clean, e)
		}
	}
	cmd.Env = append(clean, env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func TestComposeFailsNamingTheImageVariableWhenItIsUnsetOrEmptyAndStartsNothing(t *testing.T) {
	before := composeResources(t)
	for _, env := range [][]string{nil, {"JELLYSCALE_IMAGE="}} {
		for _, args := range [][]string{{"config"}, {"-p", "js124-unset-" + uuid.NewString()[:8], "up", "-d"}} {
			code, out := composeIn(t, env, args...)
			if code == 0 || !strings.Contains(out, "JELLYSCALE_IMAGE") {
				t.Errorf("docker compose %v with env %v: exit %d, output %q; want a failure naming JELLYSCALE_IMAGE", args, env, code, out)
			}
		}
	}
	if after := composeResources(t); strings.Join(after, " ") != strings.Join(before, " ") {
		t.Errorf("compose resources changed: %v -> %v", before, after)
	}
	code, out := composeIn(t, []string{"JELLYSCALE_IMAGE=example/jellyscale:x"}, "config", "--format", "json")
	if code != 0 {
		t.Fatalf("config with the image set: exit %d\n%s", code, out)
	}
	var cfg struct{ Services map[string]json.RawMessage }
	if err := json.Unmarshal([]byte(out), &cfg); err != nil || len(cfg.Services) != 2 {
		t.Errorf("config gives %d services (%v), want 2", len(cfg.Services), err)
	}
}

// composeResources lists every container, network and volume compose made, as
// kind:id, so that a run's own can be told from what was there.
func composeResources(t *testing.T) []string {
	t.Helper()
	var all []string
	for _, kind := range [][]string{{"ps", "-a", "-q"}, {"network", "ls", "-q"}, {"volume", "ls", "-q"}} {
		args := append(kind, "--filter", "label=com.docker.compose.project")
		for _, id := range strings.Fields(docker(t, args...)) {
			all = append(all, kind[0]+":"+id)
		}
	}
	sort.Strings(all)
	return all
}

func without(all, base []string) []string {
	in := map[string]bool{}
	for _, b := range base {
		in[b] = true
	}
	var out []string
	for _, a := range all {
		if !in[a] {
			out = append(out, a)
		}
	}
	return out
}

// --- test/image.sh leaves nothing behind.

// stub is an image that carries the labels of a build of HEAD and never becomes
// ready, pushed to a disposable registry as the fixtures image of a commit and
// as its server image. test/image.sh can pull and start it, and its wait ends
// in a failure.
func stub(t *testing.T) string {
	t.Helper()
	head := strings.TrimSpace(string(must(exec.Command("git", "-C", root(t), "rev-parse", "HEAD").Output())))
	addr := registry(t)
	ref := addr + "/jellyscale:main-" + head[:12]
	dockerfile := "FROM busybox:1.37\n" +
		"LABEL org.opencontainers.image.revision=" + head + " org.opencontainers.image.source=https://github.com/jellyscale-org/jellyscale org.opencontainers.image.version=main-" + head[:12] + "\n" +
		"ENTRYPOINT [\"sleep\", \"3600\"]\n"
	cmd := exec.Command("docker", "build", "-t", ref, "-t", ref+"-fixtures", "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker build of the stub: %v\n%s", err, out)
	}
	docker(t, "push", ref)
	docker(t, "push", ref+"-fixtures")
	docker(t, "rmi", ref, ref+"-fixtures") // The run must pull it.
	return ref + "-fixtures"
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// lockedBuffer is written by the process's copier and read by the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type run struct {
	cmd *exec.Cmd
	out *lockedBuffer
	end chan struct{}
}

func startImageSh(t *testing.T, image, rootDir, tmpDir string) *run {
	t.Helper()
	r := &run{out: &lockedBuffer{}, end: make(chan struct{})}
	r.cmd = exec.Command("bash", "test/image.sh", image)
	r.cmd.Dir = root(t)
	r.cmd.Env = append(os.Environ(), "JELLYSCALE_TEST_ROOT="+rootDir, "TMPDIR="+tmpDir)
	r.cmd.Stdout, r.cmd.Stderr = r.out, r.out
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { r.cmd.Wait(); close(r.end) }()                              //nolint:errcheck
	t.Cleanup(func() { syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL) }) //nolint:errcheck
	return r
}

func (r *run) exited() bool {
	select {
	case <-r.end:
		return true
	default:
		return false
	}
}

func (r *run) signal(s syscall.Signal) { syscall.Kill(-r.cmd.Process.Pid, s) } //nolint:errcheck

// project is the compose project the run prints as a line "compose project: <name>" before it starts
// anything. The test lists a run's resources by that label, so another compose project on the machine,
// or another run's half-made one, is never taken for this run's.
func (r *run) project(t *testing.T) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^compose project: ([a-z0-9][a-z0-9_-]*)$`)
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if m := re.FindStringSubmatch(r.out.String()); m != nil {
			return m[1]
		}
		if r.exited() {
			t.Fatalf("test/image.sh ended without printing a line 'compose project: <name>':\n%s", r.out)
		}
	}
	t.Fatalf("test/image.sh printed no line 'compose project: <name>' in 5 minutes:\n%s", r.out)
	return ""
}

// resourcesOf lists the containers, networks and volumes of one compose project, as kind:id.
func resourcesOf(t *testing.T, project string) []string {
	t.Helper()
	var all []string
	for _, kind := range [][]string{{"ps", "-a", "-q"}, {"network", "ls", "-q"}, {"volume", "ls", "-q"}} {
		args := append(append([]string{}, kind...), "--filter", "label=com.docker.compose.project="+project)
		for _, id := range strings.Fields(docker(t, args...)) {
			all = append(all, kind[0]+":"+id)
		}
	}
	sort.Strings(all)
	return all
}

func count(all []string, kind string) int {
	n := 0
	for _, a := range all {
		if strings.HasPrefix(a, kind+":") {
			n++
		}
	}
	return n
}

// whole waits until the run's project has both of its containers (Postgres and the server), the point
// from which what it made is complete; a snapshot before it would miss one.
func (r *run) whole(t *testing.T, project string) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got := resourcesOf(t, project); count(got, "ps") >= 2 && count(got, "network") >= 1 {
			return got
		}
		if r.exited() {
			t.Fatalf("test/image.sh ended before both its containers existed:\n%s", r.out)
		}
	}
	t.Fatalf("test/image.sh made no two containers in 5 minutes:\n%s", r.out)
	return nil
}

func (r *run) waitEnd(t *testing.T, limit time.Duration) {
	t.Helper()
	select {
	case <-r.end:
	case <-time.After(limit):
		t.Fatalf("test/image.sh did not end within %v of the signal:\n%s", limit, r.out)
	}
}

// stillGoing fails the test when the run has ended by itself: a signal sent to it would show nothing
// (the stub never becomes ready, so a run ends alone only when its wait is shorter than the test).
func (r *run) stillGoing(t *testing.T, who string) {
	t.Helper()
	if r.exited() {
		t.Fatalf("run %s ended before the test signalled it, so its signal path is not shown:\n%s", who, r.out)
	}
}

func TestImageShCleansUpAfterASignalAndAfterAKillAndNeverTouchesAnotherRun(t *testing.T) {
	image := stub(t)
	rootDir, tmpDir := t.TempDir(), t.TempDir()

	a := startImageSh(t, image, rootDir, tmpDir)
	pa := a.project(t)
	madeA := a.whole(t, pa)
	b := startImageSh(t, image, rootDir, tmpDir)
	pb := b.project(t)
	madeB := b.whole(t, pb)
	if pa == pb {
		t.Fatalf("two runs at once share the compose project %q", pa)
	}
	// The kill leaves A's containers behind: nothing of A's stops them.
	a.stillGoing(t, "A")
	a.signal(syscall.SIGKILL)
	a.waitEnd(t, time.Minute)
	if len(resourcesOf(t, pa)) == 0 {
		t.Fatal("SIGKILL left nothing of A's, so this shows nothing about the next run's cleanup")
	}
	c := startImageSh(t, image, rootDir, tmpDir)
	pc := c.project(t)
	madeC := c.whole(t, pc)
	if pc == pa || pc == pb {
		t.Fatalf("a new run reuses the compose project %q of another", pc)
	}
	c.stillGoing(t, "C")
	c.signal(syscall.SIGTERM)
	c.waitEnd(t, 2*time.Minute)
	if c.cmd.ProcessState.ExitCode() == 0 {
		t.Error("a run stopped by SIGTERM exited 0")
	}
	// C swept A's leftovers, not B's, and took its own away.
	if left := resourcesOf(t, pa); len(left) > 0 {
		t.Errorf("A was killed and a later run started, yet A's %v remain", left)
	}
	if left := resourcesOf(t, pc); len(left) > 0 {
		t.Errorf("C ended on SIGTERM, yet its %v remain (made: %v)", left, madeC)
	}
	b.stillGoing(t, "B")
	if now := resourcesOf(t, pb); strings.Join(now, " ") != strings.Join(madeB, " ") {
		t.Errorf("B was still going, but another run changed its resources: was %v, now %v", madeB, now)
	}
	b.signal(syscall.SIGINT)
	b.waitEnd(t, 2*time.Minute)
	if b.cmd.ProcessState.ExitCode() == 0 {
		t.Error("a run stopped by SIGINT exited 0")
	}
	for name, p := range map[string]string{"A": pa, "B": pb, "C": pc} {
		if left := resourcesOf(t, p); len(left) > 0 {
			t.Errorf("after every run ended, %s's %v remain", name, left)
		}
	}
	_ = madeA
	for _, dir := range []string{rootDir, tmpDir} {
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s is not empty after every run ended: %v", dir, names)
		}
	}
}
