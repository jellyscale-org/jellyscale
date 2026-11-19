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

	"jellyscale/internal/db"
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
	locks := &assetLocks{pool: w.Pool}
	defer locks.close()
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
		if err := w.sweepAssetStore(ctx, locks, uri, assets, listed[uri], state); err != nil {
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

func (w *Worker) sweepAssetStore(ctx context.Context, locks *assetLocks, uri string, assets store.Store, objects []store.Object, state assetState) error {
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
		if gone, failure = w.deleteAsset(ctx, locks, uri, assets, object.Key, state.began); failure != nil {
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
func (w *Worker) deleteAsset(ctx context.Context, locks *assetLocks, uri string, assets store.Store, key string, began time.Time) (bool, error) {
	kind := assetKind(key)
	parts := strings.Split(key, "/")
	lock, err := locks.get(ctx)
	if err != nil {
		return false, err
	}
	defer locks.unlockAll(ctx)
	var ok bool
	if kind == "images" {
		ok, err = tryScanLocks(ctx, lock, uri)
	} else {
		ok, err = lock.Try(ctx, assetLockName(key))
	}
	if err != nil || !ok {
		return false, err
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

// assetLocks is the one connection a sweep takes its delete locks on, however
// many objects it deletes: opened when the first is needed, outside the pool, so
// that the pool stays whole for the questions asked while a lock is held.
type assetLocks struct {
	pool *pgxpool.Pool
	lock *db.Lock
}

func (a *assetLocks) get(ctx context.Context) (*db.Lock, error) {
	if a.lock == nil {
		lock, err := db.NewLock(ctx, a.pool)
		if err != nil {
			return nil, err
		}
		a.lock = lock
	}
	return a.lock, nil
}

// unlockAll gives back what the last object took, so that nothing is held into
// the next. A connection that cannot do it is closed, which ends the locks, and
// the next object opens another.
func (a *assetLocks) unlockAll(ctx context.Context) {
	if err := a.lock.UnlockAll(context.WithoutCancel(ctx)); err != nil {
		a.close()
	}
}

func (a *assetLocks) close() {
	if a.lock != nil {
		a.lock.Release()
		a.lock = nil
	}
}

// tryScanLocks takes the scan lock of every library of the store, or none: ok
// is false when a scan holds one. What it took before failing is the caller's to
// give back.
func tryScanLocks(ctx context.Context, lock *db.Lock, uri string) (ok bool, err error) {
	rows, err := lock.Conn().Query(ctx, `SELECT pg_try_advisory_lock(hashtextextended('scan:' || id::text, 0))
		FROM libraries WHERE asset_store_uri = $1`, uri)
	if err != nil {
		return false, err
	}
	ok = true
	for rows.Next() {
		var got bool
		if err := rows.Scan(&got); err != nil {
			rows.Close()
			return false, err
		}
		ok = ok && got
	}
	rows.Close()
	return ok && rows.Err() == nil, rows.Err()
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
