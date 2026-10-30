CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE server (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    server_id text NOT NULL
);

CREATE TABLE users (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    password_hash text NOT NULL,
    is_admin boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);
CREATE UNIQUE INDEX users_name_key ON users (lower(name));

CREATE TABLE devices (
    token text PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    device_id text NOT NULL,
    device_name text NOT NULL,
    client text NOT NULL,
    app_version text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX devices_user_idx ON devices (user_id);

CREATE TABLE display_preferences (
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    preference_id text NOT NULL,
    client text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY (user_id, preference_id, client)
);

CREATE TABLE libraries (
    id uuid PRIMARY KEY,
    -- The id derives from the name, so a second unique index here would only
    -- race the upsert every replica runs at startup.
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('movies', 'tvshows')),
    media_store_uri text NOT NULL,
    asset_store_uri text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE items (
    id uuid PRIMARY KEY,
    library_id uuid NOT NULL REFERENCES libraries ON DELETE CASCADE,
    parent_id uuid REFERENCES items ON DELETE CASCADE,
    series_id uuid REFERENCES items ON DELETE CASCADE,
    season_id uuid REFERENCES items ON DELETE CASCADE,
    type text NOT NULL CHECK (type IN ('Movie', 'Series', 'Season', 'Episode')),
    name text NOT NULL,
    sort_name text NOT NULL,
    natural_key text NOT NULL,
    production_year integer,
    run_time_ticks bigint,
    index_number integer,
    parent_index_number integer,
    community_rating real,
    premiere_date timestamptz,
    date_created timestamptz NOT NULL DEFAULT now(),
    genres text[] NOT NULL DEFAULT '{}',
    primary_image_key text,
    primary_image_tag text,
    dto jsonb NOT NULL
);
CREATE UNIQUE INDEX items_natural_key ON items (library_id, natural_key);
CREATE INDEX items_parent_idx ON items (parent_id);
CREATE INDEX items_series_idx ON items (series_id);
CREATE INDEX items_library_type_sort_idx ON items (library_id, type, sort_name);
CREATE INDEX items_type_sort_idx ON items (type, sort_name);
CREATE INDEX items_type_created_idx ON items (type, date_created DESC);
CREATE INDEX items_genres_idx ON items USING gin (genres);
CREATE INDEX items_name_trgm_idx ON items USING gin (name gin_trgm_ops);

CREATE TABLE media_sources (
    id uuid PRIMARY KEY,
    item_id uuid NOT NULL REFERENCES items ON DELETE CASCADE,
    store_key text NOT NULL,
    size bigint NOT NULL,
    object_etag text NOT NULL,
    container text NOT NULL,
    streams jsonb NOT NULL
);
CREATE INDEX media_sources_item_idx ON media_sources (item_id);

CREATE TABLE user_item_data (
    user_id uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    item_id uuid NOT NULL REFERENCES items ON DELETE CASCADE,
    played boolean NOT NULL DEFAULT false,
    favorite boolean NOT NULL DEFAULT false,
    play_count integer NOT NULL DEFAULT 0,
    position_ticks bigint NOT NULL DEFAULT 0,
    last_played_at timestamptz,
    PRIMARY KEY (user_id, item_id)
);
CREATE INDEX user_item_data_item_idx ON user_item_data (item_id);

-- What the last scan saw, so the next one lists and diffs instead of re-probing.
CREATE TABLE scan_objects (
    library_id uuid NOT NULL REFERENCES libraries ON DELETE CASCADE,
    key text NOT NULL,
    size bigint NOT NULL,
    etag text NOT NULL,
    item_id uuid REFERENCES items ON DELETE SET NULL,
    PRIMARY KEY (library_id, key)
);
