package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playstate"
)

// maxActiveWithin caps ActiveWithinSeconds at about eight years, well inside
// what Postgres can subtract from now().
const maxActiveWithin = 1 << 28

// playbackReport is the body of the /Sessions/Playing family. PlaySessionId
// can be empty, so reports key on the user and item instead.
type playbackReport struct {
	ItemID        string `json:"ItemId"`
	MediaSourceID string `json:"MediaSourceId"`
	PositionTicks int64
	IsPaused      bool
	PlayMethod    string
	PlaySessionID string `json:"PlaySessionId"`
}

func readReport(w http.ResponseWriter, r *http.Request) (playbackReport, bool) {
	var report playbackReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&report); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return report, false
	}
	if !db.ValidID(report.ItemID) {
		http.Error(w, "invalid ItemId", http.StatusBadRequest)
		return report, false
	}
	return report, true
}

func (s *Server) playbackStarted(w http.ResponseWriter, r *http.Request) {
	report, ok := readReport(w, r)
	if !ok {
		return
	}
	s.finishReport(w, r, playstate.Started(r.Context(), s.Pool, sessionOf(r).User.ID, report.ItemID), func() error {
		return s.saveSession(r, report)
	})
}

func (s *Server) playbackProgress(w http.ResponseWriter, r *http.Request) {
	report, ok := readReport(w, r)
	if !ok {
		return
	}
	_, err := playstate.Progress(r.Context(), s.Pool, sessionOf(r).User.ID, report.ItemID, report.PositionTicks)
	s.finishReport(w, r, err, func() error { return s.saveSession(r, report) })
}

func (s *Server) playbackStopped(w http.ResponseWriter, r *http.Request) {
	report, ok := readReport(w, r)
	if !ok {
		return
	}
	err := playstate.Stopped(r.Context(), s.Pool, sessionOf(r).User.ID, report.ItemID, report.PositionTicks)
	// The device is no longer playing this item. A late report for an earlier
	// one must not clear what it has moved on to.
	s.finishReport(w, r, err, func() error {
		_, err := s.Pool.Exec(r.Context(), `UPDATE sessions SET item_id = NULL, position_ticks = 0, is_paused = false,
			play_method = NULL, play_session_id = NULL, last_seen_at = now() WHERE token = $1 AND item_id = $2::uuid`,
			sessionOf(r).Token, report.ItemID)
		return err
	})
}

func (s *Server) playbackPing(w http.ResponseWriter, r *http.Request) {
	s.finishReport(w, r, nil, func() error { return s.upsertSession(r, "last_seen_at = now()") })
}

