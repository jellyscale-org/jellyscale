package subtitles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// SupKey names the raw PGS stream of a track in the asset store: a plain
// object, so the role that serves it sends the bytes and decodes none of them.
// The ETag is in the key, as in Key.
func SupKey(src items.Source, index int) string {
	return "subtitles/" + src.ID + "/" + hashOf(src.ETag) + "/" + strconv.Itoa(index) + ".sup"
}

// FillSup copies PGS track index of src out of its file into the asset store as
// a .sup file, which jellyfin-web draws itself. Like Fill it reads the whole
// film once, because ffmpeg has to find the track's sparse packets in it, and
// only a worker calls it. A zero timeout is DefaultTimeout.
func FillSup(ctx context.Context, src items.Source, media, assets store.Store, index int, timeout time.Duration, log *slog.Logger) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log = log.With("item", src.ID, "source", src.Key, "index", index)
	log.Info("sup extraction started", "size", src.Size)
	began := time.Now()
	err := fillSup(ctx, src, media, assets, index)
	log.Info("sup extraction ended", "duration", time.Since(began), "error", err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reading the file took longer than %s: %w", timeout, err)
	}
	return err
}

func fillSup(ctx context.Context, src items.Source, media, assets store.Store, index int) error {
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, src.Key, src.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	dir, err := os.MkdirTemp("", "sup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "track.sup")
	err = runFFmpeg(ctx, []string{"-nostdin", "-v", "error", "-i", input, "-map", "0:" + strconv.Itoa(index), "-c:s", "copy", "-f", "sup", file})
	if err != nil && watched.failed.Load() {
		// As in extractInside: only a film ffmpeg can't parse is the film's doing.
		return fmt.Errorf("the film could not be read from the store: %v", err)
	}
	if err != nil {
		return err
	}
	body, err := os.Open(file)
	if err != nil {
		return err
	}
	defer body.Close()
	return assets.Put(ctx, SupKey(src, index), body)
}
