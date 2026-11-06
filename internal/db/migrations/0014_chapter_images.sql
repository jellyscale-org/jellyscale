-- Chapter images are opt-in per library (off by default, as in Jellyfin 12.4's
-- EnableChapterImageExtraction). It is a column and not worker-only config so
-- that every role stops listing tags and serving images when it is turned off.
ALTER TABLE libraries ADD COLUMN chapter_images boolean NOT NULL DEFAULT false;

-- A fourth kind of job: one per film file, which makes the pictures of its
-- chapters, or deletes them when they are not wanted (key chapters/{source}).
-- It has queue slots of its own, so a library full of them can't hold up a
-- viewer's subtitle.
ALTER TABLE subtitle_jobs DROP CONSTRAINT subtitle_jobs_kind_check;
ALTER TABLE subtitle_jobs ADD CONSTRAINT subtitle_jobs_kind_check CHECK (kind IN ('track', 'fonts', 'sup', 'chapters'));

-- What a chapter-images job has made for a film file. The pictures are under
-- chapters/{source_id}/ in the library's asset store and are found by that
-- prefix, never by this row; keys (the asset key of each chapter's picture, in
-- chapter order, '' for one that has none) are only for serving, and are
-- written as the job handles each chapter, so a later attempt resumes after them. There is no foreign key to the item or the
-- media source: the row must outlive a removed source (and a scan that writes
-- the media source again), so that the next job can still find the library's
-- asset store and delete the pictures, and only the job deletes it, after the
-- objects. The source's id is the item's.
-- etag says which file version the row is for. attempts counts the starts of a
-- job for that version and is not reset by the sweep of subtitle_jobs.
CREATE TABLE chapter_images (
    source_id uuid PRIMARY KEY,
    library_id uuid NOT NULL,
    etag text NOT NULL,
    state text NOT NULL CHECK (state IN ('making', 'done')),
    attempts integer NOT NULL DEFAULT 0,
    keys text[] NOT NULL DEFAULT '{}'
);
