//go:build integration

package api

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// generated is a segment of any size that is never held: byte i of it is a function of i, so that what a
// viewer is sent is checked without the test keeping the object either.
type generated struct {
	size, pos int64
	served    *atomic.Int64
}

func patternAt(i int64) byte { return byte(i*7 + i>>9) }

func (g *generated) Read(p []byte) (int, error) {
	if g.pos >= g.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), g.size-g.pos))
	for i := range n {
		p[i] = patternAt(g.pos + int64(i))
	}
	g.pos += int64(n)
	g.served.Add(int64(n))
	return n, nil
}
func (*generated) Close() error { return nil }

// At least 16 viewers are each mid-segment at once, on copy segments of 16 MiB, through the stream role's
// segment route and a store without a local path, each having received part of its segment and stopped
// reading. The process's peak heap growth stays under the viewers times the per-viewer bound (a sixteenth
// of the segment, whatever its size). A build that holds a segment whole holds 256 MiB; one that reads
// ahead of a viewer who has stopped reads on until the whole segment is held, and so does the same.
func TestStreamMemoryDoesNotGrowWithViewersOrSegments(t *testing.T) {
	k := newSegKit(t)
	k.reset(t)
	var numbers []int
	for n := range k.f.segment {
		if n >= 0 {
			numbers = append(numbers, n)
		}
	}
	var served atomic.Int64
	rp := k.remote
	sizes := map[string]int64{}
	for _, n := range numbers {
		sizes[k.key(n)] = memorySegmentSize
	}
	rp.store.set(func(s *segStore) {
		s.synth = sizes
		s.open = func(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
			return &generated{size: memorySegmentSize, pos: offset, served: &served}, nil
		}
	})

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stopSampling := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stopSampling:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*within)
	defer cancel()
	const held = 64 << 10            // What each viewer reads before it stops.
	var got, finished sync.WaitGroup // got: mid-segment; finished: gone.
	release := make(chan struct{})
	var wrong atomic.Int64
	for v := range memoryViewers {
		n := numbers[v%len(numbers)]
		got.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			reached := false
			defer func() {
				if !reached {
					got.Done()
				}
			}()
			response, err := askSeg(ctx, rp.server.URL, k.url(n))
			if err != nil {
				wrong.Add(1)
				t.Errorf("viewer %d: no response: %v", v, err)
				return
			}
			defer response.Body.Close()
			first := make([]byte, held)
			if _, err := io.ReadFull(response.Body, first); err != nil {
				wrong.Add(1)
				t.Errorf("viewer %d: %v", v, err)
				return
			}
			want := make([]byte, held)
			for i := range want {
				want[i] = patternAt(int64(i))
			}
			if !bytes.Equal(first, want) || response.ContentLength != memorySegmentSize {
				wrong.Add(1)
				t.Errorf("viewer %d: the first bytes were not the object's, or Content-Length %d", v, response.ContentLength)
			}
			reached = true
			got.Done() // Mid-segment: it has stopped reading, and stays until released.
			<-release
		}()
	}
	got.Wait()
	// Wait until the replica has stopped reading from the store: what it will hold is held.
	var last int64 = -1
	for stable := 0; stable < 4; {
		time.Sleep(300 * time.Millisecond)
		if now := served.Load(); now == last {
			stable++
		} else {
			stable, last = 0, now
		}
	}
	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)
	if live.HeapAlloc > peak.Load() {
		peak.Store(live.HeapAlloc)
	}
	close(stopSampling)
	<-sampled
	close(release)
	finished.Wait()

	growth := int64(peak.Load()) - int64(base.HeapAlloc)
	ceiling := int64(memoryViewers) * segmentMemoryPerViewer
	t.Logf("%d viewers mid-segment of %d MiB segments: the store gave %d MiB, peak heap growth %d KiB, ceiling %d KiB", memoryViewers, memorySegmentSize>>20, served.Load()>>20, growth>>10, ceiling>>10)
	if growth > ceiling {
		t.Errorf("peak heap growth %d KiB with %d viewers mid-segment, want under %d KiB (%d KiB per viewer, whatever the segment's size)", growth>>10, memoryViewers, ceiling>>10, segmentMemoryPerViewer>>10)
	}
	noneOpen(t, "after the viewers left", rp.store)
}
