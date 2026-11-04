-- A third kind of subtitle job: copying a PGS track out of the film as it is
-- (a .sup file), for clients that draw PGS themselves.
ALTER TABLE subtitle_jobs DROP CONSTRAINT subtitle_jobs_kind_check;
ALTER TABLE subtitle_jobs ADD CONSTRAINT subtitle_jobs_kind_check CHECK (kind IN ('track', 'fonts', 'sup'));
