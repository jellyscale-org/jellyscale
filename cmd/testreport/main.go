// Command testreport turns the `go test -json` stream of a run into something
// readable without opening a log: counts and the first lines of each failure on
// the job summary ($GITHUB_STEP_SUMMARY) and, with $GITHUB_ACTIONS set, an
// annotation with file and line for each failure, which `gh run view` shows.
//
// A skipped test fails the run, and so does a count of tests that differs from
// -want (the tests `go test -list` finds for the tier): a run that stopped
// partway must not leave CI green. The list can't see a test its tags hide;
// TestBuildTags checks those.
// test/report.sh runs it; it exits 1 if the run has any problem.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

type event struct {
	Action     string
	Package    string
	Test       string
	Output     string
	ImportPath string
}

type failure struct {
	Package, Name, File, Line string
	Lines                     []string // What the test said, without the framework's own.
	Output                    string   // All of it, for the log.
}

type report struct {
	Passed, Failed, Skipped, Ran int
	Failures                     []failure
	Skips                        []string
	Problems                     []string
}

type result struct {
	pkg, name, action string
	out               strings.Builder
}

var (
	fileLine  = regexp.MustCompile(`^\s*(\S+\.go):(\d+):`)
	frameLine = regexp.MustCompile(`^(=== |--- |\s*$)`)
	pkgNoise  = regexp.MustCompile(`^(FAIL|ok|\s*$)`)
)

// analyze reads the stream. want is the number of top-level tests the tier
// should have run; negative skips that check.
func analyze(in io.Reader, want int) (report, error) {
	tests := map[string]*result{}
	var order []string
	pkgOut := map[string]*strings.Builder{} // Output of a package outside any test, and its build.
	pkgFailed := map[string]bool{}
	sc := bufio.NewScanner(in)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // go test prints non-JSON lines when a build fails.
		}
		if e.Action == "build-output" {
			// ImportPath is "pkg [pkg.test]" for a test binary.
			e.Package = strings.Fields(e.ImportPath)[0]
		}
		if e.Test == "" {
			if e.Action == "output" || e.Action == "build-output" {
				if pkgOut[e.Package] == nil {
					pkgOut[e.Package] = &strings.Builder{}
				}
				pkgOut[e.Package].WriteString(e.Output)
			}
			if e.Action == "fail" {
				pkgFailed[e.Package] = true
			}
			continue
		}
		key := e.Package + " " + e.Test
		r := tests[key]
		if r == nil {
			r = &result{pkg: e.Package, name: e.Test, action: "none"}
			tests[key] = r
			order = append(order, key)
		}
		switch e.Action {
		case "output":
			r.out.WriteString(e.Output)
		case "pass", "fail", "skip":
			if r.action == "none" {
				r.action = e.Action
			}
		}
	}
	if err := sc.Err(); err != nil {
		return report{}, err
	}

	var rep report
	failedSub := map[string]bool{} // "pkg parent" for each parent with a failed subtest.
	for _, key := range order {
		r := tests[key]
		if r.action == "fail" {
			for p := r.name; strings.Contains(p, "/"); {
				p = p[:strings.LastIndex(p, "/")]
				failedSub[r.pkg+" "+p] = true
			}
		}
	}
	for _, key := range order {
		r := tests[key]
		switch r.action {
		case "pass":
			rep.Passed++
		case "skip":
			rep.Skipped++
			rep.Skips = append(rep.Skips, r.pkg+" "+r.name)
		}
		if r.action != "none" && !strings.Contains(r.name, "/") {
			rep.Ran++
		}
		// A test fails if a subtest of it does, so only the subtest is listed.
		if r.action == "fail" && !failedSub[key] {
			rep.Failed++
			rep.Failures = append(rep.Failures, testFailure(r))
		}
	}

	// A test that started and never finished: the package timed out or crashed
	// while running it. Of a subtest and its parents, only the subtest is named.
	unfinished := map[string]bool{}
	for _, key := range order {
		if tests[key].action == "none" {
			unfinished[key] = true
			for p := tests[key].name; strings.Contains(p, "/"); {
				p = p[:strings.LastIndex(p, "/")]
				delete(unfinished, tests[key].pkg+" "+p)
			}
		}
	}
	for _, key := range order {
		r := tests[key]
		if !unfinished[key] {
			continue
		}
		why := "stopped"
		if pkgOut[r.pkg] != nil && strings.Contains(pkgOut[r.pkg].String(), "test timed out") {
			why = "timed out"
		}
		f := testFailure(r)
		f.Lines = append([]string{why + " while running " + r.name}, f.Lines...)
		f.Output += pkgOut[r.pkg].String()
		rep.Failures = append(rep.Failures, f)
		rep.Failed++
	}

	// A package that failed with no failed test: a build error, a timeout, a panic.
	var pkgs []string
	for p := range pkgOut {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		hasFailedTest := false
		for _, f := range rep.Failures {
			hasFailedTest = hasFailedTest || f.Package == p
		}
		if hasFailedTest || (!pkgFailed[p] && !buildFailed(pkgOut[p].String())) {
			continue
		}
		rep.Failures = append(rep.Failures, packageFailure(p, pkgOut[p].String()))
		rep.Failed++
	}

	if rep.Skipped > 0 {
		rep.Problems = append(rep.Problems, "skipped tests are failures; a test lacking a dependency must fail with a message naming it:\n"+strings.Join(rep.Skips, "\n"))
	}
	if want >= 0 && rep.Ran != want {
		rep.Problems = append(rep.Problems, fmt.Sprintf("ran %d tests, but go test -list finds %d for this tier: the run stopped partway", rep.Ran, want))
	}
	if rep.Failed > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf("%d failed", rep.Failed))
	}
	return rep, nil
}

