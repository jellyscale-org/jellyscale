//go:build integration

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// JS-077. A stored segment is sent while it is read, whole under a Content-Length that is its size, and a
// body that is short is never sent as a whole one. Each case is asked on the segment route (Postgres up)
// and on the offline path (the replica's Postgres away, a token in its snapshot), of a store with a local
// path and of one without. A case that needs a store to hold bytes back, cut a body or fail a call runs
// on the store without one only: a store with a path may be served from the file, and never asked.

type target struct {
	via   string // "route" or "offline".
	local bool
	k     *segKit
}

func (tg target) rp() *segReplica {
	if tg.local {
		return tg.k.local
	}
	return tg.k.remote
}

func (tg target) base() string { return tg.rp().server.URL }

func (tg target) get(ctx context.Context, n int) reply { return fetchSeg(ctx, tg.base(), tg.k.url(n)) }

func (tg target) Name() string {
	if tg.local {
		return tg.via + "/local-path"
	}
	return tg.via + "/no-local-path"
}

// pending says the request for n goes unanswered for a window, then stops it. On the route it first
// says what was waited for; offline the request waits for Postgres, which stays away.
func (tg target) waits(t *testing.T, n int) (<-chan reply, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch, waiting := pendingFor(ctx, tg.base(), tg.k.url(n), 2*time.Second)
	if !waiting {
		r := <-ch
		cancel()
		t.Fatalf("%s: request for segment %d was answered (status %d, %d bytes, %v), want it left waiting", tg.Name(), n, r.status, len(r.body), r.err)
	}
	return ch, cancel
}

// ready is what the store must hold again for the waiting request to be answered, on the route; offline
// nothing waiting is answered, and a new request is asked.
func (tg target) thenWhole(t *testing.T, n int, data []byte, waiting <-chan reply, cancel context.CancelFunc) {
	t.Helper()
	if tg.via == "route" {
		tg.k.announce(t, "own", n, true)
		whole(t, tg.Name()+" the request that waited", mustAnswer(t, "the waiting request", waiting), data)
		cancel()
		return
	}
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), within)
	defer stop()
	whole(t, tg.Name()+" a new request", tg.get(ctx, n), data)
}

func TestSegmentResponse(t *testing.T) {
	k := newSegKit(t)
	cases := []struct {
		name       string
		needsStore bool // Holds back, cuts or fails the store: only the one without a local path.
		run        func(t *testing.T, tg target)
	}{
		{"is sent while it is read", false, sentWhileRead},
		{"an empty object is not there yet", false, func(t *testing.T, tg target) { notThereYet(t, tg, true) }},
		{"an absent object is not there yet", false, func(t *testing.T, tg target) { notThereYet(t, tg, false) }},
		{"a body cut early that a reopen completes is sent whole", true, cutThenCompleted},
		{"a cut that nothing was sent of waits and reads again", true, cutBeforeAnything},
		{"a cut after some was sent is aborted, never a clean end", true, cutAfterSome},
		{"a store failure before anything is sent is an error and no job", true, storeFails},
		{"an object replaced by one of another size is never cut to the old length", true, replacedAfterSize},
		{"every object opened is closed when the response ends", true, closedWhenDone},
		{"a viewer who leaves mid-segment ends the read", true, leavesMidSegment},
		{"a stored segment costs one Stat and one Open", false, oneStatOneOpen},
	}
	for _, via := range []string{"route", "offline"} {
		if via == "offline" {
			k.goOffline()
		}
		for _, local := range []bool{false, true} {
			tg := target{via: via, local: local, k: k}
			for _, c := range cases {
				if local && c.needsStore {
					continue
				}
				t.Run(tg.Name()+"/"+c.name, func(t *testing.T) {
					k.reset(t)
					c.run(t, tg)
				})
			}
		}
	}
}

// Init and media, both: the viewer has the segment's first bytes while the store has not given the rest,
// and gets the object's bytes unchanged under a Content-Length equal to its size (over 2048 bytes: net/http
// adds the header to a smaller body by itself).
func sentWhileRead(t *testing.T, tg target) {
	for _, n := range []int{-1, 2} {
		data := blob(int64(n)+10, 256<<10)
		tg.k.put(t, n, data)
		ctx, cancel := context.WithTimeout(context.Background(), within)
		if tg.local {
			// A file cannot be held back; what a viewer is sent must still be the object, whole.
			whole(t, fmt.Sprintf("segment %d", n), tg.get(ctx, n), data)
			cancel()
			continue
		}
		release, ended := make(chan struct{}), make(chan struct{})
		tg.rp().store.set(func(s *segStore) {
			s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
				return newGate(ctx, data, offset, 8192, release, ended), nil
			}
		})
		head, cancelHead := context.WithTimeout(ctx, 20*time.Second) // The viewer has bytes long before this: a hang.
		response, err := askSeg(head, tg.base(), tg.k.url(n))
		if err != nil {
			t.Fatalf("segment %d: nothing was sent while the store held back all but 8192 of %d bytes: %v", n, len(data), err)
		}
		first := make([]byte, 8192)
		if _, err := io.ReadFull(response.Body, first); err != nil || !bytes.Equal(first, data[:8192]) {
			t.Fatalf("segment %d: the first 8192 bytes while the store held back the rest: %v", n, err)
		}
		cancelHead()
		close(release)
		response.Body.Close()
		cancel()
		// Again, to read it all: the same object is sent whole once the store gives it.
		tg.rp().store.set(func(s *segStore) { s.open = nil })
		ctx, cancel = context.WithTimeout(context.Background(), within)
		whole(t, fmt.Sprintf("segment %d after the viewer had it released", n), tg.get(ctx, n), data)
		cancel()
		if response.ContentLength != int64(len(data)) {
			t.Errorf("segment %d: Content-Length %d on the response that began while the store held back the rest, want %d", n, response.ContentLength, len(data))
		}
	}
}

