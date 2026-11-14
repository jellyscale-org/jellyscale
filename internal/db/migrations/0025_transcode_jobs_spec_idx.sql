-- A request waiting for a segment asks, on every poll, whether any job of its
-- spec recorded the segment. Done jobs stay a day, and the only index on
-- spec_hash covers queued and running ones, so that probe scanned the table.
CREATE INDEX transcode_jobs_spec_idx ON transcode_jobs (spec_hash, start_segment);
