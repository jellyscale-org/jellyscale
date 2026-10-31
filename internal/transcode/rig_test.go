package transcode

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

// These tests need a disposable database and ffmpeg; test/integration.sh
// provides both. Segments land in a real store, ffmpeg is the real thing, and
// processes are counted by reading /proc.

var (
	filmOnce sync.Once
	filmDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if filmDir != "" {
		os.RemoveAll(filmDir)
	}
	os.Exit(code)
}

// film is a 90 second 720p clip with AAC. Tests that need ffmpeg to still be
// running when they act use slow, because how long an encode takes depends on
// the machine.
func film(t *testing.T) string {
	t.Helper()
	filmOnce.Do(func() {
		dir, err := os.MkdirTemp("", "jellyscale-film-")
		if err != nil {
			t.Fatal(err)
		}
		filmDir = dir
		run(t, "ffmpeg", "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=90",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=90",
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "28", "-g", "240", "-c:a", "aac", filepath.Join(dir, "film.mkv"))
	})
	if filmDir == "" {
		t.Skip("could not make the film")
	}
	return filmDir
}

type rig struct {
	dsn      string
	pool     *pgxpool.Pool
	items    *items.Repo
	spec     playback.Spec
	hash     string
	user     string
	segments store.Store
	scratch  string
	storeURI string // What workers register as their transcodeStore.
	log      *slog.Logger
}

func newRig(t *testing.T, segments store.Store) *rig {
	t.Helper()
	needFFmpeg(t)
	dsn := os.Getenv("JELLYSCALE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JELLYSCALE_TEST_DATABASE_URL not set")
	}
	dir := film(t)
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := &rig{dsn: dsn, pool: pool, items: &items.Repo{Pool: pool, ServerID: "srv"}, scratch: t.TempDir(),
		log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	if segments == nil {
		segments = store.NewPosix(t.TempDir())
	}
	r.segments = segments
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec("TRUNCATE libraries, users, transcode_jobs, transcode_viewers, transcode_nodes CASCADE")
	library, item := uuid.NewString(), uuid.NewString()
	r.user = uuid.NewString()
	exec(`INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri) VALUES ($1, 'L', 'movies', $2, $2)`, library, "file://"+dir)
	exec(`INSERT INTO items (id, library_id, type, name, sort_name, natural_key, run_time_ticks, dto)
		VALUES ($1, $2, 'Movie', 'Film', 'film', 'film', 900000000, '{}')`, item, library)
	exec(`INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams) VALUES ($1, $1, 'film.mkv', 1, 'etag1', 'mkv', '[]')`, item)
	exec(`INSERT INTO users (id, name, password_hash) VALUES ($1, 'viewer', 'x')`, r.user)
	r.spec = playback.Spec{Item: db.CompactID(item), Source: db.CompactID(item), ETag: "etag1", AudioIndex: 1,
		VideoBitrate: 1_500_000, AudioBitrate: 128_000, Width: 1280, Height: 720}
	r.hash = r.spec.Hash()
	return r
}

// jobs is one stream replica: its own pool, as in production.
func (r *rig) jobs(t *testing.T) *Jobs {
	t.Helper()
	pool, err := db.Open(context.Background(), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub := &Hub{Pool: pool, Log: r.log}
	go hub.Run(ctx)
	return &Jobs{Pool: pool, Segments: r.segments, Hub: hub, QueueTimeout: 5 * time.Second, WaitTimeout: 60 * time.Second}
}

// worker runs a worker until the test ends. Cancelling the returned function
// stops it and waits for its ffmpeg to be gone.
func (r *rig) worker(t *testing.T, change func(*Worker)) (stop func()) {
	t.Helper()
	pool, err := db.Open(context.Background(), r.dsn)
	if err != nil {
		t.Fatal(err)
	}
	var media store.Cache
	w := &Worker{ID: uuid.NewString(), Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Segments: r.segments, Log: r.log, Media: media.Open,
		Scratch: r.scratch, Slots: 2, Idle: 60 * time.Second, Lease: 3 * time.Second, Heartbeat: 200 * time.Millisecond}
	if change != nil {
		change(w)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Hub = &Hub{Pool: pool, Log: r.log}
	go w.Hub.Run(ctx)
	done := make(chan struct{})
	go func() {
		var registered sync.WaitGroup
		registered.Add(1)
		go func() {
			defer registered.Done()
			Register(ctx, pool, r.log, w.ID, "worker", r.storeURI, w.Slots, 200*time.Millisecond)
		}()
		w.Run(ctx)
		registered.Wait()
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
			pool.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}

// slow makes ffmpeg read its input at the speed of the film, so a job takes as
// long as the film plays and is still running when a test ends it. Without
// this, a fast machine finishes the encode before the test acts, and a test of
// stopping ffmpeg would pass because ffmpeg stopped itself.
func slow(t *testing.T) {
	t.Helper()
	needFFmpeg(t)
	film(t) // Made at full speed, before the shim is in the way.
	real, _ := exec.LookPath("ffmpeg")
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + real + " -re \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func (r *rig) viewer(psid string) Viewer {
	return Viewer{PlaySessionID: psid, DeviceID: "device", UserID: r.user}
}

// fetch asks for a segment as a viewer and returns its bytes.
func (r *rig) fetch(t *testing.T, j *Jobs, psid string, n int) ([]byte, error) {
	t.Helper()
	return r.fetchAt(t, j, psid, n, 0)
}

// fetchAt is fetch for a client whose playlist says it starts at segment
// startAt, which matters to the init segment.
func (r *rig) fetchAt(t *testing.T, j *Jobs, psid string, n, startAt int) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := j.Watch(ctx, r.viewer(psid), r.hash); err != nil {
		return nil, err
	}
	return j.Segment(ctx, r.spec, r.hash, r.viewer(psid), n, startAt)
}

func (r *rig) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// ffmpegProcesses lists the running ffmpeg command lines that write into the
// rig's scratch directory.
func (r *rig) ffmpegProcesses() []string {
	entries, _ := os.ReadDir("/proc")
	var found []string
	for _, entry := range entries {
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if filepath.Base(args[0]) == "ffmpeg" && strings.Contains(string(raw), r.scratch) {
			found = append(found, strings.Join(args, " "))
		}
	}
	return found
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// segmentStart reports which segment a running ffmpeg was started at.
func segmentStart(cmdline string) string {
	fields := strings.Fields(cmdline)
	for i, f := range fields {
		if f == "-start_number" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// s3Segments returns a segment store on each S3 target test/integration.sh set up.
func s3Segments(t *testing.T) map[string]store.Store {
	t.Helper()
	stores := map[string]store.Store{}
	for _, entry := range strings.Split(os.Getenv("JELLYSCALE_TEST_S3"), ",") {
		name, uri, _ := strings.Cut(entry, "=")
		if uri == "" {
			continue
		}
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		t.Setenv("JELLYSCALE_S3_ACCESS_KEY", query.Get("key"))
		t.Setenv("JELLYSCALE_S3_SECRET_KEY", query.Get("secret"))
		query.Del("key")
		query.Del("secret")
		parsed.RawQuery = query.Encode()
		parsed.Path = "/segments-" + uuid.NewString()
		opened, err := store.NewS3(parsed)
		if err != nil {
			t.Fatal(err)
		}
		stores[name] = opened
	}
	return stores
}

func isMP4Segment(data []byte) bool {
	return len(data) > 1000 && bytes.Contains(data[:16], []byte("styp"))
}
