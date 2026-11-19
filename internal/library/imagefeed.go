package library

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"sort"

	"jellyscale/internal/store"
)

// Feed is the stored image track (a Matroska file that holds that one track, at
// the film's times; see subtitles.ImageKey) from the picture showing at cut, as a
// stream for ffmpeg to read from a pipe: the file's head, then every cluster from
// the one that holds the picture's first packet. A run that begins mid-film is given
// that instead of the film from where the cue began, so a cue's age costs a few
// ranged reads of this file and not of the film. What it reads from the store
// follows what the reader takes, so a pipe whose reader waits (ffmpeg paused at its
// lead) reads no more. With nothing showing at the cut, the stream begins at the
// next packet (or the last, if none follows). ErrUnknownPicture says the track can't be read.
func (t ImageTrack) Feed(ctx context.Context, s store.Store, key string, size int64, cut float64) (io.ReadCloser, error) {
	file := indexFile{ctx: ctx, store: s, key: key, size: size, head: t.whole}
	from := size // A file with no packet is all head.
	var head []byte
	if len(t.Points) > 0 {
		ticks := int64(math.Floor(cut * 1e7))
		last := sort.Search(len(t.Points), func(i int) bool { return t.Points[i][0] > ticks }) - 1
		first, err := t.pictureStart(&file, last, ticks)
		if err != nil {
			return nil, err
		}
		if first < 0 {
			first = last + 1
		}
		// With no packet at or after the cut the stream still begins at one, the last: a
		// reader finds no track in a file with no cluster. It ended before the cut, or it
		// would be the picture showing.
		from = t.Points[min(first, len(t.Points)-1)][1]
		if head, err = file.at(0, t.Points[0][1]); err != nil {
			return nil, err
		}
	} else if head, _ = file.at(0, size); head == nil {
		return nil, fmt.Errorf("%w: the file holds no packet of the track", ErrUnknownPicture)
	}
	head, err := unboundedSegment(head)
	if err != nil {
		return nil, err
	}
	feed := &trackFeed{ctx: ctx, store: s, key: key, head: bytes.NewReader(head), from: from, size: size}
	if t.whole != nil {
		feed.body = io.NopCloser(bytes.NewReader(t.whole[from:]))
	}
	return feed, nil
}

// unboundedSegment gives the Segment of a file's head the size "unknown", which a
// reader of a stream that is not the whole file needs: it would find the file
// shorter than the Segment says and complain at its end.
func unboundedSegment(head []byte) ([]byte, error) {
	id, header, size, ok := ebml(head)
	if !ok || id != ebmlHeader || size < 0 || header+size >= int64(len(head)) {
		return nil, fmt.Errorf("%w: the file does not start with an EBML header", ErrUnknownPicture)
	}
	at := header + size
	id, header, _, ok = ebml(head[at:])
	if !ok || id != mkvSegment {
		return nil, fmt.Errorf("%w: no Segment follows the EBML header", ErrUnknownPicture)
	}
	length := header - 4 // The ID is four bytes.
	marker := byte(0x80) >> (length - 1)
	out := bytes.Clone(head)
	out[at+4] = marker | (marker - 1)
	for i := int64(1); i < length; i++ {
		out[at+4+i] = 0xFF
	}
	return out, nil
}

type trackFeed struct {
	ctx   context.Context
	store store.Store
	key   string
	head  *bytes.Reader
	from  int64
	size  int64
	body  io.ReadCloser
}

func (f *trackFeed) Read(p []byte) (int, error) {
	if f.head.Len() > 0 {
		return f.head.Read(p)
	}
	if f.body == nil {
		if f.from >= f.size {
			return 0, io.EOF
		}
		body, err := f.store.Open(f.ctx, f.key, f.from)
		if err != nil {
			return 0, err
		}
		f.body = body
	}
	return f.body.Read(p)
}

func (f *trackFeed) Close() error {
	if f.body == nil {
		return nil
	}
	return f.body.Close()
}
