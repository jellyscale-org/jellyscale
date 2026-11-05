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
	"maps"
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
	"jellyscale/internal/playback"
	"jellyscale/internal/store"
	"jellyscale/internal/subtitles"
)

const probeConcurrency = 4

// Config declares a library. Libraries are configured, not created by clients,
// because the dashboard can't express per-library storage.
type Config struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Media  string `json:"media"`
	Assets string `json:"assets"`
}

// Library is a configured library with its stores opened.
type Library struct {
	ID     string
	Name   string
	Kind   string
	Media  store.Store
	Assets store.Store
}

// Sync upserts configured libraries. IDs derive from the name so every replica
// agrees without coordinating.
func Sync(ctx context.Context, pool *pgxpool.Pool, configs []Config) ([]Library, error) {
	libraries := make([]Library, 0, len(configs))
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
		digest := md5.Sum([]byte("library:" + config.Name))
		id := uuid.UUID(digest).String()
		_, err = pool.Exec(ctx, `INSERT INTO libraries (id, name, kind, media_store_uri, asset_store_uri)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET kind = $3, media_store_uri = $4, asset_store_uri = $5`,
			id, config.Name, config.Kind, config.Media, config.Assets)
		if err != nil {
			return nil, err
		}
		libraries = append(libraries, Library{ID: id, Name: config.Name, Kind: config.Kind, Media: media, Assets: assets})
	}
	return libraries, nil
}

// Scanner reconciles a library's items with its media store.
type Scanner struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

type seenObject struct {
	size   int64
	etag   string
	itemID *string
	// unprobed: stored before the probe recorded level, field order and range,
	// which transcode decisions need, or which subtitle tracks are text, or
	// before keyframes were read from a file in a codec that is copied. A scan probes it again.
	unprobed bool
	// sidecars is the fingerprint of the subtitle files read at the last scan,
	// kept with the media source row; nil if the scan was before they were read,
	// or an older version's scan has rewritten the row since.
	sidecars *string
	// attachmentsUnread: a Matroska film stored before its attachments were
	// listed (sidecars is NULL), the only container whose attachments matter, or
	// one whose image subtitle tracks were stored before they were listed
	// (IsImageSubtitleStream is missing). Its probe is repeated, but its
	// keyframe index and video end are kept.
	attachmentsUnread bool
	// chaptersUnread: stored before chapters were read (media_sources.chapters
	// is NULL). Probed again like attachmentsUnread, keeping the index.
	chaptersUnread bool
}

