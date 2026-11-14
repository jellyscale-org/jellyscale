package transcode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// JS-104, failure 1 (merge queue run 43587752815): two live workers were
// forgotten by the test harness's reap loop, which called the exported Reap with
// no lock holder behind it. No role runs that: a replica forgets nobody
// (jobs.go Unavailable, docs/transcoding.md), only a worker holding its own lock
// does, from its look, in a snapshot where it sees its own lock. The exported
// function was a footgun that a harness (and so a future caller) could use to
// forget workers from a process that cannot tell an outage from a death.
//
// This reads the package's non-test source. It fails today on Reap, a function
// any process can call that calls reap, and passes once the only callers of reap
// are a worker's own look, which passes the worker's id.
func TestNoReplicaCodePathForgetsAWorker(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						id, ok := n.Fun.(*ast.Ident)
						if !ok || id.Name != "reap" || len(n.Args) < 3 {
							return true
						}
						// reap(ctx, pool, self, ...): self must be something a worker has, never "".
						if lit, ok := n.Args[2].(*ast.BasicLit); ok && lit.Value == `""` {
							t.Errorf("%s: %s calls reap with no worker as self: a process that holds no lock forgets workers", fset.Position(n.Pos()), fn.Name.Name)
						}
						if fn.Name.IsExported() && fn.Recv == nil {
							t.Errorf("%s: exported %s lets any process forget workers (reap is for a worker's own look, lookForTheDead)", fset.Position(n.Pos()), fn.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
}
