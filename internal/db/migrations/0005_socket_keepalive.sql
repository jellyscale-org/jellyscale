-- Rows that stop refreshing this came from a replica that crashed.
ALTER TABLE session_sockets ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();
