package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// JS-114: the tests of this package start jellyscale processes through one harness: one process type, one
// log sink, one free-port helper, one sign-in. This test reads every file of the package, whatever its build
// tag (a tag keeps a file out of the compile, not out of this reading), and of every package they import
// that exists for tests (a non-test file that imports "testing"), and counts what each is defined as, by
// shape and not by name, so a second under another name, or inline in another function, fails it.
//
// The shapes: a process type is a struct holding an exec.Cmd; a log sink is a type with Write([]byte) (int,
// error); a free-port helper is a function that listens on a ":0" address and closes the listener; the
// sign-in is the string literal of its route; a start is an exec.Command of anything but a tool the tests use
// (bash for a recipe script, go, ffmpeg, ffprobe, docker, fc-list).
//
// Those shapes can be written around (a Close as "_ = l.Close()", a log that is a bytes.Buffer, a process kept in
// a slice), so what is counted as well is each use: every Listen call, every place a process's output is taken,
// every mention of exec.Cmd and every "--listen" literal. Each is in the harness once and nowhere else.

type harnessReport struct {
	processTypes, logSinks, freePorts, signIns, starts []string // where each is, as "file:line"
	fixedPorts                                         []string
	// Counted by use, not by the shape of a declaration, so a second copy written another way is found too:
	listens     []string // every call of a Listen, ListenTCP, ListenUDP, ListenPacket or ListenUnix
	outputs     []string // every place a process's output is taken: an assignment to .Stdout or .Stderr, a key of that name in a literal, StdoutPipe or StderrPipe
	cmdMentions []string // every mention of exec.Cmd, in a field, slice, map, parameter or literal
	listenFlags []string // every "--listen" or "--listen=..." string, alone or at the front of a sum
}

var toolsTheTestsRun = map[string]bool{"bash": true, "go": true, "ffmpeg": true, "ffprobe": true, "docker": true, "fc-list": true, "taskset": true}

// A whole string that is an address with a port the test chose, and "--listen=addr" with one.
var listenFlag = regexp.MustCompile(`^--listen[= ]\S*:[1-9][0-9]*$`)

var fixedAddress = regexp.MustCompile(`^((127\.0\.0\.1|localhost|0\.0\.0\.0|\[::1?\])?:)[1-9][0-9]*$`)

// scanHarness reads the files (path to source) and reports where each shape is.
func scanHarness(t *testing.T, files map[string]string) harnessReport {
	t.Helper()
	var r harnessReport
	fset := token.NewFileSet()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, files[path], parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		at := func(n ast.Node) string { return path + ":" + strconv.Itoa(fset.Position(n.Pos()).Line) }
		// The uses are counted in this package's own files. A package they import for tests (internal/testenv, at
		// "../..") runs docker and ffmpeg and keeps a proxy listener, none of which is a jellyscale process; its
		// declarations are still counted by shape.
		own := !strings.HasPrefix(path, "..")
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok && holdsExecCmd(st) {
						r.processTypes = append(r.processTypes, at(ts))
					}
				}
			case *ast.FuncDecl:
				if d.Recv != nil && d.Name.Name == "Write" && isWriteSignature(d.Type) {
					r.logSinks = append(r.logSinks, at(d))
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Body != nil && listensOnZeroAndCloses(x.Body) {
					r.freePorts = append(r.freePorts, at(x))
				}
			case *ast.FuncLit:
				if listensOnZeroAndCloses(x.Body) {
					r.freePorts = append(r.freePorts, at(x))
				}
			case *ast.SelectorExpr:
				if !own {
					return true
				}
				if selector(x) == "exec.Cmd" {
					r.cmdMentions = append(r.cmdMentions, at(x))
				}
				switch x.Sel.Name {
				case "Listen", "ListenTCP", "ListenUDP", "ListenPacket", "ListenUnix":
					r.listens = append(r.listens, at(x))
				case "StdoutPipe", "StderrPipe":
					r.outputs = append(r.outputs, at(x))
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if !own {
						break
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr") {
						r.outputs = append(r.outputs, at(x))
						break
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); own && ok && (id.Name == "Stdout" || id.Name == "Stderr") {
					r.outputs = append(r.outputs, at(x))
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, _ := strconv.Unquote(x.Value)
				if own && (s == "--listen" || strings.HasPrefix(s, "--listen=")) {
					r.listenFlags = append(r.listenFlags, at(x))
				}
				if strings.Contains(s, "AuthenticateByName") {
					r.signIns = append(r.signIns, at(x))
				}
				if listenFlag.MatchString(s) {
					r.fixedPorts = append(r.fixedPorts, at(x))
				}
			case *ast.CompositeLit:
				r.fixedPorts = append(r.fixedPorts, fixedAfterListenFlag(x.Elts, at)...)
			case *ast.CallExpr:
				r.fixedPorts = append(r.fixedPorts, fixedAfterListenFlag(x.Args, at)...)
				if selector(x.Fun) == "net.Listen" && len(x.Args) == 2 {
					if lit, ok := x.Args[1].(*ast.BasicLit); ok && fixedAddress.MatchString(strings.Trim(lit.Value, "\"")) {
						r.fixedPorts = append(r.fixedPorts, at(lit))
					}
				}
				if name := selector(x.Fun); (name == "exec.Command" || name == "exec.CommandContext") && len(x.Args) > 0 {
					arg := x.Args[0]
					if name == "exec.CommandContext" && len(x.Args) > 1 {
						arg = x.Args[1]
					}
					if lit, ok := arg.(*ast.BasicLit); !ok || !toolsTheTestsRun[strings.Trim(lit.Value, "\"`")] {
						r.starts = append(r.starts, at(x))
					}
				}
			}
			return true
		})
	}
	return r
}