// Scan lists the store and applies the difference. A session advisory lock
// keeps one scan per library across all worker replicas.
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
		return nil
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "scan:"+lib.ID) //nolint:errcheck

	objects, err := lib.Media.List(ctx, "")
	if err != nil {
		return err
	}
	seen, err := s.loadSeen(ctx, lib.ID)
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
	// A video is probed again when it changed. When only its subtitle files
	// did, the film isn't read again: its tracks are listed anew from them.
	sets := map[string]sidecarSet{}
	var changed, refreshed []store.Object
	attachmentsOnly := map[string]bool{} // Keys in changed whose stored index is kept.
	for _, object := range videos {
		found := sidecarsOf(object.Key, subtitleFiles[path.Dir(object.Key)])
		sets[object.Key] = found
		old, ok := seen[object.Key]
		switch {
		case !ok || old.etag != object.ETag || old.size != object.Size || old.unprobed:
			changed = append(changed, object)
		case old.attachmentsUnread || old.chaptersUnread:
			changed = append(changed, object)
			attachmentsOnly[object.Key] = true
		case old.itemID != nil && (old.sidecars == nil || *old.sidecars != found.fingerprint):
			refreshed = append(refreshed, object)
		}
	}
	gone := map[string]seenObject{}
	for key, old := range seen {
		if !current[key] {
			gone[key] = old
		}
	}

	// Each film is stored as its probe finishes, so a long first scan shows
	// progress and a restart keeps the films done.
	if len(changed) > 0 {
		s.Log.Info("probing films", "library", lib.Name, "films", len(changed), "index kept", len(attachmentsOnly))
	}
	stored := 0
	for result := range s.probeAll(ctx, lib, changed, attachmentsOnly) {
		object, probe := result.object, result.probe
		// A file that moved keeps its item, and so its watch state.
		var keep *string
		if old, ok := seen[object.Key]; ok {
			keep = old.itemID
		}
		for key, old := range gone {
			if keep == nil && old.size == object.Size && old.etag == object.ETag {
				keep = old.itemID
				delete(gone, key)
				if err := s.dropSeen(ctx, lib.ID, key); err != nil {
					return err
				}
				break
			}
		}
		if attachmentsOnly[object.Key] {
			if err := s.keepIndex(ctx, *keep, &probe); err != nil {
				s.Log.Error("reading the stored keyframe index failed", "key", object.Key, "error", err)
				continue
			}
		}
		if err := s.upsert(ctx, lib, object, probe, sidecars, sets[object.Key], keep); err != nil {
			s.Log.Error("store item failed", "key", object.Key, "error", err)
			continue
		}
		if stored++; stored%100 == 0 {
			s.Log.Info("films probed", "library", lib.Name, "done", stored, "of", len(changed))
		}
	}
	for _, object := range refreshed {
		if err := s.refreshSidecars(ctx, lib.ID, object, *seen[object.Key].itemID, sets[object.Key]); err != nil {
			s.Log.Error("reading subtitle files failed", "key", object.Key, "error", err)
		}
	}

	for key, old := range gone {
		if old.itemID != nil {
			if _, err := s.Pool.Exec(ctx, "DELETE FROM items WHERE id = $1", *old.itemID); err != nil {
				return err
			}
		}
		if err := s.dropSeen(ctx, lib.ID, key); err != nil {
			return err
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
	s.Log.Info("scan finished", "library", lib.Name, "videos", len(current), "changed", len(changed), "sidecars", len(refreshed), "removed", len(gone))
	return nil
}

func (s *Scanner) loadSeen(ctx context.Context, libraryID string) (map[string]seenObject, error) {
	// A Matroska film scanned before its subtitle files and attachments were
	// listed (sidecars is NULL) is probed again to list the attachments; any
	// other container only has its subtitle files listed, which needs no probe.
	// So is a film with an image subtitle track stored before they were listed,
	// and a film stored before its chapters were read.
	rows, err := s.Pool.Query(ctx, `SELECT o.key, o.size, o.etag, o.item_id::text, m.sidecars,
			COALESCE(m.sidecars IS NULL AND m.container = 'mkv'
				OR jsonb_path_exists(m.streams, '$[*] ? (@.Type == "Subtitle" && @.IsTextSubtitleStream == false && !exists(@.IsImageSubtitleStream))'), false),
			COALESCE(jsonb_path_exists(m.streams, '$[*] ? ((@.Type == "Video" && (!exists(@.VideoRangeType) || !exists(@.ColorTransfer))) || (@.Type == "Subtitle" && !exists(@.IsTextSubtitleStream)))')
				OR (m.keyframes IS NULL AND m.container IN ('mkv', 'mp4')
					AND EXISTS (SELECT 1 FROM jsonb_array_elements(m.streams) v WHERE v->>'Type' = 'Video' AND v->>'Codec' = ANY($2))), false),
			COALESCE(m.item_id IS NOT NULL AND m.chapters IS NULL, false)
		FROM scan_objects o LEFT JOIN media_sources m ON m.item_id = o.item_id WHERE o.library_id = $1`, libraryID, playback.CopyCodecs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]seenObject{}
	for rows.Next() {
		var key string
		var old seenObject
		if err := rows.Scan(&key, &old.size, &old.etag, &old.itemID, &old.sidecars, &old.attachmentsUnread, &old.unprobed, &old.chaptersUnread); err != nil {
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

func (s *Scanner) upsert(ctx context.Context, lib Library, object store.Object, probe Probe, sidecars map[string]string, subtitleFiles sidecarSet, keep *string) error {
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
			return nil
		}
		parts := strings.SplitN(object.Key, "/", 2)
		seriesID, err := s.ensureSeries(ctx, lib, parts[0], episode.Series, sidecars)
		if err != nil {
			return err
		}
		seasonID, err := s.ensureSeason(ctx, lib, seriesID, episode.Series, episode.Season)
		if err != nil {
			return err
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

	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The film's own tracks keep their indices; the files beside it follow,
		// and a file that was listed before keeps the index it had.
		var raw []byte
		var counter int
		err := tx.QueryRow(ctx, `SELECT m.streams, m.next_index FROM media_sources m JOIN items i ON i.id = m.item_id
			WHERE i.id = $1::uuid OR (i.library_id = $2 AND i.natural_key = $3) FOR UPDATE OF m`, keep, lib.ID, item.naturalKey).Scan(&raw, &counter)
		var old []map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &old)
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		external, next := externalStreams(object, subtitleFiles.files, old, nextIndex(probe.Streams), counter)
		probe.Streams = append(probe.Streams, external...)
		slices.SortFunc(probe.Streams, func(a, b map[string]any) int { return indexOf(a) - indexOf(b) })
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
		if _, err := tx.Exec(ctx, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams, keyframes, video_end, next_index, sidecars, chapters)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			stored, stored, object.Key, object.Size, object.ETag, probe.Container, streams, probe.Keyframes, probe.VideoEnd, next, subtitleFiles.fingerprint, chapters); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO scan_objects (library_id, key, size, etag, item_id) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (library_id, key) DO UPDATE SET size = $3, etag = $4, item_id = $5`,
			lib.ID, object.Key, object.Size, object.ETag, stored)
		return err
	})
}

func writeItem(ctx context.Context, tx pgx.Tx, libraryID string, item record) error {
	dto, err := json.Marshal(item.dto)
	if err != nil {
		return err
	}
	// Writing by natural key means a repeat scan updates in place, and a moved
	// file (same id, new key) updates through the id conflict.
	_, err = tx.Exec(ctx, `INSERT INTO items (id, library_id, parent_id, series_id, season_id, type, name, sort_name,
			natural_key, production_year, run_time_ticks, index_number, parent_index_number,
			primary_image_key, primary_image_tag, dto)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET name = $7, sort_name = $8, natural_key = $9, production_year = $10,
			run_time_ticks = $11, index_number = $12, parent_index_number = $13,
			primary_image_key = $14, primary_image_tag = $15, dto = $16`,
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
	sources := []map[string]any{{
		"Id":                   db.CompactID(*item.id),
		"Protocol":             "File",
		"Type":                 "Default",
		"Name":                 item.name,
		"Path":                 object.Key,
		"Container":            probe.Container,
		"Size":                 object.Size,
		"ETag":                 etag(object.ETag),
		"RunTimeTicks":         probe.RunTimeTicks,
		"Bitrate":              probe.Bitrate,
		"IsRemote":             false,
		"VideoType":            "VideoFile",
		"SupportsDirectPlay":   true,
		"SupportsDirectStream": true,
		"SupportsTranscoding":  true,
		"SupportsProbing":      true,
		"RequiresOpening":      false,
		"RequiresClosing":      false,
		"MediaStreams":         streams,
		"MediaAttachments":     attachmentsOf(probe.Streams),
		"Formats":              []string{},
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
func attachmentsOf(streams []map[string]any) []map[string]any {
	list := []map[string]any{}
	for _, stream := range streams {
		if mimeType, _ := stream["MimeType"].(string); stream["Type"] == "Attachment" && subtitles.IsFont(mimeType) {
			list = append(list, map[string]any{
				"CodecTag": "[0][0][0][0]", "Index": stream["Index"], "FileName": stream["FileName"], "MimeType": mimeType,
			})
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
		var streams []map[string]any
		if err := json.Unmarshal(raw, &streams); err != nil {
			return err
		}
		own := slices.DeleteFunc(slices.Clone(streams), func(stream map[string]any) bool { return stream["IsExternal"] == true })
		external, next := externalStreams(object, files.files, streams, nextIndex(own), counter)
		streams = append(own, external...)
		slices.SortFunc(streams, func(a, b map[string]any) int { return indexOf(a) - indexOf(b) })
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
func playableStreams(streams []map[string]any) []map[string]any {
	audio := -1
	for i, stream := range streams {
		if stream["Type"] != "Audio" {
			continue
		}
		if audio == -1 || stream["IsDefault"] == true {
			audio = i
		}
		if stream["IsDefault"] == true {
			break
		}
	}
	playable := make([]map[string]any, 0, len(streams))
	for i, stream := range streams {
		if stream["Type"] == "Video" {
			// What the decision needs and the item page never listed.
			stream = maps.Clone(stream)
			for _, key := range []string{"Level", "IsInterlaced", "VideoRangeType", "DurationTicks"} {
				delete(stream, key)
			}
		}
		if stream["Type"] == "Subtitle" && stream["Fingerprint"] != nil {
			// Keys the cached track; no client has use for it.
			stream = maps.Clone(stream)
			delete(stream, "Fingerprint")
		}
		listed := stream["IsTextSubtitleStream"] == true || stream["IsImageSubtitleStream"] == true
		if stream["Type"] == "Video" || i == audio || (stream["Type"] == "Subtitle" && listed && stream["Unreadable"] != true) {
			playable = append(playable, stream)
		}
	}
	return playable
}

func etag(value string) string {
	digest := md5.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}
