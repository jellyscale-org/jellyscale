//go:build integration

package transcode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"

	"jellyscale/internal/testenv"
)

// subtitleRig is a rig whose film has a track, and a way to ask for it.
func (r *rig) track(t *testing.T) (items.Source, store.Store) {
	t.Helper()
	r.useBurnFilm(t)
	source, found, err := r.items.Source(context.Background(), r.spec.Item, "")
	if err != nil || !found {
		t.Fatalf("source: %v %v", found, err)
	}
	assets, err := store.Open(source.AssetStoreURI)
	if err != nil {
		t.Fatal(err)
	}
	return source, assets
}

// countExtractions puts an ffmpeg in front of the real one that notes each
// extraction (the run that writes track files), and returns the count.
func countExtractions(t *testing.T, extra string) func() int {
	t.Helper()
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *track.vtt*) echo run >>%q; %s;; esac\nexec %q \"$@\"\n", runs, extra, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(runs)
		return strings.Count(string(data), "run")
	}
}

// Many requests across replicas and a burn-in job that wants the same track
// run one extraction, on whichever of two workers claims it.
func TestOneExtractionForManyRequestsAndABurn(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	runs := countExtractions(t, "sleep 1 >/dev/null 2>&1")
	r.worker(t, nil)
	r.worker(t, nil)
	j := r.jobs(t)

	index := 2
	r.spec.Subtitle = &index
	r.hash = r.spec.Hash()
	var wg sync.WaitGroup
	errs := make([]error, 30)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = FetchSubtitle(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, index)
		}()
	}
	_, burnErr := r.fetch(t, j, psid(0), 0) // The job that burns it joins the same extraction.
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if burnErr != nil {
		t.Fatal(burnErr)
	}
	if n := runs(); n != 1 {
		t.Fatalf("%d extractions, want 1", n)
	}
}

// Extractions have slots of their own: with every transcode slot taken, a
// track is still extracted at once.
func TestExtractionsProceedWithEveryTranscodeSlotBusy(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	// Only the transcode is slowed, to the film's speed.
	real := testenv.FFmpeg(t)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *\"-f hls\"*) exec %q -re \"$@\";; esac\nexec %q \"$@\"\n", real, real)
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r.worker(t, func(w *Worker) { w.Slots = 1 })
	j := r.jobs(t)
	go r.fetch(t, j, psid(0), 0) //nolint:errcheck
	waitFor(t, "the transcode to run", 20*time.Second, func() bool { return len(r.ffmpegProcesses()) == 1 })
	began := time.Now()
	if _, err := FetchSubtitle(context.Background(), r.pool, j.Hub, assets, r.spec.Item, source, 2); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 10*time.Second || len(r.ffmpegProcesses()) != 1 {
		t.Fatalf("took %s with %d transcodes running; the slot should still be busy", took, len(r.ffmpegProcesses()))
	}
}

// A worker that vanished mid-extraction loses its lease, and another takes the
// job over; once a job has lost its lease three times it is failed, and the
// failure is remembered, so a track that kills its workers isn't tried forever.
func TestLostLeasesAreRetriedThenGivenUp(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	runs := countExtractions(t, "")
	r.worker(t, nil)
	// A text track is extracted by the film's pass job, any other by its own.
	key := func(index int) string {
		if slices.Contains(subtitles.TextTracks(source), index) {
			return subtitles.PassKey(source)
		}
		return subtitles.Key(source, index)
	}
	insert := func(index, attempts int) {
		t.Helper()
		if _, err := r.pool.Exec(context.Background(), `INSERT INTO subtitle_jobs (key, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
			VALUES ($1, $2, $3, $4, 'running', 'gone', $5, now() - interval '1 second')`, key(index), r.spec.Item, source.ID, index, attempts); err != nil {
			t.Fatal(err)
		}
	}

	insert(2, 1)
	if _, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 2); err != nil {
		t.Fatal(err)
	}
	if attempts := r.count(t, "SELECT attempts FROM subtitle_jobs WHERE key = $1", key(2)); attempts != 2 || runs() != 1 {
		t.Fatalf("attempts %d, runs %d; want the second attempt to have run once", attempts, runs())
	}

	insert(1, subtitleAttempts)
	began := time.Now()
	_, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 1)
	if !errors.Is(err, ErrUnreadable) || time.Since(began) > 10*time.Second || runs() != 1 {
		t.Fatalf("after %s: %v, %d runs", time.Since(began), err, runs())
	}
}

