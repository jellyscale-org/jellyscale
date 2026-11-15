package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
)

// JS-110. A stream route has one request path, whether Postgres answers or not: it resolves the session and the film
// from Postgres or, while Postgres is away, from the snapshot, and then runs one handler. So a Route names one
// handler, and the twins written for the outage (JS-085, JS-103) are gone. The behaviour that must survive is in
// oneroute_integration_test.go; this is the structure, which no request can show.

// The twins the audit (F4) found, and the one handler of the encoding configuration that had a twin too.
var outageTwins = []string{"segmentOffline", "streamOffline", "subtitleOffline", "attachmentOffline", "encodingConfigurationOffline"}

func TestARouteNamesOneHandlerAndNoStreamRouteKeepsAnOutageTwin(t *testing.T) {
	route := reflect.TypeOf(Route{})
	for i := range route.NumField() {
		if field := route.Field(i); field.Type.Kind() == reflect.Func && field.Name != "Handler" {
			t.Errorf("Route has a second function, %s %s: a route names one handler", field.Name, field.Type)
		}
	}
	// The function declarations of the package's own files, not its tests.
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				declared[fn.Name.Name] = file.Name()
			}
		}
	}
	for _, name := range outageTwins {
		if file, found := declared[name]; found {
			t.Errorf("%s is still declared in %s: the outage has no handler of its own", name, file)
		}
	}
}

// A build can keep the twins under other names: the snapshot's branch of a segment, with its own parsing, as a helper
// the one handler calls. What the criterion is about is that the request is parsed once, so this counts the calls
// that parse it in the package's own files. Today playback.ParseParams is called in transcode.go and in
// snapshot.go's segmentOffline, and playback.NewSpec and playback.ParseSegment in snapshot.go as well. (The cost of
// the check: it names three functions of internal/playback, and snapshot.go by name.)
func TestARequestIsParsedOnceWhateverGaveTheSessionAndTheFilm(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{} // "playback.ParseParams" to where it is called.
	var takers []string            // Functions that take the snapshot's session as a parameter.
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
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "playback" {
						name := "playback." + sel.Sel.Name
						calls[name] = append(calls[name], file.Name())
					}
				}
			case *ast.FuncDecl:
				if n.Type.Params == nil {
					return true
				}
				for _, param := range n.Type.Params.List {
					typ := param.Type
					if star, ok := typ.(*ast.StarExpr); ok {
						typ = star.X
					}
					if id, ok := typ.(*ast.Ident); ok && id.Name == "offlineSession" {
						takers = append(takers, n.Name.Name)
					}
				}
			}
			return true
		})
	}
	if got := calls["playback.ParseParams"]; len(got) != 1 {
		t.Errorf("playback.ParseParams is called %d times in %v: the request is parsed once, however the session was found", len(got), got)
	}
	// stream.go's NewSpec and transcode.go's second ParseSegment are other handlers' own; the snapshot's file has none.
	for _, name := range []string{"playback.NewSpec", "playback.ParseSegment"} {
		for _, file := range calls[name] {
			if file == "snapshot.go" {
				t.Errorf("%s is called in snapshot.go: the snapshot gives a session and a film, and the request is read once, in the handler", name)
			}
		}
	}
	// The one function that may take the snapshot's session is the one that sets it beside the request.
	if len(takers) > 1 {
		t.Errorf("%v take the snapshot's session as a parameter: only the auth wrapper may; a handler resolves it", takers)
	}
}
