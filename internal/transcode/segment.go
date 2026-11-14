package transcode

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"jellyscale/internal/store"
)

// chunk is what a viewer's response holds of its segment at a time. It is the
// whole of the stream role's memory per viewer, whatever the segment's size.
const chunk = 32 << 10

var errGrown = errors.New("the object is larger than when its size was read")

// StoredSegment is a segment the store holds, found and with its first bytes
// read, to be sent while the rest is read.
type StoredSegment struct {
	size   int64
	reader *store.Reader
	buf    []byte
	n      int // Bytes of buf not yet sent.
}

// openSegment finds a stored segment: store.ErrNotFound if it is absent, empty
// or cut before any of it could be read, which is not there yet. Any other
// failure of the store is returned as it is.
func openSegment(ctx context.Context, s store.Store, key string) (*StoredSegment, error) {
	size, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, store.ErrNotFound
	}
	// The Reader is not given the size, so that it can tell an object that has
	// grown since the size was read from one that ends where the size says.
	seg := &StoredSegment{size: size, reader: store.NewReader(ctx, s, key, math.MaxInt64), buf: make([]byte, chunk)}
	if seg.n, err = seg.read(); err != nil {
		seg.Close()
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return seg, nil
}

// read fills buf with at least one byte, or fails.
func (s *StoredSegment) read() (int, error) {
	for {
		n, err := s.reader.Read(s.buf)
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// Close releases the store's body.
func (s *StoredSegment) Close() { s.reader.Close() }

// Send writes the segment under a Content-Length that is its size. Its last
// byte is held back until the object is seen to end there, so a segment that
// cannot be made whole (an object cut short, or replaced by one of another
// size) returns an error with a byte unsent, and the client is left with a body
// that ended early, never a clean one.
func (s *StoredSegment) Send(w http.ResponseWriter, head bool) error {
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.FormatInt(s.size, 10))
	if head {
		return nil
	}
	flush := http.NewResponseController(w).Flush
	var sent int64
	for sent+int64(s.n) < s.size {
		if _, err := w.Write(s.buf[:s.n]); err != nil {
			return err
		}
		flush() //nolint:errcheck // A writer that cannot flush is only slower.
		sent += int64(s.n)
		var err error
		if s.n, err = s.read(); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
	}
	if sent+int64(s.n) > s.size {
		return errGrown
	}
	last := s.buf[s.n-1]
	if _, err := w.Write(s.buf[:s.n-1]); err != nil {
		return err
	}
	n, err := s.read()
	if n > 0 {
		return errGrown
	}
	if err != io.EOF {
		return err
	}
	_, err = w.Write([]byte{last})
	return err
}
