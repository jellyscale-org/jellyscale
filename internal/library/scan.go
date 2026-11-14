package library

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // Sidecar decoding.
	_ "image/png"  // Sidecar decoding.
	"io"
	"log/slog"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/media"
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

// ProbeVersion is the version of the probe: what a probe finds in a file, and
// how that is stored. Each media source stores the version of the probe that
// last wrote it, and a scan probes a source again, though its file is the same,
// when it was written by an older one. Raise it when a release probes more or
// differently (a field it reads, the label it gives one), and every source is
// brought up to date once. Sources stored before it existed are 0.
const ProbeVersion = 1

const probeConcurrency = 4

// Config declares a library. Libraries are configured, not created by clients,
// because the dashboard can't express per-library storage.
type Config struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Media  string `json:"media"`
	Assets string `json:"assets"`
	// ChapterImages makes a picture of each chapter of the library's films and
	// episodes (a worker does, after the scan). Off by default: it reads a frame
	// at every chapter, which seeks into every file in the store.
	ChapterImages bool `json:"chapterImages"`
}

// Library is a configured library with its stores opened.
type Library struct {
	ID     string
	Name   string
	Kind   string
	Media  store.Store
	Assets store.Store
}

// Sync upserts configured libraries and marks the ones left out of config.
// A library is the row its config entry finds: by its name or, when the name
// is new, by its media store and kind, which is what lets a rename keep the
// library's id and everything in it. A library new to the database gets the
// hash of its name as its id, which is the id libraries have always had, or a
// random one if a renamed library holds that. Replicas starting together take
// turns under an advisory lock, so they agree on every row.
func Sync(ctx context.Context, pool *pgxpool.Pool, configs []Config) ([]Library, error) {
	libraries := make([]Library, 0, len(configs))
	names := make([]string, 0, len(configs))
	for _, config := range configs {
		if config.Kind != "movies" && config.Kind != "tvshows" {
			return nil, fmt.Errorf("library %q: unsupported kind %q", config.Name, config.Kind)
		}
		media, err := store.Open(config.Media)
		if err != nil {
			return nil, fmt.Errorf("library %q media: %w", config.Name, err)
		}
		assets, err := store.Open(config.Assets)
		if err != nil {
			return nil, fmt.Errorf("library %q assets: %w", config.Name, err)
		}
		libraries = append(libraries, Library{Name: config.Name, Kind: config.Kind, Media: media, Assets: assets})
		names = append(names, config.Name)
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('libraries', 0))"); err != nil {
			return err
		}
		ids := []string{}
		for i, config := range configs {
			var id string
			err := tx.QueryRow(ctx, "SELECT id::text FROM libraries WHERE name = $1 ORDER BY created_at, id LIMIT 1", config.Name).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				// Renamed, if a library of this store is left that no entry names.
				err = tx.QueryRow(ctx, `SELECT id::text FROM libraries WHERE media_store_uri = $1 AND kind = $2
					AND name <> ALL($3) AND id::text <> ALL($4) ORDER BY created_at, id LIMIT 1`,
					config.Media, config.Kind, names, ids).Scan(&id)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				id = uuid.UUID(md5.Sum([]byte("library:" + config.Name))).String()
				var taken bool
				if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM libraries WHERE id = $1)", id).Scan(&taken); err != nil {
					return err
				}
				if taken {
					id = uuid.NewString()
				}
			} else if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri, chapter_images)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (id) DO UPDATE SET name = $2, kind = $3, media_store_uri = $4, asset_store_uri = $5, chapter_images = $6, gone_at = NULL`,
				id, config.Name, config.Kind, config.Media, config.Assets, config.ChapterImages)
			if err != nil {
				return err
			}
			libraries[i].ID = id
			ids = append(ids, id)
		}
		// The first start without a library stamps it; later ones keep that time.
		_, err := tx.Exec(ctx, "UPDATE libraries SET gone_at = COALESCE(gone_at, now()) WHERE id::text <> ALL($1)", ids)
		return err
	})
	if err != nil {
		return nil, err
	}
	return libraries, nil
}

// RemoveGone deletes the libraries that have been out of config for longer
// than grace, with their items and everyone's watch state. A worker calls it
// on every tick of its scan loop, which is one cheap query when nothing is due.
func RemoveGone(ctx context.Context, pool *pgxpool.Pool, grace time.Duration) error {
	_, err := pool.Exec(ctx, "DELETE FROM libraries WHERE gone_at <= now() - make_interval(secs => $1)", grace.Seconds())
	return err
}

// Scanner reconciles a library's items with its media store.
type Scanner struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// Grace is how long an item whose file is missing stays, hidden, with its
	// watch state, before a scan removes it. It runs on Postgres's clock from the
	// first scan that missed the file.
	Grace time.Duration
	// MaxLostShare is the most of a library's stored files a scan may find
	// missing before it fails and changes nothing.
	MaxLostShare float64
}

// DefaultGrace and DefaultMaxLostShare are what the config's missingGrace and
// maxLostShare default to.
const (
	DefaultGrace        = 7 * 24 * time.Hour
	DefaultMaxLostShare = 0.2
)

type seenObject struct {
	size   int64
	etag   string
	itemID *string
	// sidecars is the fingerprint of the subtitle files read at the last scan,
	// kept with the media source row; nil if the scan was before they were read,
	// or an older version's scan has rewritten the row since.
	sidecars *string
	// older: the probe that wrote the source is older than this binary's
	// (ProbeVersion), so the file is probed again though it is unchanged. Its
	// keyframe index and video end are kept when it has one (hasIndex), which a
	// probe again does not read.
	older, hasIndex bool
	// indexUnread: the index of a copied codec in mkv or mp4 is NULL, because a
	// read of it failed. The next scan reads it, with a probe.
	indexUnread bool
	// imageCuesUnread: a Matroska film whose image subtitle tracks' cue points
	// were never read (media_sources.image_cues is NULL). The next scan reads
	// them, from the file's Cues alone and not by probing the film again.
	imageCuesUnread bool
}

// ErrElsewhere is what Scan returns when another replica is scanning the
// library: nothing was scanned here.
var ErrElsewhere = errors.New("the library is being scanned by another replica")

// Scan lists the store and applies the difference. A session advisory lock
// keeps one scan per library across all worker replicas; the one that loses
// gets ErrElsewhere.
func (s *Scanner) Scan(ctx context.Context, lib Library) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1, 0))", "scan:"+lib.ID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		s.Log.Info("scan already running elsewhere", "library", lib.Name)
		return ErrElsewhere
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "scan:"+lib.ID) //nolint:errcheck

	objects, err := lib.Media.List(ctx, "")
	if err != nil {
		return err
	}
	sidecars := map[string]string{}
	subtitleFiles := map[string][]store.Object{} // By directory.
	current := map[string]bool{}
	var videos []store.Object
	for _, object := range objects {
		if IsVideo(object.Key) {
			current[object.Key] = true
			videos = append(videos, object)
			continue
		}
		name := strings.ToLower(path.Base(object.Key))
		if name == "poster.jpg" || name == "folder.jpg" || name == "poster.png" {
			sidecars[path.Dir(object.Key)] = object.Key
		}
		if IsSubtitleFile(object.Key) {
			subtitleFiles[path.Dir(object.Key)] = append(subtitleFiles[path.Dir(object.Key)], object)
		}
	}
	hidden, err := s.loadHidden(ctx, lib.ID)
	if err != nil {
		return err
	}
	// A hidden item whose file is listed again as it was needs nothing but to be
	// shown; it then goes through the scan like any item that was never hidden.
	if err := s.restore(ctx, lib.ID, videos, hidden); err != nil {
		return err
	}
	seen, err := s.loadSeen(ctx, lib.ID)
	if err != nil {
		return err
	}

	// A video is probed again when it changed. When only its subtitle files
	// did, the film isn't read again: its tracks are listed anew from them.
	sets := map[string]sidecarSet{}
	var changed, refreshed, cuesUnread []store.Object
	keepIndex := map[string]bool{} // Keys in changed whose stored index is kept.
	for _, object := range videos {
		found := sidecarsOf(object.Key, subtitleFiles[path.Dir(object.Key)])
		sets[object.Key] = found
		old, ok := seen[object.Key]
		switch {
		case !ok || old.etag != object.ETag || old.size != object.Size || old.indexUnread:
			changed = append(changed, object)
		case old.older:
			changed = append(changed, object)
			if old.hasIndex {
				keepIndex[object.Key] = true
			}
		case old.itemID != nil && (old.sidecars == nil || *old.sidecars != found.fingerprint):
			refreshed = append(refreshed, object)
			if old.imageCuesUnread {
				cuesUnread = append(cuesUnread, object)
			}
		case old.imageCuesUnread:
			cuesUnread = append(cuesUnread, object)
		}
	}
	// What the listing lacks: the items of keys it no longer shows, and those
	// already hidden. A hidden item whose key is listed is not lost; its file
	// came back changed.
	pool := &lostPool{bySize: map[int64][]*lostItem{}, claims: map[string]*lostItem{}}
	stored := 0
	for key, old := range seen {
		if old.itemID == nil {
			continue
		}
		stored++
		if !current[key] {
			pool.add(&lostItem{key: key, itemID: *old.itemID, size: old.size, etag: old.etag})
		}
	}
	for key, item := range hidden {
		stored++
		if !current[key] {
			pool.add(item)
		}
	}
	// A store that lists no video at all, for a library that has some, is far
	// more likely an empty mount point (a volume that isn't mounted under a
	// directory that is still there) than a library emptied on purpose: keep
	// the films and their watch state, and say so.
	if len(videos) == 0 && stored > 0 {
		return fmt.Errorf("library %s: the media store lists no video, but the library has %d stored; keeping them (is the volume mounted?)", lib.Name, stored)
	}

	// A new file that has the size and ETag of a lost item is that item, moved.
	// Those are settled first, so that a file with the item's ETag is not
	// beaten to it by another whose bytes only look the same.
	candidates := map[string]bool{} // Keys of files no item has been stored for.
	for _, object := range changed {
		if _, ok := seen[object.Key]; ok || hidden[object.Key] != nil {
			continue
		}
		candidates[object.Key] = true
		if item := pool.sameFile(object); item != nil {
			pool.claim(item, object.Key)
		}
	}

	// Each film is stored as its probe finishes, so a long first scan shows
	// progress and a restart keeps the films done.
	if len(changed) > 0 {
		s.Log.Info("probing films", "library", lib.Name, "films", len(changed), "index kept", len(keepIndex))
	}
	done := map[string]bool{}
	for result := range s.probeAll(ctx, lib, changed, keepIndex) {
		object, probe := result.object, result.probe
		// A file that moved keeps its item, and so its watch state; so does one
		// that comes back to the key of a hidden item, changed.
		var keep *string
		var moved *lostItem
		if old, ok := seen[object.Key]; ok {
			keep = old.itemID
		} else if item := hidden[object.Key]; item != nil {
			keep = &item.itemID
		} else if candidates[object.Key] {
			if moved = pool.claims[object.Key]; moved == nil {
				moved = pool.match(ctx, s, object, probe)
			}
			if moved != nil {
				keep = &moved.itemID
			}
		}
		if keepIndex[object.Key] {
			if err := s.keepIndex(ctx, *keep, &probe); err != nil {
				s.Log.Error("reading the stored keyframe index failed", "key", object.Key, "error", err)
				continue
			}
		}
		ok, err := s.upsert(ctx, lib, object, probe, sidecars, s.describe(ctx, lib.Media, sets[object.Key]), keep)
		if err != nil {
			s.Log.Error("store item failed", "key", object.Key, "error", err)
			continue
		}
		if !ok {
			continue
		}
		done[object.Key] = true
		if moved != nil {
			moved.taken = true
			if !moved.hidden {
				if err := s.dropSeen(ctx, lib.ID, moved.key); err != nil {
					return err
				}
			}
		}
		if len(done)%100 == 0 {
			s.Log.Info("films probed", "library", lib.Name, "done", len(done), "of", len(changed))
		}
	}
	for _, object := range cuesUnread {
		if err := s.refreshImageCues(ctx, lib, object, *seen[object.Key].itemID); err != nil {
			s.Log.Warn("reading the image tracks' cue points failed; the next scan tries again", "key", object.Key, "error", err)
		}
	}
	for _, object := range refreshed {
		if err := s.refreshSidecars(ctx, lib.ID, object, *seen[object.Key].itemID, s.describe(ctx, lib.Media, sets[object.Key])); err != nil {
			s.Log.Error("reading subtitle files failed", "key", object.Key, "error", err)
		}
	}

	// A scan that would lose most of a library fails like the empty mount does,
	// before it hides or removes anything: the likelier cause is a disk or a
	// mount that is away. The files hidden by earlier scans count, so that
	// several small losses can't add up to a big one unseen.
	lost := pool.lost()
	if float64(len(lost)) > s.MaxLostShare*float64(stored) {
		return fmt.Errorf("library %s: %d of its %d stored files are missing from the media store, more than the share allowed (maxLostShare %v); keeping them (is a disk or a mount away?)",
			lib.Name, len(lost), stored, s.MaxLostShare)
	}
	// A file that is listed and could not be read may be a lost item's, so an
	// item of its size is kept for a later scan to match.
	unread := map[int64]bool{}
	for _, object := range changed {
		if !done[object.Key] {
			unread[object.Size] = true
		}
	}
	removed := 0
	for _, item := range lost {
		switch {
		case !item.hidden:
			if err := s.hide(ctx, lib.ID, item); err != nil {
				return err
			}
		case item.expired && !unread[item.size]:
			if _, err := s.Pool.Exec(ctx, "DELETE FROM items WHERE id = $1", item.itemID); err != nil {
				return err
			}
			removed++
		}
	}
	for key, old := range seen {
		if old.itemID == nil && !current[key] {
			if err := s.dropSeen(ctx, lib.ID, key); err != nil {
				return err
			}
		}
	}
	// A series is empty only once its empty seasons are gone, which one
	// statement can't see, so the second pass catches those.
	if _, err := s.Pool.Exec(ctx, `DELETE FROM items parent WHERE library_id = $1 AND type IN ('Season', 'Series')
		AND NOT EXISTS (SELECT 1 FROM items child WHERE child.parent_id = parent.id)`, lib.ID); err != nil {
		return err
	}
	if _, err := s.Pool.Exec(ctx, `DELETE FROM items parent WHERE library_id = $1 AND type = 'Series'
		AND NOT EXISTS (SELECT 1 FROM items child WHERE child.parent_id = parent.id)`, lib.ID); err != nil {
		return err
	}
	if err := s.stampParents(ctx, lib.ID); err != nil {
		return err
	}
	// Only jobs are queued here; a worker makes the pictures and deletes the
	// ones not wanted, whether the option is on or off.
	if err := EnqueueChapterImages(ctx, s.Pool, lib.ID); err != nil {
		return err
	}
	s.Log.Info("scan finished", "library", lib.Name, "videos", len(current), "changed", len(changed), "sidecars", len(refreshed), "hidden", len(lost)-removed, "removed", removed)
	return nil
}

// lostItem is an item the listing lacks: one whose key it no longer shows, or
// one an earlier scan hid.
type lostItem struct {
	key, itemID string
	size        int64
	etag        string
	hidden      bool // Hidden by an earlier scan.
	expired     bool // Hidden for longer than the grace.
	// claimed: a new file has been set aside for the item; taken: one has been
	// stored as it.
	claimed, taken bool
	look           string // The fingerprint, once read.
	read           bool
}

// lostPool holds the lost items a new file may be.
type lostPool struct {
	all    []*lostItem
	bySize map[int64][]*lostItem
	claims map[string]*lostItem // By the key of the file.
}

func (p *lostPool) add(item *lostItem) {
	p.all = append(p.all, item)
	p.bySize[item.size] = append(p.bySize[item.size], item)
}

func (p *lostPool) claim(item *lostItem, key string) {
	item.claimed = true
	p.claims[key] = item
}

// sameFile returns an item nothing has claimed with the object's size and ETag.
func (p *lostPool) sameFile(object store.Object) *lostItem {
	for _, item := range p.bySize[object.Size] {
		if !item.claimed && item.etag == object.ETag {
			return item
		}
	}
	return nil
}

// match gives a probed file the item of the same size and fingerprint that no
// other file has, which is how bytes that were copied or uploaded again (and so
// have another ETag) find their item. Size alone never matches.
func (p *lostPool) match(ctx context.Context, s *Scanner, object store.Object, probe Probe) *lostItem {
	want := fingerprint(probe.Container, probe.RunTimeTicks, probe.Streams)
	for _, item := range p.bySize[object.Size] {
		if item.claimed {
			continue
		}
		if !item.read {
			var err error
			if item.look, err = s.fingerprintOf(ctx, item.itemID); err != nil {
				s.Log.Warn("reading a lost film's fingerprint failed", "key", item.key, "error", err)
				continue
			}
			item.read = true
		}
		if item.look == want {
			p.claim(item, object.Key)
			return item
		}
	}
	return nil
}

// lost lists the items no file was stored for.
func (p *lostPool) lost() []*lostItem {
	var lost []*lostItem
	for _, item := range p.all {
		if !item.taken {
			lost = append(lost, item)
		}
	}
	return lost
}

// fingerprint says what a file is apart from its name, date and ETag: its
// container, length and the layout of its own tracks (not the subtitle files
// beside it, which move apart from it). It is made alike from a new probe and
// from the row stored for the item, so it uses only fields a scan has always
// stored, and a field a stream lacks counts as zero on both sides.
func fingerprint(container string, ticks int64, streams []media.MediaStream) string {
	hash := sha256.New()
	fmt.Fprint(hash, container, ticks)
	for _, stream := range streams {
		if stream.IsExternal || stream.Type == "Attachment" {
			continue
		}
		fmt.Fprintf(hash, "|%s|%s|%d|%d|%d|%d\n", stream.Type, stream.Codec, stream.Width, stream.Height, stream.Channels, stream.SampleRate)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// fingerprintOf is the fingerprint of an item's stored file.
func (s *Scanner) fingerprintOf(ctx context.Context, itemID string) (string, error) {
	var container string
	var ticks int64
	var raw []byte
	err := s.Pool.QueryRow(ctx, "SELECT m.container, COALESCE(i.run_time_ticks, 0), m.streams FROM media_sources m JOIN items i ON i.id = m.item_id WHERE i.id = $1", itemID).
		Scan(&container, &ticks, &raw)
	if err != nil {
		return "", err
	}
	var streams []media.MediaStream
	if err := json.Unmarshal(raw, &streams); err != nil {
		return "", err
	}
	return fingerprint(container, ticks, streams), nil
}

// loadHidden lists the items an earlier scan hid, by the key of their file.
// Their scan_objects rows were dropped when they were hidden, so a scan of an
// earlier release that knows nothing of hiding finds nothing to delete.
func (s *Scanner) loadHidden(ctx context.Context, libraryID string) (map[string]*lostItem, error) {
	rows, err := s.Pool.Query(ctx, `SELECT m.store_key, i.id::text, m.size, m.object_etag,
			i.missing_since <= now() - make_interval(secs => $2)
		FROM items i JOIN media_sources m ON m.item_id = i.id WHERE i.library_id = $1 AND i.missing_since IS NOT NULL`,
		libraryID, s.Grace.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hidden := map[string]*lostItem{}
	for rows.Next() {
		item := &lostItem{hidden: true}
		if err := rows.Scan(&item.key, &item.itemID, &item.size, &item.etag, &item.expired); err != nil {
			return nil, err
		}
		hidden[item.key] = item
	}
	return hidden, rows.Err()
}

// restore shows again the hidden items whose file is listed as it was.
func (s *Scanner) restore(ctx context.Context, libraryID string, videos []store.Object, hidden map[string]*lostItem) error {
	for _, object := range videos {
		item := hidden[object.Key]
		if item == nil || item.size != object.Size || item.etag != object.ETag {
			continue
		}
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "UPDATE items SET missing_since = NULL WHERE id = $1", item.itemID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO scan_objects (library_id, key, size, etag, item_id) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (library_id, key) DO UPDATE SET size = $3, etag = $4, item_id = $5`, libraryID, object.Key, object.Size, object.ETag, item.itemID)
			return err
		})
		if err != nil {
			return err
		}
		delete(hidden, object.Key)
	}
	return s.stampParents(ctx, libraryID)
}

