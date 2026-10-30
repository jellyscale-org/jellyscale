package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Reader adapts a Store object to io.ReadSeeker. Seeking only records a
// position; the request happens on the next Read. So a HEAD or a client that
// seeks before reading never touches the store, provided the caller sets
// Content-Type, because http.ServeContent otherwise reads to sniff one.
type Reader struct {
	ctx    context.Context
	store  Store
	key    string
	size   int64
	pos    int64
	stream io.ReadCloser
}

// NewReader returns a Reader for an object of known size.
func NewReader(ctx context.Context, s Store, key string, size int64) *Reader {
	return &Reader{ctx: ctx, store: s, key: key, size: size}
}

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.stream == nil {
		stream, err := r.store.Open(r.ctx, r.key, r.pos)
		if err != nil {
			return 0, err
		}
		r.stream = stream
	}
	n, err := r.stream.Read(p)
	r.pos += int64(n)
	return n, err
}

// Seek moves the position, dropping any open stream that no longer lines up.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var target int64
	switch whence {
	case io.SeekStart:
		target = offset
	case io.SeekCurrent:
		target = r.pos + offset
	case io.SeekEnd:
		target = r.size + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if target < 0 {
		return 0, errors.New("negative position")
	}
	if target != r.pos {
		r.closeStream()
	}
	r.pos = target
	return target, nil
}

func (r *Reader) closeStream() {
	if r.stream != nil {
		r.stream.Close()
		r.stream = nil
	}
}

// Close releases any open stream.
func (r *Reader) Close() error {
	r.closeStream()
	return nil
}

// Input gives ffmpeg and ffprobe something to open. A filesystem object is its
// path. An S3 object goes through a loopback server, which keeps credentials
// out of argv and survives a long read, where a presigned URL would expire.
// The random path keeps other local processes out.
func Input(ctx context.Context, s Store, key string, size int64) (string, func(), error) {
	if path, ok := s.LocalPath(key); ok {
		return path, func() {}, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		listener.Close()
		return "", nil, err
	}
	path := "/" + hex.EncodeToString(token)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != path {
				http.NotFound(w, r)
				return
			}
			reader := NewReader(r.Context(), s, key, size)
			defer reader.Close()
			// Set, or ServeContent reads the object to sniff a type.
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "", time.Time{}, reader)
		}),
	}
	var once sync.Once
	go server.Serve(listener) //nolint:errcheck
	closeInput := func() { once.Do(func() { server.Close() }) }
	go func() {
		<-ctx.Done()
		closeInput()
	}()
	return "http://" + listener.Addr().String() + path, closeInput, nil
}
