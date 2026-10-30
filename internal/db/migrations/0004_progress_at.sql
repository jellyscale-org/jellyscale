-- Progress reports coalesce on this alone, so toggling favorite or played, or
-- the Playing insert, never suppresses the next report. Null until the first
-- progress or stop write.
ALTER TABLE user_item_data ADD COLUMN progress_at timestamptz;
ALTER TABLE user_item_data DROP COLUMN updated_at;
