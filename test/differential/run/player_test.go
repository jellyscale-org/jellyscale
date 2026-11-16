package main

// JS-126: the client run sends each 'player' request of compat/clients.yaml to jellyscale as listed, and holds the
// answer to the form. Names the task implies, in this package:
//
//	(*server).playerSweep(clients conform.Clients) (sent int, failures []string)
//
// It sends the token in the listed header (Authorization as MediaBrowser Token="...", the others as the bare token) or
// the listed query name and nowhere else, and no credential at all where the list says none; a request listed
// not_requested is not sent. A media request listed with no token must answer 401, one listed with a token 2xx,
// and a picture 200 however it is listed. Jellyfin's answers excuse nothing. A failure names the client, "player",
// the method, the route and the status; a sweep that sent nothing fails. main prints "clients: sent N requests"
// with N counting these (acceptance of the real run: test/differential/acceptance.sh).

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"jellyscale/compat/conform"
)

// mediaStub answers a media route (anything under /Videos or /Download) 200 to a request with a credential and
// with 401 to one without, and a picture 200 always, as jellyscale does. mediaAnswer and pictureAnswer override.
func mediaStub(t *testing.T, mediaAnswer, pictureAnswer int, seen *[]arrival) *server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		has := r.Header.Get("Authorization") != "" || r.Header.Get("X-Emby-Authorization") != "" ||
			r.Header.Get("X-Emby-Token") != "" || r.Header.Get("X-MediaBrowser-Token") != "" ||
			r.URL.Query().Get("ApiKey") != "" || r.URL.Query().Get("api_key") != ""
		mu.Lock()
		*seen = append(*seen, arrival{r.Method, r.Header.Clone(), r.URL.Query(), r.URL.Path})
		mu.Unlock()
		status := 401
		if has {
			status = 200
		}
		if strings.Contains(r.URL.Path, "/Images/") {
			status = pictureAnswer
		} else if mediaAnswer != 0 {
			status = mediaAnswer
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return &server{name: "stub", base: srv.URL, token: "tok-1234", userID: "u1", ids: map[string]string{}, byKey: map[string]string{"Movie/Alpha Film (2021)": "f1", subtitleFilmKey: "f2"}}
}

func playing(reqs ...conform.PlayerRequest) conform.Clients {
	return conform.Clients{{Name: "Findroid", Version: "test", Player: reqs}}
}

func direct(token, name string) conform.PlayerRequest {
	return conform.PlayerRequest{Kind: "direct-play", Method: "GET", Route: "/Videos/{itemId}/stream", URLFrom: "client", File: "a.kt", Line: 1,
		Query: map[string][]string{"static": {"true"}}, Token: token, TokenName: name}
}

var posterListed = func(token, name string) conform.PlayerRequest {
	return conform.PlayerRequest{Kind: "picture", Method: "GET", Route: "/Items/{itemId}/Images/Primary", URLFrom: "client", File: "a.kt", Line: 2, Token: token, TokenName: name}
}

func TestThePlayerSweepSendsTheTokenWhereTheListSaysAndNowhereElse(t *testing.T) {
	var seen []arrival
	s := mediaStub(t, 0, 200, &seen)
	hls := conform.PlayerRequest{Kind: "hls", Method: "GET", Route: "/Videos/{itemId}/master.m3u8", URLFrom: "server", File: "a.kt", Line: 3, Token: "header", TokenName: "X-Emby-Token"}
	auth := conform.PlayerRequest{Kind: "subtitle", Method: "GET", Route: "/Videos/{itemId}/{mediaSourceId}/Subtitles/2/Stream.vtt", URLFrom: "client", File: "a.kt", Line: 4, Token: "header", TokenName: "Authorization"}
	sent, failures := s.playerSweep(playing(direct("query", "ApiKey"), hls, auth, posterListed("none", "")))
	if sent != 4 || len(failures) != 0 {
		t.Fatalf("sent %d, failures %q", sent, failures)
	}
	if len(seen) != 4 {
		t.Fatalf("the server saw %d requests", len(seen))
	}
	credentials := func(a arrival) (headers []string, query []string) {
		for _, h := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
			if a.header.Get(h) != "" {
				headers = append(headers, h)
			}
		}
		for _, q := range []string{"ApiKey", "api_key"} {
			if a.query.Get(q) != "" {
				query = append(query, q)
			}
		}
		return
	}
	for i, want := range []struct{ headers, query []string }{
		{nil, []string{"ApiKey"}}, {[]string{"X-Emby-Token"}, nil}, {[]string{"Authorization"}, nil}, {nil, nil},
	} {
		h, q := credentials(seen[i])
		if strings.Join(h, ",") != strings.Join(want.headers, ",") || strings.Join(q, ",") != strings.Join(want.query, ",") {
			t.Errorf("request %d (%s) carried headers %v and query %v, want %v and %v", i, seen[i].method, h, q, want.headers, want.query)
		}
	}
	if seen[0].query.Get("ApiKey") != "tok-1234" || seen[1].header.Get("X-Emby-Token") != "tok-1234" || !strings.Contains(seen[2].header.Get("Authorization"), "tok-1234") {
		t.Error("a credential is not the signed-in token")
	}
	if seen[0].query.Get("static") != "true" {
		t.Errorf("the listed query was lost: %v", seen[0].query)
	}
}