// An absent or zero-length object is not there yet: the route waits and asks again, the offline path
// leaves the request to wait for Postgres, and neither answers 200 with an empty body.
func notThereYet(t *testing.T, tg target, empty bool) {
	for _, n := range []int{-1, 2} {
		if empty {
			tg.k.put(t, n, nil)
		}
		waiting, cancel := tg.waits(t, n)
		data := blob(int64(n)+20, 64<<10)
		tg.k.put(t, n, data)
		tg.thenWhole(t, n, data, waiting, cancel)
		tg.k.reset(t)
	}
}

// openFrom is the real object the hook falls through to.
func openFrom(s *segStore, ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	return s.inner.Open(ctx, key, offset)
}

// A body that ends early mid-segment and that a reopen at that offset completes is sent whole, at
// several offsets of one segment, with no abort.
func cutThenCompleted(t *testing.T, tg target) {
	data := blob(30, 256<<10)
	tg.k.put(t, 2, data)
	s := tg.rp().store
	for _, at := range []int{1, 4097, 100_000, 131_072, 262_143} {
		var cutOnce atomic.Bool
		s.set(func(s *segStore) {
			s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
				body, err := openFrom(s, ctx, key, offset)
				if err != nil || offset != 0 || !cutOnce.CompareAndSwap(false, true) {
					return body, err
				}
				body.Close()
				return &cut{data: data[:at], err: io.ErrUnexpectedEOF}, nil
			}
		})
		s.opens.Store(0)
		ctx, cancel := context.WithTimeout(context.Background(), within)
		whole(t, fmt.Sprintf("a body cut at %d", at), tg.get(ctx, 2), data)
		cancel()
		if s.opens.Load() < 2 {
			t.Errorf("a body cut at %d: the store was opened %d times, so the cut never happened", at, s.opens.Load())
		}
	}
}

// heal makes the store give the whole object again.
func heal(s *segStore) { s.set(func(s *segStore) { s.open = nil }) }

// A body that gives nothing and ends early, however often it is reopened, is not a segment: nothing was
// sent, so the request waits and reads it again, and is answered whole once the object is whole.
func cutBeforeAnything(t *testing.T, tg target) {
	data := blob(31, 256<<10)
	tg.k.put(t, 2, data)
	s := tg.rp().store
	s.set(func(s *segStore) {
		s.open = func(context.Context, string, int64) (io.ReadCloser, error) {
			return &cut{err: io.ErrUnexpectedEOF}, nil
		}
	})
	waiting, cancel := tg.waits(t, 2)
	heal(s)
	tg.thenWhole(t, 2, data, waiting, cancel)
}

// Cut short after some was sent, however often it is reopened: the response is aborted, in whichever way the
// store ends the body, and a later request is served whole once the object is whole.
func cutAfterSome(t *testing.T, tg target) {
	data := blob(32, 256<<10)
	tg.k.put(t, 2, data)
	s := tg.rp().store
	for _, how := range []struct {
		name string
		err  error
	}{{"unexpected EOF", io.ErrUnexpectedEOF}, {"a clean EOF", io.EOF}, {"a read error", errors.New("disk read failed")}} {
		const at = 100_000
		s.set(func(s *segStore) {
			s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
				if offset == 0 {
					return &cut{data: data[:at], err: how.err}, nil
				}
				return &cut{err: how.err}, nil // Nothing more, however it is reopened.
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), within)
		aborted(t, "ended by "+how.name, tg.get(ctx, 2), data)
		heal(s)
		whole(t, "after the object was whole, ended by "+how.name, tg.get(ctx, 2), data)
		cancel()
	}
}

