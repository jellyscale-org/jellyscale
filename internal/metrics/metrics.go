// Package metrics serves the Prometheus text exposition on a listener of its
// own, so it stays off the client-facing port.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler answers with the Go runtime, the process and what the collectors
// report. The registry is private: a process may build several (as tests do)
// and none of them shares counters or panics on a duplicate.
func Handler(extra ...prometheus.Collector) http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(extra...)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// Serve answers /metrics on addr until ctx ends, and returns the address it
// listens on (which differs from addr when the port is 0). It listens before it
// returns, so a bad address is an error at startup. There is no authentication:
// the address is for the operator's network.
func Serve(ctx context.Context, addr string, log *slog.Logger, extra ...prometheus.Collector) (string, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler(extra...))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		server.Close() //nolint:errcheck
	}()
	log.Info("serving metrics", "addr", listener.Addr().String())
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			log.Error("the metrics listener stopped", "error", err)
		}
	}()
	return listener.Addr().String(), nil
}

// Pool reports a Postgres pool's state at scrape time.
type Pool struct{ Pool *pgxpool.Pool }

var (
	poolConns = prometheus.NewDesc("jellyscale_db_pool_connections", "Connections in the pool by state.", []string{"state"}, nil)
	poolMax   = prometheus.NewDesc("jellyscale_db_pool_max_connections", "The pool's size limit.", nil, nil)
	poolAcq   = prometheus.NewDesc("jellyscale_db_pool_acquires_total", "Connections acquired from the pool.", nil, nil)
	poolWait  = prometheus.NewDesc("jellyscale_db_pool_acquire_wait_seconds_total", "Time spent waiting to acquire a connection.", nil, nil)
)

func (Pool) Describe(chan<- *prometheus.Desc) {}

func (p Pool) Collect(ch chan<- prometheus.Metric) {
	stat := p.Pool.Stat()
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(stat.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(stat.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(stat.TotalConns()), "total")
	ch <- prometheus.MustNewConstMetric(poolMax, prometheus.GaugeValue, float64(stat.MaxConns()))
	ch <- prometheus.MustNewConstMetric(poolAcq, prometheus.CounterValue, float64(stat.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(poolWait, prometheus.CounterValue, stat.AcquireDuration().Seconds())
}
