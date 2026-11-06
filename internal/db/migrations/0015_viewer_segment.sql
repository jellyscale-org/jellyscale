-- The segment a viewer asked for last, or -1 if it has asked for none. A worker
-- stops a job that is transcodeLead ahead of the furthest of its viewers.
ALTER TABLE transcode_viewers ADD COLUMN segment integer NOT NULL DEFAULT -1;
