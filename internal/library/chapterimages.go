package library

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// A chapter picture is one frame at the chapter's start, kept in the library's
// asset store under chapters/{source}/. One job per source (a subtitle_jobs row
// of kind chapters, claimed by workers on slots of their own) both makes the
// pictures and deletes the ones that are not wanted; the scan only queues it.
// The stream role serves the stored JPEG as it is.

const (
	// chapterWidth is the widest picture kept. jellyfin-web asks for 400 and
	// the stream role does not resize, so a picture is made once at that size.
	chapterWidth = 400
	// chapterAttempts is how many times making may start for one file version.
	// The count is in chapter_images, not on the job row, which the daily sweep
	// of subtitle_jobs deletes.
	chapterAttempts = 3
)

// chapterFrameTimeout bounds the read of one frame, which seeks into the file
// (a few requests to the store when it is on S3). A var for the tests.
var chapterFrameTimeout = 2 * time.Minute

// readableTimeout bounds the check that the file can still be read, made after
// a frame fails. A var for the tests.
var readableTimeout = 30 * time.Second

// wantedSQL says that the source m, of library l, is to have pictures: the
// library's option is on and the file has chapters and a video stream to take
// frames from. Both the scan's queueing and the job read it, so they agree.
const wantedSQL = `(l.chapter_images AND jsonb_array_length(m.chapters) > 0 AND jsonb_path_exists(m.streams, '$[*] ? (@.Type == "Video")'))`

// ChapterDir is where a file version's chapter pictures are in the asset
// store: the ETag is in it, so a changed file gets new pictures and the old
// ones are never served.
func ChapterDir(sourceID, etag string) string {
	sum := md5.Sum([]byte(etag))
	return "chapters/" + sourceID + "/" + hex.EncodeToString(sum[:])
}

// ChapterKey names the picture of chapter n (from 0) in dir.
func ChapterKey(dir string, n int) string {
	return fmt.Sprintf("%s/%d.jpg", dir, n)
}

// EnqueueChapterImages queues the job of each source of the library that has
// work, which is all the scan does for chapter pictures. It runs after every
// scan, the option on or off. A source has work when it is wanted and has no
// row, a row of another file version, or a row that is not done (the job itself
// gives up once the attempts are used); when it is not wanted but has a row; or
// when its row has no source any more (it was removed). A source whose row is done for its file
// costs nothing. A job that is queued or running is left alone.
func EnqueueChapterImages(ctx context.Context, pool *pgxpool.Pool, libraryID string) error {
	_, err := pool.Exec(ctx, `INSERT INTO subtitle_jobs (key, kind, item_id, source_id, idx)
		SELECT 'chapters/' || id, 'chapters', id, id, 0 FROM (
			SELECT m.id::text AS id
			FROM media_sources m JOIN shown_items i ON i.id = m.item_id JOIN libraries l ON l.id = i.library_id
				LEFT JOIN chapter_images c ON c.source_id = m.id
			WHERE i.library_id = $1 AND CASE WHEN `+wantedSQL+`
				THEN c.source_id IS NULL OR c.etag <> m.object_etag OR c.state <> 'done'
				ELSE c.source_id IS NOT NULL END
			UNION ALL
			SELECT c.source_id::text FROM chapter_images c LEFT JOIN media_sources m ON m.id = c.source_id
			WHERE c.library_id = $1 AND m.id IS NULL
		) due
		ON CONFLICT (key) DO UPDATE SET state = 'queued', worker = NULL, error = NULL, permanent = false,
			ended_at = NULL, lease_expires_at = NULL, created_at = now(), attempts = 0
		WHERE subtitle_jobs.state IN ('done', 'failed')`, libraryID)
	return err
}

