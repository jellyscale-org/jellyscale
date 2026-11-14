//go:build integration

package api

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"jellyscale/internal/transcode"
)

// JS-077. A request waiting for a segment that is not made yet.

// segmentNumbers are the numbers of the media segments of the film's playlist, lowest first.
func (k *segKit) segmentNumbers() []int {
	var ns []int
	for n := range k.f.segment {
		if n >= 0 {
			ns = append(ns, n)
		}
	}
	sort.Ints(ns)
	return ns
}

// A request waiting for a segment makes no store request of any kind after its first look, over a wait of at
// least 5 seconds in which three other segments of the spec are stored, recorded and hinted. It looks
// again when its job is cancelled, and once its own segment is stored and recorded it is answered, with at
// most one Stat and one Open of the store after the window.
func TestWaitingRequestAsksTheStoreNothingUntilItsSegmentIsStored(t *testing.T) {
	k := newSegKit(t)
	k.reset(t)
	ns := k.segmentNumbers()
	if len(ns) < 5 {
		t.Fatalf("the film has %d segments, the check needs 5", len(ns))
	}
	n := ns[len(ns)-1]
	s := k.remote.store
	ctx, cancel := context.WithTimeout(context.Background(), 4*within)
	defer cancel()
	answer := make(chan reply, 1)
	go func() { answer <- fetchSeg(ctx, k.remote.server.URL, k.url(n)) }()
	waitLong(t, "the waiting request's job and its first look at the store", func() bool {
		return scalar[int](t, k.pool, "SELECT count(*)::int FROM transcode_jobs") >= 1 && s.requests() >= 1
	})
	time.Sleep(time.Second) // Whatever else the first look does is done.
	mark := s.requests()

	// Over 5 seconds, three other segments of the spec are stored, recorded and hinted.
	for i := range 3 {
		k.put(t, ns[i], blob(int64(i), 4096))
		k.announce(t, "other", ns[i], true)
		time.Sleep(1800 * time.Millisecond)
	}
	if during := s.requests() - mark; during != 0 {
		t.Errorf("the waiting request made %d store requests (%d Stat, %d Open, %d List in all) over a wait of 5 seconds in which other segments of its spec were stored and hinted, want none", during, s.stats.Load(), s.opens.Load(), s.lists.Load())
	}
	select {
	case r := <-answer:
		t.Fatalf("answered (status %d, %d bytes) before its segment was stored", r.status, len(r.body))
	default:
	}

	// Its job is cancelled: it looks once more, as it does today, and goes on waiting.
	mustExec(t, k.pool, "UPDATE transcode_jobs SET state = 'cancelled' WHERE state = 'queued'")
	transcode.Notify(ctx, k.pool, "segment:"+k.f.hash)
	waitLong(t, "the request to look once more when its job was cancelled", func() bool { return s.requests() > mark })

	data := blob(99, 128<<10)
	s.stats.Store(0)
	s.opens.Store(0)
	k.put(t, n, data)
	k.announce(t, "other", n, true)
	whole(t, "the waiting request", mustAnswer(t, "the waiting request", answer), data)
	if s.stats.Load() > 2 || s.opens.Load() > 1 {
		t.Errorf("once its segment was stored the request made %d Stat and %d Open requests, want at most one Open and the Stat of the look", s.stats.Load(), s.opens.Load())
	}
}

// A waiting request is answered once its segment is stored and recorded, whichever job stored it (the one
// it waits on, another viewer's, a helper's), several waiters of one segment at once and waiters on two
// replicas, with the hint sent and with every hint lost (invariant 10: the poll of Postgres answers it).
func TestWaitersAreAnsweredWhicheverJobStoredTheSegment(t *testing.T) {
	k := newSegKit(t)
	ns := k.segmentNumbers()
	n := ns[len(ns)-1]
	for _, kind := range []string{"own", "other", "helper"} {
		for _, hint := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s job, hint sent %v", kind, hint), func(t *testing.T) {
				k.reset(t)
				ctx, cancel := context.WithTimeout(context.Background(), 4*within)
				defer cancel()
				onRemote, onSecond := 3, 2
				answers := make(chan reply, onRemote+onSecond)
				var started sync.WaitGroup
				for i := range onRemote + onSecond {
					rp := k.remote
					if i >= onRemote {
						rp = k.second
					}
					started.Add(1)
					go func() {
						started.Done()
						answers <- fetchSeg(ctx, rp.server.URL, k.url(n))
					}()
				}
				started.Wait()
				waitLong(t, "a job and a first look on each replica", func() bool {
					return scalar[int](t, k.pool, "SELECT count(*)::int FROM transcode_jobs") >= 1 && k.remote.store.requests() >= 1 && k.second.store.requests() >= 1
				})
				time.Sleep(time.Second) // Every waiter is in its wait.
				data := blob(int64(len(kind))+50, 192<<10)
				k.put(t, n, data)
				k.announce(t, kind, n, hint)
				for i := range onRemote + onSecond {
					whole(t, fmt.Sprintf("waiter %d", i), mustAnswer(t, "a waiter", answers), data)
				}
			})
		}
	}
}