// fixedAfterListenFlag: in a list of arguments, an address with a chosen port right after "--listen".
func fixedAfterListenFlag(list []ast.Expr, at func(ast.Node) string) []string {
	var out []string
	for i := 0; i+1 < len(list); i++ {
		flag, ok1 := list[i].(*ast.BasicLit)
		addr, ok2 := list[i+1].(*ast.BasicLit)
		if ok1 && ok2 && strings.Trim(flag.Value, "\"") == "--listen" && fixedAddress.MatchString(strings.Trim(addr.Value, "\"")) {
			out = append(out, at(addr))
		}
	}
	return out
}

func selector(e ast.Expr) string {
	if s, ok := e.(*ast.SelectorExpr); ok {
		if id, ok := s.X.(*ast.Ident); ok {
			return id.Name + "." + s.Sel.Name
		}
	}
	return ""
}

func holdsExecCmd(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		typ := f.Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if selector(typ) == "exec.Cmd" {
			return true
		}
	}
	return false
}

func isWriteSignature(ft *ast.FuncType) bool {
	if ft.Params == nil || ft.Results == nil || len(ft.Params.List) != 1 || len(ft.Results.List) != 2 {
		return false
	}
	arr, ok := ft.Params.List[0].Type.(*ast.ArrayType)
	if !ok {
		return false
	}
	elem, ok := arr.Elt.(*ast.Ident)
	return ok && elem.Name == "byte"
}

// listensOnZeroAndCloses: a free-port helper asks the kernel for a port and gives it back, in the function's
// own statements. A listener kept for the test and closed at its end (a proxy's) is not one.
func listensOnZeroAndCloses(body *ast.BlockStmt) bool {
	listens, closes := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && selector(call.Fun) == "net.Listen" && len(call.Args) == 2 {
			if lit, ok := call.Args[1].(*ast.BasicLit); ok && strings.HasSuffix(lit.Value, `:0"`) {
				listens = true
			}
		}
		return true
	})
	isClose := func(e ast.Expr) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := call.Fun.(*ast.SelectorExpr)
		return ok && s.Sel.Name == "Close"
	}
	for _, stmt := range body.List {
		switch x := stmt.(type) {
		case *ast.DeferStmt:
			closes = closes || isClose(x.Call)
		case *ast.ExprStmt:
			closes = closes || isClose(x.X)
		}
	}
	return listens && closes
}

