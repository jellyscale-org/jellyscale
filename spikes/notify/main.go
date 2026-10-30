// Load test for Postgres LISTEN/NOTIFY as jellyscale's wakeup bus.
//
// Writers commit a small row, with or without pg_notify, at a target rate.
// Listeners stand in for api and stream replicas. Reports commit and delivery
// latency, because NOTIFY's cost is a global lock taken at commit.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(float64(len(sorted)-1)*p)]
}

func listen(ctx context.Context, dsn string, ready *sync.WaitGroup, record func(time.Duration)) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN bus"); err != nil {
		log.Fatal(err)
	}
	ready.Done()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		sent, err := strconv.ParseInt(n.Payload, 10, 64)
		if err != nil {
			log.Fatal(err)
		}
		record(time.Since(time.UnixMicro(sent)))
	}
}

// Notification strategies. inline puts pg_notify in the data transaction;
// async commits the data durably, then notifies in a second transaction with
// synchronous_commit=off, which skips the WAL flush under the notify lock.
const (
	modeNone   = "none"
	modeInline = "inline"
	modeAsync  = "async"
)

type result struct {
	commits, delivered int64
	commit, delivery   []time.Duration
	late               int64
}

func run(ctx context.Context, dsn string, pool *pgxpool.Pool, rate, listeners int, mode string, d time.Duration) result {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	var res result
	var ready, listening sync.WaitGroup
	for range listeners {
		ready.Add(1)
		listening.Add(1)
		go func() {
			defer listening.Done()
			listen(runCtx, dsn, &ready, func(l time.Duration) {
				mu.Lock()
				res.delivery = append(res.delivery, l)
				res.delivered++
				mu.Unlock()
			})
		}()
	}
	ready.Wait()

	var late atomic.Int64
	var writers sync.WaitGroup
	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	stop := time.After(d)
	slots := make(chan struct{}, 64)

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-ticker.C:
		}
		select {
		case slots <- struct{}{}:
		default:
			// All 64 writers are busy, so the target rate isn't being met.
			late.Add(1)
			continue
		}
		writers.Add(1)
		go func() {
			defer writers.Done()
			defer func() { <-slots }()
			start := time.Now()
			tx, err := pool.Begin(ctx)
			if err != nil {
				log.Fatal(err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO spike_events (payload) VALUES ('x')"); err != nil {
				log.Fatal(err)
			}
			payload := strconv.FormatInt(start.UnixMicro(), 10)
			if mode == modeInline {
				if _, err := tx.Exec(ctx, "SELECT pg_notify('bus', $1)", payload); err != nil {
					log.Fatal(err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				log.Fatal(err)
			}
			if mode == modeAsync {
				hint, err := pool.Begin(ctx)
				if err != nil {
					log.Fatal(err)
				}
				if _, err := hint.Exec(ctx, "SET LOCAL synchronous_commit = off"); err != nil {
					log.Fatal(err)
				}
				if _, err := hint.Exec(ctx, "SELECT pg_notify('bus', $1)", payload); err != nil {
					log.Fatal(err)
				}
				if err := hint.Commit(ctx); err != nil {
					log.Fatal(err)
				}
			}
			elapsed := time.Since(start)
			mu.Lock()
			res.commit = append(res.commit, elapsed)
			res.commits++
			mu.Unlock()
		}()
	}
	writers.Wait()
	time.Sleep(time.Second)
	cancel()
	listening.Wait()
	res.late = late.Load()
	return res
}

func main() {
	dsn := flag.String("dsn", "", "Postgres DSN of a disposable database")
	duration := flag.Duration("d", 20*time.Second, "duration per step")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("-dsn is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn+"?pool_max_conns=64")
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS spike_events (id bigserial PRIMARY KEY, payload text)"); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%7s %6s %9s %10s %10s %10s %10s %10s %6s\n",
		"rate/s", "mode", "listeners", "commit/s", "commit p50", "commit p99", "deliv p50", "deliv p99", "late")
	for _, rate := range []int{100, 500, 2000, 5000} {
		for _, mode := range []string{modeNone, modeInline, modeAsync} {
			r := run(ctx, *dsn, pool, rate, 8, mode, *duration)
			slices.Sort(r.commit)
			slices.Sort(r.delivery)
			fmt.Printf("%7d %6s %9d %10.0f %10s %10s %10s %10s %6d\n", rate, mode, 8,
				float64(r.commits)/duration.Seconds(),
				percentile(r.commit, .5).Round(10*time.Microsecond), percentile(r.commit, .99).Round(10*time.Microsecond),
				percentile(r.delivery, .5).Round(10*time.Microsecond), percentile(r.delivery, .99).Round(10*time.Microsecond),
				r.late)
			if mode != modeNone && r.delivered != r.commits*8 {
				fmt.Printf("        lost notifications: delivered %d, expected %d\n", r.delivered, r.commits*8)
			}
		}
	}
}
