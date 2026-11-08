-- A helper job encodes a later chunk of a spec that a slower-than-real-time job
-- is still working towards (parallel transcoding). It belongs to the spec's
-- viewers as a whole, not to one of them, so stopping one of several viewers
-- leaves it, and it is bounded by the furthest segment any of them asked for.
ALTER TABLE transcode_jobs ADD COLUMN helper boolean NOT NULL DEFAULT false;
