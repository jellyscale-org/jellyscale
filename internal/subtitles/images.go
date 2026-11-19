package subtitles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// ErrTooSlow means the read of the film for its image tracks ran into the time
// limit, so reading it again, at that speed, takes as long.
var ErrTooSlow = errors.New("reading the file took longer than the time limit")

// ImageKey names an image track (PGS, VobSub, DVB) inside the film as it is kept
// in the asset store: a Matroska file with that one track, its packets as the
// film has them and at the film's own times (JS-089). A run that begins mid-film
// reads the cue showing at its cut from it (library.ImageTrack.Feed) instead of
// from the film. The ETag is in the key, as in Key.
func ImageKey(src items.Source, index int) string {
	return "subtitles/" + src.ID + "/" + hashOf(src.ETag) + "/" + strconv.Itoa(index) + ".mkv"
}

// ImagesKey names the job that stores every image track inside src's file in one
// read. It is a job's key, not an object's.
func ImagesKey(src items.Source) string {
	return "subtitles/" + src.ID + "/" + hashOf(src.ETag) + "/images"
}

// ImageTracks lists the image tracks inside src's file, the ones one pass over it
// stores. A file beside the film is not in it, and a burn reads it as it is.
func ImageTracks(src items.Source) []int {
	var indexes []int
	for _, s := range src.Streams {
		if s.Type == "Subtitle" && s.IsImage() && !s.IsExternal && s.Fingerprint == "" && !s.Unreadable {
			indexes = append(indexes, s.Index)
		}
	}
	return indexes
}

// FillImages reads src's file once and puts each image track inside it in the
// asset store (see ImageKey). As FillText does, it leaves out the tracks ffmpeg
// refuses (one the file lacks, one it can't decode) and reads the film again
// without them, so a film with any number of such tracks costs two reads of it
// and the others lose nothing; the tracks left out are returned. A run that
// fails without naming a stream, or for a reason that is not the film's (the
// store, a timeout, a signal), stores nothing. Only a worker calls it, through
// the film's images job. A zero timeout is DefaultTimeout.
func FillImages(ctx context.Context, src items.Source, media, assets store.Store, timeout time.Duration, log *slog.Logger) (left []int, err error) {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	log = log.With("item", src.ID, "source", src.Key)
	indexes := ImageTracks(src)
	log.Info("image track extraction started", "size", src.Size, "tracks", indexes)
	began := time.Now()
	dir, err := os.MkdirTemp("", "images-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	err = extractImages(ctx, media, src, indexes, dir, timeout)
	decoders := false
	for errors.Is(err, ErrBadFile) {
		failed := inPass(streamsIn(err, noStream), indexes)
		if len(failed) == 0 && !decoders {
			failed, decoders = inPass(streamsIn(err, failedStream), indexes), true
		}
		if len(failed) == 0 {
			break
		}
		log.Warn("image tracks that can't be extracted are left out", "tracks", failed, "error", err)
		left = append(left, failed...)
		indexes = slices.DeleteFunc(indexes, func(i int) bool { return slices.Contains(failed, i) })
		err = extractImages(ctx, media, src, indexes, dir, timeout)
	}
	log.Info("image track extraction ended", "duration", time.Since(began), "error", err)
	if err != nil {
		return left, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, index := range indexes {
		if err := putFile(ctx, assets, ImageKey(src, index), filepath.Join(dir, strconv.Itoa(index)+".mkv")); err != nil {
			return left, err
		}
	}
	return left, nil
}

func putFile(ctx context.Context, assets store.Store, key, path string) error {
	body, err := os.Open(path)
	if err != nil {
		return err
	}
	defer body.Close()
	return assets.Put(ctx, key, body)
}

// extractImages is one read of the film, under the whole timeout. The packets
// are copied at the film's own times (-copyts), which a run that draws them
// with the film's video counts on.
func extractImages(ctx context.Context, media store.Store, src items.Source, indexes []int, dir string, timeout time.Duration) error {
	if len(indexes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, src.Key, src.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	args := []string{"-nostdin", "-v", "error", "-y", "-copyts", "-i", input}
	for _, index := range indexes {
		args = append(args, "-map", "0:"+strconv.Itoa(index), "-c:s", "copy", "-f", "matroska", filepath.Join(dir, strconv.Itoa(index)+".mkv"))
	}
	err = runFFmpeg(ctx, args)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w (%s): %v", ErrTooSlow, timeout, err)
	}
	if err != nil && watched.failed.Load() {
		// As in extractInside: only a film ffmpeg can't parse is the film's doing.
		return fmt.Errorf("the film could not be read from the store: %v", err)
	}
	return err
}
