package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/compat"
	"jellyscale/internal/db"
)

// User is an account.
type User struct {
	ID          string
	Name        string
	IsAdmin     bool
	LastLoginAt *time.Time
}

// EnsureAdmin creates the bootstrap admin from configuration if it's missing.
// Existing accounts keep their password.
func EnsureAdmin(ctx context.Context, pool *pgxpool.Pool, name, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (id, name, password_hash, is_admin) VALUES ($1, $2, $3, true)
		ON CONFLICT (lower(name)) DO NOTHING`, uuid.NewString(), name, string(hash))
	return err
}

// EnsureServerID returns the persisted server ID, creating it once.
func EnsureServerID(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	generated := db.CompactID(uuid.NewString())
	if _, err := pool.Exec(ctx, "INSERT INTO server (server_id) VALUES ($1) ON CONFLICT DO NOTHING", generated); err != nil {
		return "", err
	}
	var id string
	err := pool.QueryRow(ctx, "SELECT server_id FROM server").Scan(&id)
	return id, err
}

func (s *Server) userByToken(ctx context.Context, token string) (User, error) {
	var user User
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text, u.name, u.is_admin, u.last_login_at
		FROM devices d JOIN users u ON u.id = d.user_id WHERE d.token = $1`, token).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt)
	return user, err
}

func (s *Server) authenticateByName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string
		Pw       string
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var user User
	var hash string
	err := s.Pool.QueryRow(r.Context(), `SELECT id::text, name, is_admin, last_login_at, password_hash
		FROM users WHERE lower(name) = lower($1)`, body.Username).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Compare anyway, so timing doesn't reveal which names exist.
		bcrypt.CompareHashAndPassword(dummyHash, []byte(body.Pw)) //nolint:errcheck
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Pw)) != nil {
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}

	info := compat.ParseAuth(r)
	accessToken := randomHex(16)
	now := time.Now().UTC()
	// One token per device, so signing in again replaces the old one.
	if err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), "DELETE FROM devices WHERE user_id = $1 AND device_id = $2", user.ID, info.DeviceID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `INSERT INTO devices (token, user_id, device_id, device_name, client, app_version)
			VALUES ($1, $2, $3, $4, $5, $6)`, accessToken, user.ID, info.DeviceID, info.Device, info.Client, info.Version); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), "UPDATE users SET last_login_at = $2 WHERE id = $1", user.ID, now)
		return err
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	user.LastLoginAt = &now

	writeJSON(w, map[string]any{
		"User": s.userDTO(user),
		"SessionInfo": map[string]any{
			"PlayState":       map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone"},
			"AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{},
			"Id": db.CompactID(uuid.NewString()), "UserId": db.CompactID(user.ID), "UserName": user.Name,
			"Client": info.Client, "DeviceName": info.Device, "DeviceId": info.DeviceID,
			"ApplicationVersion": info.Version, "LastActivityDate": now.Format(time.RFC3339Nano),
			"ServerId": s.ServerID, "IsActive": true, "SupportsMediaControl": false, "SupportsRemoteControl": false,
			"HasCustomDeviceName": false, "NowPlayingQueue": []any{},
		},
		"AccessToken": accessToken,
		"ServerId":    s.ServerID,
	})
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("jellyscale"), bcrypt.DefaultCost)

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Pool.Exec(r.Context(), "DELETE FROM devices WHERE token = $1", sessionOf(r).Token); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.userDTO(sessionOf(r).User))
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	session := sessionOf(r)
	if db.CompactID(id) == db.CompactID(session.User.ID) {
		writeJSON(w, s.userDTO(session.User))
		return
	}
	if !session.User.IsAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !db.ValidID(id) {
		http.NotFound(w, r)
		return
	}
	var user User
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text, name, is_admin, last_login_at FROM users WHERE id = $1::uuid", id).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, s.userDTO(user))
}