// problems names what is wrong with a report: more or fewer than one of each, with where they are.
func (r harnessReport) problems() []string {
	var out []string
	one := func(what string, at []string) {
		if len(at) != 1 {
			out = append(out, what+": want exactly 1, found "+strconv.Itoa(len(at))+" ["+strings.Join(at, ", ")+"]")
		}
	}
	one("process types (a struct holding an exec.Cmd)", r.processTypes)
	one("log sinks (a Write([]byte) (int, error) method)", r.logSinks)
	one("free-port helpers (a function that listens on port 0 and closes)", r.freePorts)
	one("sign-ins (the AuthenticateByName route)", r.signIns)
	one("starts of a jellyscale process (exec.Command of anything but go, ffmpeg, ffprobe, docker, fc-list)", r.starts)
	one("calls of a Listen (the one in the free-port helper)", r.listens)
	one("places a process's output is taken (an assignment to Stdout or Stderr, StdoutPipe ...)", r.outputs)
	one("mentions of exec.Cmd (a field, slice, map, parameter or literal)", r.cmdMentions)
	one(`"--listen" strings (the one that hands a process the helper's address)`, r.listenFlags)
	if len(r.fixedPorts) > 0 {
		out = append(out, "a fixed port, where the kernel must choose: "+strings.Join(r.fixedPorts, ", "))
	}
	for _, at := range append(append(append(append(r.processTypes, r.logSinks...), r.freePorts...), r.signIns...), r.starts...) {
		if strings.Contains(filepath.ToSlash(at), "internal/transcode/") {
			out = append(out, "the harness is in internal/transcode: "+at)
		}
	}
	return out
}

const harnessSource = `package x
import ("net"; "os/exec")
type proc struct{ cmd *exec.Cmd }
type sink struct{}
func (s *sink) Write(p []byte) (int, error) { return len(p), nil }
func freeAddr() string { l, _ := net.Listen("tcp", "127.0.0.1:0"); defer l.Close(); return l.Addr().String() }
func start(w io.Writer) { cmd := exec.Command(binary, "--roles=api"); cmd.Stdout, cmd.Stderr = w, w; args = append(args, "--listen", addr); exec.Command("ffmpeg", "-i", "x"); post("/Users/AuthenticateByName") }
`

