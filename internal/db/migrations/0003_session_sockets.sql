-- One row per open WebSocket. A device can hold several at once (a reconnect
-- overlaps the socket it replaces, and the old one may be on another replica),
-- so sessions.ws_replica follows the newest row instead of the last writer.
-- A crashed replica leaves its rows behind; cleaning those up is a later phase.
ALTER TABLE sessions DROP COLUMN IF EXISTS ws_conn;
CREATE TABLE session_sockets (
    connection_id text PRIMARY KEY,
    token text NOT NULL REFERENCES sessions ON DELETE CASCADE,
    replica text NOT NULL,
    connected_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX session_sockets_token_idx ON session_sockets (token, connected_at DESC);
