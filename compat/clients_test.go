package compat

// compat/clients.yaml lists, for each SDK client and each of its main paths,
// every request it sends (JS-070). Names the task implies, in package conform:
//
//	conform.LoadClients(path string) (conform.Clients, error)
//	conform.ClientNames = []string{"Findroid", "Android TV", "Swiftfin", "Streamyfin"}
//	conform.PathNames   = []string{"startup", "sign-in", "home", "browse", "detail", "playback", "progress", "stop"}
//	Clients []Client; Client{Name, Version string; Paths map[string][]ClientRequest}
//	ClientRequest{Method, Route string; Query map[string][]string; Body, Auth, From string}
//	    Route is the template as in endpoints.yaml; Query maps each name as the client's SDK spells it to sample
//	    values; Auth is "mediabrowser" (the Authorization: MediaBrowser ... header), "emby" (X-Emby-Authorization)
//	    or "none"; From is "source" or "traffic".
//	(Clients).Find(name string) *Client
//	(*Client).Request(path, method, route string) *ClientRequest   nil when the cell has none
//	conform.Rules{Aliases, Unchecked []Unchecked}; (Rules).AliasFor(method, path string) (target string, ok bool)
//	Unchecked{Method, Route, Reason string}

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"jellyscale/compat/conform"
)

