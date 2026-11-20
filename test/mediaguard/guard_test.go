// Package mediaguard is one unit test, so lint-and-unit runs it. CI skips the media tiers
// for a change confined to .claude/ and tasks/, which is safe only while nothing a media
// tier runs reads those paths. The test therefore denies by default: every file git
// tracks (but .claude/, tasks/, Markdown and this test) is reported on each line that
// names .claude or tasks/, unless the file is in allowed below, and each allowed file
// must itself run in lint-and-unit. It does not work out what the media tiers run, so no
// way of naming a script (a variable, a path joined from parts, a sourced file) hides one.
package mediaguard

import (
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// allowed lists the files that may name .claude or tasks/, each with why. Each must be a
// Go file that go test ./... runs, or one of the scripts lint-and-unit runs (see scripts).
var allowed = map[string]string{
	"compat/docs_agree_test.go":                "a unit test that reads tasks/prd.json, tasks/done.json and tasks/decisions.json",
	".github/workflows/ci.yml":                 "the changes job names them as the paths that skip the media tiers",
	"internal/api/playlist_token_docs_test.go": "a unit test that reads tasks/prd.json",
	"internal/transcode/faults_docs_test.go":   "a unit test that reads .claude/workflows/build.js",
	"test/changes.sh":                          "decides which paths skip the media tiers",
	"test/scripts_test.sh":                     "tests changes.sh and ci.yml with such paths",
	"test/differential/allowlist.go":           "names tasks/prd.json in a comment",
	"test/differential/files_test.go":          "a unit test that reads tasks/prd.json and tasks/done.json",
}

// scripts are the files other than Go tests that lint-and-unit runs.
var scripts = map[string]bool{"test/scripts_test.sh": true, "test/changes.sh": true, ".github/workflows/ci.yml": true}

// permitted narrows an allowed file: it may name only these paths, and any other line that
// names .claude or tasks/ is reported. A file with no entry may name anything, because it
// is itself a lint-and-unit script that tests those paths. tasks/prd.json must not be
// followed by more of a name (prd.json.bak), so the match stops at the end of the path.
var permitted = map[string]*regexp.Regexp{
	"compat/docs_agree_test.go":                regexp.MustCompile(`tasks/(?:prd|done|decisions)\.json([^\w./-]|\.?$|\.\s)`),
	"internal/api/playlist_token_docs_test.go": regexp.MustCompile(`tasks/prd\.json([^\w./-]|$)`),
	"internal/transcode/faults_docs_test.go":   regexp.MustCompile(`\.claude/workflows/build\.js([^\w./-]|$)`),
	"test/differential/allowlist.go":           regexp.MustCompile(`tasks/prd\.json([^\w./-]|$)`),
	"test/differential/files_test.go":          regexp.MustCompile(`tasks/(prd|done)\.json([^\w./-]|$)`),
}

// ciExempt is the part of ci.yml that may name them: the comments before jobs: and the
// two jobs that run on every change. Every other job runs a media tier or its caches.
func ciExempt(lines []string) []bool {
	exempt := make([]bool, len(lines))
	in, started := true, false
	job := regexp.MustCompile(`^  ([\w-]+):`)
	for i, line := range lines {
		if line == "jobs:" {
			started = true
		}
		if m := job.FindStringSubmatch(line); started && m != nil {
			in = m[1] == "changes" || m[1] == "lint-and-unit"
		}
		exempt[i] = in
	}
	return exempt
}

var (
	// tasks counts as a path part when a slash or a quote is next to it (after it, not an
	// apostrophe, so the possessive "other tasks'" in prose is not one), so a
	// path joined from parts ("../../tasks" + "/x", $root/tasks) is found and the word in
	// prose (scheduled tasks) is not. A name split inside itself (".cla""ude") is not found:
	// the guard catches mistakes, not a file written to hide from it.
	names      = regexp.MustCompile("\\.claude|[/\"'`]tasks\\b|\\btasks[/\"`]")
	constraint = regexp.MustCompile(`(?m)^\s*(//go:build|// \+build)\b`)
	pkgLine    = regexp.MustCompile(`(?m)^package\s+(\w+)`)
)

func skipped(file string) bool {
	// Only the Markdown that changes.sh calls docs: a .md elsewhere (test/fixtures) is guarded.
	dir := path.Dir(file)
	docs := strings.HasSuffix(file, ".md") && (dir == "." || dir == "docs")
	return strings.HasPrefix(file, ".claude/") || strings.HasPrefix(file, "tasks/") ||
		strings.HasPrefix(file, "test/mediaguard/") || docs
}

// scan reports, as "file:line: reason", every line of a tracked file under root that names
// .claude or tasks/ where the file may not, and every allowed file that does not run in
// lint-and-unit.
func scan(root string, allow map[string]string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var found []string
	seen := map[string]bool{}
	for _, file := range strings.Split(string(out), "\x00") {
		if file == "" {
			continue
		}
		seen[file] = true
		_, ok := allow[file]
		if skipped(file) && !ok {
			continue
		}
		raw, err := os.ReadFile(root + "/" + file)
		if err != nil {
			return nil, err
		}
		text := string(raw)
		if ok {
			if why := notUnitTier(file, text); why != "" {
				found = append(found, file+":1: allowed to name .claude or tasks/ but "+why)
			}
		}
		lines := strings.Split(text, "\n")
		var exempt []bool
		if ok && file == ".github/workflows/ci.yml" {
			exempt = ciExempt(lines)
		}
		for i, line := range lines {
			if ok && permitted[file] == nil && exempt == nil {
				break
			}
			if exempt != nil && exempt[i] {
				continue
			}
			if re := permitted[file]; re != nil {
				line = re.ReplaceAllString(line, "$1")
			}
			if names.MatchString(line) {
				found = append(found, file+":"+strconv.Itoa(i+1)+": names .claude or tasks/, which no media tier may read: "+strings.TrimSpace(line))
			}
		}
	}
	for file := range allow {
		if !seen[file] {
			found = append(found, file+":1: in the allowlist but not tracked by git")
		}
	}
	return found, nil
}

// notUnitTier says why a file would not run in lint-and-unit, or "" when it does.
func notUnitTier(file, text string) string {
	if scripts[file] {
		return ""
	}
	switch {
	case !strings.HasSuffix(file, ".go"):
		return "is not a Go file or one of the scripts lint-and-unit runs"
	case constraint.MatchString(text):
		return "has a build constraint, so go test ./... does not run it"
	case strings.Contains("/"+file, "/testdata/"):
		return "is under testdata/, which go test ./... does not run"
	}
	if m := pkgLine.FindStringSubmatch(text); m == nil || m[1] == "main" {
		return "is package main"
	}
	return ""
}

// emptyGroups reports each group of guarded files the guard expects that has nothing in
// it, so moving test/e2e or dropping every build constraint cannot leave it checking nothing.
func emptyGroups(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var found []string
	dirs := map[string]bool{"test/e2e/": false, "test/differential/": false}
	constrained := false
	for _, file := range strings.Split(string(out), "\x00") {
		for d := range dirs {
			if strings.HasPrefix(file, d) {
				dirs[d] = true
			}
		}
		if strings.HasSuffix(file, ".go") && !constrained {
			raw, err := os.ReadFile(root + "/" + file)
			if err != nil {
				return nil, err
			}
			constrained = constraint.Match(raw)
		}
	}
	for d, has := range dirs {
		if !has {
			found = append(found, d+": no tracked file, so the guard checks nothing there")
		}
	}
	if !constrained {
		found = append(found, "no tracked Go file has a build constraint, so the guard checks nothing there")
	}
	return found, nil
}

func TestNoFileAMediaTierRunsNamesClaudeOrTasks(t *testing.T) {
	found, err := scan("../..", allowed)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := emptyGroups("../..")
	if err != nil {
		t.Fatal(err)
	}
	found = append(found, empty...)
	for _, f := range found {
		t.Error(f)
	}
}
