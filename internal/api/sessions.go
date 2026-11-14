package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jellyscale/internal/db"
	"jellyscale/internal/playstate"

	"github.com/jackc/pgx/v5"
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
	err := playstate.Progress(r.Context(), s.Pool, sessionOf(r).User.ID, report.ItemID, report.PositionTicks)
	s.finishReport(w, r, err, func() error { return s.updateSessionPosition(r, report) })
}

// updateSessionPosition only moves the item the session is already on. Only
// Playing changes the item, so a late Progress can't revive a stopped session
// or pull the device back from what it plays now.
func (s *Server) updateSessionPosition(r *http.Request, report playbackReport) error {
	_, err := s.Pool.Exec(r.Context(), `UPDATE sessions SET position_ticks = $3, is_paused = $4, last_seen_at = now()
		WHERE token = $1 AND item_id = $2::uuid`,
		sessionOf(r).Token, report.ItemID, playstate.ClampTicks(report.PositionTicks), report.IsPaused)
	return err
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
		// Closing the player sends no DELETE ActiveEncodings, so this is what
		// ends the transcode. A report with no PlaySessionId (invariant 18)
		// ends the device's transcode of this item only, so a late report for
		// an earlier item can't stop what the device plays now.
		if s.Transcode != nil {
			user := sessionOf(r)
			if err := s.Transcode.Stop(r.Context(), user.User.ID, report.PlaySessionID, user.DeviceID, db.CompactID(report.ItemID)); err != nil {
				return err
			}
		}
		_, err := s.Pool.Exec(r.Context(), `UPDATE sessions SET item_id = NULL, position_ticks = 0, is_paused = false,
			play_method = NULL, play_session_id = NULL, last_seen_at = now() WHERE token = $1 AND item_id = $2::uuid`,
			sessionOf(r).Token, report.ItemID)
		return err
	})
}

