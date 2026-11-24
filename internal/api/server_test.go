package api

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
