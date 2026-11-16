-- A viewer's saved settings (Jellyfin's UserConfiguration) as one document, so a
-- save is one statement and two saves never mix. It rides on the row every
-- request reads to authenticate. A user who has saved nothing has none (NULL),
-- and answers as a new Jellyfin user does.
ALTER TABLE users ADD COLUMN configuration jsonb;
