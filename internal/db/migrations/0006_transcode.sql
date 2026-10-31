-- One row per ffmpeg run. A spec can have several active jobs, one per viewer
-- who seeked somewhere the others' jobs don't reach.
CREATE TABLE transcode_jobs (
    id bigserial PRIMARY KEY,
    spec_hash text NOT NULL,
    spec jsonb NOT NULL,
    -- The PlaySessionId that asked for it. A new job from the same owner
    -- replaces its previous one.
    owner text NOT NULL,
    start_segment integer NOT NULL,
    -- The first segment the job hasn't stored yet.
    next_segment integer NOT NULL,
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'failed', 'cancelled')),
    worker text,
    lease_expires_at timestamptz,
    error text,
    -- When it reached done, failed or cancelled; PlaybackInfo remembers a failure from then.
    ended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX transcode_jobs_active_idx ON transcode_jobs (spec_hash) WHERE state IN ('queued', 'running');
CREATE INDEX transcode_jobs_queue_idx ON transcode_jobs (id) WHERE state = 'queued';

-- Who is watching a spec. A spec's jobs live while a viewer who hasn't been
-- stopped has asked for something recently. stopped marks a viewer that was
-- told to stop, so a request still in flight can't revive it.
CREATE TABLE transcode_viewers (
    play_session_id text NOT NULL,
    spec_hash text NOT NULL,
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    device_id text NOT NULL,
    -- The compact item ID, for a Stopped report that names no session: the
    -- device and item stop it.
    item_id text NOT NULL DEFAULT '',
    -- The job this viewer last waited on, so a viewer who seeks replaces their
    -- own earlier job but not one that another viewer joined. 0 before any.
    job_id bigint NOT NULL DEFAULT 0,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    stopped boolean NOT NULL DEFAULT false,
    PRIMARY KEY (play_session_id, spec_hash)
);
CREATE INDEX transcode_viewers_spec_idx ON transcode_viewers (spec_hash, last_seen_at);

-- Every process that takes part in transcoding, with the roles it runs and its
-- transcodeStore, renewed every few seconds. PlaybackInfo compares the stores
-- of the live rows, and refuses when they differ, because a client that is
-- told nothing retries forever. A worker has slots; other nodes have none. The
-- store is a URI, which never holds credentials (invariant 12), so it can be
-- shown in a log.
CREATE TABLE transcode_nodes (
    id text PRIMARY KEY,
    roles text NOT NULL DEFAULT '',
    store text NOT NULL DEFAULT '',
    slots integer NOT NULL DEFAULT 0,
    seen_at timestamptz NOT NULL DEFAULT now()
);