func buildFailed(out string) bool {
	return strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]")
}

func dirOf(pkg string) string {
	dir := strings.TrimPrefix(pkg, "jellyscale")
	return strings.TrimPrefix(dir, "/")
}

func testFailure(r *result) failure {
	f := failure{Package: r.pkg, Name: r.name, Output: r.out.String()}
	lines := strings.Split(r.out.String(), "\n")
	// A test can log for pages before it fails, so the reason is its last
	// located line, and what follows is cleanup or more of the message.
	for i, l := range slices.Backward(lines) {
		if m := fileLine.FindStringSubmatch(l); m != nil {
			f.File, f.Line = joinDir(dirOf(r.pkg), m[1]), m[2]
			lines = lines[i:]
			break
		}
	}
	for _, l := range lines {
		if !frameLine.MatchString(l) && len(f.Lines) < 8 {
			f.Lines = append(f.Lines, strings.TrimLeft(l, " \t"))
		}
	}
	return f
}

func packageFailure(pkg, out string) failure {
	f := failure{Package: pkg, Name: "package " + pkg, Output: out}
	for _, l := range strings.Split(out, "\n") {
		if pkgNoise.MatchString(l) || len(f.Lines) >= 12 {
			continue
		}
		f.Lines = append(f.Lines, l)
		if m := fileLine.FindStringSubmatch(l); m != nil && f.File == "" {
			f.File, f.Line = m[1], m[2] // The compiler's paths are from the repository root already.
		}
	}
	return f
}

// ledger is "<package>.<Test>", or just the package when the package itself failed.
func (f failure) ledger() string {
	if strings.HasPrefix(f.Name, "package ") {
		return f.Package
	}
	return f.Package + "." + f.Name
}

func joinDir(dir, file string) string {
	if dir == "" || strings.HasPrefix(file, "/") {
		return file
	}
	return strings.TrimPrefix(dir+"/"+strings.TrimPrefix(file, "./"), "./")
}

func escape(s string, property bool) string {
	s = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
	if property {
		s = strings.NewReplacer(":", "%3A", ",", "%2C").Replace(s)
	}
	return s
}

// render writes the plain text to out, the annotations to out too when
// annotate is set, and markdown to summary when it is not nil.
func (rep report) render(title string, out, summary io.Writer, annotate bool) {
	fmt.Fprintf(out, "%s: %d tests; with subtests %d passed, %d failed, %d skipped\n", title, rep.Ran, rep.Passed, rep.Failed, rep.Skipped)
	if summary != nil {
		fmt.Fprintf(summary, "\n### %s\n\n%d tests; with subtests %d passed, %d failed, %d skipped\n\n", title, rep.Ran, rep.Passed, rep.Failed, rep.Skipped)
	}
	for _, f := range rep.Failures {
		body := strings.Join(f.Lines, "\n")
		fmt.Fprintf(out, "FAIL  %s %s\n", f.Package, f.Name)
		for _, l := range f.Lines {
			fmt.Fprintf(out, "        %s\n", l)
		}
		if annotate {
			where := ""
			if f.File != "" {
				where = fmt.Sprintf("file=%s,line=%s,", escape(f.File, true), f.Line)
			}
			fmt.Fprintf(out, "::error %stitle=%s::%s\n", where, escape(f.Name, true), escape(body, false))
		}
		if summary != nil {
			// One greppable line per failure, for collecting flakes across runs with gh.
			fmt.Fprintf(summary, "failed: %s\n\n", f.ledger())
			at := ""
			if f.File != "" {
				at = " `" + f.File + ":" + f.Line + "`"
			}
			// <pre>, not a code fence: a test's output can hold backticks.
			fmt.Fprintf(summary, "`%s` (%s)%s\n\n<pre>\n%s\n</pre>\n\n", f.Name, f.Package, at, html.EscapeString(body))
		}
	}
	for _, p := range rep.Problems {
		if strings.HasSuffix(p, " failed") {
			continue // Said by the failures.
		}
		fmt.Fprintf(out, "FAIL  %s\n", p)
		if annotate {
			fmt.Fprintf(out, "::error title=%s::%s\n", escape(title, true), escape(p, false))
		}
		if summary != nil {
			fmt.Fprintf(summary, "<pre>\n%s\n</pre>\n\n", html.EscapeString(p))
		}
	}
}

func main() {
	jsonFile := flag.String("json", "", "the `go test -json` stream")
	title := flag.String("title", "go test", "heading for the summary")
	want := flag.Int("want", -1, "tests the tier should have run; -1 skips the check")
	logFile := flag.String("log", "", "where to write the full output of each failure")
	flag.Parse()

	in, err := os.Open(*jsonFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(1)
	}
	defer in.Close()
	rep, err := analyze(in, *want)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testreport:", err)
		os.Exit(1)
	}
	var summary io.Writer
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			defer f.Close()
			summary = f
		}
	}
	rep.render(*title, os.Stdout, summary, os.Getenv("GITHUB_ACTIONS") != "")
	if *logFile != "" && len(rep.Failures) > 0 {
		var b strings.Builder
		for _, f := range rep.Failures {
			fmt.Fprintf(&b, "=== %s %s\n%s\n", f.Package, f.Name, f.Output)
		}
		os.WriteFile(*logFile, []byte(b.String()), 0o644) //nolint:errcheck
	}
	if len(rep.Problems) > 0 {
		os.Exit(1)
	}
}
