package subtitles

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"jellyscale/internal/items"
	"jellyscale/internal/store"
)

// defaultTimeout bounds one extraction, which reads the whole file: an hour
// reads about 300 GB at 100 MB/s. It only stops an ffmpeg that hangs.
const (
	defaultTimeout = time.Hour
	pollInterval   = 200 * time.Millisecond
)

// flights shares one extraction among an API process's requests for a track,
// and slots bounds how many run at once, so a burst of requests can't use up
// connections, CPU or the object store.
var (
	flights singleflight.Group
	slots   = make(chan struct{}, 4)
)

// Cache keeps extracted tracks in the library's asset store, so each is read
// out of its file once, whichever replica or worker asks first.
type Cache struct {
	Pool *pgxpool.Pool
	// KeepGoing lets an extraction run on after its caller goes away, so
	// whoever asks next finds it done. A caller that must be able to stop it,
	// like a worker whose job was cancelled, leaves it off.
	KeepGoing bool
	// Timeout is how long one extraction may take; zero is defaultTimeout.
	Timeout time.Duration
	// Log says when an extraction starts and ends; nil is the default logger.
	Log *slog.Logger
}

// Track returns subtitle stream index of src, extracting it if no one has.
// With KeepGoing, requests in this process for one track share one extraction
// and its result, whether it worked or not. Without it each caller extracts
// for itself, so cancelling one can't fail another. Across replicas,
// extraction is under an advisory lock named for the cached object, held on a
// connection of its own: an extraction can last minutes, and holding a pooled
// connection that long would starve the replica's other requests. The others
// poll the asset store and take over if the holder fails.
func (c Cache) Track(ctx context.Context, src items.Source, media, assets store.Store, index int) (Track, error) {
	// The ETag is in the key, so a changed file reads fresh text; the old
	// object stays behind.
	sum := sha256.Sum256([]byte(src.ETag))
	key := "subtitles/" + src.ID + "/" + hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(index) + ".json"
	if track, found, err := read(ctx, assets, key); found || err != nil {
		return track, err
	}
	if !c.KeepGoing {
		return c.fill(ctx, src, media, assets, index, key)
	}
	flightCtx := context.WithoutCancel(ctx)
	result := flights.DoChan(key, func() (any, error) {
		return c.fill(flightCtx, src, media, assets, index, key)
	})
	select {
	case r := <-result:
		if r.Err != nil {
			return Track{}, r.Err
		}
		return r.Val.(Track), nil
	case <-ctx.Done():
		return Track{}, ctx.Err()
	}
}

// fill tries for the lock until it has it or the track appears, and extracts
// if the track is still missing by then.
func (c Cache) fill(ctx context.Context, src items.Source, media, assets store.Store, index int, key string) (Track, error) {
	for {
		if track, found, err := read(ctx, assets, key); found || err != nil {
			return track, err
		}
		if track, locked, err := c.tryExtract(ctx, src, media, assets, index, key); locked || err != nil {
			return track, err
		}
		select {
		case <-ctx.Done():
			return Track{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// tryExtract takes a slot and a connection for one try at the lock, and
// extracts if it gets it. The slot and connection are held only for that try
// or the extraction, so a request waiting on another replica's extraction
// takes neither for long.
func (c Cache) tryExtract(ctx context.Context, src items.Source, media, assets store.Store, index int, key string) (track Track, locked bool, err error) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return Track{}, false, ctx.Err()
	}
	conn, err := pgx.ConnectConfig(ctx, c.Pool.Config().ConnConfig)
	if err != nil {
		return Track{}, false, err
	}
	defer conn.Close(context.WithoutCancel(ctx)) //nolint:errcheck // Closing releases the lock too.
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", key).Scan(&locked); err != nil || !locked {
		return Track{}, false, err
	}
	track, err = c.extractUnder(ctx, src, media, assets, index, key)
	return track, true, err
}

// extractUnder runs with the lock held. It looks again first, since another
// holder may have finished between the caller's read and its lock.
func (c Cache) extractUnder(ctx context.Context, src items.Source, media, assets store.Store, index int, key string) (Track, error) {
	if track, found, err := read(ctx, assets, key); found || err != nil {
		return track, err
	}
	timeout := cmp.Or(c.Timeout, defaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	input, closeInput, err := store.Input(ctx, media, src.Key, src.Size)
	if err != nil {
		return Track{}, err
	}
	defer closeInput()
	log := c.Log
	if log == nil {
		log = slog.Default()
	}
	log = log.With("item", src.ID, "source", src.Key, "index", index)
	log.Info("subtitle extraction started", "size", src.Size)
	began := time.Now()
	track, err := Extract(ctx, input, index)
	log.Info("subtitle extraction ended", "duration", time.Since(began), "error", err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Track{}, fmt.Errorf("reading the file took longer than %s: %w", timeout, err)
	}
	if err != nil {
		return Track{}, err
	}
	return track, assets.Put(ctx, key, track.Encode())
}

// read finds the cached track. An object that doesn't parse counts as missing,
// so extraction replaces it.
func read(ctx context.Context, assets store.Store, key string) (Track, bool, error) {
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
