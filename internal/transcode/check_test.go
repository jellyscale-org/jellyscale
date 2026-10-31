package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"jellyscale/internal/store"
)

func TestCheckLast(t *testing.T) {
	const ticks = 100 * 10_000_000 // 25 segments of 4 s
	write := func(extinf string) string {
		dir := t.TempDir()
		text := "#EXTM3U\n#EXTINF:4.000000,\n23.m4s\n#EXTINF:" + extinf + ",\n24.m4s\n#EXT-X-ENDLIST\n"
		if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, c := range []struct {
		name    string
		n       int
		extinf  string
		failure string
	}{
		{"the real last segment", 24, "4.000000", ""},
		{"a last segment a frame short", 24, "3.958333", ""},
		{"cut short by a failed read", 24, "0.750000", "the last segment is 0.75s"},
		{"stopped before the end", 3, "4.000000", "stopped after segment 3 of 25"},
	} {
		err := checkLast(write(c.extinf), c.n, ticks)
		if c.failure == "" && err != nil || c.failure != "" && (err == nil || !strings.Contains(err.Error(), c.failure)) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.failure)
		}
	}
}

// cutStore's objects end early however often they are reopened.
type cutStore struct{ store.Store }

func (cutStore) Open(context.Context, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)), nil
}

// A segment still cut once the Reader gives up counts as not there yet, so the
// caller's wait reads it again rather than fail the request.
func TestCutSegmentIsNotThereYet(t *testing.T) {
	if _, err := readSegment(context.Background(), cutStore{}, "cut"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A segment is never empty, so an empty one is still being written, or was cut.
func TestEmptySegmentIsNotThereYet(t *testing.T) {
	ctx := context.Background()
	segments := store.NewPosix(t.TempDir())
	if err := segments.Put(ctx, "empty", strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSegment(ctx, segments, "empty"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
