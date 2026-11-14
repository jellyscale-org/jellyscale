package media_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// JS-086: one typed media model from the probe to every reader. These look at
// the code, not at what it does, for what a run cannot show: that no second
// struct shape, map or untyped assertion holds a source, a stream or an
// attachment, and that the fill-in and the hand-written staleness predicates
// are gone. What a client sees is pinned by the integration checks
// (internal/api/media_model_integration_test.go, internal/library/probe_version_integration_test.go).

// readers are the packages whose non-test code decodes or builds a source or a
// stream. The transcode worker reads them through these.
var readers = []string{"playback", "subtitles", "items", "api", "transcode", "library"}

type file struct {
	path string
	ast  *ast.File
	fset *token.FileSet
}

func parseNonTest(t *testing.T, dirs ...string) []file {
	t.Helper()
	var files []file
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join("..", dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join("..", dir, name)
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file{path, parsed, fset})
		}
	}
	return files
}

// untyped says whether a type expression is map[string]any, []map[string]any or
// []any, the shapes that held a source, a stream or an attachment.
func untyped(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.MapType:
		key, _ := e.Key.(*ast.Ident)
		return key != nil && key.Name == "string" && isAny(e.Value)
	case *ast.ArrayType:
		return isAny(e.Elt) || untyped(e.Elt)
	}
	return false
}

func isAny(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "any"
	case *ast.InterfaceType:
		return len(e.Methods.List) == 0
	}
	return false
}

var mediaName = regexp.MustCompile(`(?i)source|stream|attachment`)

// No struct field, parameter, result or variable in the readers is a map or a
// slice of any with the name of a source, a stream or an attachment: they are
// the one MediaSource and MediaStream type.
func TestNoUntypedSourceStreamOrAttachmentIsDeclared(t *testing.T) {
	for _, f := range parseNonTest(t, readers...) {
		report := func(pos token.Pos, name, what string) {
			if mediaName.MatchString(name) {
				t.Errorf("%s: %s %s is an untyped map or slice, not the media model", f.fset.Position(pos), what, name)
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Field:
				if untyped(n.Type) {
					for _, name := range n.Names {
						report(name.Pos(), name.Name, "the field or parameter")
					}
				}
			case *ast.ValueSpec:
				if n.Type != nil && untyped(n.Type) {
					for _, name := range n.Names {
						report(name.Pos(), name.Name, "the variable")
					}
				}
			case *ast.FuncDecl:
				// A function named for a source or a stream that answers with one.
				if n.Type.Results != nil && mediaName.MatchString(n.Name.Name) {
					for _, result := range n.Type.Results.List {
						if untyped(result.Type) {
							report(n.Name.Pos(), n.Name.Name, "the result of")
						}
					}
				}
			case *ast.CompositeLit:
				// []map[string]any{...} or map[string]any{...} written where a stream or a source is built:
				// recognised by a literal that has an "Index" and a "Type" key, or a "Protocol" one.
				if untyped(n.Type) && literalKeys(n, "Type") && (literalKeys(n, "Index") || literalKeys(n, "Protocol")) {
					t.Errorf("%s: a map literal builds a stream or a source", f.fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
}

func literalKeys(lit *ast.CompositeLit, key string) bool {
	for _, element := range lit.Elts {
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			if k, ok := pair.Key.(*ast.BasicLit); ok && k.Value == `"`+key+`"` {
				return true
			}
		}
	}
	return false
}

// api and subtitles hold no type assertion, single-value or with its ok
// discarded, on a source, stream or attachment, or on an item's MediaSources,
// MediaStreams or MediaAttachments.
func TestApiAndSubtitlesAssertNoTypeOnASourceStreamOrAttachment(t *testing.T) {
	for _, f := range parseNonTest(t, "api", "subtitles") {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			assertion, ok := n.(*ast.TypeAssertExpr)
			if !ok || assertion.Type == nil {
				return true
			}
			// A named map type is the same map: what is asserted TO does not matter when the
			// operand is a key read from one, as in source["MediaStreams"].([]media.MediaStream).
			byKey := false
			if index, ok := assertion.X.(*ast.IndexExpr); ok {
				if key, ok := index.Index.(*ast.BasicLit); ok && key.Kind == token.STRING {
					byKey = true
				}
			}
			if !byKey && !untyped(assertion.Type) {
				return true
			}
			var text strings.Builder
			if err := printer.Fprint(&text, f.fset, assertion.X); err != nil {
				t.Fatal(err)
			}
			operand := text.String()
			// Other maps (a session's PlayState, an item's ImageTags) are not the media model.
			if strings.Contains(operand, `"PlayState"`) || strings.Contains(operand, `"ImageTags"`) {
				return true
			}
			t.Errorf("%s: a type assertion to an untyped map or slice on %s", f.fset.Position(n.Pos()), operand)
			return true
		})
	}
}

// The pass that filled in the fields the SDK requires is gone, and nothing
// takes its place: no function, table or call by these names, in any package.
func TestTheFillInIsGone(t *testing.T) {
	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "media" {
			dirs = append(dirs, entry.Name())
		}
	}
	gone := map[string]bool{"WithSDKFields": true, "withItemSDKFields": true, "sourceDefaults": true, "streamDefaults": true}
	for _, f := range parseNonTest(t, dirs...) {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && gone[id.Name] {
				t.Errorf("%s: %s is still there", f.fset.Position(id.Pos()), id.Name)
			}
			return true
		})
	}
	if _, err := os.Stat("../items/sdk.go"); err == nil {
		t.Error("internal/items/sdk.go is still there")
	}
}

// The scan decides to probe an unchanged file again from the version alone: no
// query in scan.go asks whether a stored stream has or lacks a key.
func TestNoQueryInTheScanAsksWhetherAStoredStreamHasAKey(t *testing.T) {
	forbidden := regexp.MustCompile(`jsonb_path_exists|exists\(@|streams\s*\?|streams\s*@>|jsonb_object_keys|jsonb_exists`)
	for _, f := range parseNonTest(t, "library") {
		if filepath.Base(f.path) != "scan.go" {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && forbidden.MatchString(lit.Value) {
				t.Errorf("%s: a query asks about the keys of a stored stream: %.80s", f.fset.Position(lit.Pos()), strings.Join(strings.Fields(lit.Value), " "))
			}
			return true
		})
	}
}

// A map under a new name is still a map: in package media itself, nothing is
// declared as a map or a slice of any, whatever its name (a named
// `type MediaSource map[string]any` and a defaults table in a MarshalJSON both
// fail here), and no named type is declared as a map or an interface.
func TestTheMediaPackageHoldsNoMapOrAnyAtAll(t *testing.T) {
	files := parseNonTest(t, "media")
	if len(files) == 0 {
		t.Fatal("internal/media has no non-test Go file: the model is not there")
	}
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				switch n.Type.(type) {
				case *ast.MapType, *ast.InterfaceType:
					t.Errorf("%s: type %s is a map or an interface, not a struct", f.fset.Position(n.Pos()), n.Name.Name)
				}
			case ast.Expr:
				if untyped(n) {
					t.Errorf("%s: an untyped map or slice of any in the media model", f.fset.Position(n.Pos()))
					return false
				}
			}
			return true
		})
	}
}
