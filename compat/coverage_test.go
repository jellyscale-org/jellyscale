package compat

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jellyscale/internal/api"
)

var sliceNames = []string{"phase3", "phase4-ha", "identity", "phase5-library", "phase6-k8s", "phase7-syncplay"}

// notInSpec are the two rows that are in neither the OpenAPI document nor the
// hidden list: Jellyfin's WebSocket, and the one configuration key jellyscale
// serves by its own route.
var notInSpec = []string{"GET /socket", "GET /System/Configuration/encoding"}

type row struct{ method, path, class, slice, servedBy, webCaller, evidence string }

func (r row) id() string  { return r.method + " " + r.path }
func (r row) key() string { return r.method + " " + normalize(r.path) }

var param = regexp.MustCompile(`\{[^}]*\}`)

// Jellyfin names the same parameter differently from route to route.
func normalize(path string) string { return strings.ToLower(param.ReplaceAllString(path, "{}")) }

func readRows(t *testing.T) []row {
	t.Helper()
	raw, err := os.ReadFile("coverage-12.4.tsv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if lines[0] != "method\tpath\tclass\tslice\tserved_by\tweb_caller\tevidence" {
		t.Fatalf("unexpected header %q", lines[0])
	}
	var rows []row
	for _, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			t.Fatalf("want 7 columns, got %d: %q", len(f), line)
		}
		rows = append(rows, row{f[0], f[1], f[2], f[3], f[4], f[5], f[6]})
	}
	return rows
}

// manifest maps the normalized "METHOD path" of each route the API serves to
// its status. compat/endpoints.yaml is generated from the same list.
func manifest() map[string]string {
	routes := map[string]string{}
	for _, r := range (&api.Server{}).Routes() {
		routes[r.Method+" "+normalize(r.Path)] = string(r.Status)
	}
	return routes
}