// hide stamps an item whose file is missing, and drops what the scan knew of
// the file. Everything else of the item stays: its watch state, its media source.
func (s *Scanner) hide(ctx context.Context, libraryID string, item *lostItem) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE items SET missing_since = COALESCE(missing_since, now()) WHERE id = $1", item.itemID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM scan_objects WHERE library_id = $1 AND key = $2", libraryID, item.key)
		return err
	})
}

// stampParents hides the seasons and series that have no episode shown, and
// shows the others, seasons first for the series' sake. A hidden parent keeps
// its id and its own watch state.
func (s *Scanner) stampParents(ctx context.Context, libraryID string) error {
	for _, kind := range []string{"Season", "Series"} {
		if _, err := s.Pool.Exec(ctx, `UPDATE items parent SET missing_since = NULL WHERE library_id = $1 AND type = $2
			AND missing_since IS NOT NULL AND EXISTS (SELECT 1 FROM items child WHERE child.parent_id = parent.id AND child.missing_since IS NULL)`, libraryID, kind); err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE items parent SET missing_since = now() WHERE library_id = $1 AND type = $2
			AND missing_since IS NULL AND NOT EXISTS (SELECT 1 FROM items child WHERE child.parent_id = parent.id AND child.missing_since IS NULL)`, libraryID, kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scanner) loadSeen(ctx context.Context, libraryID string) (map[string]seenObject, error) {
	// A source is probed again when the probe that wrote it is older than this
	// binary's, whatever it holds. What a failed read left unset is retried
	// apart from that: the keyframe index of a copied codec, and the cue points
	// of the image tracks of a Matroska film.
	rows, err := s.Pool.Query(ctx, `SELECT o.key, o.size, o.etag, o.item_id::text, m.sidecars,
			COALESCE(m.probe_version < $2, false),
			COALESCE(m.keyframes IS NOT NULL, false),
			COALESCE(m.keyframes IS NULL AND m.container IN ('mkv', 'mp4')
				AND EXISTS (SELECT 1 FROM jsonb_array_elements(m.streams) v WHERE v->>'Type' = 'Video' AND v->>'Codec' = ANY($3)), false),
			COALESCE(m.image_cues IS NULL AND m.container = 'mkv', false)
		FROM scan_objects o LEFT JOIN media_sources m ON m.item_id = o.item_id WHERE o.library_id = $1`, libraryID, ProbeVersion, playback.CopyCodecs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]seenObject{}
	for rows.Next() {
		var key string
		var old seenObject
		if err := rows.Scan(&key, &old.size, &old.etag, &old.itemID, &old.sidecars, &old.older, &old.hasIndex, &old.indexUnread, &old.imageCuesUnread); err != nil {
			return nil, err
		}
		seen[key] = old
	}
	return seen, rows.Err()
}

func (s *Scanner) dropSeen(ctx context.Context, libraryID, key string) error {
	_, err := s.Pool.Exec(ctx, "DELETE FROM scan_objects WHERE library_id = $1 AND key = $2", libraryID, key)
	return err
}

type probed struct {
	object store.Object
	probe  Probe
}

// probeAll probes objects four at a time and sends each result as it is done,
// closing the channel after the last. Those in keepIndex are not read for a
// keyframe index: the caller has one. Failed probes are logged and not sent.
// The channel holds every result, so a caller that stops reading leaks nothing.
func (s *Scanner) probeAll(ctx context.Context, lib Library, objects []store.Object, keepIndex map[string]bool) <-chan probed {
	results := make(chan probed, len(objects))
	var wg sync.WaitGroup
	limit := make(chan struct{}, probeConcurrency)
	go func() {
		for _, object := range objects {
			wg.Add(1)
			limit <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-limit }()
				input, closeInput, err := store.Input(ctx, lib.Media, object.Key, object.Size)
				if err != nil {
					s.Log.Error("open input failed", "key", object.Key, "error", err)
					return
				}
				defer closeInput()
				probe, err := ProbeInput(ctx, input)
				if err != nil {
					s.Log.Error("probe failed", "key", object.Key, "error", err)
					return
				}
				if probe.wantsKeyframes() && !keepIndex[object.Key] {
					s.readIndex(ctx, lib, object, input, &probe)
				}
				if probe.Container == "mkv" {
					probe.ImageCues = s.readImageCues(ctx, lib, object, probe.Streams)
				}
				results <- probed{object, probe}
			}()
		}
		wg.Wait()
		close(results)
	}()
	return results
}

// keepIndex gives probe the keyframe index and video end already stored for an
// item, for a film that is probed again only to list its attachments.
func (s *Scanner) keepIndex(ctx context.Context, itemID string, probe *Probe) error {
	return s.Pool.QueryRow(ctx, "SELECT keyframes, video_end FROM media_sources WHERE item_id = $1", itemID).Scan(&probe.Keyframes, &probe.VideoEnd)
}

// readIndex fills in what a copied stream is cut from. A file with no index
// gets an empty one and is re-encoded; a failed read leaves it unset, so the
// next scan tries again.
func (s *Scanner) readIndex(ctx context.Context, lib Library, object store.Object, input string, probe *Probe) {
	keyframes, err := ReadKeyframes(ctx, lib.Media, object.Key, object.Size, probe.Container)
	if err == nil {
		// Reading the video's end costs the film from the last keyframe on,
		// twice. A longer gap can't be cut into segments, so don't pay for it.
		last := keyframes[len(keyframes)-1]
		if probe.RunTimeTicks-last > playback.MaxCopySegment {
			err = noIndex("the last keyframe is more than 12 seconds before the end")
		} else {
			probe.VideoEnd, err = ReadVideoEnd(ctx, input, last)
		}
	}
	switch {
	case errors.Is(err, ErrNoIndex):
		probe.Keyframes = []int64{}
		s.Log.Info("the film has no keyframe index to copy from and is re-encoded", "key", object.Key, "why", err)
	case err != nil:
		s.Log.Warn("reading the keyframe index failed; the next scan tries again", "key", object.Key, "error", err)
	default:
		probe.Keyframes = keyframes
	}
}

// readImageCues reads where the packets of the image tracks of a Matroska film
// lie, for those of the streams that are image tracks inside the film. A failed
// read gives nil, so that the next scan tries again.
func (s *Scanner) readImageCues(ctx context.Context, lib Library, object store.Object, streams []media.MediaStream) ImageCues {
	cues := ImageCues{}
	image := false
	for _, stream := range streams {
		image = image || isFilmImageTrack(stream)
	}
	if !image {
		return cues
	}
	found, err := ReadImageCues(ctx, lib.Media, object.Key, object.Size)
	if err != nil {
		s.Log.Warn("reading the image tracks' cue points failed; the next scan tries again", "key", object.Key, "error", err)
		return nil
	}
	for _, stream := range streams {
		if track, ok := found[stream.Index]; ok && isFilmImageTrack(stream) {
			cues[stream.Index] = track
		}
	}
	return cues
}

// isFilmImageTrack says whether a stream is a bitmap subtitle track inside the
// film, not a file beside it.
func isFilmImageTrack(stream media.MediaStream) bool {
	return stream.Type == "Subtitle" && stream.IsImage() && stream.Fingerprint == ""
}

// refreshImageCues stores the cue points of a film that was scanned without
// them, and whose file has not changed since: only the head and the Cues of
// the film are read.
func (s *Scanner) refreshImageCues(ctx context.Context, lib Library, object store.Object, itemID string) error {
	var raw []byte
	if err := s.Pool.QueryRow(ctx, "SELECT streams FROM media_sources WHERE item_id = $1", itemID).Scan(&raw); err != nil {
		return err
	}
	var streams []media.MediaStream
	if err := json.Unmarshal(raw, &streams); err != nil {
		return err
	}
	cues := s.readImageCues(ctx, lib, object, streams)
	if cues == nil {
		return errors.New("the read failed")
	}
	// Only if the film is still the one that was read.
	_, err := s.Pool.Exec(ctx, "UPDATE media_sources SET image_cues = $2 WHERE item_id = $1 AND object_etag = $3", itemID, cues, object.ETag)
	return err
}

// record is one row of the items table.
type record struct {
	id, parentID, seriesID, seasonID *string
	kind, name, naturalKey           string
	year, index, parentIndex         *int
	ticks                            *int64
	imageKey, imageTag               *string
	aspect                           float64 // Of the image.
	dto                              map[string]any
}

// upsert stores the file as an item; false if it is not one (a show's file with no season and episode).
func (s *Scanner) upsert(ctx context.Context, lib Library, object store.Object, probe Probe, sidecars map[string]string, subtitleFiles sidecarSet, keep *string) (bool, error) {
	var item record
	switch lib.Kind {
	case "movies":
		movie := ParseMovie(object.Key)
		item = record{kind: "Movie", name: movie.Title, naturalKey: "video:" + object.Key}
		if movie.Year > 0 {
			item.year = &movie.Year
		}
	case "tvshows":
		episode, ok := ParseEpisode(object.Key)
		if !ok {
			s.Log.Warn("skipping file with no season and episode", "key", object.Key)
			return false, nil
		}
		parts := strings.SplitN(object.Key, "/", 2)
		seriesID, err := s.ensureSeries(ctx, lib, parts[0], episode.Series, sidecars)
		if err != nil {
			return false, err
		}
		seasonID, err := s.ensureSeason(ctx, lib, seriesID, episode.Series, episode.Season)
		if err != nil {
			return false, err
		}
		name := episode.Title
		if name == "" {
			name = fmt.Sprintf("Episode %d", episode.Number)
		}
		item = record{kind: "Episode", name: name, naturalKey: "video:" + object.Key,
			parentID: &seasonID, seriesID: &seriesID, seasonID: &seasonID,
			index: &episode.Number, parentIndex: &episode.Season}
	}

	id := uuid.NewString()
	if keep != nil {
		id = *keep
	}
	item.id = &id
	item.ticks = &probe.RunTimeTicks

	if key, ok := sidecars[path.Dir(object.Key)]; ok && lib.Kind == "movies" {
		if err := s.copySidecar(ctx, lib, &item, key); err != nil {
			s.Log.Warn("sidecar image failed", "key", key, "error", err)
		}
	}
	if item.imageKey == nil {
		if err := s.extractFrame(ctx, lib, &item, object, probe); err != nil {
			s.Log.Warn("frame extraction failed", "key", object.Key, "error", err)
		}
	}

	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The film's own tracks keep their indices; the files beside it follow,
		// and a file that was listed before keeps the index it had.
		var raw []byte
		var counter int
		err := tx.QueryRow(ctx, `SELECT m.streams, m.next_index FROM media_sources m JOIN items i ON i.id = m.item_id
			WHERE i.id = $1::uuid OR (i.library_id = $2 AND i.natural_key = $3) FOR UPDATE OF m`, keep, lib.ID, item.naturalKey).Scan(&raw, &counter)
		var old []media.MediaStream
		if err == nil {
			err = json.Unmarshal(raw, &old)
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		external, next := externalStreams(object, subtitleFiles, old, nextIndex(probe.Streams), counter)
		probe.Streams = append(probe.Streams, external...)
		slices.SortFunc(probe.Streams, func(a, b media.MediaStream) int { return a.Index - b.Index })
		item.dto = videoDTO(item, object, probe)
		if err := writeItem(ctx, tx, lib.ID, item); err != nil {
			return err
		}
		// A repeat scan keeps the first ID through the natural key's conflict.
		var stored string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM items WHERE library_id = $1 AND natural_key = $2",
			lib.ID, item.naturalKey).Scan(&stored); err != nil {
			return err
		}
		streams, err := json.Marshal(probe.Streams)
		if err != nil {
			return err
		}
		chapters, err := json.Marshal(probe.Chapters)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM media_sources WHERE item_id = $1", stored); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end, next_index, sidecars, chapters, image_cues, probe_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			stored, stored, object.Key, object.Size, object.ETag, probe.Container, streams, probe.Keyframes, probe.VideoEnd, next, subtitleFiles.fingerprint, chapters, probe.ImageCues, ProbeVersion); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO scan_objects (library_id, key, size, etag, item_id) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (library_id, key) DO UPDATE SET size = $3, etag = $4, item_id = $5`,
			lib.ID, object.Key, object.Size, object.ETag, stored)
		return err
	})
	return err == nil, err
}

