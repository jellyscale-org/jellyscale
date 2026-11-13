package main

// How the runner sends compat/clients.yaml to the running jellyscale (JS-070), in the differential tier
// because only there are a real scan (series, episodes, posters), workers, a signed-in user, the schema
// checker and a Jellyfin to say what a film without a poster answers. Names the task implies:
//
//	(*server).sweep(clients conform.Clients, served func(method, route string) bool, reference *server) (sent int, failures []string)
//
// It sends each listed request that served accepts (the runner passes the routes of compat/endpoints.yaml, not
// the running binary's, so a route removed from the binary is still sent and fails), with {itemId}, {seriesId},
// {seasonId}, {mediaSourceId}, {parentId} and {userId} filled from the server's own items and the query as the
// list spells it. Each answer goes through s.do, so it is also held to the schema. A failure names the client,
// the path, the method and the route. A request that answers other than 2xx is excused only if reference, asked
// the same request, answered the same status. main prints "clients: sent N requests" and fails on any failure
// or when it sent none (test/differential/acceptance.sh shows it on a real run).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"jellyscale/compat/conform"
)

func findroidHome(reqs ...conform.ClientRequest) conform.Clients {
	return conform.Clients{{Name: "Findroid", Version: "test", Paths: map[string][]conform.ClientRequest{"home": reqs}}}
}

var suggestions = conform.ClientRequest{Method: "GET", Route: "/Items/Suggestions", Auth: "mediabrowser", From: "traffic",
	Query: map[string][]string{"mediaType": {"Video"}, "type": {"Movie", "Series"}, "limit": {"6"}}}

func everything(string, string) bool { return true }

// pathStub answers by path; a path it does not know is a 404.
func pathStub(t *testing.T, answers map[string]string, seen *[]string) *server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		body, ok := answers[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &server{name: "stub", base: srv.URL, userID: "u1", ids: map[string]string{}, byKey: map[string]string{"Movie/Alpha Film (2021)": "f1"}}
}

func TestTheSweepNamesTheClientPathAndRouteOfARequestThatIsNotAnswered(t *testing.T) {
	var seen []string
	js := pathStub(t, map[string]string{"/Items/f1": `{}`}, &seen)
	sent, failures := js.sweep(findroidHome(suggestions, conform.ClientRequest{Method: "GET", Route: "/Items/{itemId}", Auth: "mediabrowser", From: "traffic"}), everything, nil)
	if sent != 2 {
		t.Errorf("sent %d requests, want 2", sent)
	}
	if len(failures) != 1 {
		t.Fatalf("failures %q, want one", failures)
	}
	for _, want := range []string{"Findroid", "home", "GET", "/Items/Suggestions", "404"} {
		if !strings.Contains(failures[0], want) {
			t.Errorf("the failure %q does not hold %q", failures[0], want)
		}
	}
}

func TestTheSweepSendsTheQueryAsTheListSpellsItAndFillsTheIds(t *testing.T) {
	var seen []string
	js := pathStub(t, map[string]string{"/Items/Suggestions": `{}`, "/Users/u1/Items/f1": `{}`}, &seen)
	reqs := findroidHome(suggestions, conform.ClientRequest{Method: "GET", Route: "/Users/{userId}/Items/{itemId}", Auth: "emby", From: "source"})
	if _, failures := js.sweep(reqs, everything, nil); len(failures) != 0 {
		t.Fatalf("failures %q", failures)
	}
	if len(seen) != 2 {
		t.Fatalf("saw %q", seen)
	}
	_, raw, _ := strings.Cut(seen[0], "?")
	q, _ := url.ParseQuery(raw)
	if strings.Join(q["type"], ",") != "Movie,Series" || q.Get("mediaType") != "Video" || q.Get("limit") != "6" {
		t.Errorf("sent the query %q, want mediaType=Video&type=Movie&type=Series&limit=6", raw)
	}
}

func TestTheSweepDoesNotSendARequestTheServedListLeavesOut(t *testing.T) {
	var seen []string
	js := pathStub(t, nil, &seen)
	sent, failures := js.sweep(findroidHome(suggestions), func(string, string) bool { return false }, nil)
	if sent != 0 || len(seen) != 0 {
		t.Errorf("sent %d, the server saw %q", sent, seen)
	}
	// And a sweep that sent nothing is itself a failure: a run that checked nothing proves nothing.
	if len(failures) == 0 {
		t.Error("a sweep that sent no request did not fail")
	}
}

func TestTheSweepExcusesANon2xxOnlyWhereJellyfinAnsweredTheSameStatus(t *testing.T) {
	var seen []string
	poster := conform.ClientRequest{Method: "GET", Route: "/Items/{itemId}/Images/Primary", Auth: "mediabrowser", From: "source"}
	js := pathStub(t, nil, &seen)
	jfSame := pathStub(t, nil, &seen)
	if _, failures := js.sweep(findroidHome(poster), everything, jfSame); len(failures) != 0 {
		t.Errorf("a 404 both servers give failed: %q", failures)
	}
	jfServes := pathStub(t, map[string]string{"/Items/f1/Images/Primary": `{}`}, &seen)
	if _, failures := js.sweep(findroidHome(poster), everything, jfServes); len(failures) != 1 {
		t.Errorf("a 404 where Jellyfin answers 200 gave %q, want one failure", failures)
	}
	if _, failures := js.sweep(findroidHome(poster), everything, nil); len(failures) != 1 {
		t.Errorf("a 404 with no reference gave %q, want one failure", failures)
	}
}

func TestAnAnswerOfTheSweepIsHeldToTheSchemaToo(t *testing.T) {
	var seen []string
	js := pathStub(t, map[string]string{"/System/Info/Public": `{"ServerName":5}`}, &seen)
	js.conform = schemaChecker(t)
	info := conform.ClientRequest{Method: "GET", Route: "/System/Info/Public", Auth: "none", From: "traffic"}
	if _, failures := js.sweep(findroidHome(info), everything, nil); len(failures) != 0 {
		t.Fatalf("a 200 is not a sweep failure: %q", failures)
	}
	if len(js.violations()) == 0 {
		t.Error("an answer that breaks the schema was not reported")
	}
}
