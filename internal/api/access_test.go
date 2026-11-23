package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// JS-125. Every route declares who may call it (Route.Access: "anyone" or "viewer"), none is open by default, and an
// open one says why (Route.Why). docs/access.md is the model and a row per route. These checks need nothing outside
// Go; TestAccess (access_integration_test.go) asks every route.
//
// They name Route.Access and Route.Why, which the task adds, and are written to compile against a Route whose Access
// is a string or a named string type.

func routeName(r Route) string { return r.Method + " " + r.Path }

// openList is the routes anyone may call: written out here and read from neither Routes() nor the manifest, so that
// opening a route takes a change of this list, which a reviewer sees. The pictures are open by the owner's decision of
// 2026-11-16 (jellyfin-web builds picture URLs with no token, an <img> cannot send one, and item ids are random).
var openList = []string{
	"GET /System/Info/Public",
	"GET /System/Ping",
	"POST /System/Ping",
	"GET /Branding/Configuration",
	"GET /Branding/Css",
	"GET /Branding/Css.css",
	"GET /QuickConnect/Enabled",
	"GET /Users/Public",
	"POST /Users/AuthenticateByName",
	"GET /Items/{itemId}/Images/{imageType}",
	"GET /Items/{itemId}/Images/{imageType}/{imageIndex}",
}

func TestEveryRouteDeclaresAnAccessOfAnyoneOrViewer(t *testing.T) {
	routes := (&Server{}).Routes()
	if len(routes) < 88 {
		t.Fatalf("%d routes, want at least the 88 there were", len(routes))
	}
	for _, route := range routes {
		if access := string(route.Access); access != "anyone" && access != "viewer" {
			t.Errorf("%s: Access is %q, want \"anyone\" or \"viewer\" (every route declares it; none is open by default)", routeName(route), access)
		}
	}
}

func TestAnOpenRouteSaysWhyAndAViewerRouteSaysNothing(t *testing.T) {
	for _, route := range (&Server{}).Routes() {
		why := strings.TrimSpace(route.Why)
		switch string(route.Access) {
		case "anyone":
			if why == "" {
				t.Errorf("%s is open to anyone and says no why", routeName(route))
			}
		case "viewer":
			if route.Why != "" {
				t.Errorf("%s requires a viewer and has a Why (%q): only an open route says why", routeName(route), route.Why)
			}
		}
	}
}

func TestTheOpenRoutesAreExactlyTheOpenList(t *testing.T) {
	if len(openList) != 11 {
		t.Fatalf("the open list in this test has %d routes, the task pins 11", len(openList))
	}
	routes := (&Server{}).Routes()
	have := map[string]Route{}
	for _, route := range routes {
		have[routeName(route)] = route
	}
	for _, name := range openList {
		route, found := have[name]
		switch {
		case !found:
			t.Errorf("%s is in the open list and is not a route: change the list", name)
		case string(route.Access) != "anyone":
			t.Errorf("%s is in the open list and its Access is %q: change the list or the route", name, route.Access)
		}
	}
	for _, route := range routes {
		if string(route.Access) == "anyone" && !slices.Contains(openList, routeName(route)) {
			t.Errorf("%s is open to anyone and is not in the open list of this test: opening a route is a change of that list", routeName(route))
		}
	}
	viewers := 0
	for _, route := range routes {
		if string(route.Access) == "viewer" {
			viewers++
		}
	}
	if want := len(routes) - len(openList); viewers != want {
		t.Errorf("%d viewer routes of %d, want all but the %d open ones (%d)", viewers, len(routes), len(openList), want)
	}
}

