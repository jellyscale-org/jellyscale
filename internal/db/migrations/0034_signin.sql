-- What bounds failed sign-ins (JS-145, D-020). Every time is compared with Postgres's clock and no process keeps a copy,
-- so the bounds hold across replicas and restarts.

-- One row per failed sign-in that is still being counted, by what it counts toward: kind is name (a stranger's failures
-- for a name, by a hash of its lower-case text, so that what a viewer typed in the name box is not stored), device (a known
-- device's, user id and device id) or address. Rows are kept for twice the counting window, so that a refusal that began
-- when the count reached its limit is found for its whole length.
CREATE TABLE signin_failures (
    id bigserial PRIMARY KEY,
    kind text NOT NULL,
    key text NOT NULL,
    at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX signin_failures_key_idx ON signin_failures (kind, key, at);
CREATE INDEX signin_failures_at_idx ON signin_failures (at);

-- The names that are no account and have failed, so that what is stored for them has a fixed bound: when there are more than
-- the bound, the one with the fewest failures, then the oldest, is forgotten. signin_unknown_count is how many there are, as
-- the sum of its rows.
CREATE TABLE signin_unknown (
    key text PRIMARY KEY,
    failures integer NOT NULL,
    last_at timestamptz NOT NULL
);
CREATE INDEX signin_unknown_weight_idx ON signin_unknown (failures, last_at);
CREATE TABLE signin_unknown_count (
    stripe integer PRIMARY KEY,
    n bigint NOT NULL
);

-- When a device's token for an account ended (signed out, or replaced): devices keeps only live tokens.
CREATE TABLE signin_device_ended (
    user_id uuid NOT NULL,
    device_id text NOT NULL,
    ended_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, device_id)
);
