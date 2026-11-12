package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Canned `go test -json` streams. Each line is one event.
const (
	passStream = `{"Action":"run","Package":"jellyscale/a","Test":"TestA"}
{"Action":"output","Package":"jellyscale/a","Test":"TestA","Output":"--- PASS: TestA\n"}
{"Action":"pass","Package":"jellyscale/a","Test":"TestA"}
{"Action":"pass","Package":"jellyscale/a"}
`
	failStream = `{"Action":"run","Package":"jellyscale/internal/a","Test":"TestA"}
{"Action":"output","Package":"jellyscale/internal/a","Test":"TestA","Output":"=== RUN   TestA\n"}
{"Action":"output","Package":"jellyscale/internal/a","Test":"TestA","Output":"    a_test.go:12: want 1, got 2\n"}
{"Action":"output","Package":"jellyscale/internal/a","Test":"TestA","Output":"--- FAIL: TestA (0.00s)\n"}
{"Action":"fail","Package":"jellyscale/internal/a","Test":"TestA"}
{"Action":"run","Package":"jellyscale/internal/a","Test":"TestB"}
{"Action":"pass","Package":"jellyscale/internal/a","Test":"TestB"}
{"Action":"fail","Package":"jellyscale/internal/a"}
`
	skipStream = `{"Action":"run","Package":"jellyscale/a","Test":"TestA"}
{"Action":"skip","Package":"jellyscale/a","Test":"TestA"}
{"Action":"skip","Package":"jellyscale/a"}
`
	subtestStream = `{"Action":"output","Package":"jellyscale/a","Test":"TestA/x","Output":"    a_test.go:7: bad x\n"}
{"Action":"fail","Package":"jellyscale/a","Test":"TestA/x"}
{"Action":"pass","Package":"jellyscale/a","Test":"TestA/y"}
{"Action":"fail","Package":"jellyscale/a","Test":"TestA"}
{"Action":"fail","Package":"jellyscale/a"}
`
	buildStream = `{"ImportPath":"jellyscale/internal/a [jellyscale/internal/a.test]","Action":"build-output","Output":"internal/a/a_test.go:5:2: undefined: x\n"}
{"ImportPath":"jellyscale/internal/a [jellyscale/internal/a.test]","Action":"build-fail"}
{"Action":"output","Package":"jellyscale/internal/a","Output":"FAIL\tjellyscale/internal/a [build failed]\n"}
{"Action":"fail","Package":"jellyscale/internal/a"}
`
)

func TestAnalyze(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		stream                       string
		want                         int
		passed, failed, skipped, ran int
		problems                     int
		file, line                   string // Of the first failure.
	}{
		{"pass", passStream, 1, 1, 0, 0, 1, 0, "", ""},
		{"fail", failStream, 2, 1, 1, 0, 2, 1, "internal/a/a_test.go", "12"},
		{"skip fails the run", skipStream, 1, 0, 0, 1, 1, 1, "", ""},
		{"a vanished test fails the run", passStream, 2, 1, 0, 0, 1, 1, "", ""},
		{"no count check", passStream, -1, 1, 0, 0, 1, 0, "", ""},
		{"a failed subtest is the failure, not its parent", subtestStream, -1, 1, 1, 0, 1, 1, "a/a_test.go", "7"},
		{"a build failure", buildStream, -1, 0, 1, 0, 0, 1, "internal/a/a_test.go", "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := analyze(strings.NewReader(tc.stream), tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Passed != tc.passed || rep.Failed != tc.failed || rep.Skipped != tc.skipped || rep.Ran != tc.ran || len(rep.Problems) != tc.problems {
				t.Fatalf("passed %d failed %d skipped %d ran %d problems %q, want %d %d %d %d and %d problems",
					rep.Passed, rep.Failed, rep.Skipped, rep.Ran, rep.Problems, tc.passed, tc.failed, tc.skipped, tc.ran, tc.problems)
			}
			if tc.file != "" && (len(rep.Failures) != 1 || rep.Failures[0].File != tc.file || rep.Failures[0].Line != tc.line) {
				t.Fatalf("failures %+v, want one at %s:%s", rep.Failures, tc.file, tc.line)
			}
		})
	}
}

