package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// JS-081: a test that passes or fails on a measured duration or speed passes on
// a developer's machine and fails on a shared runner. This finds every line of
// a test file that compares a measured time or speed with a bound, and each
// must be listed in test/timing-bounds.txt with the reason it only stops a hang.
// A bar (a duration with an upper bound, two durations compared, a speed with a
// lower bound or a not-slow flag) has no reason, so it is not listed: the test
// fails and names it, and the fix is a check of what the code does. The rule is
// in docs/testing.md.

var (
	// A measured thing next to a comparison, with strings and comments gone.
	goTiming = regexp.MustCompile(`(?:\b(?:took|elapsed|slowest|total|claim|gone|after|speed|alone|freeze|timings\[\w+\]|time\.Since\([^()]*\))|\.Seconds\(\)|\bleft|\bEncodeSpeed)\s*[<>]=?\s*[^<>=\-]` +
		`|[^<>=\-]\s*[<>]=?\s*(?:\w+\.)?(?:took|elapsed|slowest|time\.Since\([^()]*\))\b`)
	jsTiming = regexp.MustCompile(`Date\.now\(\)\s*-\s*[\w.]+\s*[<>]|\.at\s*-\s*\w+\s*[<>]|\bfreeze\s*[<>]|\bcopy\s*>\s*encoded`)
	// A bound is found by what it is compared with as well as by the name of
	// what is measured, so a bar under a new name is found too: a duration
	// constant on one side of a comparison, or a variable that was assigned a
	// measured time (time.Since, Sub, Date.now() minus a start) and is compared
	// later in the same file.
	goBound    = regexp.MustCompile(`[<>]=?\s*[\w.()+*/ ]*\btime\.(?:Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)\b|\btime\.(?:Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)\s*[<>]`)
	goMeasured = regexp.MustCompile(`^\s*(?:if |\} else if )?(\w+(?:,\s*\w+)*)\s*:?=\s*[^=].*(?:\btime\.Since\(|\.Sub\(|\.Seconds\(\)|\.Milliseconds\(\)|[Dd]uration|[Ss]peed|[Ll]atency|[Ee]lapsed)`)
	jsBound    = regexp.MustCompile(`Date\.now\(\)[^;]*[<>]=?|[<>]=?[^;]*Date\.now\(\)|performance\.now\(\)[^;]*[<>]=?`)
	jsMeasured = regexp.MustCompile(`^\s*(?:const|let|var)\s+(\w+)\s*=\s*(?:Date|performance)\.now\(\)\s*-`)
	shBound    = regexp.MustCompile(`\bSECONDS\b.*(?:[<>]|-le\b|-lt\b|-ge\b|-gt\b)|(?:[<>]|-le\b|-lt\b|-ge\b|-gt\b).*\bSECONDS\b|date \+%s.*(?:[<>]|-le\b|-lt\b|-ge\b|-gt\b)`)
	goString   = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`")
	jsString   = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|'(?:[^'\\\\]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`")
)

type bound struct{ path, line, reason string }

