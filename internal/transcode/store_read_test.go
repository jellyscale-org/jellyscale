package transcode

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
)

// endingStore opens every key and gives a body that ends early, as a store does whose
// connection drops in the middle of an object.
type endingStore struct{ store.Store }

type endingBody struct{ sent bool }

func (b *endingBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.ErrUnexpectedEOF
	}
	b.sent = true
	return copy(p, "half of it"), nil
}

func (endingStore) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(&endingBody{}), nil
}

// JS-107: a copy out of a store has two ends too. A read of the store that ends early
// (io.ErrUnexpectedEOF) is the store's failure and names it; the local file that could
// not be written is not the store's.
func TestAStoreReadThatEndsEarlyNamesTheStoreAndALocalWriteThatFailsDoesNot(t *testing.T) {
	ctx := context.Background()
	copies := map[string]func(dir string) error{
		"the image subtitle file": func(dir string) error {
			_, err := fetchImageFile(ctx, endingStore{}, mediaStore, dir, playback.Stream{Path: "film.sup"})
			return err
		},
		"a font": func(dir string) error {
			return copyFont(ctx, endingStore{}, "sum", filepath.Join(dir, "font"), mediaStore)
		},
	}
	for name, copyTo := range copies {
		t.Run(name+", the store's read ends early", func(t *testing.T) {
			state, reason := endState(ctx, copyTo(t.TempDir()))
			if state != "failed" || !strings.Contains(reason, mediaStore) {
				t.Errorf("ended %s %q, want failed with %s", state, reason, mediaStore)
			}
		})
		t.Run(name+", the local file cannot be made", func(t *testing.T) {
			state, reason := endState(ctx, copyTo(filepath.Join(t.TempDir(), "no such directory")))
			if state != "failed" || strings.Contains(reason, mediaStore) {
				t.Errorf("ended %s %q, want failed without %s, which did not fail", state, reason, mediaStore)
			}
		})
	}
}
