-- A fifth kind of subtitle job, JS-089: one per film file, which reads the film
-- once and stores each image track inside it (PGS, VobSub, DVB) as a file of its
-- own in the asset store (key subtitles/{source}/{etag}/images), so that a run that
-- begins mid-film draws the cue showing at its cut from that file and not by
-- reading the film from where the cue began.
ALTER TABLE subtitle_jobs DROP CONSTRAINT subtitle_jobs_kind_check;
ALTER TABLE subtitle_jobs ADD CONSTRAINT subtitle_jobs_kind_check CHECK (kind IN ('track', 'fonts', 'sup', 'chapters', 'images'));
-- The tracks an images job that ended done left out because the film cannot give
-- them (ffmpeg names no such stream, or cannot decode it): they are not read for
-- again until the file changes, which is a new key and so a new row.
ALTER TABLE subtitle_jobs ADD COLUMN skipped integer[] NOT NULL DEFAULT '{}';
