package library

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // Sidecar decoding.
	_ "image/png"  // Sidecar decoding.
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
	"jellyscale/internal/store"
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
	// which transcode decisions need, or which subtitle tracks are text. A scan
	// probes it again.
	unprobed bool
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
	current := map[string]bool{}
	var changed []store.Object
	for _, object := range objects {
		if IsVideo(object.Key) {
			current[object.Key] = true
			if old, ok := seen[object.Key]; !ok || old.etag != object.ETag || old.size != object.Size || old.unprobed {
				changed = append(changed, object)
			}
			continue
		}
		name := strings.ToLower(path.Base(object.Key))
		if name == "poster.jpg" || name == "folder.jpg" || name == "poster.png" {
			sidecars[path.Dir(object.Key)] = object.Key
		}
	}
	gone := map[string]seenObject{}
	for key, old := range seen {
		if !current[key] {
			gone[key] = old
		}
	}

	probes := s.probeAll(ctx, lib, changed)
	for _, object := range changed {
		probe, ok := probes[object.Key]
		if !ok {
			continue
		}
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
		if err := s.upsert(ctx, lib, object, probe, sidecars, keep); err != nil {
			s.Log.Error("store item failed", "key", object.Key, "error", err)
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
	s.Log.Info("scan finished", "library", lib.Name, "videos", len(current), "changed", len(changed), "removed", len(gone))
	return nil
}

func (s *Scanner) loadSeen(ctx context.Context, libraryID string) (map[string]seenObject, error) {
	rows, err := s.Pool.Query(ctx, `SELECT o.key, o.size, o.etag, o.item_id::text,
			EXISTS (SELECT 1 FROM media_sources m WHERE m.item_id = o.item_id
				AND jsonb_path_exists(m.streams, '$[*] ? ((@.Type == "Video" && !exists(@.VideoRangeType)) || (@.Type == "Subtitle" && !exists(@.IsTextSubtitleStream)))'))
		FROM scan_objects o WHERE o.library_id = $1`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]seenObject{}
	for rows.Next() {
		var key string
		var old seenObject
		if err := rows.Scan(&key, &old.size, &old.etag, &old.itemID, &old.unprobed); err != nil {
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

func (s *Scanner) probeAll(ctx context.Context, lib Library, objects []store.Object) map[string]Probe {
	results := make(map[string]Probe, len(objects))
	var mu sync.Mutex
	var wg sync.WaitGroup
	limit := make(chan struct{}, probeConcurrency)
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
			mu.Lock()
			results[object.Key] = probe
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
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

func (s *Scanner) upsert(ctx context.Context, lib Library, object store.Object, probe Probe, sidecars map[string]string, keep *string) error {
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

	item.dto = videoDTO(item, object, probe)
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
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
		if _, err := tx.Exec(ctx, "DELETE FROM media_sources WHERE item_id = $1", stored); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO media_sources (id, item_id, store_key, size, object_etag, container, streams)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			stored, stored, object.Key, object.Size, object.ETag, probe.Container, streams); err != nil {
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
		"MediaAttachments":     []string{},
		"Formats":              []string{},
	}}
	dto["IsFolder"] = false
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
	dto["Chapters"] = []string{}
	dto["Trickplay"] = map[string]any{}
	return dto
}

// playableStreams is what direct play can deliver: the video, the default audio
// track, or else the first, and the text subtitle tracks, which are extracted
// on request. Image subtitles can't be delivered yet. Clients build their
// track menus from this, so anything else would be a dead choice.
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
		if stream["Type"] == "Video" || i == audio || (stream["Type"] == "Subtitle" && stream["IsTextSubtitleStream"] == true) {
			playable = append(playable, stream)
		}
	}
	return playable
}

func etag(value string) string {
	digest := md5.Sum([]byte(value))
	return hex.EncodeToString(digest[:])
}