func writeItem(ctx context.Context, tx pgx.Tx, libraryID string, item record) error {
	dto, err := json.Marshal(item.dto)
	if err != nil {
		return err
	}
	// Writing by natural key means a repeat scan updates in place, and a moved
	// file (same id, new key, perhaps another season) updates through the id
	// conflict, shown again if it was hidden.
	_, err = tx.Exec(ctx, `INSERT INTO items (id, library_id, parent_id, series_id, season_id, type, name, sort_name,
			natural_key, production_year, run_time_ticks, index_number, parent_index_number,
			primary_image_key, primary_image_tag, dto)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET parent_id = $3, series_id = $4, season_id = $5, name = $7, sort_name = $8, natural_key = $9,
			production_year = $10, run_time_ticks = $11, index_number = $12, parent_index_number = $13,
			primary_image_key = $14, primary_image_tag = $15, dto = $16, missing_since = NULL`,
		*item.id, libraryID, item.parentID, item.seriesID, item.seasonID, item.kind, item.name, SortName(item.name),
		item.naturalKey, item.year, item.ticks, item.index, item.parentIndex, item.imageKey, item.imageTag, dto)
	return err
}

func (s *Scanner) ensureSeries(ctx context.Context, lib Library, folder, name string, sidecars map[string]string) (string, error) {
	key := "series:" + folder
	var id string
	err := s.Pool.QueryRow(ctx, "SELECT id::text FROM items WHERE library_id = $1 AND natural_key = $2", lib.ID, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	id = uuid.NewString()
	item := record{id: &id, kind: "Series", name: name, naturalKey: key}
	if sidecar, ok := sidecars[folder]; ok {
		if err := s.copySidecar(ctx, lib, &item, sidecar); err != nil {
			s.Log.Warn("sidecar image failed", "key", sidecar, "error", err)
		}
	}
	item.dto = folderDTO(item)
	return id, pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return writeItem(ctx, tx, lib.ID, item) })
}