func TestFindroidsDirectPlayAnswering401IsExpectedAndPasses(t *testing.T) {
	var seen []arrival
	s := mediaStub(t, 0, 200, &seen)
	if sent, failures := s.playerSweep(playing(direct("none", ""))); sent != 1 || len(failures) != 0 {
		t.Errorf("sent %d, failures %q", sent, failures)
	}
}

func TestADirectPlayListedWithNoTokenFailsWhenItAnswersAnythingButTheRefusal(t *testing.T) {
	for _, status := range []int{200, 206, 404, 500} {
		var seen []arrival
		s := mediaStub(t, status, 200, &seen)
		_, failures := s.playerSweep(playing(direct("none", "")))
		if len(failures) != 1 {
			t.Errorf("status %d: failures %q, want one", status, failures)
			continue
		}
		for _, want := range []string{"Findroid", "player", "GET", "/Videos/{itemId}/stream", strconv.Itoa(status)} {
			if !strings.Contains(failures[0], want) {
				t.Errorf("the failure %q does not hold %q", failures[0], want)
			}
		}
	}
}

func TestAMediaRequestListedWithATokenFailsWhenItAnswers401(t *testing.T) {
	var seen []arrival
	s := mediaStub(t, 401, 200, &seen)
	_, failures := s.playerSweep(playing(direct("query", "ApiKey")))
	if len(failures) != 1 || !strings.Contains(failures[0], "401") || !strings.Contains(failures[0], "Findroid") {
		t.Errorf("failures %q, want one naming 401", failures)
	}
}

func TestAPictureAnswers200HoweverItIsListedAndFailsOtherwise(t *testing.T) {
	var seen []arrival
	ok := mediaStub(t, 0, 200, &seen)
	if _, failures := ok.playerSweep(playing(posterListed("none", ""), posterListed("query", "ApiKey"))); len(failures) != 0 {
		t.Errorf("a picture answering 200 failed: %q", failures)
	}
	for _, status := range []int{401, 404} {
		bad := mediaStub(t, 0, status, &seen)
		for _, listed := range []conform.PlayerRequest{posterListed("none", ""), posterListed("query", "ApiKey")} {
			if _, failures := bad.playerSweep(playing(listed)); len(failures) != 1 {
				t.Errorf("a picture listed %q answering %d gave %q, want one failure", listed.Token, status, failures)
			}
		}
	}
}

func TestARequestListedAsNotRequestedIsNotSentAndASweepThatSentNothingFails(t *testing.T) {
	var seen []arrival
	s := mediaStub(t, 0, 200, &seen)
	none := conform.PlayerRequest{Kind: "subtitle", NotRequested: true, File: "a.kt", Line: 9}
	sent, failures := s.playerSweep(playing(none))
	if sent != 0 || len(seen) != 0 {
		t.Errorf("sent %d, the server saw %d", sent, len(seen))
	}
	if len(failures) == 0 {
		t.Error("a sweep that sent nothing did not fail")
	}
	if sent, failures := s.playerSweep(playing(none, direct("none", ""))); sent != 1 || len(failures) != 0 {
		t.Errorf("one request and one not requested: sent %d, failures %q", sent, failures)
	}
}

// The film that joins the tier's libraries for its text subtitle and attached font is named by the runner, as a
// "Type/Name" key of learnItems and the index of its text subtitle, so a sweep and this stub agree on it. Alpha
// Film has video and audio only: a subtitle request filled from it would ask for a stream the film lacks.
func TestTheSubtitleFilmIsAKeyOfItsOwnAndTheSubtitleRequestIsFilledFromIt(t *testing.T) {
	if !strings.HasPrefix(subtitleFilmKey, "Movie/") || subtitleFilmKey == "Movie/Alpha Film (2021)" || subtitleFilmIndex < 1 {
		t.Fatalf("subtitleFilmKey %q, subtitleFilmIndex %d", subtitleFilmKey, subtitleFilmIndex)
	}
	var seen []arrival
	s := mediaStub(t, 0, 200, &seen)
	sub := conform.PlayerRequest{Kind: "subtitle", Method: "GET", Route: "/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/Stream.vtt",
		URLFrom: "client", File: "a.kt", Line: 4, Token: "header", TokenName: "X-Emby-Token"}
	hls := conform.PlayerRequest{Kind: "hls", Method: "GET", Route: "/Videos/{itemId}/master.m3u8", URLFrom: "server", File: "a.kt", Line: 3, Token: "header", TokenName: "X-Emby-Token"}
	sent, failures := s.playerSweep(playing(sub, hls))
	if sent != 2 || len(failures) != 0 || len(seen) != 2 {
		t.Fatalf("sent %d, failures %q, the server saw %d", sent, failures, len(seen))
	}
	if want := "/Videos/f2/f2/Subtitles/" + strconv.Itoa(subtitleFilmIndex) + "/Stream.vtt"; seen[0].path != want {
		t.Errorf("the subtitle request was %q, want %q", seen[0].path, want)
	}
	if seen[1].path != "/Videos/f1/master.m3u8" {
		t.Errorf("the HLS request was %q, want it filled from Alpha Film", seen[1].path)
	}
}
