package items

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jellyscale/internal/db"
)

// Repo reads items for one server.
type Repo struct {
	Pool     *pgxpool.Pool
	ServerID string
}

// Result is the envelope every list endpoint returns.
type Result struct {
	Items            []map[string]any `json:"Items"`
	TotalRecordCount int              `json:"TotalRecordCount"`
	StartIndex       int              `json:"StartIndex"`
}

// Empty is a Result with no items.
func Empty() Result {
	return Result{Items: []map[string]any{}}
}

// Fields names the optional DTO fields a caller asked for. A nil set means the
// full DTO, which is what single-item requests return.
type Fields map[string]bool

// ParseFields reads the comma-separated fields parameter.
func ParseFields(value string) Fields {
	fields := Fields{}
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			fields[strings.ToLower(name)] = true
		}
	}
	return fields
}

// optional are DTO keys /Items only returns when requested. Each maps to the
// fields value that enables it.
var optional = map[string]string{
	"Overview":          "overview",
	"Path":              "path",
	"MediaSources":      "mediasources",
	"MediaStreams":      "mediastreams",
	"ProviderIds":       "providerids",
	"Genres":            "genres",
	"GenreItems":        "genres",
	"Studios":           "studios",
	"People":            "people",
	"Tags":              "tags",
	"Etag":              "etag",
	"SortName":          "sortname",
	"DateCreated":       "datecreated",
	"ExternalUrls":      "externalurls",
	"Taglines":          "taglines",
	"Chapters":          "chapters",
	"Trickplay":         "trickplay",
	"RemoteTrailers":    "remotetrailers",
	"LockedFields":      "",
	"LockData":          "",
	"PlayAccess":        "",
	"CanDelete":         "",
	"CanDownload":       "",
	"LocalTrailerCount": "",
	"Width":             "width",
	"Height":            "height",
}

