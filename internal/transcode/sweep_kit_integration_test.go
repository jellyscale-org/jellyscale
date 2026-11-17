//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// JS-078: the kit of the sweep checks (budget_integration_test.go,
// asset_sweep_integration_test.go, sweep_processes_integration_test.go).
//
// Names the checks take from the task, which do not exist yet:
//   - Worker.Budget, int64: the transcode store's budget in bytes; 0 is none.
//   - Worker.SweepEvery, time.Duration: the sweep period of sweepLoop; 0 is
//     the minute it has always been.
//   - the metric jellyscale_transcode_store_bytes, a gauge of the worker's Collect.
// Everything else is Worker.Sweep, the stores and the tables as they are.

// target is a store the sweeps run on: a directory, or a bucket of an S3 server.
type target struct {
	name, uri string
	env       map[string]string // What store.Open reads the credentials from.
	s3        bool
}

// targets are the POSIX directory and each S3 target of test/integration.sh,
// each with a prefix of its own.
func targets(t *testing.T) []target {
	t.Helper()
	found := []target{{name: "posix", uri: "file://" + t.TempDir()}}
	for _, entry := range testenv.S3(t) {
		name, raw, _ := strings.Cut(entry, "=")
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		env := map[string]string{"JELLYSCALE_S3_ACCESS_KEY": query.Get("key"), "JELLYSCALE_S3_SECRET_KEY": query.Get("secret")}
		query.Del("key")
		query.Del("secret")
		parsed.RawQuery = query.Encode()
		parsed.Path = "/sweep-" + uuid.NewString()
		found = append(found, target{name: name, uri: parsed.String(), env: env, s3: true})
	}
	return found
}

// fresh is the same kind of store under a prefix or directory of its own, so a
// subtest starts from an empty store whatever the subtests before it left.
func (tg target) fresh(t *testing.T) target {
	t.Helper()
	out := tg
	if !tg.s3 {
		out.uri = "file://" + t.TempDir()
		return out
	}
	parsed, err := url.Parse(tg.uri)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/sweep-" + uuid.NewString()
	out.uri = parsed.String()
	return out
}

// open puts the target's credentials in the environment of this test and of the
// processes it starts, and returns the store.
func (tg target) open(t *testing.T) store.Store {
	t.Helper()
	tg.use(t)
	opened, err := store.Open(tg.uri)
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func (tg target) use(t *testing.T) {
	t.Helper()
	for name, value := range tg.env {
		t.Setenv(name, value)
	}
}

// spyStore counts and can hold what a sweep does to a store, and fail its listing.
type spyStore struct {
	store.Store
	mu           sync.Mutex
	lists        int
	deleted      []string
	listErr      error
	afterList    func()
	beforeDelete func(key string)
	afterDelete  func(key string)
	afterPut     func(key string)
}

func (s *spyStore) List(ctx context.Context, prefix string) ([]store.Object, error) {
	s.mu.Lock()
	s.lists++
	err, hook := s.listErr, s.afterList
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	objects, err := s.Store.List(ctx, prefix)
	if err == nil && hook != nil {
		hook()
	}
	return objects, err
}

func (s *spyStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deleted = append(s.deleted, key)
	hook := s.beforeDelete
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	if err := s.Store.Delete(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	after := s.afterDelete
	s.mu.Unlock()
	if after != nil {
		after(key)
	}
	return nil
}

func (s *spyStore) Put(ctx context.Context, key string, body io.Reader) error {
	if err := s.Store.Put(ctx, key, body); err != nil {
		return err
	}
	s.mu.Lock()
	hook := s.afterPut
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	return nil
}

func (s *spyStore) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func (s *spyStore) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deleted)
}

func (s *spyStore) failLists(err error) {
	s.mu.Lock()
	s.listErr = err
	s.mu.Unlock()
}

// logs is a log a test reads.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newLogs() (*slog.Logger, *logs) {
	l := &logs{}
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug})), l
}

// hex32 is a spec hash made of one character.
func hex32(c string) string { return strings.Repeat(c, 32) }

func putSized(t *testing.T, st store.Store, key string, size int64) {
	t.Helper()
	if err := st.Put(context.Background(), key, bytes.NewReader(bytes.Repeat([]byte("x"), int(size)))); err != nil {
		t.Fatal(err)
	}
}

