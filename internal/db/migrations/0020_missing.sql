-- A file a scan no longer lists hides its item instead of deleting it, and a
-- library left out of config hides itself: both keep their rows (and so every
-- viewer's watch state) until a grace has passed. The stamps are Postgres's
-- own clock, so every worker agrees on how long ago it was.
--
-- items.missing_since: set by the scan that first missed the item's file (or,
-- for a season or series, that left it no episode that is listed); cleared when
-- the file returns.
ALTER TABLE items ADD COLUMN missing_since timestamptz;
-- libraries.gone_at: set by the first start without the library in config.
ALTER TABLE libraries ADD COLUMN gone_at timestamptz;

-- What a viewer can reach. Every reader outside the scan reads this and not
-- items, so a hidden item is as unknown to the next route as to the first.
CREATE VIEW shown_items AS
SELECT i.* FROM items i JOIN libraries l ON l.id = i.library_id
WHERE i.missing_since IS NULL AND l.gone_at IS NULL;

-- The scan looks for a library's hidden items.
CREATE INDEX items_missing_idx ON items (library_id) WHERE missing_since IS NOT NULL;

-- Deleting an item (the scan's removals, a library's) checks these two foreign
-- keys for each row; unindexed, each check read the whole table.
CREATE INDEX items_season_idx ON items (season_id);
CREATE INDEX scan_objects_item_idx ON scan_objects (item_id);
