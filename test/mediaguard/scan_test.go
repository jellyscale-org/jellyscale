package mediaguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tree makes a git repository holding files and returns what scan reports for it.
func tree(t *testing.T, files map[string]string, allow map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		c := exec.Command("git", args...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	found, err := scan(root, allow)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(found, "\n")
}

func TestReportsTheFileAndLineOfEveryNamingInAFileNobodyAllowed(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a new integration test":        {"internal/z/z_integration_test.go": "//go:build integration\n\npackage z\n\nvar p = \"../../.claude/agents/x.md\"\n"},
		"a script called through $repo": {"test/integration.sh": "bash \"$repo/test/sub/x.sh\"\n", "test/sub/x.sh": "cat tasks/backlog.json\n"},
		"ls tasks/":                     {"test/a.sh": "echo\nls tasks/\n"},
		"cat .claude":                   {"test/a.sh": "cat .claude\n"},
		"a path joined from parts":      {"x/x_test.go": "package x\n\nvar p = filepath.Join(\".claude\", \"agents\")\nvar q = filepath.Join(\"tasks\", \"backlog.json\")\n"},
		"tasks ending a literal":        {"x/y_test.go": "package x\n\nvar p = \"../../tasks\" + \"/backlog.json\"\n"},
		"tasks after a variable":        {"test/b.sh": "cat \"$root/tasks\"/backlog.json\n"},
		"a slash before tasks":          {"x/z_test.go": "package x\n\nvar p = root + \"/tasks\"\n"},
		"a comment":                     {"x/x.go": "package x\n\n// see .claude/agents\n"},
		"a yaml step of a media job":    {".github/workflows/other.yml": "run: cat tasks/backlog.json\n"},
	} {
		got := tree(t, files, nil)
		if got == "" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	got := tree(t, map[string]string{"test/sub/x.sh": "echo\n\ncat tasks/backlog.json\ncat .claude\n"}, nil)
	if !strings.Contains(got, "test/sub/x.sh:3:") || !strings.Contains(got, "test/sub/x.sh:4:") {
		t.Errorf("want both lines named, got %q", got)
	}
	if got := tree(t, map[string]string{"internal/z/z_integration_test.go": "//go:build integration\n\npackage z\n\nvar p = \".claude/x\"\n"}, nil); !strings.Contains(got, "z_integration_test.go:5:") {
		t.Errorf("want file and line, got %q", got)
	}
}

func TestFilesThatMayNotBeSearchedAreLeftAlone(t *testing.T) {
	got := tree(t, map[string]string{
		".claude/agents/x.md": ".claude tasks/",
		"tasks/backlog.json":  ".claude tasks/",
		"README.md":           ".claude tasks/",
		"docs/a.md":           "tasks/",
		"x/x.go":              "package x\n\n// the other tasks' work\n",
	}, nil)
	if got != "" {
		t.Errorf("got %q", got)
	}
}

func TestAnAllowedGoFileMustRunInLintAndUnit(t *testing.T) {
	ok := "package p\n\nvar x = \"tasks/prd.json\"\n"
	for name, c := range map[string]struct{ file, text string }{
		"go:build":           {"p/p_test.go", "//go:build integration\n\n" + ok},
		"go:build, indented": {"p/p_test.go", "  //go:build integration\n\n" + ok},
		"+build":             {"p/p_test.go", "// +build integration\n\n" + ok},
		"+build, tab":        {"p/p_test.go", "\t// +build integration\n\n" + ok},
		"package main":       {"p/p_test.go", "package main\n\nvar x = \"tasks/prd.json\"\n"},
		"under testdata":     {"p/testdata/p_test.go", ok},
		"not Go":             {"p/run.mjs", "tasks/prd.json\n"},
		"a script not run":   {"test/e2e/run.sh", "tasks/prd.json\n"},
	} {
		got := tree(t, map[string]string{c.file: c.text}, map[string]string{c.file: "why"})
		if !strings.HasPrefix(got, c.file+":1:") {
			t.Errorf("%s: got %q", name, got)
		}
	}
	for _, file := range []string{"p/p_test.go", "p/p.go", "test/changes.sh"} {
		text := ok
		if strings.HasSuffix(file, ".sh") {
			text = "cat tasks/other.json\n"
		}
		if got := tree(t, map[string]string{file: text}, map[string]string{file: "why"}); got != "" {
			t.Errorf("%s: got %q", file, got)
		}
	}
}

func TestAnAllowedFileThatIsGoneIsReported(t *testing.T) {
	got := tree(t, map[string]string{"a.txt": "x"}, map[string]string{"p/gone_test.go": "why"})
	if !strings.Contains(got, "p/gone_test.go") {
		t.Errorf("got %q", got)
	}
}

func TestAnAllowedFileMayNameOnlyWhatItIsPermitted(t *testing.T) {
	file := "test/differential/files_test.go"
	text := "package differential\n\nvar a = \"tasks/prd.json\"\nvar b = \"tasks/backlog.json\"\nvar c = \"tasks/prd.json.bak\"\n// .claude/x\n"
	got := tree(t, map[string]string{file: text}, map[string]string{file: "why"})
	if strings.Contains(got, file+":3:") || !strings.Contains(got, file+":4:") ||
		!strings.Contains(got, file+":5:") || !strings.Contains(got, file+":6:") {
		t.Errorf("got %q", got)
	}
	file = "internal/transcode/faults_docs_test.go"
	got = tree(t, map[string]string{file: "package transcode\n\nvar a = \".claude/workflows/build.js\"\nvar b = \".claude/agents/x.md\"\n"}, map[string]string{file: "why"})
	if strings.Contains(got, file+":3:") || !strings.Contains(got, file+":4:") {
		t.Errorf("got %q", got)
	}
}

func TestOnlyTheChangesAndLintJobsOfCIMayNameThem(t *testing.T) {
	ci := ".github/workflows/ci.yml"
	text := "# .claude/ and tasks/ skip\njobs:\n  changes:\n    steps:\n      - run: ls .claude\n  lint-and-unit:\n    steps:\n      - run: ls tasks/\n" +
		"  integration-shard:\n    steps:\n      - run: cat tasks/backlog.json && bash test/integration.sh\n" +
		"  jars-cache:\n    key: ${{ hashFiles('.claude/x') }}\n"
	got := tree(t, map[string]string{ci: text}, map[string]string{ci: "why"})
	for _, line := range []string{"1", "5", "8"} {
		if strings.Contains(got, ci+":"+line+":") {
			t.Errorf("line %s reported: %q", line, got)
		}
	}
	if !strings.Contains(got, ci+":11:") || !strings.Contains(got, ci+":13:") {
		t.Errorf("got %q", got)
	}
}

func TestMarkdownOutsideTheRootAndDocsIsGuarded(t *testing.T) {
	got := tree(t, map[string]string{"test/fixtures/NOTES.md": "See tasks/backlog.json\n", "docs/sub/x.md": "tasks/\n"}, nil)
	if !strings.Contains(got, "test/fixtures/NOTES.md:1:") || !strings.Contains(got, "docs/sub/x.md:1:") {
		t.Errorf("got %q", got)
	}
}

func TestAnEmptyExpectedGroupIsReported(t *testing.T) {
	root := t.TempDir()
	write := func(name, text string) {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	add := func() {
		for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
			c := exec.Command("git", args...)
			c.Dir = root
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
	}
	write("test/e2e/a.mjs", "x")
	write("test/differential/a.go", "package d\n")
	write("x/x_test.go", "package x\n")
	add()
	got, _ := emptyGroups(root)
	if len(got) != 1 || !strings.Contains(got[0], "build constraint") {
		t.Errorf("no constraint: %q", got)
	}
	write("x/x_test.go", "//go:build integration\n\npackage x\n")
	add()
	if got, _ := emptyGroups(root); len(got) != 0 {
		t.Errorf("full: %q", got)
	}
}