func (s *Scanner) ensureSeason(ctx context.Context, lib Library, seriesID, seriesName string, number int) (string, error) {
	key := fmt.Sprintf("season:%s:%d", seriesID, number)
	var id string
	err := s.Pool.QueryRow(ctx, "SELECT id::text FROM items WHERE library_id = $1 AND natural_key = $2", lib.ID, key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return "", err
	}
	id = uuid.NewString()
	name := fmt.Sprintf("Season %d", number)
	if number == 0 {
		name = "Specials"
	}
	item := record{id: &id, parentID: &seriesID, seriesID: &seriesID, kind: "Season", name: name, naturalKey: key, index: &number}
	item.dto = folderDTO(item)
	item.dto["SeriesName"] = seriesName
	return id, pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return writeItem(ctx, tx, lib.ID, item) })
}

// copySidecar, extractFrame and saveImage set the item's image on success.
func (s *Scanner) copySidecar(ctx context.Context, lib Library, item *record, key string) error {
	reader, err := lib.Media.Open(ctx, key, 0)
	if err != nil {
		return err
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	return s.saveImage(ctx, lib, item, body)
}

// extractFrame captures one frame a tenth of the way in, far enough to skip
// logos and black. Metadata providers replace this in Phase 5.
func (s *Scanner) extractFrame(ctx context.Context, lib Library, item *record, object store.Object, probe Probe) error {
	input, closeInput, err := store.Input(ctx, lib.Media, object.Key, object.Size)
	if err != nil {
		return err
	}
	defer closeInput()
	at := float64(probe.RunTimeTicks) / ticksPerSecond / 10
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-ss", fmt.Sprintf("%.3f", at), "-i", input,
		"-frames:v", "1", "-vf", "scale=-2:720", "-q:v", "3", "-f", "image2pipe", "-c:v", "mjpeg", "-")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return s.saveImage(ctx, lib, item, out.Bytes())
}