func timingBounds(t *testing.T, root string) (found []bound) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web-dist", ".results", "spikes":
				return filepath.SkipDir
			}
			return nil
		}
		isGo := strings.HasSuffix(path, "_test.go")
		isSh := strings.HasPrefix(rel, "test"+string(filepath.Separator)) && strings.HasSuffix(path, ".sh") && !strings.HasSuffix(path, "_test.sh")
		isJS := strings.HasPrefix(rel, "test"+string(filepath.Separator)+"e2e") && strings.HasSuffix(path, ".mjs") && !strings.HasSuffix(path, "_test.mjs")
		if !isGo && !isJS && !isSh {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Names assigned a measured time anywhere in the file, compared on any later line.
		var names []string
		for line := range strings.Lines(string(source)) {
			if isGo {
				if m := goMeasured.FindStringSubmatch(goString.ReplaceAllString(line, `""`)); m != nil {
					names = append(names, strings.Fields(strings.ReplaceAll(m[1], ",", " "))...)
				}
			} else if isJS {
				if m := jsMeasured.FindStringSubmatch(line); m != nil {
					names = append(names, m[1])
				}
			}
		}
		compared := func(code string) bool {
			for _, name := range names {
				if name == "_" || name == "err" || name == "ok" {
					continue
				}
				if regexp.MustCompile(`\b` + name + `\b\s*[<>]=?[^<>=\-]|[^<>=\-]\s*[<>]=?\s*` + name + `\b`).MatchString(code) {
					return true
				}
			}
			return false
		}
		for line := range strings.Lines(string(source)) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "for ") || strings.HasPrefix(line, "while ") || strings.HasPrefix(line, "while(") {
				continue // A loop that waits is a wait with a timeout.
			}
			code := line
			hit := false
			switch {
			case isGo:
				code = goString.ReplaceAllString(code, `""`)
				code, _, _ = strings.Cut(code, " //")
				hit = goTiming.MatchString(code) || goBound.MatchString(code) || compared(code)
			case isJS:
				code = jsString.ReplaceAllString(code, `""`)
				code, _, _ = strings.Cut(code, " //")
				code = strings.ReplaceAll(code, "=>", "->")
				hit = jsTiming.MatchString(code) || jsBound.MatchString(code) || compared(code)
			default:
				hit = shBound.MatchString(code)
			}
			if hit {
				found = append(found, bound{path: filepath.ToSlash(rel), line: line})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// accounted reads test/timing-bounds.txt: path, the line as it is in the file,
// and the reason, separated by tabs. A line of "*" accounts for every line
// found in the file (a unit test that gives the clock itself).
func accounted(t *testing.T, root string) (entries []bound) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "test", "timing-bounds.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range strings.Split(string(raw), "\n") {
		if text = strings.TrimSpace(text); text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(text, "\t")
		if len(parts) != 3 || strings.TrimSpace(parts[2]) == "" {
			t.Errorf("test/timing-bounds.txt: %q needs a path, a line and a reason, separated by tabs", text)
			continue
		}
		entries = append(entries, bound{strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])})
	}
	return entries
}

func TestNoTestPassesOrFailsOnAMeasuredTimeOrSpeed(t *testing.T) {
	root := filepath.Join("..", "..")
	found := timingBounds(t, root)
	entries := accounted(t, root)
	if len(found) == 0 {
		t.Fatal("found no comparison of a time in any test file: the search is broken")
	}
	used := make([]bool, len(entries))
	for _, f := range found {
		listed := false
		for i, e := range entries {
			if e.path == f.path && (e.line == f.line || e.line == "*") {
				used[i], listed = true, true
			}
		}
		if !listed {
			t.Errorf("%s: %q compares a measured time or speed with a bound and is not a listed hang bound: it is a bar, so check what the code does instead (docs/testing.md)", f.path, f.line)
		}
	}
	for i, e := range entries {
		if !used[i] {
			t.Errorf("test/timing-bounds.txt lists %s: %q, which no test has any more: remove it", e.path, e.line)
		}
	}
}

