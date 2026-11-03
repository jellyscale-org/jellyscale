-- A fingerprint of the subtitle files beside a video (their keys, sizes and
-- ETags), so a scan sees a file added, changed or removed without probing the
-- film again. NULL: scanned before sidecars were listed, so the next scan
-- probes the film again to list them and its attachments. It is on the media
-- source and not on scan_objects because an older version's scan deletes and
-- rewrites the media source without it, which is how the new scan learns that
-- the files it lists are gone; scan_objects it only updates in place.
ALTER TABLE media_sources ADD COLUMN sidecars text;

-- The index the next subtitle file of a film will take: one past the highest
-- ever listed, removed files included. It survives the scan replacing the row,
-- so an index is never given to a second file.
ALTER TABLE media_sources ADD COLUMN next_index integer NOT NULL DEFAULT 0;

-- What a job makes: one subtitle track, or the fonts attached to one film.
ALTER TABLE subtitle_jobs ADD COLUMN kind text NOT NULL DEFAULT 'track' CHECK (kind IN ('track', 'fonts'));

-- A failed job whose cause is the file's (it can't be read) or that has been
-- given up on; any other failure (the store, a signal, a full disk) may pass at
-- the next ask, and waiters are told so.
ALTER TABLE subtitle_jobs ADD COLUMN permanent boolean NOT NULL DEFAULT false;
