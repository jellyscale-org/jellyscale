package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
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

// PassKey names the job that extracts every text track inside src's file in one
// read. It is a job's key, not an object's.
func PassKey(src items.Source) string {
	return "subtitles/" + src.ID + "/" + hashOf(src.ETag) + "/text"
}

// TextTracks lists the text subtitle tracks inside src's file, the ones a
// single pass over it extracts. Files beside the film, and tracks found
// unreadable, are not in it.
func TextTracks(src items.Source) []int {
	var streams []struct {
		Index                                        int
		Type                                         string
		IsTextSubtitleStream, IsExternal, Unreadable bool
	}
	if json.Unmarshal(src.Streams, &streams) != nil {
		return nil
	}
	var indexes []int
	for _, s := range streams {
		if s.Type == "Subtitle" && s.IsTextSubtitleStream && !s.IsExternal && !s.Unreadable {
			indexes = append(indexes, s.Index)
		}
	}
	return indexes
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

// Missing says whether the store answers that key is not there. Any other
// error, a store that didn't answer or a cancelled context, is returned and
// says nothing about the track, so that no one hides a track for it.
func Missing(ctx context.Context, assets store.Store, key string) (bool, error) {
	body, err := assets.Open(ctx, key, 0)
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	body.Close()
	return false, nil
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

// FillText reads every text track inside src's file in one run of ffmpeg and
// puts each in the asset store. When ffmpeg refuses the run it names the
// streams it failed on, tracks the file lacks or ones it can't decode; those
// are left out and the run is made again, so a film with any number of such
// tracks costs two reads of the film and the others lose nothing. The caller hides the
// tracks left unstored. A run that fails without naming a stream, such as a
// file ffmpeg can't open or a read that timed out, leaves nothing stored and
// nothing left out. Each run has the whole timeout, as a track read of its own
// had. Only a worker calls it, through the film's pass job. A zero timeout is
// DefaultTimeout.
func FillText(ctx context.Context, src items.Source, media, assets store.Store, timeout time.Duration, log *slog.Logger) error {
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	log = log.With("item", src.ID, "source", src.Key)
	indexes := TextTracks(src)
	log.Info("subtitle pass started", "size", src.Size, "tracks", indexes)
	began := time.Now()
	tracks, err := extractRun(ctx, media, src, indexes, timeout)
	decoders := false
	for errors.Is(err, ErrBadFile) {
		// ffmpeg refuses a -map to a stream the file lacks before it reads any
		// of the film, and names only the first, so those are dropped one run
		// at a time; the runs that read the film are two at most.
		// The error text holds the file's path, so a name that looks like
		// ffmpeg's lines must not name a stream outside the pass: only
		// streams still in indexes count, which also ends the loop.
		failed := inPass(streamsIn(err, noStream), indexes)
		if len(failed) == 0 && !decoders {
			failed, decoders = inPass(streamsIn(err, failedStream), indexes), true
		}
		if len(failed) == 0 {
			break
		}
		log.Warn("tracks that can't be extracted are left out", "tracks", failed, "error", err)
		indexes = slices.DeleteFunc(indexes, func(i int) bool { return slices.Contains(failed, i) })
		tracks, err = extractRun(ctx, media, src, indexes, timeout)
	}
	log.Info("subtitle pass ended", "duration", time.Since(began), "error", err)
	if err != nil {
		return err
	}
	// The writes are bounded too, as the one Put of a track read was.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for index, track := range tracks {
		if err := assets.Put(ctx, Key(src, index), track.Encode()); err != nil {
			return err
		}
	}
	return nil
}

// inPass keeps the streams that are still tracks of the pass.
func inPass(streams, indexes []int) []int {
	return slices.DeleteFunc(streams, func(i int) bool { return !slices.Contains(indexes, i) })
}

// extractRun is one read of the film, under the whole timeout.
func extractRun(ctx context.Context, media store.Store, src items.Source, indexes []int, timeout time.Duration) (map[int]Track, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tracks, err := extractInsideAll(ctx, media, src, indexes)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("reading the file took longer than %s: %w", timeout, err)
	}
	return tracks, err
}

// ffmpeg names a stream it failed on in two ways: its refusal of a -map to a
// stream the file lacks (its other line, "Stream map ” matches no streams",
// names none), and its decoder's lines, which start "sist#0:2/subrip". The
// decoder names every stream that failed, including one that only logged a
// warning; that track is as untrustworthy alone (any output from ffmpeg counts
// as a failure, see runFFmpeg), so it is left out too.
var (
	noStream     = regexp.MustCompile(`Failed to set value '0:(\d+)' for option 'map'`)
	failedStream = regexp.MustCompile(`sist#0:(\d+)/`)
)

// streamsIn are the streams re finds in ffmpeg's error.
func streamsIn(err error, re *regexp.Regexp) []int {
	var streams []int
	for _, m := range re.FindAllStringSubmatch(err.Error(), -1) {
		index, _ := strconv.Atoi(m[1])
		if !slices.Contains(streams, index) {
			streams = append(streams, index)
		}
	}
	return streams
}

// Unreadable says whether src's stream index has been found not to be
// extractable (see library.MarkUnreadable).
func Unreadable(src items.Source, index int) bool {
	var streams []struct {
		Index      int
		Unreadable bool
	}
	if json.Unmarshal(src.Streams, &streams) != nil {
		return false
	}
	return slices.ContainsFunc(streams, func(s struct {
		Index      int
		Unreadable bool
	}) bool {
		return s.Index == index && s.Unreadable
	})
}

func extractInsideAll(ctx context.Context, media store.Store, src items.Source, indexes []int) (map[int]Track, error) {
	if len(indexes) == 0 {
		return nil, nil
	}
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, src.Key, src.Size)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	tracks, err := ExtractAll(ctx, input, indexes)
	if err != nil && watched.failed.Load() {
		// As in extractInside.
		return nil, fmt.Errorf("the film could not be read from the store: %v", err)
	}
	return tracks, err
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