// Find runs a query. Counting is optional, because it's the expensive part.
func (r *Repo) Find(ctx context.Context, q Query, fields Fields, withCount bool) (Result, error) {
	sql, args := q.PageSQL()
	rows, err := r.Pool.Query(ctx, sql, args...)
	if err != nil {
		return Result{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Result{}, err
	}
	hydrated, err := r.hydrate(ctx, ids, q.UserID, fields)
	if err != nil {
		return Result{}, err
	}
	result := Result{Items: hydrated, StartIndex: q.Start, TotalRecordCount: len(hydrated)}
	if withCount {
		sql, args := q.CountSQL()
		if err := r.Pool.QueryRow(ctx, sql, args...).Scan(&result.TotalRecordCount); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

// Get returns one item's full DTO, or nil if it doesn't exist.
func (r *Repo) Get(ctx context.Context, id, userID string) (map[string]any, error) {
	found, err := r.hydrate(ctx, []string{id}, userID, nil)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return found[0], nil
}

func (r *Repo) hydrate(ctx context.Context, ids []string, userID string, fields Fields) ([]map[string]any, error) {
	hydrated := make([]map[string]any, 0, len(ids))
	if len(ids) == 0 {
		return hydrated, nil
	}
	rows, err := r.Pool.Query(ctx, `SELECT i.id::text, i.dto, COALESCE(d.played, false), COALESCE(d.favorite, false),
			COALESCE(d.play_count, 0), COALESCE(d.position_ticks, 0), i.run_time_ticks, d.last_played_at
		FROM items i
		LEFT JOIN user_item_data d ON d.item_id = i.id AND d.user_id = $2::uuid
		WHERE i.id = ANY($1::uuid[])`, ids, nullable(userID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := make(map[string]map[string]any, len(ids))
	for rows.Next() {
		var id string
		var raw []byte
		var played, favorite bool
		var playCount int
		var position int64
		var runtime *int64
		var lastPlayed *time.Time
		if err := rows.Scan(&id, &raw, &played, &favorite, &playCount, &position, &runtime, &lastPlayed); err != nil {
			return nil, err
		}
		var dto map[string]any
		if err := json.Unmarshal(raw, &dto); err != nil {
			return nil, err
		}
		compact := db.CompactID(id)
		dto["Id"] = compact
		dto["ServerId"] = r.ServerID
		if fields != nil {
			for key, enabledBy := range optional {
				if enabledBy == "" || !fields[enabledBy] {
					delete(dto, key)
				}
			}
		}
		dto["UserData"] = UserDataDTO(id, UserData{position, runtime, playCount, favorite, played, lastPlayed})
		byID[compact] = dto
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if dto, ok := byID[db.CompactID(id)]; ok {
			hydrated = append(hydrated, dto)
		}
	}
	return hydrated, nil
}

// Library is a library as clients see it: a folder at the root of their views.
type Library struct {
	ID, Name, Kind string
}

// Libraries lists every configured library.
func (r *Repo) Libraries(ctx context.Context) ([]Library, error) {
	rows, err := r.Pool.Query(ctx, "SELECT id::text, name, kind FROM libraries ORDER BY name")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Library, error) {
		var library Library
		err := row.Scan(&library.ID, &library.Name, &library.Kind)
		return library, err
	})
}

// LibraryDTO renders a library as a CollectionFolder.
func (r *Repo) LibraryDTO(library Library) map[string]any {
	id := db.CompactID(library.ID)
	return map[string]any{
		"Name": library.Name, "ServerId": r.ServerID, "Id": id, "Type": "CollectionFolder",
		"CollectionType": library.Kind, "IsFolder": true, "SortName": strings.ToLower(library.Name),
		"DisplayPreferencesId": id, "ImageTags": map[string]string{}, "BackdropImageTags": []string{},
		"ImageBlurHashes": map[string]any{}, "LocationType": "FileSystem", "PlayAccess": "Full",
		"EnableMediaSourceDisplay": true, "ProviderIds": map[string]string{}, "Genres": []string{},
		"UserData": map[string]any{"PlaybackPositionTicks": 0, "PlayCount": 0, "IsFavorite": false,
			"Played": false, "Key": db.DashedID(id), "ItemId": id},
	}
}

// IsLibrary reports whether id names a library, and returns it.
func (r *Repo) IsLibrary(ctx context.Context, id string) (Library, bool, error) {
	var library Library
	err := r.Pool.QueryRow(ctx, "SELECT id::text, name, kind FROM libraries WHERE id = $1::uuid", id).
		Scan(&library.ID, &library.Name, &library.Kind)
	if err == pgx.ErrNoRows {
		return Library{}, false, nil
	}
	return library, err == nil, err
}

// Ancestors walks from an item up to its library, nearest first.
func (r *Repo) Ancestors(ctx context.Context, id, userID string) ([]map[string]any, error) {
	ancestors := []map[string]any{}
	var libraryID string
	current := id
	for {
		var parent *string
		err := r.Pool.QueryRow(ctx, "SELECT parent_id::text, library_id::text FROM items WHERE id = $1::uuid", current).Scan(&parent, &libraryID)
		if err == pgx.ErrNoRows {
			return ancestors, nil
		}
		if err != nil {
			return nil, err
		}
		if parent == nil {
			break
		}
		dto, err := r.Get(ctx, *parent, userID)
		if err != nil {
			return nil, err
		}
		if dto != nil {
			ancestors = append(ancestors, dto)
		}
		current = *parent
	}
	library, ok, err := r.IsLibrary(ctx, libraryID)
	if err != nil {
		return nil, err
	}
	if ok {
		ancestors = append(ancestors, r.LibraryDTO(library))
	}
	return ancestors, nil
}

// Image locates an item's primary image.
func (r *Repo) Image(ctx context.Context, id string) (assetURI, key string, found bool, err error) {
	var imageKey *string
	err = r.Pool.QueryRow(ctx, `SELECT l.asset_store_uri, i.primary_image_key
		FROM items i JOIN libraries l ON l.id = i.library_id WHERE i.id = $1::uuid`, id).Scan(&assetURI, &imageKey)
	if err == pgx.ErrNoRows || (err == nil && imageKey == nil) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return assetURI, *imageKey, true, nil
}

// UserData is one user's state for one item.
type UserData struct {
	Position   int64
	Runtime    *int64
	PlayCount  int
	Favorite   bool
	Played     bool
	LastPlayed *time.Time
}

// UserDataDTO renders the UserItemData clients read.
func UserDataDTO(itemID string, d UserData) map[string]any {
	dto := map[string]any{
		"PlaybackPositionTicks": d.Position,
		"PlayCount":             d.PlayCount,
		"IsFavorite":            d.Favorite,
		"Played":                d.Played,
		"Key":                   db.DashedID(itemID),
		"ItemId":                db.CompactID(itemID),
	}
	if d.Position > 0 && d.Runtime != nil && *d.Runtime > 0 {
		dto["PlayedPercentage"] = float64(d.Position) / float64(*d.Runtime) * 100
	}
	if d.LastPlayed != nil {
		dto["LastPlayedDate"] = d.LastPlayed.UTC().Format(time.RFC3339Nano)
	}
	return dto
}

// UserDataFor reads one user's state for an item. The bool is false when the
// item doesn't exist.
func (r *Repo) UserDataFor(ctx context.Context, userID, itemID string) (map[string]any, bool, error) {
	var d UserData
	err := r.Pool.QueryRow(ctx, `SELECT COALESCE(d.position_ticks, 0), i.run_time_ticks, COALESCE(d.play_count, 0),
			COALESCE(d.favorite, false), COALESCE(d.played, false), d.last_played_at
		FROM items i LEFT JOIN user_item_data d ON d.item_id = i.id AND d.user_id = $1::uuid
		WHERE i.id = $2::uuid`, userID, itemID).
		Scan(&d.Position, &d.Runtime, &d.PlayCount, &d.Favorite, &d.Played, &d.LastPlayed)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return UserDataDTO(itemID, d), true, nil
}

// MediaSources returns an item's stored media sources.
func (r *Repo) MediaSources(ctx context.Context, itemID string) ([]map[string]any, bool, error) {
	var raw []byte
	err := r.Pool.QueryRow(ctx, "SELECT COALESCE(dto->'MediaSources', '[]'::jsonb) FROM items WHERE id = $1::uuid", itemID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var sources []map[string]any
	if err := json.Unmarshal(raw, &sources); err != nil {
		return nil, false, err
	}
	return sources, true, nil
}

// Source is the playable object behind a media source, with the media store
// of its library.
type Source struct {
	Key, ETag, Container, StoreURI string
	Size                           int64
}

// Source finds an item's media source, a specific one if mediaSourceID is set.
func (r *Repo) Source(ctx context.Context, itemID, mediaSourceID string) (Source, bool, error) {
	var s Source
	err := r.Pool.QueryRow(ctx, `SELECT m.store_key, m.object_etag, m.container, l.media_store_uri, m.size
		FROM media_sources m JOIN items i ON i.id = m.item_id JOIN libraries l ON l.id = i.library_id
		WHERE m.item_id = $1::uuid AND ($2 = '' OR m.id = NULLIF($2, '')::uuid)
		ORDER BY m.id LIMIT 1`, itemID, mediaSourceID).
		Scan(&s.Key, &s.ETag, &s.Container, &s.StoreURI, &s.Size)
	if err == pgx.ErrNoRows {
		return Source{}, false, nil
	}
	return s, err == nil, err
}
