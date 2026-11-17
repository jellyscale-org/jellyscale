-- What the workers' sweeps share (JS-078). One row per kind of sweep: at is
-- when a worker last took the turn, so that however many workers there are the
-- stores are listed once per sweep period (a worker takes it with one UPDATE
-- that only the first of them wins); store_bytes is what the last good listing
-- of the transcode store found, which every worker reports as the size metric
-- and which a failed listing leaves as it was (NULL before the first).
CREATE TABLE sweeps (
    name text PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    store_bytes bigint
);