func (s *Scanner) saveImage(ctx context.Context, lib Library, item *record, body []byte) error {
	config, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	digest := sha256.Sum256(body)
	tag := hex.EncodeToString(digest[:16])
	key := fmt.Sprintf("images/%s/%s", *item.id, tag)
	if err := lib.Assets.Put(ctx, key, bytes.NewReader(body)); err != nil {
		return err
	}
	item.imageKey, item.imageTag = &key, &tag
	item.aspect = float64(config.Width) / float64(config.Height)
	return nil
}

func baseDTO(item record) map[string]any {
	dto := map[string]any{
		"Name":              item.name,
		"Type":              item.kind,
		"SortName":          SortName(item.name),
		"ImageTags":         map[string]string{},
		"BackdropImageTags": []string{},
		"ImageBlurHashes":   map[string]any{},
		"ProviderIds":       map[string]string{},
		"Genres":            []string{},
		"GenreItems":        []string{},
		"Studios":           []string{},
		"People":            []string{},
		"Tags":              []string{},
		"ExternalUrls":      []string{},
		"RemoteTrailers":    []string{},
		"Taglines":          []string{},
		"LockedFields":      []string{},
		"LockData":          false,
		"CanDelete":         false,
		"CanDownload":       false,
		"LocalTrailerCount": 0,
		"PlayAccess":        "Full",
		"ChannelId":         nil,
		"LocationType":      "FileSystem",
		"DateCreated":       time.Now().UTC().Format(time.RFC3339Nano),
	}
	if item.imageTag != nil {
		dto["ImageTags"] = map[string]string{"Primary": *item.imageTag}
		dto["PrimaryImageAspectRatio"] = item.aspect
	}
	if item.year != nil {
		dto["ProductionYear"] = *item.year
	}
	if item.index != nil {
		dto["IndexNumber"] = *item.index
	}
	if item.parentIndex != nil {
		dto["ParentIndexNumber"] = *item.parentIndex
	}
	if item.parentID != nil {
		dto["ParentId"] = db.CompactID(*item.parentID)
	}
	if item.seriesID != nil {
		dto["SeriesId"] = db.CompactID(*item.seriesID)
	}
	if item.seasonID != nil {
		dto["SeasonId"] = db.CompactID(*item.seasonID)
	}
	return dto
}

