package transcode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// JS-105: one lease keeper for every leased job. These checks read the module's
// non-test source for what the criteria say about its shape, which no run can
// show: how many statements move a lease later, and that the three places that
// kept a lease each their own way (the transcode loop, awaitJob, runSubtitle)
// keep none of their own. The runs that show the behaviour are in
// lease_keeper_integration_test.go.

// moduleSource parses every non-test Go file under internal/ and cmd/.
func moduleSource(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			files = append(files, f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return fset, files
}

// leaseStatement is a SQL string literal that sets lease_expires_at to now()
// plus something, which is how a lease is made later.
type leaseStatement struct {
	at     token.Position
	tables []string // The job tables it updates; both when it names neither (a statement shared by the two).
	claim  bool     // It also sets state = 'running': a claim.
	fn     *ast.FuncDecl
}

var movesLease = regexp.MustCompile(`lease_expires_at = now\(\) \+`)

func leaseStatements(t *testing.T) []leaseStatement {
	t.Helper()
	fset, files := moduleSource(t)
	var found []leaseStatement
	for _, f := range files {
		for _, d := range f.Decls {
			ast.Inspect(d, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				sql := strings.Join(strings.Fields(text), " ")
				if !movesLease.MatchString(sql) || !strings.Contains(sql, "UPDATE") {
					return true
				}
				s := leaseStatement{at: fset.Position(lit.Pos()), claim: strings.Contains(sql, "SET state = 'running'")}
				s.fn, _ = d.(*ast.FuncDecl)
				for _, table := range []string{"transcode_jobs", "subtitle_jobs"} {
					if regexp.MustCompile(`UPDATE ` + table + `\b`).MatchString(sql) {
						s.tables = append(s.tables, table)
					}
				}
				if len(s.tables) == 0 {
					s.tables = []string{"transcode_jobs", "subtitle_jobs"}
				}
				found = append(found, s)
				return true
			})
		}
	}
	return found
}

// In non-test code exactly one statement per job table moves a running job's
// lease later: the lease keeper's renewal. The segment commit sets next_segment
// and no longer the lease; the claims (state = 'running') and Reap's cut (LEAST,
// which only shortens) are the others, and they stay.
func TestExactlyOneStatementPerJobTableRenewsALease(t *testing.T) {
	renewals, claims := map[string][]string{}, map[string]int{}
	for _, s := range leaseStatements(t) {
		for _, table := range s.tables {
			if s.claim {
				claims[table]++
			} else {
				renewals[table] = append(renewals[table], s.at.String())
			}
		}
	}
	for _, table := range []string{"transcode_jobs", "subtitle_jobs"} {
		if n := len(renewals[table]); n != 1 {
			t.Errorf("%s: %d statements move a running job's lease later, want exactly 1 (the lease keeper's renewal): %v", table, n, renewals[table])
		}
		if claims[table] != 1 {
			t.Errorf("%s: %d claims set a lease, want the 1 there is", table, claims[table])
		}
	}
	_, files := moduleSource(t)
	cut := false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "LEAST(lease_expires_at") {
				cut = true
			}
			return true
		})
	}
	if !cut {
		t.Error("Reap's cut (lease_expires_at = LEAST(lease_expires_at, ...)) is gone; it stays as it is")
	}
}

// funcNamed finds the method of that name in package transcode.
func funcNamed(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	_, files := moduleSource(t)
	for _, f := range files {
		if f.Name.Name != "transcode" {
			continue
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv != nil {
				return fn
			}
		}
	}
	t.Fatalf("no method %s in package transcode", name)
	return nil
}

// The transcode loop, awaitJob and runSubtitle keep their lease only through
// the helper: none has a heartbeat ticker, a renewal, a lease clock or a timer of
// its own, which is what a call of heartbeat() or lease(), a time.AfterFunc, a
// beat, or a lastBeat in them would be. (A claim and the fenced put's own bound
// ask for the lease in other functions, which stay.)
func TestNoLeaseClockOutsideTheLeaseKeeper(t *testing.T) {
	for _, name := range []string{"encode", "awaitJob", "runSubtitle"} {
		fn := funcNamed(t, name)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "heartbeat", "lease", "beat":
						t.Errorf("%s calls .%s(): it keeps lease time of its own; only the lease keeper does", name, sel.Sel.Name)
					case "AfterFunc":
						t.Errorf("%s starts a timer with time.AfterFunc: the lease clock is the lease keeper's", name)
					}
				}
			case *ast.Ident:
				if n.Name == "lastBeat" {
					t.Errorf("%s keeps lastBeat: the time of the last renewal is the lease keeper's", name)
				}
			}
			return true
		})
	}
}

