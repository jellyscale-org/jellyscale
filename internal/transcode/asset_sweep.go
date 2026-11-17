package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/items"
	"jellyscale/internal/library"
	"jellyscale/internal/media"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// assetState is what the sweep of the asset stores reads from Postgres after it
// has listed them, so that whatever is in a listing was put before the state
// that judges it.
type assetState struct {
	began    time.Time                  // When the sweep began, before it listed anything.
	scanning map[string]bool            // Asset store URIs a scan is writing images to now.
	keep     map[string]map[string]bool // By asset store URI: the keys, and chapter directories, of every current version.
	busy     []string                   // Key prefixes that a queued or running job may be writing.
	fonts    bool                       // A fonts job runs, or ended since the sweep began: the fonts a manifest will name are not all named yet.
}

// sweepAssets removes from each library's asset store what no version of any
// file needs: the subtitle text, .sup copies, primary image, fonts manifest and
// chapter pictures of a file that was removed (its item deleted after
// missingGrace) or of the previous version of a changed file, and the fonts
// that no remaining manifest names. Each store is listed once, then Postgres is
// read (a handful of queries, however many films), and only then is anything
// removed: a sweep that cannot do both removes nothing from that store.
func (w *Worker) sweepAssets(ctx context.Context) {
	var began time.Time
	rows, err := w.Pool.Query(ctx, "SELECT now(), asset_store_uri FROM libraries WHERE asset_store_uri <> '' GROUP BY asset_store_uri")
	if err != nil {
		w.logSweepFailure(ctx, "asset stores", err)
		return
	}
	var uris []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&began, &uri); err != nil {
			rows.Close()
			w.logSweepFailure(ctx, "asset stores", err)
			return
		}
		uris = append(uris, uri)
	}
	if rows.Err() != nil || len(uris) == 0 {
		if rows.Err() != nil {
			w.logSweepFailure(ctx, "asset stores", rows.Err())
		}
		return
	}
	stores := map[string]store.Store{}
	listed := map[string][]store.Object{}
	for _, uri := range uris {
		assets, err := w.Media(uri)
		if err == nil {
			var objects []store.Object
			if objects, err = assets.List(ctx, ""); err == nil {
				stores[uri], listed[uri] = assets, objects
			}
		}
		if err != nil {
			w.logSweepFailure(ctx, "asset store "+uri, fmt.Errorf("listing it: %w", err))
		}
	}
	if len(stores) == 0 {
		return
	}
	state, err := w.readAssetState(ctx, began)
	if err != nil {
		w.logSweepFailure(ctx, "asset stores", err)
		return
	}
	for uri, assets := range stores {
		if err := w.sweepAssetStore(ctx, uri, assets, listed[uri], state); err != nil {
			w.logSweepFailure(ctx, "asset store "+uri, err)
		}
	}
}

