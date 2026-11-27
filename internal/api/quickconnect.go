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
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"jellyscale/internal/compat"
	"jellyscale/internal/db"
	"jellyscale/internal/quickconnect"
	"jellyscale/internal/signin"
)

// approvalTimeout bounds an approval that outlives its client. It is longer than a client waits for an answer, so a
// hang-up cannot change the outcome, and short enough that a stuck database does not hold the statement forever.
const approvalTimeout = 30 * time.Second

func (s *Server) approvalCut() (context.Context, context.CancelFunc) {
	s.shutdownOnce.Do(func() { s.shutdownCut, s.cutAtShutdown = context.WithCancel(context.Background()) })
	return s.shutdownCut, s.cutAtShutdown
}

// CutApprovals ends the approvals in flight, which no request's end does. A stopping replica calls it when its shutdown
// grace expires. A cut approval keeps its reservation, as any the database never answered.
func (s *Server) CutApprovals() {
	_, cut := s.approvalCut()
	cut()
}

// logAbandonedFailure records why an approval failed when its client is gone: fail says nothing then, and nobody else
// hears of it. The code is never logged.
func (s *Server) logAbandonedFailure(r *http.Request, user User, err error) {
	if r.Context().Err() == nil {
		return
	}
	s.Log.Warn("an approval failed after its client hung up", "user", user.Name, "error", err)
}

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

// storable refuses text Postgres cannot keep, before a query carries it. A NUL or an invalid UTF-8 byte in a device
// field, a secret, a code or a user id is the client's mistake (400), never a server error.
func storable(values ...string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return true
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
	if info.DeviceID == "" || info.Device == "" || info.Client == "" || info.Version == "" ||
		!storable(info.DeviceID, info.Device, info.Client, info.Version) {
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
	if !storable(secret) {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
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
	// normalize lowercases query keys once, in middleware, so the key arrives as "userid".
	named := r.URL.Query().Get("userid")
	if value == "" || !storable(value, named) {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
		return
	}
	user := sessionOf(r).User
	// Approval is always for the approver's own account, administrators included. A plain 403 (no Retry-After) is
	// not a refusal of the guessing bound and does not count toward it (D-078).
	if named != "" &&
		!strings.EqualFold(db.CompactID(named), db.CompactID(user.ID)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The approval and its reservation run on a context detached from the request: a client that hangs up must not
	// change whether a guess is counted or whether the code is approved. The timeout bounds a database that hangs, and
	// the end of the shutdown grace bounds a replica that is stopping.
	approval, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), approvalTimeout)
	defer cancel()
	shutdownCut, _ := s.approvalCut()
	stopCut := context.AfterFunc(shutdownCut, cancel)
	defer stopCut()
	ticket, refusal, err := signin.BeginApproval(approval, s.Pool, user.ID)
	if err != nil {
		s.logAbandonedFailure(r, user, err)
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
	err = quickconnect.Approve(approval, s.Pool, value, user.ID, sessionOf(r).Token)
	if err == nil {
		if releaseErr := ticket.Release(approval, s.Pool); releaseErr != nil {
			s.Log.Warn("an approval's reservation was not given back", "error", releaseErr)
		}
		writeJSON(w, true)
		return
	}
	s.logAbandonedFailure(r, user, err)
	// A code that is unknown, expired or already approved is a guess at someone's account, so it keeps the
	// reservation. So does every outcome the database never answered: the timeout, a lost connection. An error it
	// did answer with (a cancelled statement or a terminated backend included) says nothing about the code, and a
	// client cannot cause one, so that gives it back.
	// A token that ended meanwhile says nothing about the code either, and none was looked at.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) || errors.Is(err, quickconnect.ErrSignedOut) {
		if releaseErr := ticket.Release(approval, s.Pool); releaseErr != nil {
			s.Log.Warn("an approval's reservation was not given back", "error", releaseErr)
		}
	}
	switch {
	case errors.Is(err, quickconnect.ErrSignedOut):
		s.Log.Warn("an approval was refused: its token ended while it was made", "user", user.Name)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Secret == "" ||
		!storable(body.Secret) {
		http.Error(w, "Error processing request.", http.StatusBadRequest)
		return
	}
	code, account, err := quickconnect.Redeem(r.Context(), s.Pool, body.Secret)
	if errors.Is(err, quickconnect.ErrUnknownSecret) || errors.Is(err, quickconnect.ErrNotApproved) {
		http.Error(w, "Error processing request.", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The account was read in the transaction that issued the token, so a sign-in or sign-out that ends the token
	// after its commit cannot turn this answer into a server error.
	user := User{
		ID: account.ID, Name: account.Name, IsAdmin: account.IsAdmin, AllowDownload: account.AllowDownload,
		LastLoginAt: account.LastLoginAt, Configuration: account.Configuration,
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