func folderDTO(item record) map[string]any {
	dto := baseDTO(item)
	dto["IsFolder"] = true
	dto["Etag"] = etag(item.naturalKey)
	return dto
}

func videoDTO(item record, object store.Object, probe Probe) map[string]any {
	dto := baseDTO(item)
	streams := playableStreams(probe.Streams)
	sources := []media.MediaSource{{
		ID:                   db.CompactID(*item.id),
		Protocol:             "File",
		Type:                 "Default",
		Name:                 item.name,
		Path:                 object.Key,
		Container:            probe.Container,
		Size:                 object.Size,
		ETag:                 etag(object.ETag),
		RunTimeTicks:         probe.RunTimeTicks,
		Bitrate:              probe.Bitrate,
		VideoType:            "VideoFile",
		SupportsDirectPlay:   true,
		SupportsDirectStream: true,
		SupportsTranscoding:  true,
		SupportsProbing:      true,
		MediaStreams:         streams,
		MediaAttachments:     attachmentsOf(probe.Streams),
		Formats:              []string{},
	}}
	dto["IsFolder"] = false
	// Jellyfin sends true for a film or episode, for a user allowed to download
	// (policy EnableContentDownloading, always on here); jellyfin-web shows its
	// Download entry only then.
	dto["CanDownload"] = true
	dto["MediaType"] = "Video"
	dto["VideoType"] = "VideoFile"
	dto["Container"] = probe.Container
	dto["Path"] = object.Key
	dto["RunTimeTicks"] = probe.RunTimeTicks
	dto["Width"], dto["Height"] = probe.Width, probe.Height
	dto["IsHD"] = probe.Height >= 720
	dto["Etag"] = etag(object.ETag)
	dto["MediaSources"] = sources
	dto["MediaStreams"] = streams
	dto["Chapters"] = probe.Chapters
	dto["Trickplay"] = map[string]any{}
	return dto
}

