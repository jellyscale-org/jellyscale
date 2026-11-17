// Package toolguard is one unit test (JS-112): ffmpeg and ffprobe are started in
// one place, internal/mediatool, so the rule that a killed worker leaves no tool
// behind has one place to hold.
package toolguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// calls are the ways to start a process, by the package that has them.
var calls = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"os":      {"StartProcess"},
	"syscall": {"ForkExec", "Exec"},
}

func TestOnlyMediatoolStartsAProcessAndSetsItsDeathWithTheParent(t *testing.T) {
	root, _ := filepath.Abs("../..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "web-dist" || d.Name() == "vendor":
				return filepath.SkipDir
			case rel == "internal/mediatool" || rel == "internal/testenv" || rel == "test" || strings.HasPrefix(rel, "test/"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		names := map[string]string{} // The name a package has in this file, to its path.
		for _, spec := range file.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if _, watched := calls[imported]; !watched {
				continue
			}
			name := filepath.Base(imported)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			names[name] = imported
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && slices.Contains(calls[names[pkg.Name]], n.Sel.Name) {
					found = append(found, rel+": "+pkg.Name+"."+n.Sel.Name)
				}
				if n.Sel.Name == "SysProcAttr" || n.Sel.Name == "Pdeathsig" {
					found = append(found, rel+": "+n.Sel.Name)
				}
			case *ast.Ident:
				if n.Name == "DieWithParent" || n.Name == "SysProcAttr" || n.Name == "Pdeathsig" {
					found = append(found, rel+": "+n.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	found = slices.Compact(found)
	for _, f := range found {
		t.Errorf("outside internal/mediatool: %s", f)
	}
}
