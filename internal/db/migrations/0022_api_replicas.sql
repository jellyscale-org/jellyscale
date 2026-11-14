-- One row per api replica, beaten every second while it lives. A socket on a
-- replica that has stopped beating is a dead one's, whatever its client does.
CREATE TABLE api_replicas (
    id text PRIMARY KEY,
    beat_at timestamptz NOT NULL DEFAULT now()
);
