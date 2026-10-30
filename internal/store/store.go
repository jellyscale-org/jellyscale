// Package store abstracts where media and generated assets live, so a library
// can be backed by a filesystem or an S3-compatible bucket.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
)

// ErrNotFound reports a key that doesn't exist.
var ErrNotFound = errors.New("object not found")

// Object describes one stored object. ETag changes when the content does.
type Object struct {
	Key  string
	Size int64
	ETag string
}

// Store is the subset of object semantics both backends can honor.
type Store interface {
	// List returns every object under prefix, ordered by key.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Open reads an object from offset to its end.
	Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error)
	// Put writes an object, replacing any existing one.
	Put(ctx context.Context, key string, body io.Reader) error
	// LocalPath returns a filesystem path for key, if the backend has one.
	LocalPath(key string) (string, bool)
}

// Open returns the Store for a URI such as file:///media/movies or
// s3://bucket/prefix?endpoint=https://s3.example.com&region=auto.
// S3 credentials come from the environment, never the URI, because URIs are
// stored in the database.
func Open(uri string) (Store, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("parse store uri: %w", err)
	}
	switch parsed.Scheme {
	case "file":
		return NewPosix(parsed.Path), nil
	case "s3":
		return NewS3(parsed)
	default:
		return nil, fmt.Errorf("unsupported store scheme %q", parsed.Scheme)
	}
}
