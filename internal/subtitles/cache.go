package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// DefaultTimeout bounds one extraction, which reads the whole file: an hour
// reads about 300 GB at 100 MB/s. It only stops an ffmpeg that hangs.
const DefaultTimeout = time.Hour

// Key names a track in the library's asset store, where each is kept once
// extracted. The ETag is in the key, so a changed file reads fresh text; the
// old object stays behind.
func Key(src items.Source, index int) string {
	sum := sha256.Sum256([]byte(src.ETag))
	return "subtitles/" + src.ID + "/" + hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(index) + ".json"
}

// Read finds the cached track. An object that doesn't parse counts as missing,
// so extraction replaces it.
func Read(ctx context.Context, assets store.Store, key string) (Track, bool, error) {
	body, err := assets.Open(ctx, key, 0)
	if errors.Is(err, store.ErrNotFound) {
		return Track{}, false, nil
	}
	if err != nil {
		return Track{}, false, err
	}
	defer body.Close()
	track, err := Decode(body)
	return track, err == nil, nil
}

// Exists says whether the track is cached, for deciding what to list.
func Exists(ctx context.Context, assets store.Store, key string) bool {
	body, err := assets.Open(ctx, key, 0)
	if err != nil {
		return false
	}
	body.Close()
	return true
}

// Fill reads stream index of src out of its file and puts the result in the
// asset store. Only a worker calls it, through a subtitle job, which is what
// keeps it to one extraction of a track. A zero timeout is DefaultTimeout.
func Fill(ctx context.Context, src items.Source, media, assets store.Store, index int, timeout time.Duration, log *slog.Logger) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	input, closeInput, err := store.Input(ctx, media, src.Key, src.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	log = log.With("item", src.ID, "source", src.Key, "index", index)
	log.Info("subtitle extraction started", "size", src.Size)
	began := time.Now()
	track, err := Extract(ctx, input, index)
	log.Info("subtitle extraction ended", "duration", time.Since(began), "error", err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reading the file took longer than %s: %w", timeout, err)
	}
	if err != nil {
		return err
	}
	return assets.Put(ctx, Key(src, index), track.Encode())
}