// finishReport answers 204 once the play state and the device's session row
// are both written.
func (s *Server) finishReport(w http.ResponseWriter, r *http.Request, err error, session func() error) {
	if err == nil {
		err = session()
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// upsertSession creates the device's row from its login if there isn't one,
// and applies update if there is. Bound arguments after the token are $2 on.
func (s *Server) upsertSession(r *http.Request, update string, args ...any) error {
	_, err := s.Pool.Exec(r.Context(), `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version)
		SELECT d.token, d.user_id, d.client, d.device_id, d.device_name, d.app_version FROM devices d WHERE d.token = $1
		ON CONFLICT (token) DO UPDATE SET `+update, append([]any{sessionOf(r).Token}, args...)...)
	return err
}

// saveSession stores what the device is playing. The item may have been
// removed since the client started; the subselect then yields NULL instead of
// tripping the foreign key.
func (s *Server) saveSession(r *http.Request, report playbackReport) error {
	_, err := s.Pool.Exec(r.Context(), `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version,
			item_id, position_ticks, is_paused, play_method, play_session_id)
		SELECT d.token, d.user_id, d.client, d.device_id, d.device_name, d.app_version,
			(SELECT id FROM items WHERE id = $2::uuid), $3, $4, NULLIF(left($5, 32), ''), NULLIF(left($6, 64), '')
		FROM devices d WHERE d.token = $1
		ON CONFLICT (token) DO UPDATE SET item_id = EXCLUDED.item_id, position_ticks = EXCLUDED.position_ticks,
			is_paused = EXCLUDED.is_paused, play_method = EXCLUDED.play_method,
			play_session_id = EXCLUDED.play_session_id, last_seen_at = now()`,
		sessionOf(r).Token, report.ItemID, min(max(report.PositionTicks, 0), playstate.MaxTicks), report.IsPaused, report.PlayMethod, report.PlaySessionID)
	return err
}

// listSessions shows recently active devices. Admins see everyone's.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	caller := sessionOf(r).User
	within := 600
	raw := r.URL.Query().Get("activewithinseconds")
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		within = int(min(n, maxActiveWithin))
	} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(raw, "-") {
		within = maxActiveWithin
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT s.token, s.user_id::text, u.name, s.client, s.device_id, s.device_name,
			s.app_version, s.item_id::text, s.position_ticks, s.is_paused, COALESCE(s.play_method, ''), s.last_seen_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.last_seen_at > now() - make_interval(secs => $1)
			AND ($2 OR s.user_id = $3::uuid) AND ($4 = '' OR s.device_id = $4)
		ORDER BY s.last_seen_at DESC`,
		float64(within), caller.IsAdmin, caller.ID, r.URL.Query().Get("deviceid"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer rows.Close()
	type nowPlaying struct {
		info       map[string]any
		item, user string
	}
	var playing []nowPlaying
	sessions := []map[string]any{}
	for rows.Next() {
		var token, userID, userName, client, deviceID, deviceName, version, method string
		var itemID *string
		var position int64
		var paused bool
		var seen time.Time
		if err := rows.Scan(&token, &userID, &userName, &client, &deviceID, &deviceName, &version, &itemID, &position, &paused, &method, &seen); err != nil {
			s.fail(w, r, err)
			return
		}
		// The token is a credential, so the session's public ID is a digest.
		digest := sha256.Sum256([]byte(token))
		info := map[string]any{
			"Id": hex.EncodeToString(digest[:16]), "UserId": db.CompactID(userID), "UserName": userName,
			"Client": client, "DeviceId": deviceID, "DeviceName": deviceName, "ApplicationVersion": version,
			"LastActivityDate": seen.UTC().Format(time.RFC3339Nano), "ServerId": s.ServerID, "IsActive": true,
			"SupportsMediaControl": false, "SupportsRemoteControl": false, "HasCustomDeviceName": false,
			"AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{},
			"NowPlayingQueue": []any{}, "NowPlayingQueueFullItems": []any{},
			"PlayState": map[string]any{
				"PositionTicks": position, "CanSeek": true, "IsPaused": paused, "IsMuted": false,
				"RepeatMode": "RepeatNone", "PlaybackOrder": "Default", "PlayMethod": method,
			},
		}
		sessions = append(sessions, info)
		if itemID != nil {
			playing = append(playing, nowPlaying{info, *itemID, userID})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, p := range playing {
		item, err := s.Items.Get(r.Context(), p.item, p.user)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if item != nil {
			p.info["NowPlayingItem"] = item
		}
	}
	writeJSON(w, sessions)
}

// markPlayed and favorite serve both the current routes and the web aliases.
// The aliases carry a userId, which is ignored: a caller only ever changes
// their own data.
func (s *Server) markPlayed(played bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.changeUserData(w, r, func(user, item string) (bool, error) {
			return playstate.SetPlayed(r.Context(), s.Pool, user, item, played)
		})
	}
}

func (s *Server) markFavorite(favorite bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.changeUserData(w, r, func(user, item string) (bool, error) {
			return playstate.SetFavorite(r.Context(), s.Pool, user, item, favorite)
		})
	}
}

func (s *Server) changeUserData(w http.ResponseWriter, r *http.Request, change func(user, item string) (bool, error)) {
	item := r.PathValue("itemId")
	if !db.ValidID(item) {
		http.NotFound(w, r)
		return
	}
	user := sessionOf(r).User.ID
	found, err := change(user, item)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	data, found, err := s.Items.UserDataFor(r.Context(), user, item)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, data)
}