// attachmentsOf lists the fonts among the streams, as Jellyfin lists a film's
// attachments. Others are ones no client draws.
func attachmentsOf(streams []media.MediaStream) []media.MediaAttachment {
	list := []media.MediaAttachment{}
	for _, stream := range streams {
		if stream.Type == "Attachment" && subtitles.IsFont(stream.MimeType) {
			list = append(list, media.MediaAttachment{CodecTag: "[0][0][0][0]", Index: stream.Index, FileName: stream.FileName, MimeType: stream.MimeType})
		}
	}
	return list
}

// refreshSidecars lists a film's subtitle files again when the files beside it
// changed, and nothing else: the stored probe is kept, so the film isn't read
// again, and the item keeps its image and its state. It reads no subtitle file
// and writes nothing to the asset store; a worker converts a file when somebody
// asks for it.
func (s *Scanner) refreshSidecars(ctx context.Context, libraryID string, object store.Object, itemID string, files sidecarSet) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var raw []byte
		var counter int
		if err := tx.QueryRow(ctx, "SELECT streams, next_index FROM media_sources WHERE item_id = $1 FOR UPDATE", itemID).Scan(&raw, &counter); err != nil {
			return err
		}
		var streams []media.MediaStream
		if err := json.Unmarshal(raw, &streams); err != nil {
			return err
		}
		own := slices.DeleteFunc(slices.Clone(streams), func(stream media.MediaStream) bool { return stream.IsExternal })
		external, next := externalStreams(object, files, streams, nextIndex(own), counter)
		streams = append(own, external...)
		slices.SortFunc(streams, func(a, b media.MediaStream) int { return a.Index - b.Index })
		if err := storeStreams(ctx, tx, itemID, streams, next); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE media_sources SET sidecars = $2 WHERE item_id = $1", itemID, files.fingerprint)
		return err
	})
}

