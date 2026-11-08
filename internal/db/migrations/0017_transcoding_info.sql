-- What GET /Sessions says about a transcode (TranscodingInfo), kept in Postgres
-- so any api replica can answer.
--
-- encode_speed is film seconds per second of the job's running time, as the
-- worker measures it (transcode.progress), written with its heartbeat. A job
-- paused at its lead limit keeps the speed it had.
ALTER TABLE transcode_jobs ADD COLUMN encode_speed double precision;

-- The reasons PlaybackInfo gave for a play session's transcode of a spec, which
-- the session lists later; nothing else of the decision is kept. Rows are
-- pruned when a newer one is written.
CREATE TABLE transcode_reasons (
    play_session_id text NOT NULL,
    spec_hash text NOT NULL,
    reasons text[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (play_session_id, spec_hash)
);

-- PlaybackInfo prunes rows older than a day each time it writes one.
CREATE INDEX transcode_reasons_created_at ON transcode_reasons (created_at);