// With no worker, a track that isn't cached can't be delivered, and the
// request says so within seconds. A cached track is read with no worker.
func TestNoWorkerAnswersQuicklyAndCachedTracksStillWork(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	stop := r.worker(t, nil)
	if _, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 2); err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 2); err != nil {
		t.Fatalf("a cached track needs no worker: %v", err)
	}
	began := time.Now()
	_, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 1)
	if !errors.Is(err, ErrNoSubtitleWorker) || time.Since(began) > 10*time.Second {
		t.Fatalf("after %s: %v", time.Since(began), err)
	}
}

// reportWorker keeps a process that claims nothing reporting in as a worker.
func (r *rig) reportWorker(t *testing.T) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() {
		for ctx.Err() == nil {
			r.pool.Exec(ctx, `INSERT INTO transcode_nodes (id, roles, slots) VALUES ('old', 'worker', 2)
				ON CONFLICT (id) DO UPDATE SET seen_at = now()`) //nolint:errcheck // Ends with the test.
			time.Sleep(200 * time.Millisecond)
		}
	}()
}

// A worker from before subtitle jobs reports in but never claims them. The
// request must not wait on it for hours: an unclaimed job is answered like no
// worker at all.
func TestAnUnclaimedJobAnswersQuicklyWhileAWorkerReportsIn(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	r.reportWorker(t)
	began := time.Now()
	_, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 1)
	if !errors.Is(err, ErrNoSubtitleWorker) || time.Since(began) > 15*time.Second {
		t.Fatalf("after %s: %v", time.Since(began), err)
	}
}

// With every extraction slot busy a job stays queued, and the request waits its
// turn: jellyfin-web asks once, so a refusal would lose the track.
func TestAQueuedJobWaitsBehindBusyWorkers(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	r.reportWorker(t)
	// Another track is being extracted, so something is claiming jobs.
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO subtitle_jobs (key, item_id, source_id, idx, state, worker, attempts, lease_expires_at)
		VALUES ('busy', $1, $2, 2, 'running', 'busy', 1, now() + interval '1 hour')`, r.spec.Item, source.ID); err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	go func() {
		_, err := FetchSubtitle(context.Background(), r.pool, nil, assets, r.spec.Item, source, 2)
		answered <- err
	}()
	select {
	case err := <-answered:
		t.Fatalf("answered with a job queued behind a busy worker: %v", err)
	case <-time.After(queueTimeout + 2*time.Second):
	}
	r.worker(t, nil) // A slot comes free.
	select {
	case err := <-answered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the job was not run when a slot came free")
	}
}

// blackhole forwards connections to the database until cut, after which it
// holds them open and passes nothing on, as a lost network does.
func blackhole(t *testing.T, dsn string) (through string, cut func()) {
	t.Helper()
	target, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var lost atomic.Bool
	forward := func(from, to net.Conn) {
		buffer := make([]byte, 32*1024)
		for {
			n, err := from.Read(buffer)
			if err != nil {
				return
			}
			if !lost.Load() {
				to.Write(buffer[:n]) //nolint:errcheck // The reader sees the same failure.
			}
		}
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target.Host)
			if err != nil {
				client.Close()
				continue
			}
			t.Cleanup(func() { client.Close(); server.Close() })
			go forward(client, server)
			go forward(server, client)
		}
	}()
	proxied := *target
	proxied.Host = listener.Addr().String()
	return proxied.String(), func() { lost.Store(true) }
}

// A worker cut off from the database stops extracting before its lease lapses.
// Another worker takes the job once it does, so going on would read the file twice.
func TestAWorkerCutOffFromTheDatabaseStopsBeforeItsLeaseLapses(t *testing.T) {
	testenv.FFmpeg(t)
	r := newRig(t, nil)
	source, assets := r.track(t)
	slow(t)
	through, cut := blackhole(t, r.dsn)
	pool, err := db.Open(context.Background(), through)
	if err != nil {
		t.Fatal(err)
	}
	r.worker(t, func(w *Worker) {
		w.Pool = pool
		w.Lease, w.Heartbeat = 6*time.Second, 2*time.Second
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go FetchSubtitle(ctx, r.pool, nil, assets, r.spec.Item, source, 2) //nolint:errcheck
	extracting := func() bool {
		found, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, path := range found {
			if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "track.vtt") {
				return true
			}
		}
		return false
	}
	waitFor(t, "the extraction to start", 20*time.Second, extracting)
	cut()
	waitFor(t, "the extraction to stop", 10*time.Second, func() bool { return !extracting() })
	if live := r.count(t, "SELECT count(*) FROM subtitle_jobs WHERE lease_expires_at > now()"); live != 1 {
		t.Fatal("the extraction outlived its lease")
	}
}