// manifestLines reads compat/endpoints.yaml: a route's method, path and access word, from its line.
func manifestLines(t *testing.T) (access map[string]string, order []string) {
	t.Helper()
	raw, err := os.ReadFile("../../compat/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`method: (\w+), path: "([^"]*)"`)
	accessWord := regexp.MustCompile(`\baccess: (\w+)`)
	access = map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := pattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1] + " " + m[2]
		word := ""
		if a := accessWord.FindStringSubmatch(line); a != nil {
			word = a[1]
		}
		if _, dup := access[key]; dup {
			t.Errorf("compat/endpoints.yaml lists %s twice", key)
		}
		access[key] = word
		order = append(order, key)
	}
	return access, order
}

func TestTheManifestPrintsEachRoutesAccessOnItsLine(t *testing.T) {
	manifest := Manifest((&Server{}).Routes())
	for _, route := range (&Server{}).Routes() {
		want := "access: " + string(route.Access)
		found := false
		for _, line := range strings.Split(manifest, "\n") {
			if strings.Contains(line, `method: `+route.Method+`, path: "`+route.Path+`"`) {
				found = true
				if !regexp.MustCompile(`\b` + want + `\b`).MatchString(line) {
					t.Errorf("the line of %s says no %q: %s", routeName(route), want, line)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the manifest", routeName(route))
		}
	}
}

func TestTheCheckedInManifestHasTheOpenListAndNothingElseOpen(t *testing.T) {
	access, order := manifestLines(t)
	if len(order) < 88 {
		t.Fatalf("compat/endpoints.yaml lists %d routes, want at least 88", len(order))
	}
	var open []string
	for _, key := range order {
		switch access[key] {
		case "anyone":
			open = append(open, key)
		case "viewer":
		default:
			t.Errorf("the line of %s in compat/endpoints.yaml has access %q: run go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml", key, access[key])
		}
	}
	slices.Sort(open)
	want := slices.Clone(openList)
	slices.Sort(want)
	if !slices.Equal(open, want) {
		t.Errorf("the lines of compat/endpoints.yaml that say access: anyone are\n  %s\nwant exactly the open list\n  %s", strings.Join(open, "\n  "), strings.Join(want, "\n  "))
	}
}

// Server.Handler() refuses a route whose Access is not declared or is not "anyone" or "viewer", whichever roles this
// server has: a binary must not start with a route that nobody decided. The real tree has no such route, so what this
// holds is that the build is quiet; the two tests below run it against a tree that has one. JELLYSCALE_MUTANT names
// the mutant compiled in (test/mutant.sh), whose routes the handler must refuse, and the routes it names.
var mutantRoutes = map[string][]string{
	"empty-access":      {"get /mutant/empty"},
	"other-word-access": {"get /mutant/admin", "get /mutant/capital", "get /mutant/public"},
}

func TestHandlerStartsOnlyWithEveryAccessDeclared(t *testing.T) {
	named := mutantRoutes[os.Getenv("JELLYSCALE_MUTANT")]
	for _, roles := range [][]string{{RoleAPI}, {RoleStream}, {RoleAPI, RoleStream}} {
		recovered := func() (r any) {
			defer func() { r = recover() }()
			(&Server{Roles: roles, Log: slog.New(slog.DiscardHandler)}).Handler()
			return nil
		}()
		message := strings.ToLower(strings.TrimSpace(fmt.Sprint(recovered)))
		switch {
		case named == nil && recovered != nil:
			t.Errorf("Handler with roles %v panicked: %v", roles, recovered)
		case named != nil && recovered == nil:
			t.Errorf("Handler with roles %v started with a route whose Access is not \"anyone\" or \"viewer\"", roles)
		case named != nil && !slices.ContainsFunc(named, func(route string) bool { return strings.Contains(message, route) }):
			t.Errorf("Handler with roles %v panicked without naming a route that declares no valid Access (%v): %v", roles, named, recovered)
		}
	}
}

// Named mutants: a new Route with an empty Access, and three whose Access is another word ("admin", "Viewer",
// "public"), each run through test/mutant.sh. The declarations test names every such route, and Handler() of a server
// with the api role, the stream role or both panics naming one, though the route is the api's. A build that refuses
// only an empty Access, or only the words it knows to be wrong, serves the second mutant with no guard.
func TestAHandlerRefusesARouteWithNoValidAccess(t *testing.T) {
	for mutant, routes := range mutantRoutes {
		t.Run(mutant, func(t *testing.T) {
			cmd := exec.Command("bash", "../../test/mutant.sh", mutant, "go", "test", "-count=1", "-run",
				"^(TestEveryRouteDeclaresAnAccessOfAnyoneOrViewer|TestHandlerStartsOnlyWithEveryAccessDeclared)$", ".")
			cmd.Env = append(os.Environ(), "JELLYSCALE_MUTANT="+mutant)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the declarations test and the handler passed with a route that declares no valid Access:\n%s", out)
			}
			if strings.Contains(string(out), "the mutant no longer applies") {
				t.Fatalf("the mutant is out of date: %s", out)
			}
			// The declarations test names each route; the handler test is the only thing that panics, so it
			// reports its three server shapes itself.
			for _, route := range routes {
				if !strings.Contains(strings.ToLower(string(out)), route) {
					t.Errorf("the output does not name %q:\n%s", route, out)
				}
			}
			if !strings.Contains(string(out), "--- FAIL: TestEveryRouteDeclaresAnAccessOfAnyoneOrViewer") {
				t.Errorf("the declarations test did not fail:\n%s", out)
			}
			if strings.Contains(string(out), "--- FAIL: TestHandlerStartsOnlyWithEveryAccessDeclared") || strings.Contains(string(out), "panic:") {
				// Handler() panicking is what the mutant run expects of it; a test that fails or a panic that escapes means
				// the panic did not name a route for every set of roles.
				t.Errorf("Handler did not refuse the route as expected for every set of roles:\n%s", out)
			}
		})
	}
}

// Outside Routes() the server registers the health probes, the web client's files and the root redirect, and the
// catch-all that answers 404. Any other pattern is a route that nobody declared. This reads the package's own files:
// the one registration that is not a literal is the loop over Routes().
func TestNothingIsRegisteredOnTheMuxOutsideRoutes(t *testing.T) {
	allowed := []string{"GET /healthz", "GET /readyz", "GET /web/", "GET /{$}", "/"}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	dynamic := 0
	seen := map[string]bool{}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") || len(call.Args) == 0 {
				return true
			}
			where := fset.Position(call.Pos()).String()
			literal, isLiteral := call.Args[0].(*ast.BasicLit)
			if !isLiteral {
				dynamic++
				if file.Name() != "server.go" {
					t.Errorf("%s registers a pattern that is not a literal, outside the loop over Routes() in server.go", where)
				}
				return true
			}
			pattern, _ := strconv.Unquote(literal.Value)
			seen[pattern] = true
			if !slices.Contains(allowed, pattern) {
				t.Errorf("%s registers %q, which is not in Routes(): the server answers only GET /healthz, GET /readyz, GET /web/, GET /{$}, and the catch-all", where, pattern)
			}
			return true
		})
	}
	if dynamic != 1 {
		t.Errorf("%d registrations with a pattern that is not a literal, want the one loop over Routes()", dynamic)
	}
	for _, pattern := range allowed {
		if !seen[pattern] {
			t.Errorf("the server no longer registers %q: the access table (docs/access.md) lists what is served outside it; change both", pattern)
		}
	}
}