// A store failure that is not "not found" before anything is sent: an error on the route as today,
// nothing answered offline, no job either way.
func storeFails(t *testing.T, tg target) {
	data := blob(33, 64<<10)
	for _, c := range []struct {
		name string
		set  func(*segStore)
	}{
		{"Stat fails", func(s *segStore) { s.statErr = func(string) error { return errors.New("stat failed") } }},
		{"Stat times out", func(s *segStore) { s.statErr = func(string) error { return context.DeadlineExceeded } }},
		{"Open fails", func(s *segStore) {
			s.open = func(context.Context, string, int64) (io.ReadCloser, error) { return nil, errors.New("open failed") }
		}},
		{"Open times out", func(s *segStore) {
			s.open = func(context.Context, string, int64) (io.ReadCloser, error) { return nil, context.DeadlineExceeded }
		}},
	} {
		tg.k.put(t, 2, data)
		tg.rp().store.set(c.set)
		if tg.via == "route" {
			ctx, cancel := context.WithTimeout(context.Background(), within)
			r := tg.get(ctx, 2)
			cancel()
			if r.err != nil || r.status < 500 {
				t.Errorf("%s: answered %d with %d bytes (%v), want an error status", c.name, r.status, len(r.body), r.err)
			}
		} else {
			_, cancel := tg.waits(t, 2)
			cancel()
		}
		if jobs := scalar[int](t, tg.k.pool, "SELECT count(*)::int FROM transcode_jobs"); jobs != 0 {
			t.Errorf("%s: %d jobs were started; a store failure is not a segment that is not there yet", c.name, jobs)
		}
		tg.rp().store.reset()
	}
}

// An object replaced after its size was read is one object whole under its own length, or aborted: never
// the new object's bytes cut to the old length.
func replacedAfterSize(t *testing.T, tg target) {
	old := blob(34, 256<<10)
	for _, size := range []int{100_000, 400_000} {
		replacement := blob(35, size)
		tg.k.put(t, 2, old)
		var swapped atomic.Bool
		tg.rp().store.set(func(s *segStore) {
			s.afterStat = func(key string) {
				if swapped.CompareAndSwap(false, true) {
					tg.k.put(t, 2, replacement)
				}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), within)
		r := tg.get(ctx, 2)
		cancel()
		switch {
		case r.err == nil && r.status == 200 && r.length == int64(len(old)) && bytes.Equal(r.body, old):
		case r.err == nil && r.status == 200 && r.length == int64(len(replacement)) && bytes.Equal(r.body, replacement):
		case r.err != nil && r.status == 200 && int64(len(r.body)) < r.length:
		default:
			t.Errorf("object replaced by %d bytes after its size was read: status %d, Content-Length %d, %d bytes, error %v; want one object whole or an aborted response", size, r.status, r.length, len(r.body), r.err)
		}
		tg.rp().store.reset()
	}
}

// A response that ends closes what it opened, whether it was complete or aborted.
func closedWhenDone(t *testing.T, tg target) {
	data := blob(36, 256<<10)
	tg.k.put(t, 2, data)
	s := tg.rp().store
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	whole(t, "complete", tg.get(ctx, 2), data)
	noneOpen(t, "after a complete response", s)
	s.set(func(s *segStore) {
		s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
			return &cut{data: data[:100_000*(1-min(int(offset), 1))], err: io.ErrUnexpectedEOF}, nil
		}
	})
	tg.get(ctx, 2) // Aborted.
	noneOpen(t, "after an aborted response", s)
}

// A viewer who goes away mid-segment ends the read from the store, and what was opened is closed.
func leavesMidSegment(t *testing.T, tg target) {
	data := blob(37, 256<<10)
	tg.k.put(t, 2, data)
	s := tg.rp().store
	release, ended := make(chan struct{}), make(chan struct{})
	s.set(func(s *segStore) {
		s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
			return newGate(ctx, data, offset, 8192, release, ended), nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	response, err := askSeg(ctx, tg.base(), tg.k.url(2))
	if err != nil {
		t.Fatalf("nothing was sent while the store held back the rest: %v", err)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	response.Body.Close() // The viewer leaves.
	select {
	case <-ended:
	case <-time.After(within):
		t.Fatal("the read from the store was still waiting a minute after the viewer left")
	}
	noneOpen(t, "after the viewer left", s)
}

// A segment already in the store costs at most one Stat and one Open of the store when no body is cut and
// nothing else (no List), and the offline path asks Postgres nothing.
func oneStatOneOpen(t *testing.T, tg target) {
	for _, n := range []int{-1, 2} {
		data := blob(int64(n)+40, 256<<10)
		tg.k.put(t, n, data)
		s := tg.rp().store
		s.reset()
		before, accepted := tg.rp().proxy.Received(), tg.rp().proxy.Accepted()
		ctx, cancel := context.WithTimeout(context.Background(), within)
		whole(t, fmt.Sprintf("segment %d", n), tg.get(ctx, n), data)
		cancel()
		if s.stats.Load() > 1 || s.opens.Load() > 1 || s.lists.Load() != 0 {
			t.Errorf("segment %d: the store had %d Stat, %d Open and %d List requests, want at most 1, 1 and 0", n, s.stats.Load(), s.opens.Load(), s.lists.Load())
		}
		if tg.via == "offline" && (tg.rp().proxy.Received() != before || tg.rp().proxy.Accepted() != accepted) {
			t.Errorf("segment %d: the offline path asked Postgres", n)
		}
	}
}
