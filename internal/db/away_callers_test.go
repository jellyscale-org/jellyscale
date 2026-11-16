package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// JS-107: db.IsAway and db.Retry are given the error of a call on the pool and nothing
// else. What encode, a store call or ffmpeg returned is not Postgres's to judge: a
// store's EOF read as "Postgres is away" cancelled a job that should have failed with its cause.
//
// A source check cannot follow an error through every helper, so it holds two rules the
// call sites can be seen to keep, and the integration checks (job_end_integration_test.go,
// store_refused_integration_test.go) hold the rest by running them:
//
//   - a function that calls IsAway calls no store, no ffmpeg and no encode: the error it
//     judges can only be the pool's, and a function that does both is split;
//   - the function given to Retry calls none of them either, so it makes pool calls only.

// foreign are the calls whose errors are not the pool's: stores (a method of that name on
// anything but the packages that share it), ffmpeg and the job's whole run.
var foreign = map[string]bool{
	"encode": true, "put": true, "Put": true, "upload": true, "Input": true,
	"Command": true, "CommandContext": true, "fetchImageFile": true, "checkImageFile": true,
	"extractSubtitle": true, "Media": true, "cause": true,
	// A store's own methods; os, filepath, db and pgx have functions of these names too.
	"Open": true, "Stat": true, "List": true, "Delete": true,
}

var notStores = map[string]bool{"os": true, "filepath": true, "db": true, "pgx": true, "pgxpool": true, "fs": true, "exec": true, "strings": true, "bytes": true}

// foreignCalls lists the calls in node that are not the pool's.
func foreignCalls(node ast.Node) (found []string) {
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if foreign[fn.Name] && fn.Name != "Open" && fn.Name != "Stat" && fn.Name != "List" && fn.Name != "Delete" {
				found = append(found, fn.Name)
			}
		case *ast.SelectorExpr:
			if recv, ok := fn.X.(*ast.Ident); ok && notStores[recv.Name] {
				if !(recv.Name == "exec" && strings.HasPrefix(fn.Sel.Name, "Command")) {
					return true
				}
			}
			if foreign[fn.Sel.Name] {
				found = append(found, fn.Sel.Name)
			}
		}
		return true
	})
	return found
}

func isCallTo(call *ast.CallExpr, name string) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		return ok && pkg.Name == "db" && fn.Sel.Name == name
	}
	return false
}

func TestIsAwayAndRetryAreGivenOnlyTheErrorsOfCallsOnThePool(t *testing.T) {
	root := filepath.Join("..", "..")
	var problems []string
	var judged int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web-dist", ".results", "spikes", "db":
				if d.Name() != "db" || filepath.Base(filepath.Dir(path)) == "internal" {
					return filepath.SkipDir // The package itself defines both and calls them on its own tracker.
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var judging, retrying []*ast.CallExpr
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch {
					case isCallTo(call, "IsAway"):
						judging = append(judging, call)
					case isCallTo(call, "Retry"):
						retrying = append(retrying, call)
					}
				}
				return true
			})
			judged += len(judging) + len(retrying)
			if len(judging) > 0 {
				if calls := foreignCalls(fn.Body); len(calls) > 0 {
					problems = append(problems, path+": "+fn.Name.Name+" calls db.IsAway and also "+strings.Join(dedupe(calls), ", ")+": split it, so that IsAway is given a pool call's error only")
				}
			}
			for _, call := range retrying {
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.FuncLit); ok {
						if calls := foreignCalls(lit.Body); len(calls) > 0 {
							problems = append(problems, path+": a function given to db.Retry in "+fn.Name.Name+" calls "+strings.Join(dedupe(calls), ", ")+", which is not a call on the pool")
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if judged < 5 {
		t.Fatalf("found only %d calls of db.IsAway and db.Retry in the tree; the walk is wrong", judged)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
