package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

// defaultSocketIdle is the interval ForceKeepAlive promises clients. The web
// client pings at half of it, so a silent socket is a dead one.
const defaultSocketIdle = 60 * time.Second

// socket upgrades to the WebSocket jellyfin-web opens after login. Behavior is
// from a real Jellyfin: the server opens with ForceKeepAlive carrying the
// interval in seconds, the client answers KeepAlive, and the server echoes it.
// Pushing commands from other replicas is a later phase.
func (s *Server) socket(w http.ResponseWriter, r *http.Request) {
	token := sessionOf(r).Token
	// The token in the query authenticates the upgrade, and browsers can't
	// send custom headers on one, so an Origin check adds nothing here.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has answered.
	}
	idle := s.SocketIdle
	if idle <= 0 {
		idle = defaultSocketIdle
	}
	replica := s.replica()
	connID := make([]byte, 8)
	rand.Read(connID) //nolint:errcheck
	connectionID := hex.EncodeToString(connID)
	if err := s.attachSocket(r.Context(), token, connectionID, replica); err != nil {
		s.Log.Error("record socket replica", "error", err)
		conn.Close(websocket.StatusInternalError, "internal error")
		return
	}
	defer func() {
		// The request context may be gone by now.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.detachSocket(ctx, token, connectionID); err != nil {
			s.Log.Error("clear socket replica", "error", err)
		}
	}()

	timer := time.AfterFunc(idle, func() { conn.Close(websocket.StatusGoingAway, "idle") })
	defer timer.Stop()
	ctx := r.Context()
	if err := writeMessage(ctx, conn, map[string]any{"MessageType": "ForceKeepAlive", "Data": int(defaultSocketIdle / time.Second)}); err != nil {
		return
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow() //nolint:errcheck
			return
		}
		timer.Reset(idle)
		var message struct{ MessageType string }
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message.MessageType == "KeepAlive" {
			if _, err := s.Pool.Exec(ctx, "UPDATE session_sockets SET last_seen_at = now() WHERE connection_id = $1", connectionID); err != nil {
				s.Log.Error("record socket keepalive", "error", err)
			}
			if err := writeMessage(ctx, conn, map[string]any{"MessageType": "KeepAlive"}); err != nil {
				return
			}
		}
	}
}

// syncWSReplica points ws_replica at the newest open socket, or NULL. Callers
// hold the session row's lock, so the choice can't interleave with another
// socket's connect or close.
const syncWSReplica = `UPDATE sessions SET ws_replica = (SELECT replica FROM session_sockets
	WHERE token = $1 ORDER BY connected_at DESC, connection_id DESC LIMIT 1) WHERE token = $1`

// lockSession takes the device's session row lock for the transaction. Every
// connect and close of one device queues here, and each statement after it sees
// what the previous holder committed.
const lockSession = "SELECT 1 FROM sessions WHERE token = $1 FOR UPDATE"

// attachSocket records an open socket, creating the session row from the login
// if it has none. A device can hold several sockets at once, so the claim
// follows the newest rather than the last to write.
func (s *Server) attachSocket(ctx context.Context, token, connectionID, replica string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The upsert also takes the row lock.
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (token, user_id, client, device_id, device_name, app_version)
			SELECT d.token, d.user_id, d.client, d.device_id, d.device_name, d.app_version FROM devices d WHERE d.token = $1
			ON CONFLICT (token) DO UPDATE SET last_seen_at = now()`, token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, deleteQuietSockets, token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO session_sockets (connection_id, token, replica) VALUES ($1, $2, $3)`,
			connectionID, token, replica); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, syncWSReplica, token)
		return err
	})
}

// detachSocket forgets a closed socket. ws_replica falls back to the newest
// socket still open, here or on another replica, and clears only when none is.
func (s *Server) detachSocket(ctx context.Context, token, connectionID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, lockSession, token); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM session_sockets WHERE connection_id = $1", connectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, deleteQuietSockets, token); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, syncWSReplica, token)
		return err
	})
}

// deleteQuietSockets drops rows left by a crashed replica, so one can't become
// the fallback claim. Live sockets refresh last_seen_at on every KeepAlive,
// which clients send well inside this window.
const deleteQuietSockets = `DELETE FROM session_sockets WHERE token = $1 AND last_seen_at < now() - interval '2 minutes'`

func writeMessage(ctx context.Context, conn *websocket.Conn, message map[string]any) error {
	id := make([]byte, 16)
	rand.Read(id) //nolint:errcheck
	message["MessageId"] = hex.EncodeToString(id)
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, body)
}
