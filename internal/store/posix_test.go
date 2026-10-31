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

func TestPosixDeleteStaysInsideRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	secret := filepath.Join(parent, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewPosix(root).Delete(context.Background(), "../secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("delete reached outside the root: %v", err)
	}
}

func TestPosixDeleteRemovesEmptyDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := NewPosix(root)
	for _, key := range []string{"specs/a/1", "specs/a/2"} {
		if err := s.Put(ctx, key, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	s.Delete(ctx, "specs/a/1") //nolint:errcheck
	if _, err := os.Stat(filepath.Join(root, "specs/a")); err != nil {
		t.Fatalf("directory with a file left was removed: %v", err)
	}
	s.Delete(ctx, "specs/a/2") //nolint:errcheck
	if _, err := os.Stat(filepath.Join(root, "specs/a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty directory remains: %v", err)
	}
}

func TestPosixDeleteKeepsTheRoot(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "root")
	s := NewPosix(root)
	if err := s.Put(ctx, "only.bin", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "only.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("deleting the last file removed the root: %v", err)
	}
}
