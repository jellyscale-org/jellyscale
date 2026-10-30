package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Posix stores objects as files beneath a root directory.
type Posix struct {
	root string
}

// NewPosix returns a Store rooted at root.
func NewPosix(root string) *Posix {
	return &Posix{root: root}
}

func (p *Posix) resolve(key string) (string, error) {
	cleaned := path.Clean("/" + key)
	if cleaned == "/" || strings.Contains(key, "\x00") {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return filepath.Join(p.root, filepath.FromSlash(cleaned)), nil
}

// List walks the tree. The ETag combines size and mtime, which survives a
// rename, so a moved file can keep its item.
func (p *Posix) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	root := filepath.Join(p.root, filepath.FromSlash(prefix))
	err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(p.root, file)
		if err != nil {
			return err
		}
		objects = append(objects, Object{
			Key:  filepath.ToSlash(rel),
			Size: info.Size(),
			ETag: fmt.Sprintf("%x-%x", info.Size(), info.ModTime().UnixNano()),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", root, err)
	}
	slices.SortFunc(objects, func(a, b Object) int { return strings.Compare(a.Key, b.Key) })
	return objects, nil
}

// Open reads key from offset to the end of the file.
func (p *Posix) Open(_ context.Context, key string, offset int64) (io.ReadCloser, error) {
	file, err := p.resolve(key)
	if err != nil {
		return nil, err
	}
	handle, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := handle.Seek(offset, io.SeekStart); err != nil {
		handle.Close()
		return nil, err
	}
	return handle, nil
}

// Put writes to a temporary file and renames it, so readers on other replicas
// never see a partial object.
func (p *Posix) Put(_ context.Context, key string, body io.Reader) error {
	file, err := p.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".put-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := io.Copy(temp, body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file)
}

// LocalPath returns the file behind key.
func (p *Posix) LocalPath(key string) (string, bool) {
	file, err := p.resolve(key)
	return file, err == nil
}
