//go:build integration

package transcode

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
	"jellyscale/internal/testenv"
)

// failNthOpen fails the n-th Open of a stored subtitle track, as a store that
// doesn't answer once would. Only the worker's check of what it stored opens one.
type failNthOpen struct {
	store.Store
	n     int32
	count *atomic.Int32
}

func (f *failNthOpen) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if strings.HasPrefix(key, "subtitles/") && strings.HasSuffix(key, ".json") && f.count.Add(1) == f.n {
		return nil, errors.New("injected: the store did not answer")
	}
	return f.Store.Open(ctx, key, offset)
}

// A store that fails once when the worker checks what the pass stored is not
// the track's fault: no good track is hidden, whichever check it fails.
func TestAStoreErrorAfterThePassHidesNoGoodTrack(t *testing.T) {
	testenv.FFmpeg(t)
	for _, n := range []int32{1, 2, 3} {
		t.Run(string(rune('0'+n)), func(t *testing.T) {
			r := newRig(t, nil)
			source, assets := r.textTracks(t)
			var cache store.Cache
			var count atomic.Int32
			stop := r.worker(t, func(w *Worker) {
				w.Media = func(uri string) (store.Store, error) {
					s, err := cache.Open(uri)
					if err != nil {
						return nil, err
					}
					return &failNthOpen{Store: s, n: n, count: &count}, nil
				}
			})
			defer stop()
			// The job may fail on the error; the tracks are stored all the same.
			r.pick(t, r.jobs(t), assets, source, 2) //nolint:errcheck
			if count.Load() < n {
				t.Fatal("the injected failure was never used")
			}
			src := r.source(t)
			for _, index := range []int{2, 3, 4} {
				if subtitles.Unreadable(src, index) {
					t.Errorf("track %d is good but is marked unreadable after one store error", index)
				}
			}
		})
	}
}