// The files the task names have no bar left, whatever the search above finds.
func TestTheNamedBarsAreGone(t *testing.T) {
	root := filepath.Join("..", "..")
	for file, bars := range map[string][]string{
		"internal/transcode/helpers_integration_test.go":         {"total > 63*time.Second", "slowest > 9*time.Second", "took > 9*time.Second", "timings[n] > most[n]", "timings[1] * 80 / 100"},
		"internal/transcode/helpers_preempt_integration_test.go": {"slowest > 9*time.Second", "res.took > alone+3*time.Second"},
		"internal/transcode/hdrcopy_media_integration_test.go":   {"took > limit", "10x real time"},
		"internal/transcode/failover_integration_test.go":        {"after > 7*time.Second", "took > lease+3*time.Second"},
		"internal/transcode/chapter_jobs_integration_test.go":    {"took > 2*time.Second"},
		"internal/transcode/metrics_integration_test.go":         {"speed < 0.6 || speed > 1.5"},
		"internal/api/transcoding_info_integration_test.go":      {"speed < 0.1 || speed > 0.6", "speed < 1.5 || speed > 6", "speed < 1 {"},
		"test/roles.sh":          {"SECONDS - began <= 10"},
		"test/e2e/transcode.mjs": {"> 78000", "> 6000", "> 5000", "copy > encoded"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, bar := range bars {
			if strings.Contains(string(raw), bar) {
				t.Errorf("%s still has %q", file, bar)
			}
		}
	}
}

// Every timing a bar measured is still logged, and docs/testing.md says that
// tests check mechanisms, gives the rule for a time bound that may stay, and
// says where a run's logged timings are kept.
func TestTheLoggedTimingsStayAndTheDocsSayWhere(t *testing.T) {
	root := filepath.Join("..", "..")
	for file, logs := range map[string][]string{
		"internal/transcode/helpers_integration_test.go":       {"speedup over one worker", "slowest"},
		"internal/transcode/hdrcopy_media_integration_test.go": {"x real time"},
		"internal/transcode/failover_integration_test.go":      {"SIGKILL to the replacement's first segment"},
		"test/e2e/transcode.mjs":                               {"the longest stand-still was", "after it started"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		for _, log := range logs {
			if !strings.Contains(string(raw), log) {
				t.Errorf("%s no longer logs %q", file, log)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "docs", "testing.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(raw))
	for _, want := range []string{"mechanism", "timing-bounds.txt", "hang", "ten times", "tests.json", ".results"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/testing.md does not mention %q", want)
		}
	}
	// It says what a run that passed keeps, locally and in CI.
	if !regexp.MustCompile(`(?s)timings[^.]*\bpassed\b|\bpassed\b[^.]*timings`).MatchString(text) {
		t.Error("docs/testing.md does not say where the timings of a run that passed are kept")
	}
}

// The search finds a bar by what it compares, not by the name it was given, in
// Go, in the e2e scripts and in the shell scripts. Each file below holds one
// bar under a name no list knows, and a hang bound that is a wait.
func TestTheSearchFindsABarWhateverItIsCalled(t *testing.T) {
	root := t.TempDir()
	for file, source := range map[string]string{
		"internal/x/a_test.go":    "package x\nfunc f() {\n\td := time.Since(began)\n\tif d > 2*time.Second {\n\t}\n\tif x := time.Since(b); x > limit {\n\t}\n\tfor time.Since(began) < 5*time.Second {\n\t}\n}\n",
		"test/e2e/b.mjs":          "const waited = Date.now() - began;\nif (waited > 78e3) throw 1;\nif (Date.now() - began > 78000) throw 2;\nwhile (Date.now() - began < 5000) {}\nawait waitFor(\"x\", async () => ok, 5000);\n",
		"test/roles.sh":           "began=$SECONDS\ncheck \"x\" 1 \"$(( SECONDS - began <= 10 ))\"\n# SECONDS - began > 3\n",
		"test/sub/other.sh":       "[ $(( $(date +%s) - start )) -gt 5 ] && exit 1\n",
		"internal/x/skip_test.go": "package x\nfunc g() { for n := 0; n < 3; n++ {} ; if n > 2 {} }\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]int{}
	for _, f := range timingBounds(t, root) {
		got[f.path]++
	}
	want := map[string]int{"internal/x/a_test.go": 2, "test/e2e/b.mjs": 2, "test/roles.sh": 1, "test/sub/other.sh": 1}
	for path, n := range want {
		if got[path] != n {
			t.Errorf("%s: found %d bars, want %d (found: %v)", path, got[path], n, got)
		}
	}
	if got["internal/x/skip_test.go"] != 0 {
		t.Errorf("a loop counter is not a time: %v", got)
	}
}
