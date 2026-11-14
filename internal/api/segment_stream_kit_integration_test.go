//go:build integration

package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/items"
	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
	"jellyscale/internal/transcode"
)

// JS-077's kit. The segment checks drive the stream role over HTTP, as a player does, and decide from
// what the player is sent and from what the role asked the store, so they name no function of the build.
// They name one thing the task implies: Stat(ctx, key) (int64, error) on store.Store.

// segStore is the store under a stream replica: the real directory, with what a test needs to withhold
// bytes, cut a body, fail a call, and count what the replica asked.
type segStore struct {
	inner *store.Posix
	local bool // Whether it says a segment has a path on this machine; an S3 store does not.

	stats, opens, lists atomic.Int64
	live                atomic.Int64 // Bodies opened and not yet closed.

	mu        sync.Mutex
	statErr   func(key string) error
	afterStat func(key string)
	open      func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) // nil: the real object.
	synth     map[string]int64                                                           // Objects that are not in the directory: key to size.
}

func (s *segStore) hooks() (func(string) error, func(string), func(context.Context, string, int64) (io.ReadCloser, error), map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statErr, s.afterStat, s.open, s.synth
}

func (s *segStore) set(f func(*segStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *segStore) reset() {
	s.set(func(s *segStore) { s.statErr, s.afterStat, s.open, s.synth = nil, nil, nil, nil })
	s.stats.Store(0)
	s.opens.Store(0)
	s.lists.Store(0)
}

// requests is every request of any kind the store has had since reset.
func (s *segStore) requests() int64 { return s.stats.Load() + s.opens.Load() + s.lists.Load() }

func (s *segStore) Stat(ctx context.Context, key string) (int64, error) {
	s.stats.Add(1)
	statErr, afterStat, _, synth := s.hooks()
	if statErr != nil {
		if err := statErr(key); err != nil {
			return 0, err
		}
	}
	if size, ok := synth[key]; ok {
		return size, nil
	}
	objects, err := s.inner.List(ctx, key)
	if err != nil {
		return 0, err
	}
	for _, o := range objects {
		if o.Key == key {
			if afterStat != nil {
				defer afterStat(key)
			}
			return o.Size, nil
		}
	}
	return 0, store.ErrNotFound
}

func (s *segStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	s.opens.Add(1)
	_, _, open, _ := s.hooks()
	var body io.ReadCloser
	var err error
	if open != nil {
		body, err = open(ctx, key, offset)
	} else {
		body, err = s.inner.Open(ctx, key, offset)
	}
	if err != nil {
		return nil, err
	}
	s.live.Add(1)
	return &tracked{ReadCloser: body, live: &s.live}, nil
}

func (s *segStore) List(ctx context.Context, prefix string) ([]store.Object, error) {
	s.lists.Add(1)
	return s.inner.List(ctx, prefix)
}
func (s *segStore) Put(ctx context.Context, key string, body io.Reader) error {
	return s.inner.Put(ctx, key, body)
}
func (s *segStore) Delete(ctx context.Context, key string) error { return s.inner.Delete(ctx, key) }
func (s *segStore) LocalPath(key string) (string, bool) {
	if !s.local {
		return "", false
	}
	return s.inner.LocalPath(key)
}

type tracked struct {
	io.ReadCloser
	live *atomic.Int64
	once sync.Once
}

func (b *tracked) Close() error {
	b.once.Do(func() { b.live.Add(-1) })
	return b.ReadCloser.Close()
}

// blob is n bytes no two stretches of which are alike, so a body cut and padded, shifted or
// taken from another object is told from the right one.
func blob(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// gate gives the first `first` bytes of an object at once and the rest only when released; a read
// that waits ends when the request that opened it is cancelled or the body is closed.
type gate struct {
	data    []byte
	first   int
	release chan struct{}
	ctx     context.Context
	pos     int
	closed  chan struct{}
	once    sync.Once
	ended   chan struct{} // Closed when a read that waited was ended by the context or Close.
	endOnce sync.Once
}

func newGate(ctx context.Context, data []byte, offset int64, first int, release chan struct{}, ended chan struct{}) *gate {
	return &gate{data: data, pos: int(offset), first: first, release: release, ctx: ctx, closed: make(chan struct{}), ended: ended}
}

func (g *gate) Read(p []byte) (int, error) {
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	if g.pos >= g.first {
		select {
		case <-g.release:
		case <-g.ctx.Done():
			g.endOnce.Do(func() { close(g.ended) })
			return 0, g.ctx.Err()
		case <-g.closed:
			g.endOnce.Do(func() { close(g.ended) })
			return 0, io.ErrClosedPipe
		}
		g.first = len(g.data)
	}
	end := min(g.first, len(g.data))
	n := copy(p, g.data[g.pos:end])
	g.pos += n
	return n, nil
}

func (g *gate) Close() error { g.once.Do(func() { close(g.closed) }); return nil }

// cut is data and then err, as a body that ends early.
type cut struct {
	data []byte
	err  error
}

func (c *cut) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, c.err
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}
func (*cut) Close() error { return nil }

// segReplica is a stream replica over a store of the kit's.
type segReplica struct {
	*replica
	store *segStore
}

func newSegReplica(t *testing.T, e *env, inner *store.Posix, local bool) *segReplica {
	t.Helper()
	proxy := testenv.NewDBProxy(t, testenv.DatabaseURL(t))
	pool, err := db.Open(context.Background(), proxy.DSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	rp := &replica{role: RoleStream, proxy: proxy, log: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(rp.log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ss := &segStore{inner: inner, local: local}
	hub := &transcode.Hub{Pool: pool, Log: log}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)
	s := &Server{Pool: pool, Items: &items.Repo{Pool: pool, ServerID: "srv"}, Roles: []string{RoleStream}, Log: log, SocketIdle: time.Minute}
	// No worker takes a job here, and a request waits as long as a viewer's player lets it: the queue timeout is long.
	s.Transcode = &transcode.Jobs{Pool: pool, Segments: ss, Hub: hub, QueueTimeout: time.Hour, WaitTimeout: 2 * time.Minute, Log: log}
	rp.server = httptest.NewServer(s.Handler())
	t.Cleanup(rp.server.Close)
	return &segReplica{replica: rp, store: ss}
}

// segKit is a copied film with a stream replica over a store that says a segment has a path and one over a
// store that does not (S3), and a second of the latter, so that waiters can be on two replicas.
type segKit struct {
	*playAway
	f      *awayFilm
	inner  *store.Posix
	remote *segReplica // Its store has no local path.
	local  *segReplica
	second *segReplica // Another replica over a store with no local path.

	offline sync.Once
	others  map[string]int64 // The other job rows by kind.
}

func newSegKit(t *testing.T) *segKit {
	t.Helper()
	p := newPlayAway(t, 1)
	inner, ok := p.segments.(*store.Posix)
	if !ok {
		t.Fatalf("the kit's store is %T", p.segments)
	}
	k := &segKit{playAway: p, f: p.films[0], inner: inner, others: map[string]int64{}}
	if k.f.name != "copy" {
		t.Fatalf("the kit's film is a %s, want the copy", k.f.name)
	}
	p.playing(t, 0)
	k.remote = newSegReplica(t, p.env, inner, false)
	k.local = newSegReplica(t, p.env, inner, true)
	k.second = newSegReplica(t, p.env, inner, false)
	_, refresh := timings(t)
	refreshed(refresh) // So that each replica holds a snapshot with the viewer's token.
	return k
}

// goOffline puts Postgres away from the replicas that have a snapshot, once.
func (k *segKit) goOffline() {
	k.offline.Do(func() {
		k.remote.proxy.Away()
		k.local.proxy.Away()
	})
}

func (k *segKit) key(n int) string { return transcode.SegmentKey(k.f.hash, n) }

// url is the path of segment n (-1 is the init) as the first viewer asks for it.
func (k *segKit) url(n int) string {
	path := k.f.segment[n]
	if n == -1 {
		path = k.f.init
	}
	return k.as(k.f, path, 0)
}

// reset forgets what the last case stored and hooked, and its jobs.
func (k *segKit) reset(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, rp := range []*segReplica{k.remote, k.local, k.second} {
			rp.store.reset()
		}
		for n := -1; n < 64; n++ {
			k.inner.Delete(context.Background(), k.key(n)) //nolint:errcheck
		}
		if _, err := k.pool.Exec(context.Background(), "TRUNCATE transcode_jobs, transcode_viewers"); err != nil {
			t.Error(err)
		}
		k.others = map[string]int64{}
	}
	clear()
	t.Cleanup(clear)
}

func (k *segKit) put(t *testing.T, n int, data []byte) {
	t.Helper()
	if err := k.inner.Put(context.Background(), k.key(n), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

// announce is what a worker does after it stores segment n (-1 is the init): the job row says it is
// stored, and a hint goes out. kind says whose job it is: "own" is the job the waiter waits on, "other"
// another viewer's and "helper" a helper's. withHint false is a hint lost.
func (k *segKit) announce(t *testing.T, kind string, n int, withHint bool) {
	t.Helper()
	ctx := context.Background()
	id, ok := k.others[kind]
	if !ok {
		switch kind {
		case "own":
			id = scalar[int64](t, k.pool, "SELECT id FROM transcode_jobs WHERE spec_hash = $1 AND owner <> 'someone else' ORDER BY id DESC LIMIT 1", k.f.hash)
			mustExec(t, k.pool, "UPDATE transcode_jobs SET state = 'running', worker = 'w9', lease_expires_at = now() + interval '1 hour' WHERE id = $1", id)
		default:
			id = scalar[int64](t, k.pool, `INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at, helper)
				VALUES ($1, '{}', 'someone else', 0, 0, 'running', 'w9', now() + interval '1 hour', $2) RETURNING id`, k.f.hash, kind == "helper")
		}
		k.others[kind] = id
	}
	mustExec(t, k.pool, "UPDATE transcode_jobs SET next_segment = greatest(next_segment, $2, 1) WHERE id = $1", id, n+1)
	if withHint {
		transcode.Notify(ctx, k.pool, "segment:"+k.f.hash) // The payload the workers send, transcode/hub.go segmentHint.
	}
}

// reply is what a viewer's request got.
type reply struct {
	status int
	length int64 // Content-Length, -1 when the response had none.
	body   []byte
	err    error // A body that did not end cleanly.
	header http.Header
	at     time.Time
}

var plain = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

// ask sends the request and returns when the response header arrives.
func askSeg(ctx context.Context, base, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", base+path, nil)
	if err != nil {
		return nil, err
	}
	return plain.Do(request)
}

// fetch reads a whole response; err is that of the header or of the body.
func fetchSeg(ctx context.Context, base, path string) reply {
	response, err := askSeg(ctx, base, path)
	if err != nil {
		return reply{err: err, length: -1, at: time.Now()}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return reply{status: response.StatusCode, length: response.ContentLength, body: body, err: err, header: response.Header, at: time.Now()}
}

// pendingFor starts the request and says it was not answered for the window: the replica is waiting.
// It returns the answer channel and whether none came. A window is how long a viewer's request has not
// been answered, not a speed: a build that answers sooner, with a 200 or an error, fails it.
func pendingFor(ctx context.Context, base, path string, window time.Duration) (<-chan reply, bool) {
	done := make(chan reply, 1)
	go func() { done <- fetchSeg(ctx, base, path) }()
	select {
	case r := <-done:
		done <- r
		return done, false
	case <-time.After(window):
		return done, true
	}
}

// within is the wait for an answer that must come: ten times what any of them takes, so it bounds a hang.
const within = 60 * time.Second

func mustAnswer(t *testing.T, what string, ch <-chan reply) reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("%s: not answered", what)
		return reply{}
	}
}

// whole checks a reply is the object, whole, under a Content-Length that is its size.
func whole(t *testing.T, what string, r reply, want []byte) {
	t.Helper()
	switch {
	case r.err != nil:
		t.Errorf("%s: %v after %d of %d bytes", what, r.err, len(r.body), len(want))
	case r.status != 200:
		t.Errorf("%s: status %d: %.100s", what, r.status, r.body)
	case r.length != int64(len(want)):
		t.Errorf("%s: Content-Length %d, want %d", what, r.length, len(want))
	case !bytes.Equal(r.body, want):
		t.Errorf("%s: %d bytes that are not the object's %d", what, len(r.body), len(want))
	}
}

// aborted checks a reply was a 200 whose body the replica cut off: an error after fewer bytes than
// the Content-Length it promised, never a clean end and never padding.
func aborted(t *testing.T, what string, r reply, want []byte) {
	t.Helper()
	switch {
	case r.status != 200 && r.err == nil:
		t.Errorf("%s: status %d: %.100s, want a 200 whose body is aborted", what, r.status, r.body)
	case r.err == nil:
		t.Errorf("%s: the body ended cleanly after %d bytes of a Content-Length of %d", what, len(r.body), r.length)
	case int64(len(r.body)) >= r.length:
		t.Errorf("%s: %d bytes arrived of a Content-Length of %d, and still an error: %v", what, len(r.body), r.length, r.err)
	case !bytes.HasPrefix(want, r.body):
		t.Errorf("%s: the %d bytes that arrived are not the start of the object", what, len(r.body))
	}
}

// noneOpen waits for every body a store opened to be closed: a response that ended closes what it opened.
func noneOpen(t *testing.T, what string, stores ...*segStore) {
	t.Helper()
	waitLong(t, what+": every object opened to be closed", func() bool {
		for _, s := range stores {
			if s.live.Load() != 0 {
				return false
			}
		}
		return true
	})
}
