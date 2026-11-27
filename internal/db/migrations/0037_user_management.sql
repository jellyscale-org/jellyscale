-- User management (JS-033). The two Policy fields jellyscale enforces beside is_admin. An account with no password has
-- an empty password_hash, which no password matches (D-105).
ALTER TABLE users ADD COLUMN is_disabled boolean NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN allow_download boolean NOT NULL DEFAULT true;