// SyncChapterImages is the job of one source, the only thing that makes or
// deletes chapter pictures. It reads what is wanted itself, and which asset
// store through the library of the row or the source, since a removed source
// has no media source. It lists chapters/{source}/ once and deletes everything
// outside the directory of the file's current version (everything when the
// pictures are not wanted, and then the row). When they are wanted it makes
// the chapters in order, writing each one's outcome (its key, or "" for a skip)
// to the row as it goes, so a later attempt resumes after them and a skip is
// final, and marks the row done at the end.
//
// The row says 'making' from the first statement, so nothing is served from a
// file whose pictures are being made, and counts the start as an attempt. The
// count is never given back, not for a worker that is stopping nor for a store
// that is down: that is what keeps starts at most 3 for a file version. If a
// replacement file moved the row on meanwhile, the final statement writes
// nothing, and the next job's sweep deletes these pictures: what is under the
// prefix is what is stored, the keys in the row are only for serving.
//
// A frame that can't be read for the file's doing is skipped for good, which
// is a "" key. A failure of a store, a lost lease or a killed ffmpeg fails the
// job, and the next scan queues it again. A job that finds the file version's
// attempts used up gives up instead: it deletes the pictures made so far and
// records the version as done with none, so nothing is left behind and nothing
// more is read until the file changes. The cap is checked here, not in the
// queue, so a job a worker takes over after a lapsed lease is held to it too.
func SyncChapterImages(ctx context.Context, pool *pgxpool.Pool, sourceID string, openStore func(uri string) (store.Store, error), log *slog.Logger) error {
	var libraryID, assetURI, mediaURI, etag, key, rowEtag string
	var size int64
	var attempts int
	var progress []string // Chapters handled so far for this file version, in order: a key, or "" for a skip.
	var wanted, done bool
	var streamsJSON, chaptersJSON []byte
	err := pool.QueryRow(ctx, `SELECT l.id::text, l.asset_store_uri, l.media_store_uri, COALESCE(`+wantedSQL+`, false),
			COALESCE(m.object_etag, ''), COALESCE(m.store_key, ''), COALESCE(m.size, 0), COALESCE(m.streams, '[]'), COALESCE(m.chapters, '[]'),
			COALESCE(c.state = 'done' AND c.etag = m.object_etag, false), COALESCE(c.etag, ''), COALESCE(c.attempts, 0),
			CASE WHEN c.etag = m.object_etag THEN c.keys ELSE '{}' END
		FROM (SELECT $1::uuid AS id) s
		LEFT JOIN chapter_images c ON c.source_id = s.id
		LEFT JOIN media_sources m ON m.id = s.id
		LEFT JOIN items i ON i.id = m.item_id
		JOIN libraries l ON l.id = COALESCE(c.library_id, i.library_id)`, sourceID).Scan(
		&libraryID, &assetURI, &mediaURI, &wanted, &etag, &key, &size, &streamsJSON, &chaptersJSON, &done, &rowEtag, &attempts, &progress)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // No row and no source: nothing was made and nothing is wanted.
	}
	if err != nil {
		return err
	}
	gaveUp := wanted && !done && rowEtag == etag && attempts >= chapterAttempts
	if wanted && !done && !gaveUp {
		// Counted at the start, so a job that is killed or hangs is counted too.
		if _, err := pool.Exec(ctx, `INSERT INTO chapter_images (source_id, library_id, etag, state, attempts)
			VALUES ($1, $2, $3, 'making', 1)
			ON CONFLICT (source_id) DO UPDATE SET library_id = excluded.library_id, etag = excluded.etag, state = 'making',
				keys = CASE WHEN chapter_images.etag = excluded.etag THEN chapter_images.keys ELSE '{}' END,
				attempts = CASE WHEN chapter_images.etag = excluded.etag THEN chapter_images.attempts + 1 ELSE 1 END`,
			sourceID, libraryID, etag); err != nil {
			return err
		}
	}
	assets, err := openStore(assetURI)
	if err != nil {
		return err
	}
	prefix := "chapters/" + sourceID + "/"
	found, err := assets.List(ctx, prefix)
	if err != nil {
		return err
	}
	dir := ChapterDir(sourceID, etag)
	made := map[string]bool{}
	for _, object := range found {
		// A .put- file is a picture whose write was cut off (a killed worker).
		if wanted && !gaveUp && strings.HasPrefix(object.Key, dir+"/") && !strings.HasPrefix(path.Base(object.Key), ".put-") {
			made[object.Key] = true
		} else if err := assets.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	if !wanted {
		_, err := pool.Exec(ctx, "DELETE FROM chapter_images WHERE source_id = $1", sourceID)
		return err
	}
	if gaveUp {
		_, err := pool.Exec(ctx, "UPDATE chapter_images SET keys = '{}', state = 'done' WHERE source_id = $1 AND etag = $2", sourceID, etag)
		if err == nil {
			log.Warn("chapter images given up: making started the most times allowed for this file version, whatever stopped it (a bad file, or a store that was down); to try again turn chapterImages off and on across two scans", "source", sourceID, "attempts", attempts)
		}
		return err
	}
	if done {
		return nil
	}

	var chapters []struct{ StartPositionTicks int64 }
	if err := json.Unmarshal(chaptersJSON, &chapters); err != nil {
		return err
	}
	streams, err := playback.ParseStreams(streamsJSON)
	if err != nil {
		return err
	}
	toneMap := playback.Source{Streams: streams}.ToneMap()
	media, err := openStore(mediaURI)
	if err != nil {
		return err
	}

	log = log.With("source", sourceID, "chapters", len(chapters))
	log.Info("chapter images started")
	began := time.Now()
	// ffmpeg reads the media through a local server, so a failure of the media
	// store reaches it only as a short or failed read: watch for it here.
	watched := &watchedStore{Store: media}
	input, closeInput, err := store.Input(ctx, watched, key, size)
	if err != nil {
		return err
	}
	defer closeInput()
	keys := make([]string, len(chapters))
	var created int
	for i, chapter := range chapters {
		picture := ChapterKey(dir, i)
		if i < len(progress) && (progress[i] == "" || made[picture]) {
			keys[i] = progress[i] // Handled by an earlier attempt: a skip stays a skip.
			continue
		}
		if made[picture] {
			keys[i] = picture
			continue
		}
		frame, err := chapterFrame(ctx, input, chapter.StartPositionTicks, toneMap)
		if ctx.Err() != nil {
			return ctx.Err() // The job was cut off, or its lease lost.
		}
		if storeErr := watched.err(); storeErr != nil {
			return fmt.Errorf("media store: %w", storeErr)
		}
		var exited *exec.ExitError
		var skip string
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			skip = "its frame took too long"
		case errors.As(err, &exited) && exited.Exited():
			skip = "its frame can't be read: " + err.Error()
		case err != nil:
			return err // ffmpeg did not run, or was killed: not the file's doing.
		}
		// A frame with no error is a picture; neither is a chapter that starts
		// at or past the end of the picture, which gets none.
		if skip == "" && len(frame) > 0 {
			keys[i] = picture
			if err := assets.Put(ctx, picture, bytes.NewReader(frame)); err != nil {
				return err
			}
			created++
		} else if skip != "" {
			// From a POSIX store ffmpeg reads the file itself, so a file that
			// can't be read at all looks like a bad frame: tell them apart.
			if err := readable(ctx, media, key); err != nil {
				return fmt.Errorf("media store: %w", err)
			}
			log.Warn("chapter has no picture: "+skip, "chapter", i)
		}
		// Kept as it goes, so an attempt that fails later doesn't read again
		// what this one handled.
		if _, err := pool.Exec(ctx, "UPDATE chapter_images SET keys = $3 WHERE source_id = $1 AND etag = $2 AND state = 'making'", sourceID, etag, keys[:i+1]); err != nil {
			return err
		}
	}

	if _, err := pool.Exec(ctx, "UPDATE chapter_images SET keys = $3, state = 'done' WHERE source_id = $1 AND etag = $2", sourceID, etag, keys); err != nil {
		return err
	}
	log.Info("chapter images ended", "made", created, "duration", time.Since(began))
	return nil
}

