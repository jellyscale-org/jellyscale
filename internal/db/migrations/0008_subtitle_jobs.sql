-- One row per subtitle track that has been asked for and isn't in the asset
-- store yet, keyed by the object it will become, so a track has one extraction
-- however many requests, replicas and burn-in jobs ask. It is not in
-- transcode_jobs because it has no spec, viewer or owner and must not count
-- against transcode slots.
CREATE TABLE subtitle_jobs (
    key text PRIMARY KEY,
    item_id text NOT NULL,
    source_id text NOT NULL,
    idx integer NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed')),
    worker text,
    lease_expires_at timestamptz,
    -- Claims so far. A lease that expires after the third fails the job.
    attempts integer NOT NULL DEFAULT 0,
    error text,
    ended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