func (w *Worker) readAssetState(ctx context.Context, began time.Time) (assetState, error) {
	state := assetState{began: began, scanning: map[string]bool{}, keep: map[string]map[string]bool{}}
	keep := func(uri string) map[string]bool {
		if state.keep[uri] == nil {
			state.keep[uri] = map[string]bool{}
		}
		return state.keep[uri]
	}
	// A scan holds its library's lock from before it puts an image until it has
	// committed the item's key, so an image no item names yet may be its. The
	// try takes the lock of each library that is not scanning for the instant
	// of this transaction, which a scan that starts then answers as it does
	// for a scan elsewhere.
	err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT asset_store_uri, pg_try_advisory_xact_lock(hashtextextended('scan:' || id::text, 0))
			FROM libraries WHERE asset_store_uri <> ''`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var uri string
			var free bool
			if err := rows.Scan(&uri, &free); err != nil {
				return err
			}
			if !free {
				state.scanning[uri] = true
			}
		}
		return rows.Err()
	})
	if err != nil {
		return state, err
	}

	rows, err := w.Pool.Query(ctx, `SELECT l.asset_store_uri, m.id::text, m.object_etag,
			COALESCE(jsonb_path_query_array(m.streams, '$[*] ? (@.Type == "Subtitle")'), '[]'::jsonb)
		FROM media_sources m JOIN items i ON i.id = m.item_id JOIN libraries l ON l.id = i.library_id`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var src items.Source
		var uri string
		var streams []media.MediaStream
		if err := rows.Scan(&uri, &src.ID, &src.ETag, &streams); err != nil {
			rows.Close()
			return state, err
		}
		src.Streams = streams
		current := keep(uri)
		for _, stream := range streams {
			current[subtitles.Key(src, stream.Index)] = true
			current[subtitles.SupKey(src, stream.Index)] = true
		}
		current[subtitles.FontsKey(src.ID, src.ETag)] = true
		current[library.ChapterDir(src.ID, src.ETag)] = true
	}
	if err := rows.Err(); err != nil {
		return state, err
	}

	rows, err = w.Pool.Query(ctx, `SELECT l.asset_store_uri, i.primary_image_key FROM items i JOIN libraries l ON l.id = i.library_id
		WHERE i.primary_image_key IS NOT NULL`)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var uri, key string
		if err := rows.Scan(&uri, &key); err != nil {
			rows.Close()
			return state, err
		}
		keep(uri)[key] = true
	}
	if err := rows.Err(); err != nil {
		return state, err
	}

	// A job writes under its source's prefix, and the fonts job puts the fonts
	// before the manifest that names them, so while one runs (or has ended since
	// the sweep began, with its manifest put after the listing) no font is
	// judged.
	rows, err = w.Pool.Query(ctx, `SELECT kind, source_id FROM subtitle_jobs
		WHERE state IN ('queued', 'running') OR (kind = 'fonts' AND ended_at >= $1)`, began)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var kind, source string
		if err := rows.Scan(&kind, &source); err != nil {
			rows.Close()
			return state, err
		}
		state.fonts = state.fonts || kind == "fonts"
		state.busy = append(state.busy, jobPrefix(kind, source))
	}
	return state, rows.Err()
}

// jobPrefix is the key prefix a job of the kind writes under.
func jobPrefix(kind, source string) string {
	switch kind {
	case "fonts":
		return "attachments/" + source + "/"
	case "chapters":
		return "chapters/" + source + "/"
	}
	return "subtitles/" + source + "/"
}

// assetKind says what an object of an asset store is, or "" for one that is
// none of what the sweep judges (an operator's own file, a temporary file): the
// sweep never removes what it does not know.
func assetKind(key string) string {
	parts := strings.Split(key, "/")
	switch {
	case parts[0] == "subtitles" && len(parts) == 4 && strings.HasSuffix(key, ".json"):
		return "subtitles"
	case parts[0] == "subtitles" && len(parts) == 4 && strings.HasSuffix(key, ".sup"):
		return "sup"
	case parts[0] == "attachments" && len(parts) == 3 && strings.HasSuffix(key, ".fonts"):
		return "manifests"
	case parts[0] == "fonts" && len(parts) == 2:
		return "fonts"
	case parts[0] == "images" && len(parts) == 3:
		return "images"
	case parts[0] == "chapters" && len(parts) == 4 && strings.HasSuffix(key, ".jpg"):
		return "chapters"
	}
	return ""
}

func (w *Worker) sweepAssetStore(ctx context.Context, uri string, assets store.Store, objects []store.Object, state assetState) error {
	current := state.keep[uri]
	var kept []string // The manifests that stay, which name the fonts that stay.
	var remove []store.Object
	var fonts []store.Object
	for _, object := range objects {
		kind := assetKind(object.Key)
		if kind == "" || busyKey(state.busy, object.Key) {
			if kind == "manifests" {
				kept = append(kept, object.Key)
			}
			continue
		}
		switch kind {
		case "fonts":
			fonts = append(fonts, object)
		case "images":
			if !current[object.Key] && !state.scanning[uri] {
				remove = append(remove, object)
			}
		case "chapters":
			if !current[path.Dir(object.Key)] {
				remove = append(remove, object)
			}
		default:
			if current[object.Key] {
				if kind == "manifests" {
					kept = append(kept, object.Key)
				}
			} else {
				remove = append(remove, object)
			}
		}
	}
	// Reading the manifests costs a request each, so it is for a store that has
	// fonts and a sweep that may judge them.
	if len(fonts) > 0 && !state.fonts {
		named := map[string]bool{}
		readAll := true
		for _, key := range kept {
			sums, err := readManifest(ctx, assets, key)
			if err != nil {
				readAll = false
				w.Log.Warn("a fonts manifest could not be read, so no font of this store is removed", "store", uri, "manifest", key, "error", err)
				break
			}
			for _, sum := range sums {
				named[subtitles.FontKey(sum)] = true
			}
		}
		if readAll {
			for _, object := range fonts {
				if !named[object.Key] {
					remove = append(remove, object)
				}
			}
		}
	}
	removed := map[string]int{}
	var failure error
	for _, object := range remove {
		var gone bool
		if gone, failure = w.deleteAsset(ctx, uri, assets, object.Key, state.began); failure != nil {
			break
		}
		if gone {
			removed[assetKind(object.Key)]++
		}
	}
	w.Log.Info("swept an asset store", "store", uri, "subtitles", removed["subtitles"], "sup", removed["sup"], "images", removed["images"],
		"fonts", removed["fonts"], "chapters", removed["chapters"], "manifests", removed["manifests"])
	return failure
}

// deleteAsset removes an orphan of the sweep's listing unless it has been put
// again since the sweep read its state: a scan or a job writes under keys that
// an earlier version used, so each object is asked for once more, with a keyed
// query (never a read of the library). A scan puts an image before it commits
// the item that names it, under its library's lock, which is held here from the
// question to the delete, so that a scan that starts then waits for the next
// sweep to see its image named. Any other object is held under its owner's
// asset lock (see lockedAssets) for the same span, so a job that starts then
// puts after the delete, not between the question and it. A lock that is held
// is a put in flight, and the object is left for the next sweep. It reports
// whether the object was removed.
func (w *Worker) deleteAsset(ctx context.Context, uri string, assets store.Store, key string, began time.Time) (bool, error) {
	kind := assetKind(key)
	parts := strings.Split(key, "/")
	if kind == "images" {
		release, ok, err := w.holdScanLocks(ctx, uri)
		if err != nil || !ok {
			return false, err
		}
		defer release()
	} else {
		release, ok, err := w.tryAssetLock(ctx, assetLockName(key))
		if err != nil || !ok {
			return false, err
		}
		defer release()
	}
	wanted, err := w.assetWanted(ctx, kind, parts[1], key, began)
	if err != nil || wanted {
		return false, err
	}
	return true, assets.Delete(ctx, key)
}

// assetLockName names the advisory lock of the objects a job puts and the sweep
// deletes: those of one source, and all fonts together, since a font belongs to
// no source.
func assetLockName(key string) string {
	if assetKind(key) == "fonts" {
		return "asset:fonts"
	}
	return "asset:" + strings.Split(key, "/")[1]
}

// tryAssetLock takes the advisory lock without waiting; ok is false when a put
// holds it. A lock that cannot be released would stay on a pooled connection
// for good, so the connection is closed instead.
func (w *Worker) tryAssetLock(ctx context.Context, name string) (release func(), ok bool, err error) {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", name).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		ctx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", name); err != nil {
			conn.Hijack().Close(ctx) //nolint:errcheck
			return
		}
		conn.Release()
	}, true, nil
}

// lockedAssets is an asset store whose puts of what a job writes hold the
// object's asset lock for the put alone, so the sweep never deletes between
// asking for an object and removing it while a put of it lands. Images are
// put by a scan, under its library's lock.
type lockedAssets struct {
	store.Store
	pool *pgxpool.Pool
}

func (l lockedAssets) Put(ctx context.Context, key string, body io.Reader) error {
	if assetKind(key) == "" || assetKind(key) == "images" {
		return l.Store.Put(ctx, key, body)
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", assetLockName(key)); err != nil {
		return err
	}
	return l.Store.Put(ctx, key, body)
}

// jobAssets opens the asset store of a job's put.
func (w *Worker) jobAssets(uri string) (store.Store, error) {
	assets, err := w.Media(uri)
	if err != nil {
		return nil, err
	}
	return lockedAssets{assets, w.Pool}, nil
}

// holdScanLocks takes the scan lock of every library of the store, or none: ok
// is false when a scan holds one. release gives them back.
func (w *Worker) holdScanLocks(ctx context.Context, uri string) (release func(), ok bool, err error) {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	rows, err := conn.Query(ctx, `SELECT id::text, pg_try_advisory_lock(hashtextextended('scan:' || id::text, 0))
		FROM libraries WHERE asset_store_uri = $1`, uri)
	if err != nil {
		conn.Release()
		return nil, false, err
	}
	var taken []string
	ok = true
	for rows.Next() {
		var id string
		var got bool
		if err := rows.Scan(&id, &got); err != nil {
			rows.Close()
			err = errors.Join(err, unlockScans(ctx, conn, taken))
			return nil, false, err
		}
		if got {
			taken = append(taken, id)
		} else {
			ok = false
		}
	}
	rows.Close()
	release = func() { unlockScans(context.WithoutCancel(ctx), conn, taken) } //nolint:errcheck
	if err := rows.Err(); err != nil || !ok {
		release()
		return nil, false, err
	}
	return release, true, nil
}

// unlockScans releases the locks and the connection; a lock that cannot be
// released would stay on a pooled connection for good, so the connection is
// closed instead.
func unlockScans(ctx context.Context, conn *pgxpool.Conn, ids []string) error {
	for _, id := range ids {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended('scan:' || $1::text, 0))", id); err != nil {
			conn.Hijack().Close(ctx) //nolint:errcheck
			return err
		}
	}
	conn.Release()
	return nil
}

// assetWanted asks Postgres about one object: whether the item names the image,
// whether the source's current version has the object, and whether a job of the
// source (for a font, any fonts job) is writing it or ended since the sweep began.
func (w *Worker) assetWanted(ctx context.Context, kind, id, key string, began time.Time) (bool, error) {
	if kind == "fonts" {
		var running bool
		err := w.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM subtitle_jobs WHERE kind = 'fonts'
			AND (state IN ('queued', 'running') OR ended_at >= $1))`, began).Scan(&running)
		return running, err
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return false, nil // No item or source has this id.
	}
	if kind == "images" {
		var named bool
		err := w.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM items WHERE id = $1 AND primary_image_key = $2)", uid, key).Scan(&named)
		return named, err
	}
	rows, err := w.Pool.Query(ctx, `SELECT kind FROM subtitle_jobs WHERE source_id = $1
		AND (state IN ('queued', 'running') OR (kind = 'fonts' AND ended_at >= $2))`, uid, began)
	if err != nil {
		return false, err
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	for _, kind := range kinds {
		if strings.HasPrefix(key, jobPrefix(kind, id)) {
			return true, nil
		}
	}
	src := items.Source{ID: id}
	var streams []media.MediaStream
	err = w.Pool.QueryRow(ctx, "SELECT object_etag, streams FROM media_sources WHERE id = $1", uid).Scan(&src.ETag, &streams)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, stream := range streams {
		if stream.Type == "Subtitle" && (key == subtitles.Key(src, stream.Index) || key == subtitles.SupKey(src, stream.Index)) {
			return true, nil
		}
	}
	return key == subtitles.FontsKey(src.ID, src.ETag) || path.Dir(key) == library.ChapterDir(src.ID, src.ETag), nil
}

func busyKey(prefixes []string, key string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// readManifest returns the hash of each font a fonts manifest names. A manifest
// that is gone names none, and one that does not parse is an error: the job
// would replace it, but this sweep cannot say what it named.
func readManifest(ctx context.Context, assets store.Store, key string) ([]string, error) {
	body, err := assets.Open(ctx, key, 0)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var listed map[int]string
	if err := json.NewDecoder(body).Decode(&listed); err != nil {
		return nil, err
	}
	sums := make([]string, 0, len(listed))
	for _, sum := range listed {
		sums = append(sums, sum)
	}
	return sums, nil
}