// readable says whether the store can still give the file's first byte. It has
// a deadline of its own, as the store may be what stalled the frame, and keeps
// it even on a POSIX mount whose read ignores its context: the read is left
// behind in its goroutine and the job goes on to fail.
func readable(ctx context.Context, media store.Store, key string) error {
	ctx, cancel := context.WithTimeout(ctx, readableTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		body, err := media.Open(ctx, key, 0)
		if err != nil {
			result <- err
			return
		}
		defer body.Close()
		if _, err = body.Read(make([]byte, 1)); err == io.EOF {
			err = nil
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chapterFrame reads one JPEG at start ticks of the file's own clock, the one
// chapters are on. -seek_timestamp makes -ss that clock: by default ffmpeg adds
// the file's start time, which a file whose streams start late would put
// seconds too far in. A start at or past the end gives no picture and no error.
func chapterFrame(ctx context.Context, input string, start int64, toneMap string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, chapterFrameTimeout)
	defer cancel()
	// An HDR picture is mapped to SDR as playback maps it, or its code values
	// would be shown as they are.
	filter := fmt.Sprintf("scale='min(%d,iw)':-2", chapterWidth)
	if toneMap != "" {
		filter += "," + playback.ToneMapFilter(toneMap)
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-seek_timestamp", "1",
		"-ss", fmt.Sprintf("%.3f", float64(start)/ticksPerSecond), "-i", input,
		"-frames:v", "1", "-an", "-sn", "-vf", filter,
		"-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "-")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	// Wait returns soon after the kill even if something still holds ffmpeg's
	// output open.
	cmd.WaitDelay = 5 * time.Second
	// Pdeathsig fires when the thread that started ffmpeg exits (see subtitles).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	subtitles.DieWithParent(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("ffmpeg: %w", ctx.Err())
	}
	// ffmpeg says so, and exits with an error or without by version, when the
	// file had no frame there: not a failure.
	if out.Len() == 0 && (strings.Contains(stderr.String(), "Nothing was written into output file") || strings.Contains(stderr.String(), "Output file is empty")) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// watchedStore remembers the first error the store gave a read, other than one
// from a read that was cut off (ffmpeg closing its connection once it has its
// frame cancels the read, which is not a failure).
type watchedStore struct {
	store.Store
	mu     sync.Mutex
	failed error
}

func (w *watchedStore) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (w *watchedStore) note(ctx context.Context, err error) {
	if err == nil || err == io.EOF || ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed == nil {
		w.failed = err
	}
}

func (w *watchedStore) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	body, err := w.Store.Open(ctx, key, offset)
	w.note(ctx, err)
	if err != nil {
		return nil, err
	}
	return &watchedBody{ReadCloser: body, w: w, ctx: ctx}, nil
}

type watchedBody struct {
	io.ReadCloser
	w   *watchedStore
	ctx context.Context
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != io.ErrUnexpectedEOF { // The reader resumes after this one.
		b.w.note(b.ctx, err)
	}
	return n, err
}
