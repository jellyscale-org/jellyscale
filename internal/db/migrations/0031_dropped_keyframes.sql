-- The keyframe index a job found wrong (JS-088). The index itself is emptied, so that the film is no longer offered
-- as a copy; the playlists already cut from it go on, as the re-encode of the copy (playback.Spec.Recode), and are
-- still cut from this list. A rescan writes a new row.
ALTER TABLE media_sources ADD COLUMN dropped_keyframes bigint[] NOT NULL DEFAULT '{}';
