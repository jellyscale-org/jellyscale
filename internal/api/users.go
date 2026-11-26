package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"jellyscale/internal/compat"
	"jellyscale/internal/db"
	"jellyscale/internal/signin"
)

// User is an account.
type User struct {
	ID          string
	Name        string
	IsAdmin     bool
	LastLoginAt *time.Time
	// Configuration is the stored settings document, empty until the user saves.
	Configuration []byte
	// TrackPreferences is the stored track preferences document (JS-118), empty until the user saves.
	TrackPreferences []byte
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

func (s *Server) queryUser(ctx context.Context, token string) (User, error) {
	var user User
	err := s.Pool.QueryRow(ctx, `SELECT u.id::text, u.name, u.is_admin, u.last_login_at, u.configuration, u.track_preferences
		FROM devices d JOIN users u ON u.id = d.user_id WHERE d.token = $1`, token).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt, &user.Configuration, &user.TrackPreferences)
	return user, err
}

// userByToken asks Postgres, and waits for it while it is away. Only a route
// that can serve from the snapshot goes on without it (invariant 37); a token
// is otherwise refused at once when signed out, and never accepted without
// Postgres saying its device is signed in.
func (s *Server) userByToken(ctx context.Context, token string) (User, error) {
	var user User
	err := db.Retry(ctx, s.Pool, func() (err error) {
		user, err = s.queryUser(ctx, token)
		return
	})
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
	hash := string(dummyHash)
	err := s.Pool.QueryRow(r.Context(), `SELECT id::text, name, is_admin, last_login_at, configuration, password_hash
		FROM users WHERE lower(name) = lower($1)`, body.Username).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt, &user.Configuration, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		s.fail(w, r, err)
		return
	}
	// A name that is no account is compared with the dummy hash anyway, so that neither the answer nor the time it
	// takes tells which names exist.
	info := compat.ParseAuth(r)
	attempt := signin.Attempt{Name: body.Username, UserID: user.ID, DeviceID: info.DeviceID}
	peer := signin.Peer(r).String()
	if s.Proxies != nil {
		if client, ok := s.Proxies.Client(r); ok {
			attempt.Address = signin.Key(client)
		} else {
			// A declared proxy that says nothing of the client: counting the proxy would make one guesser refuse everyone behind it.
			signinUnattributed.Inc()
			s.unattributedOnce.Do(func() {
				s.Log.Warn("a sign-in came through a declared proxy with no usable forwarded address, so the per-address bound was not applied to it; check that the ingress sets X-Forwarded-For and that trustedProxies lists every proxy (docs/running.md)", "peer", peer)
			})
		}
	}
	ticket, refusal, err := signin.Begin(r.Context(), s.Pool, attempt)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if refusal != nil {
		signinRefusals.WithLabelValues(refusal.Bound).Inc()
		fields := []any{"bound", refusal.Bound}
		if attempt.Address != "" {
			fields = append(fields, "address", attempt.Address)
		} else {
			// No address was attributed to the request: the peer's is every viewer's behind an ingress, and the header is the
			// peer's word, so it is logged as received (escaped by the logger, cut short) and never counted (D-023).
			fields = append(fields, "peer", peer)
			if forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ", "); forwarded != "" {
				fields = append(fields, "forwarded_unverified", forwarded[:min(len(forwarded), 256)])
			}
		}
		if user.ID != "" {
			fields = append(fields, "user", user.Name)
		}
		s.Log.Warn("sign-in refused", fields...)
		// The same answer for every name: a refusal never tells a name that exists from one that does not.
		w.Header().Set("Retry-After", strconv.Itoa(int(refusal.After/time.Second)))
		http.Error(w, "too many failed sign-ins", http.StatusForbidden)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Pw)) != nil || user.ID == "" {
		signinFailures.Inc()
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	if err := ticket.Release(r.Context(), s.Pool); err != nil {
		s.fail(w, r, err)
		return
	}

	var accessToken string
	// One token per device, issued by the same path a Quick Connect redemption uses, so the two cannot race one
	// another into two tokens for the same user and device.
	if err := pgx.BeginFunc(r.Context(), s.Pool, func(tx pgx.Tx) error {
		issued, err := signin.Issue(r.Context(), tx, user.ID, signin.Device{
			ID: info.DeviceID, Name: info.Device, App: info.Client, Version: info.Version,
		})
		if err != nil {
			return err
		}
		accessToken, user.LastLoginAt = issued.Value, &issued.LoginAt
		return nil
	}); err != nil {
		s.fail(w, r, err)
		return
	}

	writeJSON(w, s.signInJSON(user, accessToken, info))
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("jellyscale"), bcrypt.DefaultCost)

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := signin.SignedOut(r.Context(), s.Pool, sessionOf(r).Token); err != nil {
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
	err := s.Pool.QueryRow(r.Context(), "SELECT id::text, name, is_admin, last_login_at, configuration FROM users WHERE id = $1::uuid", id).
		Scan(&user.ID, &user.Name, &user.IsAdmin, &user.LastLoginAt, &user.Configuration)
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
		"Configuration":   configurationOf(user.Configuration),
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
			// Jellyfin's default providers, by type name; SDK clients require both fields.
			"AuthenticationProviderId": "Jellyfin.Server.Implementations.Users.DefaultAuthenticationProvider",
			"PasswordResetProviderId":  "Jellyfin.Server.Implementations.Users.DefaultPasswordResetProvider",
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