// The renewal is in one helper, not in a function that also does the job's
// work, and the helper writes no duration literal: its two timings are
// expressions of the worker's lease and heartbeat, whose defaults are rows of the
// table in liveness.go. Any other timing it adds is a row, by liveTiming(name).
func TestTheLeaseKeeperWritesNoDurationAndIsNotTheJobsOwnLoop(t *testing.T) {
	var renewal *leaseStatement
	for _, s := range leaseStatements(t) {
		if !s.claim && (len(s.tables) == 2 || s.tables[0] == "transcode_jobs") {
			renewal = &s
			break
		}
	}
	if renewal == nil || renewal.fn == nil {
		t.Fatal("no renewal statement of a transcode job in a function")
	}
	switch renewal.fn.Name.Name {
	case "encode", "awaitJob", "runSubtitle", "upload":
		t.Fatalf("the renewal is in %s, which does the job's work: it belongs to the lease keeper", renewal.fn.Name.Name)
	}
	// The helper is this function and, when it is a type of its own, its methods.
	helper := []*ast.FuncDecl{renewal.fn}
	if recv := receiverName(renewal.fn); recv != "" && recv != "Worker" {
		_, files := moduleSource(t)
		helper = nil
		for _, f := range files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && receiverName(fn) == recv {
					helper = append(helper, fn)
				}
			}
		}
	}
	units := map[string]bool{"Nanosecond": true, "Microsecond": true, "Millisecond": true, "Second": true, "Minute": true, "Hour": true}
	names := map[string]bool{}
	for _, fn := range helper {
		ast.Inspect(fn, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok && id.Name == "time" && units[n.Sel.Name] {
					t.Errorf("%s writes time.%s: the helper's timings are expressions of the lease and the heartbeat", fn.Name.Name, n.Sel.Name)
				}
				if id, ok := n.X.(*ast.Ident); ok && id.Name == "time" && n.Sel.Name == "ParseDuration" {
					t.Errorf("%s parses a duration: the helper writes none", fn.Name.Name)
				}
				names[n.Sel.Name] = true
			case *ast.Ident:
				names[n.Name] = true
			}
			return true
		})
	}
	if !names["lease"] || !names["heartbeat"] {
		t.Errorf("the helper %s does not use both the worker's lease and its heartbeat (lease %v, heartbeat %v): its renewal bound is half the heartbeat and its give-up is a lease less a heartbeat",
			renewal.fn.Name.Name, names["lease"], names["heartbeat"])
	}
}

func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// The keeper's connection is configured apart from the liveness lock's (Worker
// has a KeeperConfig beside LivenessConfig, defaulting to the pool's settings):
// a renewal that times out closes its connection, which would drop the lock and
// get a live worker forgotten, and the lock's connection is parked waiting for
// its loss. So nothing of the keeper reads LivenessConfig, and the lock's code
// does not read KeeperConfig. The runs that show it are in
// lease_keeper_faults_integration_test.go (the lock's connection refused).
func TestTheKeepersConnectionIsConfiguredApartFromTheLocks(t *testing.T) {
	_, files := moduleSource(t)
	var worker *ast.TypeSpec
	for _, f := range files {
		if f.Name.Name != "transcode" {
			continue
		}
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok {
				for _, spec := range g.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.Name == "Worker" {
						worker = ts
					}
				}
			}
		}
	}
	if worker == nil {
		t.Fatal("no type Worker")
	}
	fields := map[string]bool{}
	for _, field := range worker.Type.(*ast.StructType).Fields.List {
		for _, name := range field.Names {
			fields[name.Name] = true
		}
	}
	if !fields["KeeperConfig"] || !fields["LivenessConfig"] {
		t.Fatalf("Worker has KeeperConfig %v and LivenessConfig %v, want both", fields["KeeperConfig"], fields["LivenessConfig"])
	}
	var renewal *leaseStatement
	for _, s := range leaseStatements(t) {
		if !s.claim && s.fn != nil && (len(s.tables) == 2 || s.tables[0] == "transcode_jobs") {
			renewal = &s
			break
		}
	}
	if renewal == nil {
		t.Fatal("no renewal statement of a transcode job in a function")
	}
	uses := func(fn *ast.FuncDecl, field string) bool {
		found := false
		ast.Inspect(fn, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
				found = true
			}
			return true
		})
		return found
	}
	// The keeper's side: the renewal's function and its type's methods (the Worker's, when it is the Worker's own).
	recv := receiverName(renewal.fn)
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			keeperSide := fn == renewal.fn || (recv != "" && recv != "Worker" && receiverName(fn) == recv)
			if keeperSide && uses(fn, "LivenessConfig") {
				t.Errorf("%s, part of the lease keeper, reads LivenessConfig: the keeper's connection is KeeperConfig's", fn.Name.Name)
			}
			if fn.Name.Name == "Present" && uses(fn, "KeeperConfig") {
				t.Error("Present, the liveness lock's, reads KeeperConfig: the two connections are configured apart")
			}
		}
	}
}
