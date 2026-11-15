//go:build integration

package transcode

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/store"
	"jellyscale/internal/testenv"
)

// A job whose pool call hangs before its loop first turns (the setup between the
// claim and the loop is calls to Postgres, the store and the library) is given up
// a lease after it was kept, though the keeper's own connection answers every
// renewal: the keeper counts a job's stall from when it kept it, not from its
// first turn. The pool is held as the claim is answered, so the first call after
// it hangs.
func TestAJobHungBeforeItsLoopFirstTurnsIsGivenUp(t *testing.T) {
	slowAt(t, "2")
	s := newStack(t, 1)
	r, w := s.r, s.workers[0]
	claimed := make(chan struct{})
	w.db.OnStatement(func(sent []byte) bool {
		return bytes.Contains(sent, []byte("FROM media_sources m JOIN shown_items"))
	}, func() { w.db.Hold(); close(claimed) }, nil)
	t.Cleanup(w.db.Release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.fetch(ctx, s.jobs, r.spec, psid(0), 0) //nolint:errcheck
	select {
	case <-claimed:
	case <-time.After(keeperHang):
		t.Fatal("the job never asked for its source")
	}
	job := r.jobOf(t, r.hash)
	first := r.leaseOf(t, job)
	waitFor(t, "the keeper to renew the hung job", keeperHang, func() bool { return !r.leaseOf(t, job).Equal(first) })
	waitFor(t, "the hung job's lease to run out, which only a keeper that gave it up allows", keeperHang, func() bool {
		return r.leaseRunOut(t, job)
	})
	w.db.Release()
	waitFor(t, "the worker to log the job as interrupted", keeperHang, func() bool { return len(interruptedLines(w)) > 0 })
	theReasonIsTheLease(t, w, 1)
}

// slowImageStore serves the sidecar image file in pieces, a pause before each; after
// the last piece that moves it stops for good (until the read's context ends), when
// stopAfter is not zero. Every other object comes as stored.
type slowImageStore struct {
	store.Store
	pieces, stopAfter int
	pause             time.Duration
}

func (s slowImageStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := s.Store.Open(ctx, key, offset)
	if err != nil || !strings.HasSuffix(key, ".sup") {
		return body, err
	}
	raw, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return nil, err
	}
	size := len(raw)/s.pieces + 1
	return io.NopCloser(&pacedReader{ctx: ctx, raw: raw, size: size, pause: s.pause, stopAfter: s.stopAfter}), nil
}

type pacedReader struct {
	ctx       context.Context
	raw       []byte
	size      int
	pause     time.Duration
	stopAfter int // Pieces served before it stops moving; 0 never does.
	served    int
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if len(p.raw) == 0 {
		return 0, io.EOF
	}
	wait := p.pause
	if p.stopAfter > 0 && p.served >= p.stopAfter {
		wait = keeperHang
	}
	select {
	case <-time.After(wait):
	case <-p.ctx.Done():
		return 0, p.ctx.Err()
	}
	n := copy(b[:min(len(b), p.size)], p.raw)
	p.raw = p.raw[n:]
	p.served++
	return n, nil
}

// logLines is a log handler's output, safe to read while the worker writes it.
type logLines struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *logLines) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(b)
}

func (l *logLines) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.text.String(), sub)
}

// The setup of a burn of a sidecar image track copies the file out of the store before
// the loop turns. The rig's lease is 3 s and the copy here takes 6 s, in twenty
// pieces: slow, and never still for longer than a lease. It is no stall, so the job
// is not cut and makes its first segment. A build that counts only the loop's turns
// cuts the job at the lease, again and again, and the pick never gets its segment.
func TestACopyOfASidecarImageFileThatIsSlowButMovingIsNotCutAsStalled(t *testing.T) {
	testenv.FFmpeg(t)
	good := testenv.PGS(1920, 1080, testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120})
	r := newRig(t, nil)
	r.useBesideImage(t, supEntry, map[string][]byte{"damaged.sup": good})
	log := &logLines{}
	r.worker(t, func(w *Worker) {
		w.Log = slog.New(slog.NewTextHandler(log, nil))
		open := w.Media
		w.Media = func(uri string) (store.Store, error) {
			media, err := open(uri)
			return slowImageStore{Store: media, pieces: 20, pause: 300 * time.Millisecond}, err
		}
	})
	if err, _ := r.burn(t, r.jobs(t), 0); err != nil {
		t.Fatalf("the pick failed: %v", err)
	}
	if log.has("has not come round") {
		t.Error("a copy that kept moving was cut as stalled")
	}
}

// A copy that has stopped moving is a hang, and is cut as one: no byte for a lease
// and the job is given up with the stall's reason, though the keeper's connection
// answers every renewal.
func TestACopyOfASidecarImageFileThatStoppedMovingIsCutAsStalled(t *testing.T) {
	testenv.FFmpeg(t)
	good := testenv.PGS(1920, 1080, testenv.Cue{Start: 6, End: 10, X: 660, Y: 100, W: 600, H: 120})
	r := newRig(t, nil)
	r.useBesideImage(t, supEntry, map[string][]byte{"damaged.sup": good})
	log := &logLines{}
	r.worker(t, func(w *Worker) {
		w.Log = slog.New(slog.NewTextHandler(log, nil))
		open := w.Media
		w.Media = func(uri string) (store.Store, error) {
			media, err := open(uri)
			return slowImageStore{Store: media, pieces: 20, pause: 100 * time.Millisecond, stopAfter: 3}, err
		}
	})
	j := r.jobs(t)
	index := 2
	r.spec.Subtitle, r.spec.SubtitleFingerprint = &index, "f1"
	r.hash = r.spec.Hash()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		j.Watch(ctx, r.viewer(psid(0)), r.hash, 0)              //nolint:errcheck
		j.Segment(ctx, r.spec, r.hash, r.viewer(psid(0)), 0, 0) //nolint:errcheck
	}()
	waitFor(t, "the stopped copy to be cut as stalled", keeperHang, func() bool { return log.has("has not come round") })
	cancel()
	<-watching
}
