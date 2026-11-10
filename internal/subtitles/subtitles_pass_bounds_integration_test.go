//go:build integration

package subtitles

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"jellyscale/internal/store"
)

// stalledPut is a store whose writes wait for their context to end.
type stalledPut struct{ store.Store }

func (stalledPut) Put(ctx context.Context, key string, body io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}

// A store write that never returns must not hold the pass past its limit, as
// the one write of a track read didn't.
func TestFillTextBoundsTheStoreWritesByTheLimit(t *testing.T) {
	dir, src := threeTrackFilm(t, 0)
	done := make(chan error, 1)
	go func() {
		done <- FillText(context.Background(), src, store.NewPosix(dir), stalledPut{store.NewPosix(t.TempDir())}, 2*time.Second, slog.New(slog.DiscardHandler))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("FillText = %v, want the deadline", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("FillText still running 15s after a 2s limit; the store write holds the pass")
	}
}

// ffmpeg's error holds the file's path, so a file named like one of its lines
// must not make the pass drop streams it doesn't have, or repeat forever.
func TestFillTextIgnoresStreamsNamedByTheFilePath(t *testing.T) {
	for _, name := range []string{
		"a Failed to set value '0:99' for option 'map'.mkv",
	} {
		t.Run(name, func(t *testing.T) {
			dir, src := threeTrackFilm(t, 0)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o644); err != nil {
				t.Fatal(err)
			}
			src.Key, src.Size = name, 4
			done := make(chan error, 1)
			go func() {
				done <- FillText(context.Background(), src, store.NewPosix(dir), store.NewPosix(t.TempDir()), 0, slog.New(slog.DiscardHandler))
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrBadFile) {
					t.Errorf("FillText = %v, want ErrBadFile", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("FillText still looping after 10s")
			}
		})
	}
}
