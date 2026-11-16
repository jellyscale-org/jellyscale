//go:build integration

package transcode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// The kit for the checks of JS-106: a worker process whose segment store is slow in a way the
// test sets while it runs. The worker is the test binary run again (workerProcess), whose store is
// wrapped when JELLYSCALE_TEST_SLOWPUT names a directory; each Put reads the file <dir>/<worker id>
// for what to do, so a test, or a step of a fault schedule, changes it at any moment and the next
// Put takes it. The file holds "<behaviour> <which puts> <duration>":
//
//	steady   the Put reads its body in pieces, a pause between each, over the duration: slow, never still,
//	         and deaf to its context (a store that goes on after it was cancelled)
//	seekback the Put reads the whole body at once, seeks back to its start, then reads it as steady does
//	gated    as steady, but after its thirteenth read of fifteen the Put waits, reading nothing, for the
//	         file <dir>/<worker id>.release (releaseGate), a few seconds at most: a put the test lets go
//	stall    the Put reads nothing and returns when its context ends
//	stallpart the Put reads half of its body and returns when its context ends
//	hang     the Put reads nothing, takes no notice of its context, stores the object and returns nil
//	         after the duration
//
// which puts: init (the init segment), first (segment 0), all. A Put that does not match is the
// store's own. The Put reads the body it is handed, whatever the worker made it, so the worker's
// own counting of what the store reads is what a test judges; it never reads a copy.
//
// Every Put writes to <dir>/<worker id>.log: "begin", "read" for the last byte it read, "end".
// The pauses are a tenth of the lease or less, so a build that counts what the store reads sees
// a byte every few seconds, and a build that counts the Put as one step sees none for a lease.
type slowPut struct {
	store.Store
	id, dir string
	mu      sync.Mutex
}

func (s *slowPut) mode() (behaviour, which string, d time.Duration) {
	raw, err := os.ReadFile(filepath.Join(s.dir, s.id))
	if err != nil {
		return "", "", 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 3 {
		return "", "", 0
	}
	d, _ = time.ParseDuration(fields[2])
	return fields[0], fields[1], d
}

func (s *slowPut) note(event, key, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, err := os.OpenFile(filepath.Join(s.dir, s.id+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintf(f, "%d %s %s %s\n", time.Now().UnixNano(), event, key, detail)
		f.Close()
	}
}

func (s *slowPut) Put(ctx context.Context, key string, body io.Reader) error {
	behaviour, which, d := s.mode()
	matches := which == "all" || (which == "init" && strings.HasSuffix(key, "/init.mp4")) || (which == "first" && strings.HasSuffix(key, "/0.mp4"))
	if behaviour == "" || !matches {
		return s.Store.Put(ctx, key, body)
	}
	s.note("begin", key, behaviour)
	defer func() { s.note("end", key, behaviour) }()
	seeker, canSeek := body.(io.Seeker)
	s.note("seekable", key, fmt.Sprint(canSeek))
	switch behaviour {
	case "stall":
		<-ctx.Done()
		return ctx.Err()
	case "hang":
		time.Sleep(d)
		return s.Store.Put(context.WithoutCancel(ctx), key, body)
	case "stallpart":
		raw := make([]byte, 64*1024)
		n, _ := body.Read(raw)
		s.note("read", key, "")
		_ = n
		<-ctx.Done()
		return ctx.Err()
	}
	var all []byte
	reads := 0
	if behaviour == "seekback" {
		// All of it at once, then back to the start, and the slow reading is of the second pass.
		first, err := io.ReadAll(body)
		if err != nil || !canSeek {
			return fmt.Errorf("seekback needs a body that reads and seeks: %v, seekable %v", err, canSeek)
		}
		s.note("read", key, "")
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return err
		}
		all = make([]byte, 0, len(first))
	}
	// Fifteen pieces over the duration: a byte every d/15, a tenth of a lease and a little more for
	// d of one lease and a half.
	pieces := 15
	size := 1 << 20
	if canSeek {
		end, _ := seeker.Seek(0, io.SeekEnd)
		seeker.Seek(0, io.SeekStart) //nolint:errcheck
		size = int(end)
	}
	piece := make([]byte, size/pieces+1)
	for {
		n, err := body.Read(piece)
		all = append(all, piece[:n]...)
		if n > 0 {
			s.note("read", key, "")
			reads++
		}
		if behaviour == "gated" && reads == 13 {
			for _, err := os.Stat(filepath.Join(s.dir, s.id+".release")); err != nil; _, err = os.Stat(filepath.Join(s.dir, s.id+".release")) {
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// No notice of the context: a put that is cancelled and goes on is the stricter store, and
		// a worker that begins the next put after it is the worker the fence cases look for.
		time.Sleep(d / time.Duration(pieces))
	}
	return s.Store.Put(context.WithoutCancel(ctx), key, bytes.NewReader(all))
}

// slowPutEnv is the environment of a worker whose store is slow as its directory says.
func slowPutEnv(dir string) string { return "JELLYSCALE_TEST_SLOWPUT=" + dir }

// releaseGate lets a gated Put go on.
func releaseGate(t testing.TB, dir string, w *outageWorker) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, w.id+".release"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// setPuts says how the worker's Puts behave from now on; "" makes them the store's own.
func setPuts(t testing.TB, dir string, w *outageWorker, mode string) {
	t.Helper()
	path := filepath.Join(dir, w.id)
	if mode == "" {
		os.Remove(path)
		return
	}
	if err := os.WriteFile(path+".tmp", []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// slowEvent is one line of a worker's slow-store log.
type slowEvent struct {
	at         time.Time
	event, key string
	detail     string
}

func slowEvents(t testing.TB, dir string, w *outageWorker) []slowEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, w.id+".log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var found []slowEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var nanos int64
		var e slowEvent
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		fmt.Sscan(f[0], &nanos)
		e.at, e.event, e.key = time.Unix(0, nanos), f[1], f[2]
		if len(f) > 3 {
			e.detail = f[3]
		}
		found = append(found, e)
	}
	return found
}
