package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// JS-077. These checks name Stat(ctx, key) (size int64, err error) on Store, which the task implies and
// nothing has yet, so they do not compile until the build adds it.

// Stat gives an object's size on every backend, a missing key is ErrNotFound, an empty object is 0 and
// not missing, a replaced object has its new size, and a key that leaves the root is refused.
func TestConformanceStat(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			put := func(key string, size int) {
				t.Helper()
				if err := s.Put(ctx, key, bytes.NewReader(bytes.Repeat([]byte("x"), size))); err != nil {
					t.Fatal(err)
				}
			}
			put("seg/big.m4s", 100_000)
			if size, err := s.Stat(ctx, "seg/big.m4s"); err != nil || size != 100_000 {
				t.Fatalf("Stat of a 100000 byte object: %d, %v", size, err)
			}
			if _, err := s.Stat(ctx, "seg/missing.m4s"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Stat of a missing key: %v, want ErrNotFound", err)
			}
			put("seg/empty.m4s", 0)
			if size, err := s.Stat(ctx, "seg/empty.m4s"); err != nil || size != 0 {
				t.Fatalf("Stat of an empty object: %d, %v, want 0 and no error", size, err)
			}
			put("seg/big.m4s", 7)
			if size, err := s.Stat(ctx, "seg/big.m4s"); err != nil || size != 7 {
				t.Fatalf("Stat after a replace by 7 bytes: %d, %v", size, err)
			}
			for _, key := range []string{"../outside", "seg/../../outside", "../../outside"} {
				if size, err := s.Stat(ctx, key); err == nil {
					t.Fatalf("Stat(%q) = %d, nil: a key that leaves the root is refused", key, size)
				}
			}
		})
	}
}

// On a filesystem the refusal has a file to be fooled by: one beside the root.
func TestPosixStatStaysInsideRoot(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "secret"), []byte("twelve bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewPosix(filepath.Join(parent, "root"))
	if size, err := s.Stat(context.Background(), "../secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat(../secret) = %d, %v, want ErrNotFound", size, err)
	}
}

// Stat on S3 asks for the size alone: a HEAD, never a GET, so no body is transferred.
func TestS3StatSendsNoGet(t *testing.T) {
	var heads, gets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
			if strings.Contains(r.URL.Path, "gone") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", "4242")
			w.Header().Set("ETag", `"e"`)
		default:
			gets.Add(1)
			w.Header().Set("Content-Length", "4242")
			w.Write(bytes.Repeat([]byte("x"), 4242)) //nolint:errcheck
		}
	}))
	defer server.Close()
	t.Setenv("JELLYSCALE_S3_ACCESS_KEY", "k")
	t.Setenv("JELLYSCALE_S3_SECRET_KEY", "s")
	parsed, _ := url.Parse("s3://bucket/p?endpoint=" + server.URL)
	s, err := NewS3(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if size, err := s.Stat(context.Background(), "present"); err != nil || size != 4242 {
		t.Fatalf("Stat = %d, %v, want 4242", size, err)
	}
	if _, err := s.Stat(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat of a key the server answers 404: %v, want ErrNotFound", err)
	}
	if gets.Load() != 0 || heads.Load() != 2 {
		t.Fatalf("Stat sent %d GETs and %d HEADs, want 0 and 2", gets.Load(), heads.Load())
	}
}

// A server that fails is an error that is not ErrNotFound: the caller must not take it for a key that is
// not there yet.
func TestS3StatFailureIsNotNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("JELLYSCALE_S3_ACCESS_KEY", "k")
	t.Setenv("JELLYSCALE_S3_SECRET_KEY", "s")
	parsed, _ := url.Parse(fmt.Sprintf("s3://bucket?endpoint=%s", server.URL))
	s, err := NewS3(parsed)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // Bounds the SDK's retries: a hang.
	defer cancel()
	if _, err := s.Stat(ctx, "k"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat against a server that answers 500: %v, want an error that is not ErrNotFound", err)
	}
}
