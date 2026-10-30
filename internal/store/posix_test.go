package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPosixPutListOpen(t *testing.T) {
	ctx := context.Background()
	s := NewPosix(t.TempDir())
	if err := s.Put(ctx, "b/two.txt", strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a/one.txt", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}

	objects, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[0].Key != "a/one.txt" || objects[1].Key != "b/two.txt" {
		t.Fatalf("unexpected listing: %+v", objects)
	}
	if objects[0].Size != 5 || objects[0].ETag == "" {
		t.Fatalf("missing metadata: %+v", objects[0])
	}

	reader, err := s.Open(ctx, "b/two.txt", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ond" {
		t.Fatalf("got %q", body)
	}
}

func TestPosixOpenMissing(t *testing.T) {
	_, err := NewPosix(t.TempDir()).Open(context.Background(), "missing", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

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
