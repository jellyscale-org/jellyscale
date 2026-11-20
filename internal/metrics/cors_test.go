package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// JS-144 (D-016): /metrics is an operator's, not an API route. Every role serves it on this one listener, so a GET
// with an Origin and a preflight carry no Access-Control-* header, and answer what they answered before.
func TestTheMetricsListenerOpensNothingToAnotherOrigin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, err := Serve(ctx, "127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"tauri://localhost", "https://app.example.test"} {
		for _, tc := range []struct {
			method string
			header map[string]string
			want   int
		}{
			{"GET", map[string]string{"Origin": origin}, 200},
			{"OPTIONS", map[string]string{"Origin": origin, "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"}, 405},
		} {
			request, _ := http.NewRequest(tc.method, "http://"+addr+"/metrics", nil)
			for name, value := range tc.header {
				request.Header.Set(name, value)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != tc.want {
				t.Errorf("%s /metrics from %s answered %d, want %d (what it answers today)", tc.method, origin, response.StatusCode, tc.want)
			}
			for name := range response.Header {
				if strings.HasPrefix(strings.ToLower(name), "access-control-") {
					t.Errorf("%s /metrics from %s carries %s", tc.method, origin, name)
				}
			}
			if tc.method == "GET" && !strings.Contains(string(body), "go_goroutines") {
				t.Errorf("GET /metrics from %s sent no metrics: %.60q", origin, body)
			}
		}
	}
}
