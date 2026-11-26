package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"jellyscale/internal/compat"
	"jellyscale/internal/db"
	"jellyscale/internal/quickconnect"
	"jellyscale/internal/signin"
)

// A Quick Connect code is shown by a device that has no token: Initiate, the poll that reads the code's state and the
// redemption are open. Only the approval is a viewer's. Every route refuses when this replica has Quick Connect off,
// as Jellyfin 12.4 does, without touching a code another replica may still offer.

func (s *Server) quickConnectEnabled(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, !s.QuickConnectOff)
}

// refusalLog is the structured log of a refusal. It names the bound or the cap and the account, but never a code,
// secret or token, so an operator can see who is guessing without the log becoming a credential store.
func (s *Server) refusalLog(fields ...any) {
	log := s.AuditLog
	if log == nil {
		log = s.Log
	}
	if log != nil {
		log.Warn("quick connect refused", fields...)
	}
}

func writeCode(w http.ResponseWriter, code quickconnect.Code, approved bool) {
	writeJSON(w, map[string]any{
		"Authenticated": approved,
		"Secret":        code.Secret,
		"Code":          code.Value,
		"DeviceId":      code.Device.ID,
		"DeviceName":    code.Device.Name,
		"AppName":       code.Device.App,
		"AppVersion":    code.Device.Version,
		"DateAdded":     code.Created.UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) quickConnectInitiate(w http.ResponseWriter, r *http.Request) {
	// Jellyfin validates the device fields before it checks whether Quick Connect is on, so a request with no
	// Authorization is 400 either way (the capture's "initiate when off with no Authorization").
	info := compat.ParseAuth(r)
	if info.DeviceID == "" || info.Device == "" || info.Client == "" || info.Version == "" {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
		return
	}
	if s.QuickConnectOff {
		s.quickConnectDisabled(w)
		return
	}
	code, err := quickconnect.Initiate(r.Context(), s.Pool, quickconnect.Device{
		ID: info.DeviceID, Name: info.Device, App: info.Client, Version: info.Version,
	})
	if errors.Is(err, quickconnect.ErrFull) {
		// A cap refusal must not read as Quick Connect turned off: Moonfin falls back to the password form on
		// 401 or the word "disabled", and a TV already showing a code keeps working (D-079).
		quickconnectCapRefusals.WithLabelValues("live_codes").Inc()
		s.refusalLog("cap", "live_codes", "reason", "too many live codes")
		http.Error(w, "Error processing request.", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCode(w, code, false)
}

func (s *Server) quickConnectConnect(w http.ResponseWriter, r *http.Request) {
	if s.QuickConnectOff {
		s.quickConnectDisabled(w)
		return
	}
	secret := r.URL.Query().Get("secret")
	if secret == "" {
		http.Error(w, `{"title":"One or more validation errors occurred.","errors":{"secret":["The secret field is required."]}}`, http.StatusBadRequest)
		return
	}
	code, err := quickconnect.Connect(r.Context(), s.Pool, secret)
	if errors.Is(err, quickconnect.ErrUnknownSecret) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode("Unknown secret") //nolint:errcheck
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeCode(w, code, code.Approved != nil)
}

// quickConnectDisabled answers what Jellyfin 12.4 answers to the four routes when its setting is off. The body is a
// JSON string, which is what the capture holds for Enabled, Initiate, Connect and approval.
func (s *Server) quickConnectDisabled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode("Quick connect is disabled") //nolint:errcheck
}

func (s *Server) quickConnectAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.QuickConnectOff {
		s.quickConnectDisabled(w)
		return
	}
	value := r.URL.Query().Get("code")
	if value == "" {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
		return
	}
	user := sessionOf(r).User
	// Approval is always for the approver's own account, administrators included. A plain 403 (no Retry-After) is
	// not a refusal of the guessing bound and does not count toward it (D-078).
	// normalize lowercases query keys once, in middleware, so the key arrives as "userid".
	if named := r.URL.Query().Get("userid"); named != "" &&
		!strings.EqualFold(db.CompactID(named), db.CompactID(user.ID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ticket, refusal, err := signin.BeginApproval(r.Context(), s.Pool, user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if refusal != nil {
		quickconnectBoundRefusals.WithLabelValues(refusal.Bound).Inc()
		s.refusalLog("bound", refusal.Bound, "user", user.Name,
			"after_seconds", int(refusal.After/time.Second))
		w.Header().Set("Retry-After", strconv.Itoa(int(refusal.After/time.Second)))
		http.Error(w, "too many failed approvals", http.StatusForbidden)
		return
	}
	err = quickconnect.Approve(r.Context(), s.Pool, value, user.ID)
	if err == nil {
		if err := ticket.Release(r.Context(), s.Pool); err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, true)
		return
	}
	// A failure keeps the reservation: an unknown, expired or already approved code is a guess at someone's account.
	switch {
	case errors.Is(err, quickconnect.ErrUnknownCode):
		http.Error(w, "Error processing request.", http.StatusNotFound)
	case errors.Is(err, quickconnect.ErrAlreadyApproved):
		http.Error(w, "already approved", http.StatusConflict)
	default:
		s.fail(w, r, err)
	}
}

func (s *Server) authenticateWithQuickConnect(w http.ResponseWriter, r *http.Request) {
	if s.QuickConnectOff {
		http.Error(w, "Error processing request.", http.StatusUnauthorized)
		return
	}
	var body struct{ Secret string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Secret == "" {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
		return
	}
	code, err := quickconnect.Redeem(r.Context(), s.Pool, body.Secret)
	if errors.Is(err, quickconnect.ErrUnknownSecret) || errors.Is(err, quickconnect.ErrNotApproved) ||
		errors.Is(err, quickconnect.ErrAccountGone) {
		http.Error(w, "Error processing request.", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	user, err := s.queryUser(r.Context(), code.Token)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, s.signInJSON(user, code.Token, compat.ClientInfo{
		Client: code.Device.App, Device: code.Device.Name, DeviceID: code.Device.ID, Version: code.Device.Version,
	}))
}

// The sign-in's session id is a fresh compact id, as a password sign-in's is; the device is the one that asked for
// the code, never the one that happened to redeem it.
func (s *Server) signInJSON(user User, token string, info compat.ClientInfo) map[string]any {
	now := time.Now().UTC()
	if user.LastLoginAt != nil {
		now = *user.LastLoginAt
	}
	return map[string]any{
		"User": s.userDTO(user),
		"SessionInfo": map[string]any{
			"PlayState": map[string]any{"CanSeek": false, "IsPaused": false, "IsMuted": false, "RepeatMode": "RepeatNone",
				"PlaybackOrder": "Default"},
			"AdditionalUsers": []any{}, "PlayableMediaTypes": []any{}, "SupportedCommands": []any{},
			"Id": db.CompactID(uuid.NewString()), "UserId": db.CompactID(user.ID), "UserName": user.Name,
			"Client": info.Client, "DeviceName": info.Device, "DeviceId": info.DeviceID,
			"ApplicationVersion": info.Version, "LastActivityDate": now.Format(time.RFC3339Nano),
			"LastPlaybackCheckIn": "0001-01-01T00:00:00.0000000Z",
			"ServerId":            s.ServerID, "IsActive": true, "SupportsMediaControl": false, "SupportsRemoteControl": false,
			"HasCustomDeviceName": false, "NowPlayingQueue": []any{},
		},
		"AccessToken": token,
		"ServerId":    s.ServerID,
	}
}