func loadClients(t *testing.T) conform.Clients {
	t.Helper()
	clients, err := conform.LoadClients("clients.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return clients
}

func TestEveryClientAndPathHasRequestsAndSaysWhereTheyCameFrom(t *testing.T) {
	clients := loadClients(t)
	if len(clients) != len(conform.ClientNames) {
		t.Fatalf("%d clients, want %v", len(clients), conform.ClientNames)
	}
	for _, name := range conform.ClientNames {
		c := clients.Find(name)
		if c == nil {
			t.Errorf("no client %s", name)
			continue
		}
		if strings.TrimSpace(c.Version) == "" {
			t.Errorf("%s: no version or commit", name)
		}
		for _, path := range conform.PathNames {
			reqs := c.Paths[path]
			if len(reqs) == 0 {
				t.Errorf("%s %s: no requests (the 32 cells are never empty)", name, path)
			}
			for _, r := range reqs {
				where := name + " " + path + " " + r.Method + " " + r.Route
				if !slices.Contains([]string{"GET", "POST", "DELETE", "PUT", "HEAD"}, r.Method) || !strings.HasPrefix(r.Route, "/") {
					t.Errorf("%s: not a method and a route", where)
				}
				if !slices.Contains([]string{"mediabrowser", "emby", "none"}, r.Auth) {
					t.Errorf("%s: Authorization form %q", where, r.Auth)
				}
				if r.From != "source" && r.From != "traffic" {
					t.Errorf("%s: from %q, want source or traffic", where, r.From)
				}
			}
		}
		for path := range c.Paths {
			if !slices.Contains(conform.PathNames, path) {
				t.Errorf("%s: unknown path %q", name, path)
			}
		}
	}
}

func TestTheFindroidRequestsSeenOnTheDogfoodAreListed(t *testing.T) {
	f := loadClients(t).Find("Findroid")
	if f == nil {
		t.Fatal("no Findroid")
	}
	has := func(method, route string, query ...string) {
		t.Helper()
		for _, reqs := range f.Paths {
			for _, r := range reqs {
				if r.Method == method && strings.EqualFold(r.Route, route) {
					for _, q := range query {
						if _, ok := r.Query[q]; !ok {
							t.Errorf("%s %s lacks the query name %s", method, route, q)
						}
					}
					return
				}
			}
		}
		t.Errorf("Findroid lists no %s %s", method, route)
	}
	has("GET", "/System/Info/Public")
	has("GET", "/Users/Public")
	has("GET", "/Branding/Configuration")
	has("GET", "/QuickConnect/Enabled")
	has("POST", "/Users/AuthenticateByName")
	has("GET", "/Items/Suggestions", "mediaType", "type", "limit")
	has("GET", "/Items/{itemId}")
	has("POST", "/Items/{itemId}/PlaybackInfo")
	sug := f.Request("home", "GET", "/Items/Suggestions")
	if sug == nil || strings.Join(sug.Query["type"], ",") != "Movie,Series" || strings.Join(sug.Query["mediaType"], ",") != "Video" || strings.Join(sug.Query["limit"], ",") != "6" {
		t.Errorf("Findroid's home suggestions request is %+v, want mediaType=Video&type=Movie&type=Series&limit=6", sug)
	}
	if auth := f.Request("sign-in", "POST", "/Users/AuthenticateByName"); auth == nil || auth.Auth != "mediabrowser" || auth.From != "traffic" {
		t.Errorf("Findroid's sign-in request is %+v, want a MediaBrowser header seen in traffic", auth)
	}
}

// unserved are the listed requests the API has no route for.
type unserved struct{ client, path, method, route, class, evidence string }

func unservedRequests(t *testing.T) (all []unserved, listed int) {
	t.Helper()
	routes := manifest()
	rows := map[string]row{}
	for _, r := range readRows(t) {
		rows[r.key()] = r
	}
	for _, c := range loadClients(t) {
		for path, reqs := range c.Paths {
			for _, r := range reqs {
				listed++
				key := r.Method + " " + normalize(r.Route)
				if _, ok := routes[key]; ok {
					continue
				}
				u := unserved{client: c.Name, path: path, method: r.Method, route: r.Route}
				if row, ok := rows[key]; ok {
					u.class, u.evidence = row.class, row.evidence
				}
				all = append(all, u)
			}
		}
	}
	return all, listed
}

// A listed request may go unanswered only as a decision: its row is
// out-of-scope and its evidence names the client and path that call it. A row
// that is "needed" is not a decision.
func TestAListedRequestGoesUnansweredOnlyWhereItsRowIsOutOfScopeNamingTheClientAndPath(t *testing.T) {
	all, listed := unservedRequests(t)
	if listed < 32 {
		t.Fatalf("only %d requests listed", listed)
	}
	for _, u := range all {
		where := u.client + " " + u.path + " " + u.method + " " + u.route
		switch {
		case u.class == "":
			t.Errorf("%s: no route and no row in coverage-12.4.tsv", where)
		case u.class != "out-of-scope":
			t.Errorf("%s: not served and its row is %q; only out-of-scope is a decision", where, u.class)
		case !strings.Contains(strings.ToLower(u.evidence), strings.ToLower(u.client)) || !strings.Contains(strings.ToLower(u.evidence), u.path):
			t.Errorf("%s: out-of-scope, but its evidence does not name the client %q and the path %q: %s", where, u.client, u.path, u.evidence)
		}
	}
}

func TestEveryRouteTheClientsListIsAnOperationOfTheSchemaOrAHiddenRoute(t *testing.T) {
	known := map[string]bool{}
	for _, r := range readRows(t) {
		known[r.key()] = true
	}
	for _, c := range loadClients(t) {
		for path, reqs := range c.Paths {
			for _, r := range reqs {
				if !known[r.Method+" "+normalize(r.Route)] {
					t.Errorf("%s %s: %s %s is no row of coverage-12.4.tsv, so no Jellyfin 12.4 route", c.Name, path, r.Method, r.Route)
				}
			}
		}
	}
}

// Every alias route is checked as the route it stands for, and every
// unchecked route says why.
func TestEveryAliasRouteStandsForARouteTheSchemaTypes(t *testing.T) {
	rules, err := conform.LoadRules("conform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile("schema-12.4.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := conform.ParseSchema(file)
	if err != nil {
		t.Fatal(err)
	}
	for key, status := range manifest() {
		if status != "alias" {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		if _, ok := schema.Operation(method, path); ok {
			continue
		}
		if target, ok := rules.AliasFor(method, path); !ok {
			t.Errorf("alias %s has no entry under aliases: in conform.yaml and no operation of its own", key)
		} else if _, ok := schema.Operation(method, target); !ok {
			t.Errorf("alias %s stands for %s, which the schema does not have", key, target)
		}
	}
	for _, u := range rules.Unchecked {
		if strings.TrimSpace(u.Reason) == "" {
			t.Errorf("unchecked %s %s has no reason", u.Method, u.Route)
		}
	}
}

var statusRow = regexp.MustCompile(`^\|\s*(Findroid|Android TV|Swiftfin|Streamyfin)\s*\|\s*(startup|sign-in|home|browse|detail|playback|progress|stop)\s*\|\s*(yes|no)\s*\|(.*)\|\s*$`)

// docs/status.md has a table "Client coverage" with a row for each client and
// path: `| client | path | yes or no | the routes left unserved, each as METHOD /route (decision), or - |`.
func TestStatusSaysForEachClientAndPathWhetherEveryListedRequestAnswers(t *testing.T) {
	raw, err := os.ReadFile("../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "Client coverage") {
		t.Fatal("docs/status.md has no Client coverage section")
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(text, "\n") {
		if m := statusRow.FindStringSubmatch(line); m != nil {
			rows[m[1]+"/"+m[2]] = []string{m[3], m[4]}
		}
	}
	all, _ := unservedRequests(t)
	for _, name := range conform.ClientNames {
		for _, path := range conform.PathNames {
			got, ok := rows[name+"/"+path]
			if !ok {
				t.Errorf("docs/status.md has no row for %s %s", name, path)
				continue
			}
			var left []unserved
			for _, u := range all {
				if u.client == name && u.path == path {
					left = append(left, u)
				}
			}
			if want := map[bool]string{true: "yes", false: "no"}[len(left) == 0]; got[0] != want {
				t.Errorf("%s %s: status.md says %q, the list says %q", name, path, got[0], want)
			}
			for _, u := range left {
				if !strings.Contains(got[1], u.method+" "+u.route) || !strings.Contains(got[1], u.class) {
					t.Errorf("%s %s: status.md does not name the unserved %s %s with its decision (%s)", name, path, u.method, u.route, u.class)
				}
			}
			if len(left) == 0 && strings.TrimSpace(got[1]) != "-" {
				t.Errorf("%s %s: nothing is unserved, but status.md lists %q", name, path, got[1])
			}
		}
	}
}

func TestStatusSaysWhatTheCoverageIsEvidenceOfAndCallsNoClientWorkingUnplayed(t *testing.T) {
	raw, err := os.ReadFile("../docs/status.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "The document is not kept in the repository") {
		t.Error("docs/status.md still says the OpenAPI document is not kept in the repository; compat/schema-12.4.json is made from it")
	}
	for _, want := range []string{"compat/schema-12.4.json", "from source", "from traffic", "played on a device"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/status.md never says %q", want)
		}
	}
	for _, sdk := range []string{"SDK decode", "schema"} {
		if !strings.Contains(text, sdk) {
			t.Errorf("docs/status.md does not say the coverage is evidence of %s", sdk)
		}
	}
}
