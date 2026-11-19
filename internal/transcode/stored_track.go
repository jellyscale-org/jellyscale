package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"

	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// feedPiece is the most a read of the stored track asks of the store at once,
// so that a run that waits (ffmpeg paused at its lead, or held by its pipe)
// has read little more than ffmpeg took.
const feedPiece = 8 << 10

// storedTrack is an image track inside the film, stored in the asset store as a
// file of its own (JS-089), as a run that begins mid-film reads it.
type storedTrack struct {
	assets store.Store
	uri    string
	key    string
	size   int64
	cues   library.ImageTrack
}

// findStoredTrack looks for the stored copy of image track index of source.
// found is false for a track that is not stored and the film's own read
// draws as JS-071 does: not asked for yet, being extracted, failed or left out
// for the film's own reason. A track that was stored and is not there
// (the object is gone), or can't be read, is the store's failure, not a track
// without cues: a run that drew without it would store segments a run from zero
// does not make.
func (w *Worker) findStoredTrack(ctx context.Context, source items.Source, index int) (track storedTrack, found bool, err error) {
	assets, err := w.Media(source.AssetStoreURI)
	if err != nil {
		return track, false, fromStore(source.AssetStoreURI, err)
	}
	key := subtitles.ImageKey(source, index)
	size, err := assets.Stat(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		var state string
		var left []int32
		err := w.Pool.QueryRow(ctx, "SELECT state, skipped FROM subtitle_jobs WHERE key = $1", subtitles.ImagesKey(source)).Scan(&state, &left)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return track, false, nil
		case err != nil:
			return track, false, fromPool(err)
		case state == "done" && !slices.Contains(left, int32(index)):
			return track, false, fromStore(source.AssetStoreURI, errors.New("the stored image track is gone: "+key))
		}
		return track, false, nil
	}
	if err != nil {
		return track, false, fromStore(source.AssetStoreURI, err)
	}
	cues, err := library.ReadStoredTrack(ctx, assets, key, size)
	if err != nil {
		return track, false, fromStore(source.AssetStoreURI, err)
	}
	if len(cues.Points) == 0 {
		return track, false, nil // A track with no packet draws nothing, from the stored file or the film.
	}
	return storedTrack{assets, source.AssetStoreURI, key, size, cues}, true, nil
}

// trackRead is a run's read of the stored track, which ffmpeg takes through its
// standard input. A read that fails for the store's reason would otherwise end the
// input, and ffmpeg would go on and make segments with no cue, so the failed read
// waits for the run to end it (failed says the run must, and why).
type trackRead struct {
	ctx     context.Context
	failed  chan struct{}
	release chan struct{}
	once    sync.Once
	let     sync.Once
	mu      sync.Mutex
	err     error
	closers []io.Closer
}

func newTrackRead(ctx context.Context) *trackRead {
	return &trackRead{ctx: ctx, failed: make(chan struct{}), release: make(chan struct{})}
}

// open is the track's stream from the picture showing at t, as the read end of a
// pipe that a goroutine fills from the store as ffmpeg takes it. A pipe's own buffer
// is what is read ahead, which is why it is a page (shrinkPipe).
func (r *trackRead) open(track storedTrack, t float64) (io.ReadCloser, error) {
	body, err := track.cues.Feed(r.ctx, track.assets, track.key, track.size, t)
	if err != nil {
		return nil, fromStore(track.uri, err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		body.Close()
		return nil, err
	}
	shrinkPipe(writer)
	r.mu.Lock()
	r.closers = []io.Closer{body, reader, writer}
	r.mu.Unlock()
	go func() {
		defer writer.Close()
		piece := make([]byte, feedPiece)
		for {
			n, err := body.Read(piece)
			if n > 0 {
				if _, werr := writer.Write(piece[:n]); werr != nil {
					return // ffmpeg is gone, or the run ended.
				}
			}
			switch {
			case err == io.EOF:
				return // The end of the track is the end of the input.
			case err != nil && r.ctx.Err() == nil:
				// A failed read is not: it waits for the run, which ends it (see trackRead).
				r.fail(err)
				<-r.release
				return
			case err != nil:
				return
			}
		}
	}()
	return reader, nil
}

func (r *trackRead) fail(err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		close(r.failed)
	})
}

// cause is the failure of the read, or nil.
func (r *trackRead) cause() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// end lets a read that waits go, and closes the pipe and the read of the store.
func (r *trackRead) end() {
	r.let.Do(func() { close(r.release) })
	r.mu.Lock()
	for _, c := range r.closers {
		c.Close()
	}
	r.closers = nil
	r.mu.Unlock()
}
