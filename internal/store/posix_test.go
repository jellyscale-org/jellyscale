package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPosixKeysStayInsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	secret := filepath.Join(parent, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPosix(root).Open(context.Background(), "../secret", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("traversal should miss, got %v", err)
	}
}

func TestPosixListKeepsETagAcrossRename(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewPosix(dir)
	if err := s.Put(ctx, "old.mkv", strings.NewReader("video")); err != nil {
		t.Fatal(err)
	}
	before, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "old.mkv"), filepath.Join(dir, "new.mkv")); err != nil {
		t.Fatal(err)
	}
	after, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if before[0].ETag != after[0].ETag {
		t.Fatal("rename changed the ETag")
	}
}