func (s *Server) userDTO(user User) map[string]any {
	dto := map[string]any{
		"Name": user.Name, "ServerId": s.ServerID, "Id": db.CompactID(user.ID),
		"HasPassword": true, "HasConfiguredPassword": true, "HasConfiguredEasyPassword": false,
		"EnableAutoLogin": false,
		"Configuration": map[string]any{
			"PlayDefaultAudioTrack": true, "SubtitleLanguagePreference": "", "DisplayMissingEpisodes": false,
			"GroupedFolders": []any{}, "SubtitleMode": "Default", "DisplayCollectionsView": false,
			"EnableLocalPassword": false, "OrderedViews": []any{}, "LatestItemsExcludes": []any{},
			"MyMediaExcludes": []any{}, "HidePlayedInLatest": true, "RememberAudioSelections": true,
			"RememberSubtitleSelections": true, "EnableNextEpisodeAutoPlay": true,
		},
		"Policy": map[string]any{
			"IsAdministrator": user.IsAdmin, "IsHidden": false, "IsDisabled": false,
			"EnableCollectionManagement": false, "EnableSubtitleManagement": false, "EnableLyricManagement": false,
			"BlockedTags": []any{}, "AllowedTags": []any{}, "EnableUserPreferenceAccess": true,
			"AccessSchedules": []any{}, "BlockUnratedItems": []any{}, "EnableRemoteControlOfOtherUsers": user.IsAdmin,
			"EnableSharedDeviceControl": true, "EnableRemoteAccess": true, "EnableLiveTvManagement": false,
			"EnableLiveTvAccess": false, "EnableMediaPlayback": true, "EnableAudioPlaybackTranscoding": true,
			"EnableVideoPlaybackTranscoding": true, "EnablePlaybackRemuxing": true, "ForceRemoteSourceTranscoding": false,
			"EnableContentDeletion": false, "EnableContentDeletionFromFolders": []any{}, "EnableContentDownloading": true,
			"EnableSyncTranscoding": true, "EnableMediaConversion": false, "EnabledDevices": []any{}, "EnableAllDevices": true,
			"EnabledChannels": []any{}, "EnableAllChannels": true, "EnabledFolders": []any{}, "EnableAllFolders": true,
			"InvalidLoginAttemptCount": 0, "LoginAttemptsBeforeLockout": -1, "MaxActiveSessions": 0,
			"EnablePublicSharing": false, "BlockedMediaFolders": []any{}, "BlockedChannels": []any{},
			"RemoteClientBitrateLimit": 0, "SyncPlayAccess": "None",
		},
	}
	if user.LastLoginAt != nil {
		dto["LastLoginDate"] = user.LastLoginAt.UTC().Format(time.RFC3339Nano)
		dto["LastActivityDate"] = user.LastLoginAt.UTC().Format(time.RFC3339Nano)
	}
	return dto
}

func (s *Server) getDisplayPreferences(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("preferenceId")
	client := r.URL.Query().Get("client")
	var data []byte
	err := s.Pool.QueryRow(r.Context(), `SELECT data FROM display_preferences
		WHERE user_id = $1 AND preference_id = $2 AND client = $3`, sessionOf(r).User.ID, id, client).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, map[string]any{
			"Id": id, "SortBy": "SortName", "RememberIndexing": false, "PrimaryImageHeight": 250,
			"PrimaryImageWidth": 250, "CustomPrefs": map[string]string{}, "ScrollDirection": "Horizontal",
			"ShowBackdrop": true, "RememberSorting": false, "SortOrder": "Ascending",
			"ShowSidebar": false, "Client": client,
		})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data) //nolint:errcheck
}

func (s *Server) setDisplayPreferences(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	_, err = s.Pool.Exec(r.Context(), `INSERT INTO display_preferences (user_id, preference_id, client, data)
		VALUES ($1, $2, $3, $4) ON CONFLICT (user_id, preference_id, client) DO UPDATE SET data = $4`,
		sessionOf(r).User.ID, r.PathValue("preferenceId"), r.URL.Query().Get("client"), body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
