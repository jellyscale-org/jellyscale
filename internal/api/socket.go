package api

import (
	"context"
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
	connectionID := randomHex(8)
	replica := s.replica()
	// A replica told to stop closes its sockets and waits here until their rows are gone.
	s.sockets.Add(1)
	defer s.sockets.Done()
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
	stopWatching := make(chan struct{})
	defer close(stopWatching)
	go func() {
		select {
		case <-s.socketsClosing():
			conn.Close(websocket.StatusGoingAway, "the server is restarting")
		case <-stopWatching:
		}
	}()
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
		// Any frame proves the socket is alive, as it does to the idle timer.
		if err := s.touchSocket(ctx, token, connectionID, replica); err != nil {
			s.Log.Error("record socket activity", "error", err)
		}
		var message struct{ MessageType string }
		if json.Unmarshal(data, &message) == nil && message.MessageType == "KeepAlive" {
			if err := writeMessage(ctx, conn, map[string]any{"MessageType": "KeepAlive"}); err != nil {
				return
			}
		}
	}
}

// touchSocket marks the socket seen. A row that is gone (swept while its replica
// could not reach Postgres) is made again, and the claim on the device is
// settled, so a live socket never stays unowned.
func (s *Server) touchSocket(ctx context.Context, token, connectionID, replica string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := beat(ctx, tx, replica); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, lockSession, token); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE session_sockets SET last_seen_at = now() WHERE connection_id = $1", connectionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO session_sockets (connection_id, token, replica) VALUES ($1, $2, $3)`, connectionID, token, replica); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, syncWSReplica, token)
		return err
	})
}

func beat(ctx context.Context, tx pgx.Tx, replica string) error {
	_, err := tx.Exec(ctx, `INSERT INTO api_replicas (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET beat_at = now()`, replica)
	return err
}

func (s *Server) socketsClosing() chan struct{} {
	s.socketsOnce.Do(func() { s.closeSockets = make(chan struct{}) })
	return s.closeSockets
}

// CloseSockets closes every WebSocket this replica holds, with a close frame, and
// returns once their rows are removed or ctx ends. Hijacked connections are not
// something http.Server.Shutdown waits for, so this does.
func (s *Server) CloseSockets(ctx context.Context) {
	closing := s.socketsClosing()
	select {
	case <-closing:
	default:
		close(closing)
	}
	done := make(chan struct{})
	go func() { s.sockets.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// KeepReplica beats for this replica every second, and every few seconds
// sweeps: the sockets of replicas that have stopped beating are deleted, and
// each device's ws_replica goes to its newest socket on a live replica, or none.
// A killed replica's claim on a device is gone seconds after its last beat, with
// no help from the device. It runs on every api replica until ctx ends.
func (s *Server) KeepReplica(ctx context.Context) {
	ticker := time.NewTicker(beatEvery)
	defer ticker.Stop()
	for tick := 1; ; tick++ {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO api_replicas (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET beat_at = now()`, s.replica()); err == nil && tick%sweepEvery == 0 {
			s.sweepSockets(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) sweepSockets(ctx context.Context) {
	s.Pool.Exec(ctx, "DELETE FROM api_replicas WHERE beat_at < now() - interval '1 hour'") //nolint:errcheck // Only housekeeping.
	// Devices claimed by, or holding sockets of, a replica that is not beating.
	rows, err := s.Pool.Query(ctx, `SELECT token FROM sessions WHERE ws_replica IS NOT NULL AND ws_replica NOT IN `+liveReplicas+`
		UNION SELECT token FROM session_sockets WHERE replica NOT IN `+forgottenReplicas)
	if err != nil {
		return // Postgres away: the next sweep asks again.
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return
	}
	for _, token := range tokens {
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, lockSession, token); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, deleteDeadSockets, token); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, syncWSReplica, token)
			return err
		})
		if err != nil && ctx.Err() == nil {
			s.Log.Error("sweep sockets of dead replicas", "error", err)
		}
	}
}

// syncWSReplica points ws_replica at the newest open socket, or NULL. Callers
// hold the session row's lock, so the choice can't interleave with another
// socket's connect or close.
const syncWSReplica = `UPDATE sessions SET ws_replica = (SELECT replica FROM session_sockets
	WHERE token = $1 AND replica IN ` + liveReplicas + ` ORDER BY connected_at DESC, connection_id DESC LIMIT 1) WHERE token = $1`

// lockSession takes the device's session row lock for the transaction. Every
// connect and close of one device queues here, and each statement after it sees
// what the previous holder committed.
const lockSession = "SELECT 1 FROM sessions WHERE token = $1 FOR UPDATE"

// attachSocket records an open socket, creating the session row from the login
// if it has none. A device can hold several sockets at once, so the claim
// follows the newest rather than the last to write.
func (s *Server) attachSocket(ctx context.Context, token, connectionID, replica string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := beat(ctx, tx, replica); err != nil {
			return err
		}
		// The upsert also takes the row lock.
		if _, err := tx.Exec(ctx, touchSession, token); err != nil {
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
		if _, err := tx.Exec(ctx, deleteDeadSockets, token); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, syncWSReplica, token)
		return err
	})
}

// A replica is live for the three seconds after its last beat, so the claim on a
// device leaves a killed replica within that (two missed beats of a loaded host
// are not a death, and a claim that wrongly lapses is made again by the next
// frame of the socket). Its sockets are forgotten after 30 seconds without a
// beat. A replica that could not reach Postgres has its rows deleted meanwhile,
// and a live socket makes its row again with its next frame (touchSocket), which
// jellyfin-web sends every 30 seconds.
const (
	liveReplicas      = `(SELECT id FROM api_replicas WHERE beat_at > now() - interval '3 seconds')`
	forgottenReplicas = `(SELECT id FROM api_replicas WHERE beat_at > now() - interval '30 seconds')`
	deleteDeadSockets = "DELETE FROM session_sockets WHERE token = $1 AND replica NOT IN " + forgottenReplicas

	beatEvery  = time.Second
	sweepEvery = 5 // Beats between sweeps.
)

func writeMessage(ctx context.Context, conn *websocket.Conn, message map[string]any) error {
	message["MessageId"] = randomHex(16)
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, body)
}