func TestRenderAnnotatesWithFileAndLine(t *testing.T) {
	rep, _ := analyze(strings.NewReader(failStream), 2)
	var out, summary strings.Builder
	rep.render("go test", &out, &summary, true)
	for _, want := range []string{
		"go test: 2 tests; with subtests 1 passed, 1 failed, 0 skipped",
		"::error file=internal/a/a_test.go,line=12,title=TestA::a_test.go:12: want 1, got 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(summary.String(), "`TestA` (jellyscale/internal/a) `internal/a/a_test.go:12`\n\n<pre>\na_test.go:12: want 1, got 2\n</pre>") {
		t.Errorf("summary lacks the failure:\n%s", summary.String())
	}
	out.Reset()
	rep.render("go test", &out, nil, false)
	if strings.Contains(out.String(), "::error") {
		t.Errorf("annotations outside CI:\n%s", out.String())
	}
}

func TestSummaryKeepsAFailureInItsBlock(t *testing.T) {
	rep := report{Failures: []failure{{Name: "TestA", Package: "a", Lines: []string{"```", "<b>", "- x"}}}}
	var summary strings.Builder
	rep.render("go test", &strings.Builder{}, &summary, false)
	if !strings.Contains(summary.String(), "<pre>\n```\n&lt;b&gt;\n- x\n</pre>") {
		t.Errorf("the output can break out of its block:\n%s", summary.String())
	}
}

func TestFailureIsWhatTheTestSaidLast(t *testing.T) {
	var stream strings.Builder
	line := func(text string) {
		fmt.Fprintf(&stream, `{"Action":"output","Package":"a","Test":"TestA","Output":%q}`+"\n", text+"\n")
	}
	for i := range 12 {
		line(fmt.Sprintf("    a_test.go:%d: ffmpeg line", i+1))
	}
	line("    a_test.go:50: the real reason")
	line(`time=now level=INFO msg="stopping ffmpeg"`)
	stream.WriteString(`{"Action":"fail","Package":"a","Test":"TestA"}` + "\n")
	rep, err := analyze(strings.NewReader(stream.String()), -1)
	if err != nil {
		t.Fatal(err)
	}
	f := rep.Failures[0]
	if f.Line != "50" || !strings.Contains(strings.Join(f.Lines, "\n"), "the real reason") {
		t.Fatalf("line %s, lines %q", f.Line, f.Lines)
	}
}

// A package that hit go test's limit leaves tests that started and never
// finished; they are the failure, and the report names them.
const timeoutStream = `{"Action":"run","Package":"jellyscale/internal/a","Test":"TestDone"}
{"Action":"pass","Package":"jellyscale/internal/a","Test":"TestDone"}
{"Action":"run","Package":"jellyscale/internal/a","Test":"TestSlow"}
{"Action":"run","Package":"jellyscale/internal/a","Test":"TestParent"}
{"Action":"run","Package":"jellyscale/internal/a","Test":"TestParent/sub"}
{"Action":"output","Package":"jellyscale/internal/a","Output":"panic: test timed out after 30m0s\n"}
{"Action":"fail","Package":"jellyscale/internal/a"}
`

func TestATimedOutPackageNamesTheTestsThatDidNotFinish(t *testing.T) {
	rep, err := analyze(strings.NewReader(timeoutStream), -1)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range rep.Failures {
		names = append(names, f.Name+": "+f.Lines[0])
	}
	want := []string{
		"TestSlow: timed out while running TestSlow",
		"TestParent/sub: timed out while running TestParent/sub",
	}
	if !slices.Equal(names, want) || rep.Failed != 2 || len(rep.Problems) == 0 {
		t.Fatalf("failures %q, failed %d, problems %q, want %q", names, rep.Failed, rep.Problems, want)
	}
}

func TestSummaryHasOneGreppableLinePerFailure(t *testing.T) {
	rep, _ := analyze(strings.NewReader(timeoutStream), -1)
	var summary strings.Builder
	rep.render("go test", &strings.Builder{}, &summary, false)
	for _, want := range []string{"\nfailed: jellyscale/internal/a.TestSlow\n", "\nfailed: jellyscale/internal/a.TestParent/sub\n"} {
		if !strings.Contains(summary.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, summary.String())
		}
	}
}
