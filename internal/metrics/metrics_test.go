package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestServeAnswersMetricsAndNothingElse(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "jellyscale_test_total", Help: "x"})
	counter.Add(3)
	addr, err := Serve(ctx, "127.0.0.1:0", slog.New(slog.DiscardHandler), counter)
	if err != nil {
		t.Fatal(err)
	}
	status, body := get(t, "http://"+addr+"/metrics")
	if status != 200 || !strings.Contains(body, "jellyscale_test_total 3\n") || !strings.Contains(body, "go_goroutines") {
		t.Fatalf("%d: %.300s", status, body)
	}
	if status, _ := get(t, "http://"+addr+"/Items"); status != 404 {
		t.Fatalf("a path that is not /metrics: %d", status)
	}
	stop()
}

// A process may serve several, as a test of two servers does; none of them
// shares counters with another.
func TestServersKeepTheirOwnRegistry(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	log := slog.New(slog.DiscardHandler)
	a, err := Serve(ctx, "127.0.0.1:0", log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Serve(ctx, "127.0.0.1:0", log); err != nil {
		t.Fatal(err)
	}
	if status, _ := get(t, "http://"+a+"/metrics"); status != 200 {
		t.Fatal(status)
	}
}

func TestServeRefusesAnAddressInUse(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	addr, err := Serve(ctx, "127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Serve(ctx, addr, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("two listeners on one address")
	}
}