// playableStreams is what can be delivered: the video, the default audio
// track, or else the first, the text subtitle tracks, which are extracted on
// request, and the image ones that are copied out or drawn into a transcode
// (imageSubtitleCodecs). Clients build their track menus from this, so anything
// else would be a dead choice.
// media_sources.streams keeps the full list, and the probe's fields for
// transcode decisions, which the DTO leaves out so direct play replies stay as
// they were.
func playableStreams(streams []media.MediaStream) []media.MediaStream {
	audio := -1
	for i, stream := range streams {
		if stream.Type != "Audio" {
			continue
		}
		if audio == -1 || stream.IsDefault {
			audio = i
		}
		if stream.IsDefault {
			break
		}
	}
	playable := make([]media.MediaStream, 0, len(streams))
	for i, stream := range streams {
		if stream.Type == "Video" {
			// What the decision needs and the item page never listed.
			stream.Level, stream.IsInterlaced, stream.DurationTicks = 0, false, 0
		}
		if stream.Type == "Subtitle" && stream.Fingerprint != "" {
			// Keys the cached track and says where the file is; no client has use for it.
			stream.Fingerprint, stream.FileSize, stream.FileETag, stream.SubPath, stream.SubSize = "", 0, "", "", 0
		}
		listed := stream.IsTextSubtitleStream || stream.IsImage()
		if stream.Type == "Video" || i == audio || (stream.Type == "Subtitle" && listed && !stream.Unreadable) {
			playable = append(playable, stream)
		}
	}
	return playable
}

func etag(value string) string {
	digest := md5.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}