func (s *Server) playbackPing(w http.ResponseWriter, r *http.Request) {
	s.finishReport(w, r, nil, func() error {
		_, err := s.Pool.Exec(r.Context(), touchSession, sessionOf(r).Token)
		return err
	})
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

// touchSession creates the device's session row from its login if there isn't
// one, and marks it seen.
const touchSession = `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version)
	SELECT d.token, d.user_id, d.client, d.device_id, d.device_name, d.app_version FROM devices d WHERE d.token = $1
	ON CONFLICT (token) DO UPDATE SET last_seen_at = now()`

// saveSession stores what the device is playing. The item may have been
// removed since the client started; the subselect then yields NULL instead of
// tripping the foreign key.
func (s *Server) saveSession(r *http.Request, report playbackReport) error {
	_, err := s.Pool.Exec(r.Context(), `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version,
			item_id, position_ticks, is_paused, play_method, play_session_id)
		SELECT d.token, d.user_id, d.client, d.device_id, d.device_name, d.app_version,
			(SELECT id FROM shown_items WHERE id = $2::uuid), $3, $4, NULLIF(left($5, 32), ''), NULLIF(left($6, 64), '')
		FROM devices d WHERE d.token = $1
		ON CONFLICT (token) DO UPDATE SET item_id = EXCLUDED.item_id, position_ticks = EXCLUDED.position_ticks,
			is_paused = EXCLUDED.is_paused, play_method = EXCLUDED.play_method,
			play_session_id = EXCLUDED.play_session_id, last_seen_at = now()`,
		sessionOf(r).Token, report.ItemID, playstate.ClampTicks(report.PositionTicks), report.IsPaused, report.PlayMethod, report.PlaySessionID)
	return err
}

// listSessionsSQL takes $1 the seconds of activity, $2 whether the caller is an admin,
// $3 the caller's id and $4 a device id or ”.
const listSessionsSQL = `SELECT s.token, s.user_id::text, u.name, s.client, s.device_id, s.device_name,
			s.app_version, s.item_id::text, s.position_ticks, s.is_paused, COALESCE(s.play_method, ''), s.last_seen_at,
			COALESCE(i.run_time_ticks, 0), t.spec, t.streams, t.state, t.worker, COALESCE(t.next_segment, 0), t.encode_speed, t.reasons
		FROM sessions s JOIN users u ON u.id = s.user_id
		LEFT JOIN shown_items i ON i.id = s.item_id
		-- The newest job of the play session's viewer, if it watches a transcode.
		LEFT JOIN LATERAL (SELECT j.spec, j.state, j.worker, j.next_segment, j.encode_speed,
				(SELECT m.streams FROM media_sources m WHERE m.id = (j.spec->>'Source')::uuid) AS streams,
				COALESCE((SELECT r.reasons FROM transcode_reasons r WHERE r.play_session_id = v.play_session_id AND r.spec_hash = v.spec_hash), '{}') AS reasons
			FROM transcode_viewers v JOIN transcode_jobs j ON j.id = v.job_id
			WHERE v.play_session_id = s.play_session_id AND NOT v.stopped AND s.item_id IS NOT NULL
			ORDER BY v.last_seen_at DESC LIMIT 1) t ON true
		WHERE s.last_seen_at > now() - make_interval(secs => $1)
			AND ($2 OR s.user_id = $3::uuid) AND ($4 = '' OR s.device_id = $4)
		ORDER BY s.last_seen_at DESC`

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
	rows, err := s.Pool.Query(r.Context(), listSessionsSQL,
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
		var runTicks int64
		var job transcodeRow
		var state *string
		if err := rows.Scan(&token, &userID, &userName, &client, &deviceID, &deviceName, &version, &itemID, &position, &paused, &method, &seen,
			&runTicks, &job.Spec, &job.Streams, &state, &job.Worker, &job.NextSegment, &job.EncodeSpeed, &job.Reasons); err != nil {
			s.fail(w, r, err)
			return
		}
		// The token is a credential, so the session's public ID is a digest.
		digest := sha256.Sum256([]byte(token))
		info := map[string]any{
			"Id": hex.EncodeToString(digest[:16]), "UserId": db.CompactID(userID), "UserName": userName,
			"Client": client, "DeviceId": deviceID, "DeviceName": deviceName, "ApplicationVersion": version,
			"LastActivityDate": seen.UTC().Format(time.RFC3339Nano), "LastPlaybackCheckIn": seen.UTC().Format(time.RFC3339Nano),
			"ServerId": s.ServerID, "IsActive": true,
			"SupportsMediaControl": false, "SupportsRemoteControl": false, "HasCustomDeviceName": false,
			"AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{},
			"NowPlayingQueue": []any{}, "NowPlayingQueueFullItems": []any{},
			"PlayState": map[string]any{
				"PositionTicks": position, "CanSeek": true, "IsPaused": paused, "IsMuted": false,
				"RepeatMode": "RepeatNone", "PlaybackOrder": "Default",
			},
		}
		if method != "" { // the schema allows a PlayMethod of Transcode, DirectStream or DirectPlay, or none
			info["PlayState"].(map[string]any)["PlayMethod"] = method
		}
		if state != nil {
			job.State = *state
			if info["TranscodingInfo"], err = transcodingInfo(job, runTicks); err != nil {
				s.fail(w, r, err)
				return
			}
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
// The aliases carry a userId, which is ignored: they only change the caller's
// own data.
func (s *Server) markPlayed(played bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.changeUserData(w, r, sessionOf(r).User.ID, func(user, item string) (bool, error) {
			return playstate.SetPlayed(r.Context(), s.Pool, user, item, played)
		})
	}
}

func (s *Server) markFavorite(favorite bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.changeUserData(w, r, sessionOf(r).User.ID, func(user, item string) (bool, error) {
			return playstate.SetFavorite(r.Context(), s.Pool, user, item, favorite)
		})
	}
}

func (s *Server) changeUserData(w http.ResponseWriter, r *http.Request, user string, change func(user, item string) (bool, error)) {
	item := r.PathValue("itemId")
	if !db.ValidID(item) {
		http.NotFound(w, r)
		return
	}
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

// userDataBody is the part of Jellyfin's UserItemDataDto that a write takes.
// A field the body leaves out stays as it is. PlayedPercentage is derived from
// the position, so it is read and ignored, as Jellyfin does.
type userDataBody struct {
	Played                *bool
	PlaybackPositionTicks *int64
	LastPlayedDate        *time.Time
	IsFavorite            *bool
}

// setUserData writes the fields the body names, for the caller. Its
// LastPlayedDate says when the data was true, so a body older than what is
// stored cannot undo a newer position or played state (see playstate.SetUserData).
func (s *Server) setUserData(w http.ResponseWriter, r *http.Request) {
	if kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || kind != "application/json" {
		// Jellyfin's answer, a problem document, for a form or untyped body.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnsupportedMediaType)
		writeJSON(w, map[string]any{"type": "https://tools.ietf.org/html/rfc9110#section-15.5.16",
			"title": "Unsupported Media Type", "status": http.StatusUnsupportedMediaType, "traceId": randomHex(8)})
		return
	}
	var body userDataBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !db.ValidID(r.PathValue("itemId")) {
		http.Error(w, "invalid item id", http.StatusBadRequest)
		return
	}
	user, ok := s.userDataTarget(w, r)
	if !ok {
		return
	}
	s.changeUserData(w, r, user, func(user, item string) (bool, error) {
		return playstate.SetUserData(r.Context(), s.Pool, user, item, playstate.UserDataUpdate{
			Played: body.Played, Position: body.PlaybackPositionTicks, LastPlayed: body.LastPlayedDate, Favorite: body.IsFavorite})
	})
}

// userDataTarget is the user a UserData write is for: the caller, or the one
// ?userId= names. As in Jellyfin, only an administrator may name someone else,
// a non-administrator is refused before the user is looked up, and an
// administrator naming nobody known gets 404.
func (s *Server) userDataTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	caller := sessionOf(r).User
	named := r.URL.Query().Get("userid")
	if named == "" {
		return caller.ID, true
	}
	if !db.ValidID(named) {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return "", false
	}
	if strings.EqualFold(db.CompactID(named), db.CompactID(caller.ID)) {
		return caller.ID, true
	}
	if !caller.IsAdmin {
		http.Error(w, "Error processing request.", http.StatusForbidden)
		return "", false
	}
	var id string
	err := s.Pool.QueryRow(r.Context(), `SELECT id::text FROM users WHERE id = $1::uuid`, named).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return "", false
	}
	if err != nil {
		s.fail(w, r, err)
		return "", false
	}
	return id, true
}
