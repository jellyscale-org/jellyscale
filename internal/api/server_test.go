package api

import (
	"bufio"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestManifestIsCurrent(t *testing.T) {
	want, err := os.ReadFile("../../compat/endpoints.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := Manifest((&Server{}).Routes()); got != string(want) {
		t.Fatal("compat/endpoints.yaml is stale; run: go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml")
	}
}

// Aliases exist only for approved obsolete routes the web client calls, so each
// must really be obsolete and anything else must not be flagged as one.
func TestAliasesAreObsoleteRoutes(t *testing.T) {
	file, err := os.Open("../../compat/hidden-routes-12.4.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	obsolete := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 4 && fields[0] == "obsolete" {
			obsolete[fields[2]+" "+strings.ToLower(fields[3])] = true
		}
	}
	for _, route := range (&Server{}).Routes() {
		key := route.Method + " " + strings.ToLower(route.Path)
		if route.Status == Alias && !obsolete[key] {
			t.Errorf("%s is marked as an alias but isn't an obsolete route", key)
		}
		if route.Status != Alias && obsolete[key] {
			t.Errorf("%s is obsolete but isn't marked as an alias", key)
		}
	}
}

func TestRoutePatternsRegister(t *testing.T) {
	s := &Server{Roles: []string{RoleAPI, RoleStream}, Log: slog.New(slog.DiscardHandler)}
	s.Handler() // ServeMux panics on conflicting patterns.
}

func TestWildcardNamesKeepTheirCase(t *testing.T) {
	if got := lowerLiterals("/Users/{userId}/Items/{itemId}/Intros"); got != "/users/{userId}/items/{itemId}/intros" {
		t.Fatalf("got %q", got)
	}
}

func TestNormalizeLowersPathAndQueryKeys(t *testing.T) {
	var path, value string
	handler := normalize(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		path, value = r.URL.Path, r.URL.Query().Get("static")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/Videos/ABC/Stream/?Static=True", nil))
	if path != "/videos/abc/stream" || value != "True" {
		t.Fatalf("got %q %q", path, value)
	}
}

func TestNormalizeLeavesWebAssetsAlone(t *testing.T) {
	var path string
	handler := normalize(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { path = r.URL.Path }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/web/Main.Bundle.js", nil))
	if path != "/web/Main.Bundle.js" {
		t.Fatalf("got %q", path)
	}
}

func TestProtectedRoutesRejectAnonymousRequests(t *testing.T) {
	s := &Server{Roles: []string{RoleAPI, RoleStream}, Log: slog.New(slog.DiscardHandler)}
	handler := s.Handler()
	for _, target := range []string{"/Items", "/Users/Me", "/System/Info", "/UserViews"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", target, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", target, recorder.Code)
		}
	}
}
