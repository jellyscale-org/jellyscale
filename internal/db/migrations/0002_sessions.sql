-- What each device is playing. Keyed by the device's token, so a new login
-- (which replaces the token) drops the row with it.
CREATE TABLE sessions (
    token text PRIMARY KEY REFERENCES devices ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    client text NOT NULL,
    device_id text NOT NULL,
    device_name text NOT NULL,
    app_version text NOT NULL,
    item_id uuid REFERENCES items ON DELETE SET NULL,
    position_ticks bigint NOT NULL DEFAULT 0,
    is_paused boolean NOT NULL DEFAULT false,
    play_method text,
    play_session_id text,
    -- The replica holding this device's WebSocket, if any.
    ws_replica text,
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- Progress reports write only when this is older than the coalescing window.
ALTER TABLE user_item_data ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
