package image

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// JS-124: the tests of the workflow files. They need no Docker and no ffmpeg.
// Each rule is run on small documents that break it (so the rule is shown to
// fail) and on the real files (so the build is shown to keep it).

const imageWorkflow = ".github/workflows/image.yml"

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(raw)
}

func parse(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, text)
	}
	return doc
}

func jobsOf(doc map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	jobs, _ := doc["jobs"].(map[string]any)
	for name, j := range jobs {
		if m, ok := j.(map[string]any); ok {
			out[name] = m
		}
	}
	return out
}

func stepsOf(job map[string]any) []map[string]any {
	var out []map[string]any
	list, _ := job["steps"].([]any)
	for _, s := range list {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// onOf is the triggers: YAML 1.1 reads the key "on" as a boolean, and yaml.v3 then gives it as "true".
func onOf(doc map[string]any) map[string]any {
	for _, k := range []string{"on", "true"} {
		if m, ok := doc[k].(map[string]any); ok {
			return m
		}
	}
	return nil
}

func text(v any) string { return fmt.Sprint(v) }

func workflowFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflow files: %v", err)
	}
	return files
}

// --- every job runs on ubuntu-24.04 hosted runners, with a time limit.

func runnerProblems(doc map[string]any) []string {
	var bad []string
	for name, job := range jobsOf(doc) {
		if runs, ok := job["runs-on"].(string); !ok || runs != "ubuntu-24.04" {
			bad = append(bad, fmt.Sprintf("job %s: runs-on is %v, want exactly the string ubuntu-24.04", name, job["runs-on"]))
		}
		if _, ok := job["timeout-minutes"]; !ok {
			bad = append(bad, "job "+name+" has no timeout-minutes")
		}
	}
	return bad
}

func TestRunnerRuleRefusesWhatItNames(t *testing.T) {
	for _, runs := range []string{
		"self-hosted", "ubuntu-24.04-8core", "ubuntu-latest", "[ubuntu-24.04]", "[self-hosted, linux]",
		"${{ matrix.os }}", "{ group: big-runners }", "ubuntu-22.04", "macos-14",
	} {
		doc := parse(t, "jobs:\n  a:\n    runs-on: "+runs+"\n    timeout-minutes: 5\n    steps: []\n")
		if len(runnerProblems(doc)) == 0 {
			t.Errorf("runs-on: %s was accepted", runs)
		}
	}
	if len(runnerProblems(parse(t, "jobs:\n  a:\n    runs-on: ubuntu-24.04\n    steps: []\n"))) == 0 {
		t.Error("a job with no timeout-minutes was accepted")
	}
	if p := runnerProblems(parse(t, "jobs:\n  a:\n    runs-on: ubuntu-24.04\n    timeout-minutes: 5\n    steps: []\n")); len(p) != 0 {
		t.Errorf("a right job was refused: %v", p)
	}
}

func TestEveryJobOfEveryWorkflowRunsOnUbuntu2404WithATimeout(t *testing.T) {
	files := workflowFiles(t)
	if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(imageWorkflow))); err != nil {
		t.Fatalf("the image workflow %s does not exist", imageWorkflow)
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		for _, p := range runnerProblems(parse(t, string(raw))) {
			t.Errorf("%s: %s", filepath.Base(f), p)
		}
	}
}

// --- triggers: a push to main and a push of a tag, nothing else.

