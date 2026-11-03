-- data_at is when a row's played state and position were last true, which a
-- synced write is compared against. last_played_at stays what clients show as
-- LastPlayedDate.
ALTER TABLE user_item_data ADD COLUMN data_at timestamptz;
UPDATE user_item_data SET data_at = last_played_at;
