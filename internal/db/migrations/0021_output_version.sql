-- The playback.OutputVersion a job's segments are made by, and a worker makes. A
-- worker claims only jobs of its own version, so an older worker leaves a newer
-- spec queued for a newer one. A replica built before this column writes none:
-- 4 was the last output version before rows recorded one.
ALTER TABLE transcode_jobs ADD COLUMN output_version integer NOT NULL DEFAULT 4;
ALTER TABLE transcode_nodes ADD COLUMN output_version integer NOT NULL DEFAULT 4;
