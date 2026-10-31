// Package store abstracts where media and generated assets live, so a library
// can be backed by a filesystem or an S3-compatible bucket.
package store

import (
	"context"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
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
	// Delete removes an object. A missing one is not an error, so a repeat or a
	// race between two sweepers is harmless.
	Delete(ctx context.Context, key string) error
	// LocalPath returns a filesystem path for key, if the backend has one.
	LocalPath(key string) (string, bool)
}

// credentialParam matches a parameter name that holds a credential.
var credentialParam = regexp.MustCompile(`(?i)[^?&;#/=]*(key|secret|access|token|pass|sig|credential)[^?&;#/=]*=`)

// refuseCredentials rejects a URI that carries credentials: userinfo, or a
// parameter named like a key, secret, password or token, wherever it hides:
// after ';' as well as '&', in the fragment, or in the query of a URL-valued
// parameter such as endpoint, plain or percent-encoded. The error never quotes
// the URI: it reaches logs.
func refuseCredentials(parsed *url.URL) error {
	rest := parsed.RawQuery + "&" + parsed.Fragment
	for range 2 { // Twice, for an encoded URL inside an encoded URL.
		if decoded, err := url.QueryUnescape(rest); err == nil {
			rest = decoded
		}
	}
	if parsed.User != nil || strings.Contains(rest, "@") || credentialParam.MatchString(rest) {
		return errors.New("a store URI must not carry credentials (a user, password, key, secret or token); S3 credentials come from the environment")
	}
	return nil
}

// Open returns the Store for a URI such as file:///media/movies or
// s3://bucket/prefix?endpoint=https://s3.example.com&region=auto.
// S3 credentials come from the environment, never the URI, because URIs are
// stored in the database and logged (invariant 12), so one that carries
// credentials is refused.
func Open(uri string) (Store, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		// The error would quote the URI.
		return nil, errors.New("store uri is not a valid URI")
	}
	if err := refuseCredentials(parsed); err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "file":
		return NewPosix(parsed.Path), nil
	case "s3":
		return NewS3(parsed)
	default:
		return nil, errors.New("unsupported store scheme: a store URI starts with file:// or s3://")
	}
}

// Cache opens each URI once. Stores hold HTTP clients and connection pools, so
// reopening per request would throw away every warm connection.
type Cache struct {
	opened sync.Map // URI -> Store
}

// Open returns the Store for uri, opening it on first use.
func (c *Cache) Open(uri string) (Store, error) {
	if cached, ok := c.opened.Load(uri); ok {
		return cached.(Store), nil
	}
	opened, err := Open(uri)
	if err != nil {
		return nil, err
	}
	cached, _ := c.opened.LoadOrStore(uri, opened)
	return cached.(Store), nil
}

// Put makes Open return s for uri, so tests can wrap a store.
func (c *Cache) Put(uri string, s Store) { c.opened.Store(uri, s) }