// keysUnder are the keys of every object under prefix.
func keysUnder(t *testing.T, st store.Store, prefix string) []string {
	t.Helper()
	objects, err := st.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	keys := make([]string, 0, len(objects))
	for _, o := range objects {
		keys = append(keys, o.Key)
	}
	slices.Sort(keys)
	return keys
}

func bytesUnder(t *testing.T, st store.Store, prefix string) int64 {
	t.Helper()
	objects, err := st.List(context.Background(), prefix)
	if err != nil {
		t.Fatalf("list %s: %v", prefix, err)
	}
	var total int64
	for _, o := range objects {
		total += o.Size
	}
	return total
}

// seedSpec stores a spec's init segment (sizes[0]) and segments 0.. (the rest)
// and, unless seen is negative, a viewer row asked for it that long ago.
func (r *rig) seedSpec(t *testing.T, st store.Store, hash string, sizes []int64, seen time.Duration) {
	t.Helper()
	for i, size := range sizes {
		putSized(t, st, SegmentKey(hash, i-1), size)
	}
	if seen >= 0 {
		r.viewerAt(t, hash, seen, false)
	}
}

// viewerAt records a viewer of the spec, last seen that long ago.
func (r *rig) viewerAt(t *testing.T, hash string, seen time.Duration, stopped bool) {
	t.Helper()
	session := strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_viewers (play_session_id, spec_hash, user_id, device_id, last_seen_at, stopped)
		VALUES ($1, $2, $3, 'd', now() - make_interval(secs => $4), $5)`, session, hash, r.user, seen.Seconds(), stopped); err != nil {
		t.Fatal(err)
	}
}

// jobRow inserts a job of the spec in the given state with a lease that does
// not run out during the test, as a live worker's has.
func (r *rig) jobRow(t *testing.T, hash, state string, helper bool) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, helper, worker, lease_expires_at)
		VALUES ($1, '{}', 'owner', 0, 0, $2, $3, 'worker', now() + interval '1 hour')`, hash, state, helper); err != nil {
		t.Fatal(err)
	}
}

// sweeper is a worker that only sweeps, over the rig's database and the store.
func (r *rig) sweeper(segments store.Store, budget int64, log *slog.Logger) *Worker {
	var media store.Cache
	return &Worker{Pool: r.pool, Segments: segments, Log: log, Media: media.Open, Retention: 24 * time.Hour, Budget: budget}
}

// metricBytes is what the worker's Collect says the transcode store holds.
func metricBytes(w *Worker) (value float64, reported bool, err error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(w)
	families, err := registry.Gather()
	if err != nil {
		return 0, false, err
	}
	for _, family := range families {
		if family.GetName() != "jellyscale_transcode_store_bytes" {
			continue
		}
		if len(family.Metric) != 1 {
			return 0, false, fmt.Errorf("jellyscale_transcode_store_bytes has %d series, want one: it is the store's, not a label's", len(family.Metric))
		}
		return family.Metric[0].GetGauge().GetValue(), true, nil
	}
	return 0, false, nil
}

func storeBytes(t *testing.T, w *Worker) (float64, bool) {
	t.Helper()
	value, reported, err := metricBytes(w)
	if err != nil {
		t.Fatal(err)
	}
	return value, reported
}

// openNamed opens a pool whose sessions say who they are in pg_stat_activity,
// so a check can ask whether that one session waits for a lock.
func openNamed(t *testing.T, dsn, name string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), withApplicationName(t, dsn, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func withApplicationName(t testing.TB, dsn, name string) string {
	return withParam(t, dsn, "application_name", name)
}

func withParam(t testing.TB, dsn, key, value string) string {
	t.Helper()
	out, err := dsnWith(dsn, key, value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func dsnWith(dsn, key, value string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// lockWaiting says whether a session of that application_name is waiting for a lock now.
func (r *rig) lockWaiting(name string) bool {
	var n int
	r.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE application_name = $1 AND wait_event_type = 'Lock'`, name).Scan(&n) //nolint:errcheck
	return n > 0
}

// untilEither waits for one of the states that only the other side's own next
// step can end, and says which came: the channel's or the lock wait of the
// named session. A bound of a minute only ends a hang.
func (r *rig) untilEither(t *testing.T, what string, done <-chan struct{}, waiter string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		select {
		case <-done:
			return
		default:
		}
		if r.lockWaiting(waiter) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
