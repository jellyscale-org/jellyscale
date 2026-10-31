package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
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
	// resumes counts bodies that ended early since one last delivered bytes.
	resumes int
}

// maxResumes bounds how often a read reopens after a body ended early without
// progress, so a truncated object fails instead of looping.
const maxResumes = 3

// NewReader returns a Reader for an object of known size. math.MaxInt64 reads
// to the end of an object whose size isn't known, such as a segment.
func NewReader(ctx context.Context, s Store, key string, size int64) *Reader {
	return &Reader{ctx: ctx, store: s, key: key, size: size}
}

func (r *Reader) Read(p []byte) (int, error) {
	for {
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
		if n > 0 {
			r.resumes = 0
		}
		// An empty 200 to a resumed GET ends cleanly, short of the size.
		if err == io.EOF && r.pos < r.size && r.size != math.MaxInt64 {
			err = io.ErrUnexpectedEOF
		}
		if err != io.ErrUnexpectedEOF || r.pos >= r.size {
			return n, err
		}
		// SeaweedFS cuts a GET when another GET of the object is dropped, and
		// S3.Open reports that as ErrUnexpectedEOF: carry on from here.
		r.closeStream()
		if r.resumes++; r.resumes > maxResumes {
			return n, io.ErrUnexpectedEOF
		}
		if n > 0 {
			return n, nil
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
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
	done := make(chan struct{})
	go server.Serve(listener) //nolint:errcheck
	closeInput := func() {
		once.Do(func() {
			close(done)
			server.Close()
		})
	}
	// ctx can outlive the input by far, so this must end when the input does.
	go func() {
		select {
		case <-ctx.Done():
			closeInput()
		case <-done:
		}
	}()
	return "http://" + listener.Addr().String() + path, closeInput, nil
}
