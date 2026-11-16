package compat

// JS-126: compat/clients.yaml says, for each client, what its player sends: the direct-play request, the HLS
// master request, a subtitle request and a picture request, as the client builds them at the commit its version
// pins, each with the file and line read and how the token travels. Names the task implies, in package conform:
//
//	conform.PlayerKinds = []string{"direct-play", "hls", "subtitle", "picture"}
//	(Client).Player []PlayerRequest `yaml:"player"`
//	PlayerRequest{Kind, Method, Route string; Query map[string][]string;
//	    NotRequested bool;                   the client never requests this kind (Method and Route are then empty)
//	    Token string;                        "header", "query" or "none"
//	    TokenName string;                     the header (Authorization, X-Emby-Authorization, X-Emby-Token,
//	                                          X-MediaBrowser-Token) or the query name (ApiKey, api_key)
//	    URLFrom string;                      "client" (it built the URL) or "server" (TranscodingUrl, DeliveryUrl)
//	    File string; Line int;               the source read, relative to the client's repository
//	    Device string}                       set only where the request was played on a device
//
// yaml keys are the lower-case field names, not_requested, token, token_name, url_from, file, line and device.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"jellyscale/compat/conform"
)

type endpointRoute struct{ Method, Path string }

func endpointRoutes(t *testing.T) []endpointRoute {
	t.Helper()
	raw, err := os.ReadFile("endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Routes []endpointRoute }
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Routes
}

// served says whether a route a client builds is one of endpoints.yaml, a {name} standing for one segment.
func served(routes []endpointRoute, method, route string) bool {
	parts := strings.Split(route, "/")
	for _, e := range routes {
		if e.Method != method {
			continue
		}
		want := strings.Split(e.Path, "/")
		if len(want) != len(parts) {
			continue
		}
		match := true
		for i := range want {
			if want[i] != parts[i] && !(strings.HasPrefix(want[i], "{") && strings.HasSuffix(want[i], "}")) {
				match = false
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestEveryClientHasAPlayerPathOfFourKindsEachWithItsSourceAndItsToken(t *testing.T) {
	clients := loadClients(t)
	routes := endpointRoutes(t)
	headers := []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"}
	for _, name := range conform.ClientNames {
		c := clients.Find(name)
		if c == nil {
			t.Errorf("no client %s", name)
			continue
		}
		seen := map[string]int{}
		for _, p := range c.Player {
			seen[p.Kind]++
			where := name + " " + p.Kind
			if p.File == "" || p.Line <= 0 {
				t.Errorf("%s: no file and line read", where)
			}
			if p.NotRequested {
				if p.Method != "" || p.Route != "" {
					t.Errorf("%s: listed as not requested and also a request", where)
				}
				continue
			}
			if !served(routes, p.Method, p.Route) {
				t.Errorf("%s: %s %s is no route of compat/endpoints.yaml", where, p.Method, p.Route)
			}
			switch p.Token {
			case "header":
				if !slicesContains(headers, p.TokenName) {
					t.Errorf("%s: the token travels in the header %q, which jellyscale does not read", where, p.TokenName)
				}
			case "query":
				if p.TokenName != "ApiKey" && p.TokenName != "api_key" {
					t.Errorf("%s: the token travels in the query as %q, which jellyscale does not read", where, p.TokenName)
				}
			case "none":
				if p.TokenName != "" {
					t.Errorf("%s: no token, and a name %q", where, p.TokenName)
				}
			default:
				t.Errorf("%s: token is %q, want header, query or none", where, p.Token)
			}
			if p.URLFrom != "client" && p.URLFrom != "server" {
				t.Errorf("%s: url_from is %q, want client or server", where, p.URLFrom)
			}
		}
		for _, kind := range conform.PlayerKinds {
			if seen[kind] != 1 {
				t.Errorf("%s has %d player entries of kind %s, want 1", name, seen[kind], kind)
			}
		}
		if len(c.Player) != len(conform.PlayerKinds) {
			t.Errorf("%s has %d player entries, want %d", name, len(c.Player), len(conform.PlayerKinds))
		}
	}
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func player(t *testing.T, client, kind string) conform.PlayerRequest {
	t.Helper()
	c := loadClients(t).Find(client)
	if c == nil {
		t.Fatalf("no client %s", client)
	}
	for _, p := range c.Player {
		if p.Kind == kind {
			return p
		}
	}
	t.Fatalf("%s has no %s", client, kind)
	return conform.PlayerRequest{}
}

// Read from the sources named in JS-126: Findroid builds its direct-play URL in JellyfinRepositoryImpl.kt:341-348
// with getVideoStreamUrl(itemId, static = true, mediaSourceId) and no token; Swiftfin appends ApiKey itself
// (RemotePlaybackURL.swift:21-25).
func TestFindroidDirectPlayCarriesNoTokenAndSwiftfinsCarriesApiKey(t *testing.T) {
	f := player(t, "Findroid", "direct-play")
	if f.Token != "none" || f.URLFrom != "client" || f.NotRequested {
		t.Errorf("Findroid direct play: %+v", f)
	}
	if !strings.HasSuffix(f.File, "JellyfinRepositoryImpl.kt") || f.Line < 341 || f.Line > 348 {
		t.Errorf("Findroid direct play was read at %s:%d, want JellyfinRepositoryImpl.kt:341-348", f.File, f.Line)
	}
	if f.Query["static"] == nil || f.Query["mediaSourceId"] == nil {
		t.Errorf("Findroid direct play asks %v, want static and mediaSourceId", f.Query)
	}
	s := player(t, "Swiftfin", "direct-play")
	if s.Token != "query" || s.TokenName != "ApiKey" {
		t.Errorf("Swiftfin direct play: %+v", s)
	}
}

func TestAClientWithoutTheRequestIsListedAsNotRequestedNeverInvented(t *testing.T) {
	for _, name := range conform.ClientNames {
		for _, p := range loadClients(t).Find(name).Player {
			if p.NotRequested && (p.Token != "" || p.Device != "") {
				t.Errorf("%s %s: not requested, and still has a token or a device: %+v", name, p.Kind, p)
			}
		}
	}
}

func TestTheHeaderOfClientsYamlNoLongerLeavesPlayerRequestsOut(t *testing.T) {
	raw, err := os.ReadFile("clients.yaml")
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(string(raw), "\nclients:")
	if regexp.MustCompile(`(?s)player or an image loader.*is not listed`).MatchString(header) {
		t.Error("the header of compat/clients.yaml still says the requests a player builds are not listed")
	}
	if !strings.Contains(header, "player") {
		t.Error("the header does not say what the player path is")
	}
}

// A path of the player is the Client's, not one of Paths, so the sweep of the eight main paths is unchanged.
func TestThePlayerPathIsNotOneOfTheEightMainPaths(t *testing.T) {
	for _, name := range conform.ClientNames {
		if _, ok := loadClients(t).Find(name).Paths["player"]; ok {
			t.Errorf("%s lists its player under paths", name)
		}
	}
}