func TestHarnessScannerFindsEachShapeUnderAnyName(t *testing.T) {
	if got := scanHarness(t, map[string]string{"a_test.go": harnessSource}).problems(); len(got) != 0 {
		t.Fatalf("one of each is right: %v", got)
	}
	for name, extra := range map[string]string{
		"a free port closed as _ = l.Close()":       "func p2() string { l, _ := net.Listen(\"tcp\", \"127.0.0.1:0\"); _ = l.Close(); return \"\" }",
		"a free port closed in an if":               "func p3() string { l, _ := net.Listen(\"tcp\", \"127.0.0.1:0\"); if err := l.Close(); err != nil { panic(err) }; return \"\" }",
		"a free port from a ListenConfig":           "func p4() { var lc net.ListenConfig; l, _ := lc.Listen(ctx, \"tcp\", \"127.0.0.1:0\"); l.Close() }",
		"a free port in a range":                    "func p5(i int) { l, _ := net.Listen(\"tcp\", fmt.Sprintf(\"127.0.0.1:%d\", 9000+i)); defer l.Close() }",
		"a log that is a bytes.Buffer":              "func l2() { var out bytes.Buffer; cmd.Stdout, cmd.Stderr = &out, &out }",
		"a log read from a pipe":                    "func l3() { pipe, _ := cmd.StdoutPipe(); _ = pipe }",
		"a log read from StderrPipe":                "func l4() { pipe, _ := cmd.StderrPipe(); _ = pipe }",
		"a start read with CombinedOutput":          "func l5() { out, _ := exec.Command(bin).CombinedOutput(); _ = out }",
		"an output set in a literal":                "var c = &exec.Cmd{Stdout: w}",
		"processes kept in a slice":                 "var all []*exec.Cmd",
		"processes kept in a map":                   "var byName map[string]*exec.Cmd",
		"a process as a parameter":                  "func kill(c *exec.Cmd) {}",
		"an address from a variable after --listen": "func a1(addr string) []string { return []string{\"--roles=api\", \"--listen\", addr} }",
		"an address from a sum after --listen=":     "func a2(addr string) string { return \"--listen=\" + addr }",
		"a second process type":                     "type replica struct{ c exec.Cmd }",
		"a second log sink":                         "type buf struct{}\nfunc (b *buf) Write(p []byte) (int, error) { return 0, nil }",
		"a second free port":                        "func port() int { l, _ := net.Listen(\"tcp\", \":0\"); l.Close(); return 0 }",
		"an inline free port":                       "func other() { f := func() { l, _ := net.Listen(\"tcp\", \"127.0.0.1:0\"); l.Close() }; f() }",
		"a second sign-in":                          "func again() { post(\"http://x/Users/AuthenticateByName\") }",
		"an inline start":                           "func run() { cmd := exec.Command(bin, \"--roles=worker\"); cmd.Start() }",
		"a start with a context":                    "func runCtx() { exec.CommandContext(ctx, bin) }",
		"a fixed port in Listen":                    "func f() { net.Listen(\"tcp\", \"127.0.0.1:8096\") }",
		"a fixed port after --listen":               "var args = []string{\"--roles=api\", \"--listen\", \":9000\"}",
		"a fixed --listen=":                         "var arg = \"--listen=127.0.0.1:9000\"",
		"a start inside a closure":                  "var f = func() { exec.Command(os.Args[0]) }",
		"a log sink on a value type":                "type v struct{}\nfunc (v v) Write(p []byte) (n int, err error) { return }",
	} {
		got := scanHarness(t, map[string]string{"a_test.go": harnessSource, "b_integration_test.go": "package x\nimport (\"net\"; \"os/exec\")\n" + extra}).problems()
		if len(got) == 0 {
			t.Errorf("%s is not found", name)
		} else if !strings.Contains(strings.Join(got, " "), "b_integration_test.go") {
			t.Errorf("%s: the report does not say where: %v", name, got)
		}
	}
	// A listener that is kept is no free-port helper, and the tools the tests run are no start.
	kept := "package x\nimport (\"net\"; \"os/exec\")\nfunc proxy() { l, _ := net.Listen(\"tcp\", \"127.0.0.1:0\"); go l.Accept() }\nfunc enc() { exec.Command(\"ffprobe\"); exec.Command(\"docker\", \"rm\") }"
	if got := scanHarness(t, map[string]string{"a_test.go": harnessSource, "../x/k.go": kept}).problems(); len(got) != 0 {
		t.Errorf("a kept listener and the tools are not harness code: %v", got)
	}
	if got := scanHarness(t, map[string]string{"internal/transcode/a_test.go": harnessSource}).problems(); len(got) == 0 {
		t.Error("a harness in internal/transcode is not refused")
	}
}

// The files read: every _test.go file of this directory, tag or none, and the non-test files of each package of
// the module that these files import and that imports "testing" itself.
func TestOneProcessHarnessInTheTestsOfCmdJellyscale(t *testing.T) {
	files := map[string]string{}
	read := func(path string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.ToSlash(path)] = string(raw)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if e.Name() == "harness_standing_test.go" {
			continue // this file names the shapes it looks for
		}
		read(e.Name())
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), files[e.Name()], parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(path, "jellyscale/") && (strings.HasSuffix(e.Name(), "_test.go")) {
				imports[strings.TrimPrefix(path, "jellyscale/")] = true
			}
		}
	}
	for dir := range imports {
		infos, err := os.ReadDir(filepath.Join("..", "..", dir))
		if err != nil {
			t.Fatal(err)
		}
		var sources []string
		forTests := false
		for _, e := range infos {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join("..", "..", dir, e.Name())
			sources = append(sources, path)
			raw, _ := os.ReadFile(path)
			if strings.Contains(string(raw), "\"testing\"") {
				forTests = true
			}
		}
		if forTests {
			for _, path := range sources {
				read(path)
			}
		}
	}
	if got := scanHarness(t, files).problems(); len(got) != 0 {
		t.Fatalf("the process harness of cmd/jellyscale's tests (JS-114) is in one place:\n  %s", strings.Join(got, "\n  "))
	}
}
