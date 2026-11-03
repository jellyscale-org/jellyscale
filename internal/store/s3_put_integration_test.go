//go:build integration

package store

import (
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A body that can seek, such as a font on disk, goes to S3 without being held
// in memory, and arrives whole; one that can't is still stored.
func TestS3PutStreamsAFile(t *testing.T) {
	const size = 48 << 20
	path := filepath.Join(t.TempDir(), "font")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(file, rand.Reader, size); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for name, s := range s3Backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			body, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if err := s.Put(ctx, "streamed", body); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			// Buffering the body allocates at least its size.
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > size/2 {
				t.Errorf("putting a %d MB file allocated %d MB", size>>20, allocated>>20)
			}
			got, err := s.Open(ctx, "streamed", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer got.Close()
			n, err := io.Copy(io.Discard, got)
			if err != nil || n != size {
				t.Errorf("read back %d bytes: %v", n, err)
			}
			if err := s.Put(ctx, "buffered", io.MultiReader(io.LimitReader(rand.Reader, 1000))); err != nil {
				t.Errorf("a body that can't seek: %v", err)
			}
		})
	}
}