func TestImageWorkflowRunsOnlyOnAPushToMainAndAPushOfAVersionTag(t *testing.T) {
	doc := parse(t, repoFile(t, imageWorkflow))
	on := onOf(doc)
	if len(on) != 1 {
		t.Fatalf("on must be a map holding only push, got %v (no pull_request, pull_request_target, merge_group, schedule, workflow_dispatch)", on)
	}
	push, ok := on["push"].(map[string]any)
	if !ok {
		t.Fatalf("on.push is %v", on["push"])
	}
	branches, _ := push["branches"].([]any)
	if len(branches) != 1 || text(branches[0]) != "main" {
		t.Errorf("push.branches is %v, want [main]", push["branches"])
	}
	tags, _ := push["tags"].([]any)
	if len(tags) == 0 {
		t.Fatal("push.tags is empty: a tag push must start the workflow")
	}
	if len(push) != 2 {
		t.Errorf("push has keys beyond branches and tags: %v", push)
	}
	// GitHub's filter is a glob over the whole tag name.
	glob := func(p string) *regexp.Regexp {
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(p); i++ {
			switch c := p[i]; c {
			case '[':
				end := strings.IndexByte(p[i:], ']')
				b.WriteString(p[i : i+end+1])
				i += end
			case '+', '?':
				b.WriteByte(c)
			case '*':
				b.WriteString("[^/]*")
			default:
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		}
		b.WriteString("$")
		return regexp.MustCompile(b.String())
	}
	matches := func(name string) bool {
		for _, p := range tags {
			if glob(text(p)).MatchString(name) {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"v0.1.0", "v1.2.3", "v10.20.30"} {
		if !matches(name) {
			t.Errorf("tag %s does not start the workflow (patterns %v)", name, tags)
		}
	}
	for _, name := range []string{"v1.2", "v1.2.3-rc1", "v1.2.3.4", "1.2.3", "v1.2.x", "latest", "vv1.2.3", "v1.2.3+build"} {
		if matches(name) {
			t.Errorf("tag %s starts the workflow (patterns %v)", name, tags)
		}
	}
}

// --- the test comes first, and the push cannot run without it.

// The documents below are what a good workflow looks like in its two shapes.
const oneJob = `
jobs:
  image:
    runs-on: ubuntu-24.04
    timeout-minutes: 60
    steps:
      - run: test/apt.sh ffmpeg
      - run: test/image.sh
      - run: test/publish.sh repo a b
`

const twoJobs = `
jobs:
  test:
    runs-on: ubuntu-24.04
    timeout-minutes: 60
    steps:
      - run: test/image.sh
  push:
    needs: test
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    steps:
      - run: test/publish.sh repo a b
`

// pushWords is every way a workflow can send an image to a registry. The one
// way allowed is test/publish.sh, which checks that no version is published twice.
var pushWords = regexp.MustCompile(`docker\s+(image\s+)?push|--push|\bpush=true|type=registry|buildx\s+imagetools|\bcrane\b|\bskopeo\b|\bdocker\s+compose\s+push`)

func isPush(s map[string]any) bool {
	if pushWords.MatchString(text(s["run"])) || strings.Contains(text(s["run"]), "test/publish.sh") {
		return true
	}
	uses := text(s["uses"])
	if strings.Contains(uses, "build-push-action") {
		return true
	}
	if with, ok := s["with"].(map[string]any); ok {
		if v, ok := with["push"]; ok && text(v) != "false" {
			return true
		}
	}
	return false
}

// orderProblems says what is wrong with how the push is gated on the test.
func orderProblems(doc map[string]any) []string {
	type at struct {
		job  string
		step int
	}
	var tests, pushes []at
	var bad []string
	jobs := jobsOf(doc)
	for name, job := range jobs {
		for i, s := range stepsOf(job) {
			run := text(s["run"])
			if strings.Contains(run, "test/image.sh") {
				tests = append(tests, at{name, i})
				// A step that tests and pushes at once has no order to check.
				if isPush(s) {
					bad = append(bad, "the test and a push are one step")
				}
			}
			if isPush(s) {
				pushes = append(pushes, at{name, i})
			}
			// Any docker action but the login and the buildx setup can build or push on its own.
			if u := text(s["uses"]); strings.HasPrefix(u, "docker/") && !strings.HasPrefix(u, "docker/login-action@") && !strings.HasPrefix(u, "docker/setup-buildx-action@") {
				bad = append(bad, "uses "+u+": only docker/login-action and docker/setup-buildx-action are allowed")
			}
		}
	}
	if len(tests) != 1 {
		return append(bad, fmt.Sprintf("want exactly one step running test/image.sh, found %d", len(tests)))
	}
	if len(pushes) != 1 {
		return append(bad, fmt.Sprintf("want exactly one push step (test/publish.sh), found %d", len(pushes)))
	}
	te, pu := tests[0], pushes[0]
	testStep, pushStep := stepsOf(jobs[te.job])[te.step], stepsOf(jobs[pu.job])[pu.step]
	if !strings.Contains(text(pushStep["run"]), "test/publish.sh") {
		bad = append(bad, "the push is not test/publish.sh")
	}
	if te.job == pu.job {
		if pu.step < te.step {
			bad = append(bad, "the push step is before the test step")
		}
	} else {
		needs := text(jobs[pu.job]["needs"])
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(te.job) + `\b`).MatchString(needs) {
			bad = append(bad, fmt.Sprintf("job %s pushes but does not need %s, which tests", pu.job, te.job))
		}
	}
	for _, v := range []any{testStep["continue-on-error"], jobs[te.job]["continue-on-error"]} {
		if v != nil && text(v) != "false" {
			bad = append(bad, "continue-on-error on the test")
		}
	}
	// A condition on the test step or its job lets a run reach the push without the test (on a tag, say).
	if testStep["if"] != nil || jobs[te.job]["if"] != nil {
		bad = append(bad, "the test step or its job has an if: some run would push without testing")
	}
	if strings.Contains(text(testStep["run"]), "|| true") {
		bad = append(bad, "the test step swallows its failure with || true")
	}
	// The push runs only when everything before it succeeded: no condition that names a status or another job's result.
	cond := regexp.MustCompile(`always\(\)|failure\(\)|cancelled\(\)|\.result|needs\.`)
	for _, v := range []any{pushStep["if"], jobs[pu.job]["if"]} {
		if v != nil && cond.MatchString(text(v)) {
			bad = append(bad, fmt.Sprintf("the push condition %q can run after a failure or a skipped test", text(v)))
		}
	}
	// Nothing is built between the test and the push: the images tested are the images pushed.
	building := regexp.MustCompile(`docker build|buildx build|build-push-action|compose build|docker commit|docker (image )?tag|docker load|docker pull`)
	for i, s := range stepsOf(jobs[pu.job]) {
		if te.job == pu.job && i <= te.step {
			continue
		}
		if building.MatchString(text(s["run"])) || building.MatchString(text(s["uses"])) {
			bad = append(bad, "an image is built, tagged or fetched after the test and before the push")
		}
	}
	// The push job of a two-job shape has the images only if the test job handed them over, and that would be a rebuild or a reload.
	if te.job != pu.job {
		for _, s := range stepsOf(jobs[pu.job]) {
			if building.MatchString(text(s["run"])) {
				bad = append(bad, "the push job builds or loads images itself")
			}
		}
	}
	return bad
}

func TestOrderRuleAcceptsTheTwoGoodShapes(t *testing.T) {
	for name, text := range map[string]string{"one job": oneJob, "two jobs": twoJobs} {
		if p := orderProblems(parse(t, text)); len(p) != 0 {
			t.Errorf("%s was refused: %v", name, p)
		}
	}
}

func TestOrderRuleFailsEachWayTheTestCanBeBypassed(t *testing.T) {
	mutants := map[string]string{
		"the test step removed":                        strings.Replace(oneJob, "      - run: test/image.sh\n", "", 1),
		"the push before the test":                     strings.Replace(strings.Replace(oneJob, "      - run: test/publish.sh repo a b\n", "", 1), "      - run: test/image.sh\n", "      - run: test/publish.sh repo a b\n      - run: test/image.sh\n", 1),
		"continue-on-error on the test":                strings.Replace(oneJob, "      - run: test/image.sh\n", "      - run: test/image.sh\n        continue-on-error: true\n", 1),
		"always() on the push":                         oneJob + "        if: always()\n",
		"failure() on the push":                        oneJob + "        if: ${{ failure() }}\n",
		"!cancelled() on the push":                     oneJob + "        if: ${{ !cancelled() }}\n",
		"a push in a job that does not need the test":  strings.Replace(twoJobs, "    needs: test\n", "", 1),
		"the push job needing something else":          strings.Replace(twoJobs, "needs: test", "needs: lint", 1),
		"an image built between the test and the push": strings.Replace(oneJob, "      - run: test/publish.sh repo a b\n", "      - run: docker build -t a .\n      - run: test/publish.sh repo a b\n", 1),
		"continue-on-error on the test job":            strings.Replace(twoJobs, "  test:\n", "  test:\n    continue-on-error: true\n", 1),
		"the push job rebuilding after the test's job": strings.Replace(twoJobs, "      - run: test/publish.sh repo a b\n", "      - run: docker buildx build .\n      - run: test/publish.sh repo a b\n", 1),
		"buildx --push with no test/publish.sh":        strings.Replace(oneJob, "      - run: test/publish.sh repo a b\n", "      - run: docker buildx build --push -t ghcr.io/o/r:latest .\n", 1),
		"buildx --push before the test":                strings.Replace(oneJob, "      - run: test/image.sh\n", "      - run: docker buildx build --push -t ghcr.io/o/r:latest .\n      - run: test/image.sh\n", 1),
		"build-push-action with push: true":            strings.Replace(oneJob, "      - run: test/image.sh\n", "      - uses: docker/build-push-action@v6\n        with:\n          push: true\n      - run: test/image.sh\n", 1),
		"docker push beside test/publish.sh":           strings.Replace(oneJob, "      - run: test/publish.sh repo a b\n", "      - run: docker push ghcr.io/o/r:latest\n      - run: test/publish.sh repo a b\n", 1),
		"any other docker action":                      strings.Replace(oneJob, "      - run: test/image.sh\n", "      - uses: docker/bake-action@v6\n      - run: test/image.sh\n", 1),
		"an if on the test step":                       strings.Replace(oneJob, "      - run: test/image.sh\n", "      - run: test/image.sh\n        if: github.ref_type == 'tag'\n", 1),
		"an if on the test job":                        strings.Replace(twoJobs, "  test:\n", "  test:\n    if: github.ref_type == 'tag'\n", 1),
		"publish.sh before image.sh in one run":        strings.Replace(oneJob, "      - run: test/image.sh\n      - run: test/publish.sh repo a b\n", "      - run: |\n          test/publish.sh repo a b\n          test/image.sh\n", 1),
		"test and push in one run":                     strings.Replace(oneJob, "      - run: test/image.sh\n      - run: test/publish.sh repo a b\n", "      - run: |\n          test/image.sh\n          test/publish.sh repo a b\n", 1),
		"push job if on the test result":               strings.Replace(twoJobs, "    needs: test\n", "    needs: test\n    if: needs.test.result != 'failure'\n", 1),
		"push job if on !cancelled()":                  strings.Replace(twoJobs, "    needs: test\n", "    needs: test\n    if: ${{ !cancelled() }}\n", 1),
		"a tag between the test and the push":          strings.Replace(oneJob, "      - run: test/publish.sh repo a b\n", "      - run: docker tag a ghcr.io/o/r:latest\n      - run: test/publish.sh repo a b\n", 1),
	}
	for name, doc := range mutants {
		if doc == oneJob || doc == twoJobs {
			t.Fatalf("the mutant %q changes nothing", name)
		}
		if len(orderProblems(parse(t, doc))) == 0 {
			t.Errorf("accepted: %s", name)
		}
	}
}

func TestImageWorkflowTestsBeforeItPushesTheImagesItTested(t *testing.T) {
	text := repoFile(t, imageWorkflow)
	doc := parse(t, text)
	for _, p := range orderProblems(doc) {
		t.Error(p)
	}
	// The films are made on the runner with the ffmpeg CI's other jobs use, by the test itself, which names no image
	// to pull (test/image.sh with no argument builds from the checkout).
	if !strings.Contains(text, "test/apt.sh") || !strings.Contains(text, "ffmpeg") {
		t.Error("the workflow does not install ffmpeg with test/apt.sh")
	}
	if regexp.MustCompile(`test/image\.sh\s+\S`).MatchString(strings.ReplaceAll(text, "test/image.sh\n", "")) {
		t.Error("test/image.sh is given an argument, which makes it pull an image instead of testing the ones just built")
	}
}

// The tags the workflow can push are the ones test/publish.sh makes: nothing in
// the file names a registry path or a tag of its own.
func TestImageWorkflowNamesNoTagButThroughPublishSh(t *testing.T) {
	raw := repoFile(t, imageWorkflow)
	for _, banned := range []string{":latest", ":edge", ":main\n", "metadata-action", "build-push-action", "--push", "docker push", "imagetools", "docker tag", "type=registry"} {
		if strings.Contains(raw, banned) {
			t.Errorf("%s contains %q: the tags pushed are test/publish.sh's alone (main-<12 hex> and X.Y.Z, each with -fixtures)", imageWorkflow, banned)
		}
	}
	if m := regexp.MustCompile(`ghcr\.io/[^\s"']+`).FindAllString(raw, -1); len(m) > 0 {
		for _, s := range m {
			if strings.Contains(s, ":") && !strings.Contains(s, "${{") {
				t.Errorf("%s names the tag-carrying reference %q", imageWorkflow, s)
			}
		}
	}
	for _, name := range []string{"v1.2", "v1.2.3-rc1", "latest"} {
		if strings.Contains(raw, name) {
			t.Errorf("%s mentions %q", imageWorkflow, name)
		}
	}
}

// --- permissions.

func permissionProblems(doc map[string]any) []string {
	var bad []string
	readOnly := func(where string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			bad = append(bad, where+": permissions must be a map of read or none")
			return
		}
		for k, val := range m {
			if text(val) != "read" && text(val) != "none" {
				bad = append(bad, fmt.Sprintf("%s: %s: %v", where, k, val))
			}
		}
	}
	readOnly("top level", doc["permissions"])
	writers := 0
	for name, job := range jobsOf(doc) {
		perm, ok := job["permissions"].(map[string]any)
		if !ok {
			continue
		}
		for k, val := range perm {
			if text(val) == "write" {
				if k != "packages" {
					bad = append(bad, fmt.Sprintf("job %s: %s: write", name, k))
				} else {
					writers++
				}
			}
		}
	}
	if writers != 1 {
		bad = append(bad, fmt.Sprintf("%d jobs have packages: write, want exactly one", writers))
	}
	return bad
}

func TestPermissionRuleFailsOnWriteAnywhereElse(t *testing.T) {
	good := "permissions:\n  contents: read\njobs:\n  a:\n    steps: []\n  b:\n    permissions:\n      contents: read\n      packages: write\n    steps: []\n"
	if p := permissionProblems(parse(t, good)); len(p) != 0 {
		t.Fatalf("a right file was refused: %v", p)
	}
	for name, doc := range map[string]string{
		"packages: write at the top":  strings.Replace(good, "contents: read\njobs", "contents: read\n  packages: write\njobs", 1),
		"write-all at the top":        strings.Replace(good, "permissions:\n  contents: read\njobs", "permissions: write-all\njobs", 1),
		"no top-level permissions":    strings.Replace(good, "permissions:\n  contents: read\njobs", "jobs", 1),
		"packages: write in a second": strings.Replace(good, "  a:\n    steps: []", "  a:\n    permissions:\n      packages: write\n    steps: []", 1),
		"contents: write in the job":  strings.Replace(good, "contents: read\n      packages: write", "contents: write\n      packages: write", 1),
		"id-token: write in the job":  strings.Replace(good, "packages: write", "packages: write\n      id-token: write", 1),
		"no job with packages: write": strings.Replace(good, "packages: write", "packages: read", 1),
	} {
		if len(permissionProblems(parse(t, doc))) == 0 {
			t.Errorf("accepted: %s", name)
		}
	}
}

func TestOnlyTheImageWorkflowCanWritePackagesAndItUsesOnlyTheJobsToken(t *testing.T) {
	writers := 0
	for _, f := range workflowFiles(t) {
		raw, _ := os.ReadFile(f)
		doc := parse(t, string(raw))
		for _, job := range jobsOf(doc) {
			if perm, ok := job["permissions"].(map[string]any); ok && text(perm["packages"]) == "write" {
				writers++
				if filepath.Base(f) != "image.yml" {
					t.Errorf("%s has a job with packages: write", filepath.Base(f))
				}
			}
		}
	}
	if writers != 1 {
		t.Errorf("%d jobs with packages: write across the workflows, want exactly one", writers)
	}
	img := repoFile(t, imageWorkflow)
	for _, p := range permissionProblems(parse(t, img)) {
		t.Error(p)
	}
	for _, m := range regexp.MustCompile(`secrets\.(\w+)`).FindAllStringSubmatch(img, -1) {
		if m[1] != "GITHUB_TOKEN" {
			t.Errorf("the image workflow reads secrets.%s; only GITHUB_TOKEN is allowed", m[1])
		}
	}
	if !strings.Contains(img, "ghcr.io") || !strings.Contains(img, "secrets.GITHUB_TOKEN") {
		t.Error("the workflow does not log in to ghcr.io with secrets.GITHUB_TOKEN")
	}
	// The login is in the job that has the right to push.
	found := false
	for _, job := range jobsOf(parse(t, img)) {
		if perm, ok := job["permissions"].(map[string]any); ok && text(perm["packages"]) == "write" {
			found = strings.Contains(text(job), "GITHUB_TOKEN") && strings.Contains(text(job), "ghcr.io")
		}
	}
	if !found {
		t.Error("the job with packages: write does not log in to ghcr.io with GITHUB_TOKEN")
	}
}

// --- no run replaces or cancels another's.

func TestImageWorkflowNeverCancelsAnEarlierCommitsRun(t *testing.T) {
	doc := parse(t, repoFile(t, imageWorkflow))
	var groups []string
	collect := func(v any) {
		switch c := v.(type) {
		case string:
			groups = append(groups, c)
		case map[string]any:
			groups = append(groups, text(c["group"]))
			if cancel := text(c["cancel-in-progress"]); c["cancel-in-progress"] != nil && cancel != "false" {
				t.Errorf("cancel-in-progress is %v: a later push to main would cancel an earlier commit's run", cancel)
			}
		}
	}
	if doc["concurrency"] != nil {
		collect(doc["concurrency"])
	}
	for _, job := range jobsOf(doc) {
		if job["concurrency"] != nil {
			collect(job["concurrency"])
		}
	}
	if len(groups) == 0 {
		t.Fatal("no concurrency group: two runs for one commit or tag could push at the same time")
	}
	for _, g := range groups {
		if !strings.Contains(g, "github.sha") {
			t.Errorf("concurrency group %q is not keyed by the commit (github.sha): runs of different commits would share it, or runs of one commit would not", g)
		}
	}
}

// --- ci.yml is as before.

func TestCIWorkflowKeepsItsJobsAndBuildsNoImage(t *testing.T) {
	raw := repoFile(t, ".github/workflows/ci.yml")
	doc := parse(t, raw)
	want := []string{"changes", "lint-and-unit", "integration-total", "integration-shard", "integration", "roles", "fixture-cache", "deb-cache", "jars-cache", "cancel-media-jobs", "e2e-shard", "e2e"}
	jobs := jobsOf(doc)
	if len(jobs) != len(want) {
		t.Errorf("ci.yml has %d jobs, want the %d it has: %v", len(jobs), len(want), want)
	}
	for _, name := range want {
		if jobs[name] == nil {
			t.Errorf("ci.yml lost job %s", name)
		}
	}
	for _, banned := range []string{"client.sh", "image.sh", "publish.sh", "docker build", "buildx", "build-push-action", "ghcr.io", "docker/login"} {
		if strings.Contains(raw, banned) {
			t.Errorf("ci.yml mentions %q: the required checks must not build or test an image", banned)
		}
	}
	on := onOf(doc)
	if _, ok := on["pull_request"]; !ok {
		t.Error("ci.yml no longer runs on pull_request and merge_group")
	}
	if _, ok := on["merge_group"]; !ok {
		t.Error("ci.yml no longer runs on pull_request and merge_group")
	}
}

func yamlJSON(raw string, into any) error { return yaml.Unmarshal([]byte(raw), into) }
