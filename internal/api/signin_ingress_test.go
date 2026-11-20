package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"jellyscale/internal/signin"
)

// With no proxy declared, a forwarded header from a loopback or private peer is logged once per process whatever route
// carries it, not only a sign-in; a declared proxy's is not logged.
func TestAForwardedHeaderIsLoggedOnAnyRouteOnce(t *testing.T) {
	const message = "forwarded header from an undeclared proxy"
	for _, tc := range []struct {
		proxies []string
		want    int
	}{{[]string{}, 1}, {[]string{"127.0.0.1"}, 0}} {
		var log bytes.Buffer
		proxies, err := signin.ParseProxies(tc.proxies)
		if err != nil {
			t.Fatal(err)
		}
		h := (&Server{Roles: []string{RoleAPI}, Log: slog.New(slog.NewTextHandler(&log, nil)), Proxies: proxies}).Handler()
		for _, path := range []string{"/System/Ping", "/healthz", "/System/Ping"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = "127.0.0.1:5555"
			r.Header.Set("X-Forwarded-For", "9.9.9.9")
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
		if got := strings.Count(log.String(), message); got != tc.want {
			t.Errorf("trustedProxies %v: %d lines say %q, want %d\n%s", tc.proxies, got, message, tc.want, &log)
		}
	}
}