// readLines returns the non-blank lines of a file in compat/.
func readLines(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// hiddenRoutes returns "METHOD path" for each route in hidden-routes-12.4.txt
// with the given status ("current" or "obsolete"), or every route for "".
func hiddenRoutes(t *testing.T, status string) []string {
	var routes []string
	for _, line := range readLines(t, "hidden-routes-12.4.txt") {
		if f := strings.Fields(line); len(f) == 4 && (status == "" || f[0] == status) {
			routes = append(routes, f[2]+" "+f[3])
		}
	}
	return routes
}

// needsBuilding says whether a row is work for a slice: a needed route, or a
// stub-ok route that is not in the API yet.
func needsBuilding(r row) bool { return r.slice != "-" }

func TestCoverageRowsAreWellFormed(t *testing.T) {
	product, err := os.ReadFile("../PRODUCT.md")
	if err != nil {
		t.Fatal(err)
	}
	var defined []string
	for _, m := range regexp.MustCompile("(?m)^\\d+\\. `([a-z0-9-]+)`:").FindAllStringSubmatch(string(product), -1) {
		defined = append(defined, m[1])
	}
	if strings.Join(defined, " ") != strings.Join(sliceNames, " ") {
		t.Errorf("PRODUCT.md roadmap defines slices %v, the test knows %v", defined, sliceNames)
	}
	seen := map[string]bool{}
	for _, r := range readRows(t) {
		if seen[r.key()] {
			t.Errorf("%s has more than one row", r.id())
		}
		seen[r.key()] = true
		switch r.class {
		case "done", "out-of-scope":
			if r.slice != "-" {
				t.Errorf("%s is %s and must have slice -, got %q", r.id(), r.class, r.slice)
			}
		case "stub-ok":
			if r.slice != "-" && !isSlice(r.slice) {
				t.Errorf("%s is stub-ok and names slice %q", r.id(), r.slice)
			}
		case "needed":
			if !isSlice(r.slice) {
				t.Errorf("%s is needed but names slice %q", r.id(), r.slice)
			}
		default:
			t.Errorf("%s has class %q", r.id(), r.class)
		}
		if r.class != "done" && r.servedBy != "-" {
			t.Errorf("%s is %s and must have served_by -, got %q", r.id(), r.class, r.servedBy)
		}
		if msg := webCallerError(r.webCaller); msg != "" {
			t.Errorf("%s: %s", r.id(), msg)
		}
		if strings.TrimSpace(r.evidence) == "" {
			t.Errorf("%s has no evidence", r.id())
		}
	}
}

func isSlice(name string) bool { return slices.Contains(sliceNames, name) }

// The OpenAPI document is not kept, so its operations are: one line each,
// generated from the document. Rows and operations must match exactly, in both
// directions, so a dropped or mistyped row fails.
func TestCoverageMatchesSpecOperations(t *testing.T) {
	spec := readLines(t, "spec-operations-12.4.txt")
	if len(spec) != 364 {
		t.Errorf("spec-operations-12.4.txt has %d operations, Jellyfin 12.4.0's document has 364", len(spec))
	}
	rows := map[string]bool{}
	for _, r := range readRows(t) {
		rows[r.id()] = true
	}
	known := map[string]bool{}
	for _, op := range spec {
		known[op] = true
		if !rows[op] {
			t.Errorf("spec operation %s has no row", op)
		}
	}
	for _, op := range hiddenRoutes(t, "") {
		known[op] = true
		if !rows[op] {
			t.Errorf("hidden route %s has no row", op)
		}
	}
	for _, op := range notInSpec {
		known[op] = true
	}
	for id := range rows {
		if !known[id] {
			t.Errorf("row %s is not in the spec, the hidden routes or the jellyscale-only list", id)
		}
	}
}

// webCallerError says whether a web_caller cell is well formed: "-" (no call
// found), "sdk" (the SDK method name occurs as .name( in web-dist, a name match)
// or "ApiClient.name", a call to the legacy jellyfin-apiclient from a page.
func webCallerError(cell string) string {
	if cell == "-" || cell == "sdk" || regexp.MustCompile(`^ApiClient\.\w+$`).MatchString(cell) {
		return ""
	}
	return fmt.Sprintf("web_caller %q is not -, sdk or ApiClient.<name>", cell)
}

// An obsolete route is an alias, and so needed, only when a jellyfin-web page
// calls it through the legacy ApiClient: the SDK only builds current routes.
func TestObsoleteRoutesNeededOnlyWhenCalled(t *testing.T) {
	byID := map[string]row{}
	for _, r := range readRows(t) {
		byID[r.id()] = r
	}
	for _, op := range hiddenRoutes(t, "obsolete") {
		if r := byID[op]; r.class == "needed" && !strings.HasPrefix(r.webCaller, "ApiClient.") {
			t.Errorf("%s is obsolete and needed, but web_caller is %q, not an ApiClient call", r.id(), r.webCaller)
		}
	}
}

func TestWebCallerError(t *testing.T) {
	for cell, bad := range map[string]bool{"-": false, "sdk": false, "ApiClient.updateUser": false, "ApiClient.": true, "legacy": true, "": true, "ApiClient.a b": true} {
		if got := webCallerError(cell) != ""; got != bad {
			t.Errorf("web_caller %q: rejected=%v, want %v", cell, got, bad)
		}
	}
}

// servedByError says why a row cannot be served by the route it cites. HEAD
// answered by the GET pattern is the only difference of method allowed.
func servedByError(rowMethod, rowPath, servedBy string) string {
	method, path, ok := strings.Cut(servedBy, " ")
	if !ok {
		return fmt.Sprintf("served_by %q is not METHOD path", servedBy)
	}
	if method != rowMethod && !(rowMethod == "HEAD" && method == "GET") {
		return fmt.Sprintf("served_by method %s does not answer %s", method, rowMethod)
	}
	if !sameRoute(rowPath, path) {
		return fmt.Sprintf("served_by path %s cannot serve %s", path, rowPath)
	}
	return ""
}

func TestServedByError(t *testing.T) {
	for _, c := range []struct {
		method, path, servedBy string
		bad                    bool
	}{
		{"GET", "/Items/{id}", "GET /Items/{itemId}", false},
		{"HEAD", "/Items/{id}", "GET /Items/{itemId}", false},
		{"POST", "/Items/{id}", "GET /Items/{itemId}", true},
		{"GET", "/Items/{id}", "POST /Items/{itemId}", true},
		{"HEAD", "/Items/{id}", "POST /Items/{itemId}", true},
		{"GET", "/Items/{id}", "HEAD /Items/{itemId}", true},
		{"GET", "/Artists", "GET /Items", true},
		{"GET", "/Items", "GET", true},
	} {
		if got := servedByError(c.method, c.path, c.servedBy) != ""; got != c.bad {
			t.Errorf("%s %s served by %s: rejected=%v, want %v", c.method, c.path, c.servedBy, got, c.bad)
		}
	}
}

// The API is the truth about what is built, so the audit must agree with it
// in both directions.
func TestCoverageAgreesWithManifest(t *testing.T) {
	built := manifest()
	rows := readRows(t)
	named := map[string]bool{}
	rowKeys := map[string]bool{}
	for _, r := range rows {
		rowKeys[r.key()] = true
		if method, path, _ := strings.Cut(r.servedBy, " "); r.servedBy != "-" {
			named[method+" "+normalize(path)] = true
		}
	}
	for route := range built {
		if !rowKeys[route] && !named[route] {
			t.Errorf("%s is served but has no row and no row is served by it", route)
		}
	}
	for _, r := range rows {
		status, inAPI := built[r.key()]
		switch r.class {
		case "done":
			own := status == string(api.Implemented) || status == string(api.Alias)
			if own == (r.servedBy != "-") {
				t.Errorf("%s is done: it is implemented in the API xor it names a served_by route (implemented %v, served_by %q)", r.id(), own, r.servedBy)
			}
			if r.servedBy == "-" {
				continue
			}
			if msg := servedByError(r.method, r.path, r.servedBy); msg != "" {
				t.Errorf("%s: %s", r.id(), msg)
			}
			method, path, _ := strings.Cut(r.servedBy, " ")
			if s := built[method+" "+normalize(path)]; s != string(api.Implemented) && s != string(api.Alias) {
				t.Errorf("%s is served_by %s, which the API does not implement", r.id(), r.servedBy)
			}
		case "stub-ok":
			if r.slice == "-" && status != string(api.Stub) {
				t.Errorf("%s is stub-ok but the API does not serve it as a stub", r.id())
			}
			if r.slice != "-" && inAPI {
				t.Errorf("%s is stub-ok with slice %s, meaning not built, but the API serves it", r.id(), r.slice)
			}
		case "needed":
			if status == string(api.Implemented) || status == string(api.Alias) {
				t.Errorf("%s is needed but the API already implements it", r.id())
			}
		case "out-of-scope":
			if inAPI {
				t.Errorf("%s is out-of-scope but the API serves it", r.id())
			}
		}
	}
}

// sameRoute says whether a route cited as the server of a row could serve it:
// the paths have the same number of segments and every segment that is
// literal in both is equal. A segment holding a parameter matches anything.
func sameRoute(rowPath, servedBy string) bool {
	a := strings.Split(strings.ToLower(rowPath), "/")
	b := strings.Split(strings.ToLower(servedBy), "/")
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Contains(a[i], "{") || strings.Contains(b[i], "{") {
			continue
		}
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSameRoute(t *testing.T) {
	if sameRoute("/Artists", "/Items") {
		t.Error("/Artists is not served by /Items")
	}
	if !sameRoute("/Videos/{itemId}/stream.{container}", "/Videos/{itemId}/{file}") {
		t.Error("a parameter segment should match")
	}
}

// Every slice that has work to do has a group in Features, and every feature
// line says where it stands.
func TestFeaturesCoverNeededSlices(t *testing.T) {
	raw, err := os.ReadFile("../PRODUCT.md")
	if err != nil {
		t.Fatal(err)
	}
	_, features, _ := strings.Cut(string(raw), "\n## Features\n")
	features, _, _ = strings.Cut(features, "\n## Feature audit")
	if features == "" {
		t.Fatal("PRODUCT.md has no Features section")
	}
	groups := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^### `([a-z0-9-]+)`$").FindAllStringSubmatch(features, -1) {
		groups[m[1]] = true
	}
	for _, r := range readRows(t) {
		if needsBuilding(r) && !groups[r.slice] {
			t.Errorf("%s is needed in %s, which has no group in Features", r.id(), r.slice)
		}
	}
	nonGoals := strings.Index(features, "### Non-goals")
	if nonGoals < 0 {
		t.Fatal("Features has no Non-goals group")
	}
	status := regexp.MustCompile(`\*\*(done|partial|not built)\*\*`)
	for _, line := range strings.Split(features[:nonGoals], "\n") {
		if strings.HasPrefix(line, "- ") && !status.MatchString(line) {
			t.Errorf("Features line has no status: %q", line)
		}
	}
}

// PRODUCT.md says how many routes each slice still has to build, so that
// "complete" can be counted.
func TestSliceCountsMatchRows(t *testing.T) {
	raw, err := os.ReadFile("../PRODUCT.md")
	if err != nil {
		t.Fatal(err)
	}
	_, audit, _ := strings.Cut(string(raw), "\n## Feature audit")
	stated := map[string]string{}
	for _, m := range regexp.MustCompile("(?m)^- `([a-z0-9-]+)`: (\\d+) routes to build$").FindAllStringSubmatch(audit, -1) {
		stated[m[1]] = m[2]
	}
	counted := map[string]int{}
	for _, r := range readRows(t) {
		if needsBuilding(r) {
			counted[r.slice]++
		}
	}
	for _, name := range sliceNames {
		if want := fmt.Sprint(counted[name]); stated[name] != want {
			t.Errorf("PRODUCT.md says slice %s has %q routes to build, rows have %s", name, stated[name], want)
		}
	}
}

// Until ratings come from the scan, a maximum rating filters nothing, so the
// list that offers it and the policy fields that apply it wait for phase5.
func TestParentalControlWaitsForRatings(t *testing.T) {
	for _, r := range readRows(t) {
		if r.id() == "GET /Localization/ParentalRatings" && r.slice != "phase5-library" {
			t.Errorf("%s is in slice %s, want phase5-library", r.id(), r.slice)
		}
	}
	raw, err := os.ReadFile("../PRODUCT.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "400 when it sets `MaxParentalRating` or `BlockUnratedItems`") {
		t.Error("Features no longer says identity refuses MaxParentalRating and BlockUnratedItems")
	}
}
