-- A viewer's track preferences (JS-118) as one document, apart from `configuration` so
-- that Jellyfin's UserConfiguration never carries them. A viewer who has saved none has
-- NULL and answers the defaults. It is written by one statement, so two PATCHes at once
-- both land.
ALTER TABLE users ADD COLUMN track_preferences jsonb;
