//go:build integration

package main

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"jellyscale/internal/db"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// gaugeOf is the value of the library's gauge, and whether it has one.
func gaugeOf(t *testing.T, vec *prometheus.GaugeVec, id string) (float64, bool) {
	t.Helper()
	metrics := make(chan prometheus.Metric, 100)
	vec.Collect(metrics)
	close(metrics)
	for metric := range metrics {
		var m dto.Metric
		if err := metric.Write(&m); err != nil {
			t.Fatal(err)
		}
		for _, label := range m.GetLabel() {
			if label.GetName() == "library" && label.GetValue() == id {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// The scan gauges say what this process's last scan of each library did. A scan
// that another replica holds the lock for is that replica's to report, so it
// leaves no series here and does not read as a success.
func TestScanLoopReportsEachLibraryScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	good := library.Library{ID: uuid.NewString(), Name: "good", Media: store.NewPosix(t.TempDir())}
	broken := library.Library{ID: uuid.NewString(), Name: "broken", Media: store.NewPosix(t.TempDir() + "/gone")}
	taken := library.Library{ID: uuid.NewString(), Name: "taken", Media: store.NewPosix(t.TempDir())}
	for _, lib := range []library.Library{good, broken, taken} {
		if _, err := pool.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, $2, 'movies', 'file:///x', 'file:///x')
			ON CONFLICT (id) DO NOTHING`, lib.ID, lib.Name+lib.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Another replica's scan of "taken".
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "scan:"+taken.ID); err != nil {
		t.Fatal(err)
	}

	go scanLoop(ctx, pool, []library.Library{taken, broken, good}, time.Hour, slog.New(slog.DiscardHandler))
	deadline := time.Now().Add(20 * time.Second)
	for _, ok := gaugeOf(t, scanOK, good.ID); !ok; _, ok = gaugeOf(t, scanOK, good.ID) {
		if time.Now().After(deadline) {
			t.Fatal("no scan gauge for the library that scans")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if value, _ := gaugeOf(t, scanOK, good.ID); value != 1 {
		t.Errorf("a scan that worked: %v", value)
	}
	if value, ok := gaugeOf(t, scanDuration, good.ID); !ok || value <= 0 || value > 20 {
		t.Errorf("duration %v, %v", value, ok)
	}
	if value, ok := gaugeOf(t, scanOK, broken.ID); !ok || value != 0 {
		t.Errorf("a scan that failed: %v, %v", value, ok)
	}
	for _, id := range []string{good.ID, broken.ID} {
		if at, ok := gaugeOf(t, scanTime, id); !ok || math.Abs(float64(time.Now().Unix())-at) > 20 {
			t.Errorf("last scan time %v, %v; want about now", at, ok)
		}
	}
	if _, ok := gaugeOf(t, scanTime, taken.ID); ok {
		t.Error("a library scanned elsewhere has a scan time here")
	}
	if _, ok := gaugeOf(t, scanOK, taken.ID); ok {
		t.Error("a library scanned elsewhere has a result here")
	}
	if _, ok := gaugeOf(t, scanDuration, taken.ID); ok {
		t.Error("a library scanned elsewhere has a duration here")
	}
}

// The log names a worker by the id that transcode_nodes and transcode_jobs use
// and by the host, so an operator can find the pod that runs a job.
func TestAWorkerLogsItsIDAndHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool, err := db.Open(ctx, testenv.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var logged bytes.Buffer
	opened, err := openTranscoding(ctx, config{}, []string{roleWorker}, pool, "srv", slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		<-opened.stopped
	}()
	match := regexp.MustCompile(`worker=(\S+)`).FindStringSubmatch(logged.String())
	if match == nil {
		t.Fatalf("no worker id in the log:\n%s", logged.String())
	}
	id := match[1]
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		pool.QueryRow(ctx, "SELECT count(*) FROM transcode_nodes WHERE id = $1", id).Scan(&n) //nolint:errcheck
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the logged id %q is not in transcode_nodes", id)
		}
		time.Sleep(50 * time.Millisecond)
	}
	host, _ := os.Hostname()
	if text := logged.String(); !strings.Contains(text, "host="+host) {
		t.Errorf("no host %q in the log:\n%s", host, text)
	}
}
