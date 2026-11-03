package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
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
// old object stays behind. A track that is a file beside the film also has the
// file's fingerprint in it, so an edited file is read fresh.
func Key(src items.Source, index int) string {
	file, _ := External(src, index)
	name := strconv.Itoa(index)
	if file.Fingerprint != "" {
		name += "-" + file.Fingerprint
	}
	return "subtitles/" + src.ID + "/" + hashOf(src.ETag) + "/" + name + ".json"
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
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

// Fill reads stream index of src out of its file, or the subtitle file beside
// it that the stream is, and puts the result in the asset store. Only a worker
// calls it, through a subtitle job, which is what keeps it to one extraction of
// a track. A file that can't be read for good is ErrBadFile. A zero timeout is
// DefaultTimeout.
func Fill(ctx context.Context, src items.Source, media, assets store.Store, index int, timeout time.Duration, log *slog.Logger) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	log = log.With("item", src.ID, "source", src.Key, "index", index)
	log.Info("subtitle extraction started", "size", src.Size)
	began := time.Now()
	var track Track
	var err error
	if file, ok := External(src, index); ok {
		track, err = extractBeside(ctx, media, file.Path, file.Language)
	} else {
		track, err = extractInside(ctx, media, src, index)
	}
	log.Info("subtitle extraction ended", "duration", time.Since(began), "error", err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("reading the file took longer than %s: %w", timeout, err)
	}
	if err != nil {
		return err
	}
	return assets.Put(ctx, Key(src, index), track.Encode())
}

func extractInside(ctx context.Context, media store.Store, src items.Source, index int) (Track, error) {
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, src.Key, src.Size)
	if err != nil {
		return Track{}, err
	}
	defer closeInput()
	track, err := Extract(ctx, input, index)
	if err != nil && watched.failed.Load() {
		// ffmpeg reports a film the store cut short as it does one it can't
		// parse; only the second is the film's doing.
		return Track{}, fmt.Errorf("the film could not be read from the store: %v", err)
	}
	return track, err
}

func extractBeside(ctx context.Context, media store.Store, key, language string) (Track, error) {
	body, err := media.Open(ctx, key, 0)
	if err != nil {
		return Track{}, err
	}
	defer body.Close()
	return ExtractFile(ctx, body, path.Ext(key), language)
}
