-- A job that failed for a cause that is not the file's (a store refusing or cutting a
-- read, ffmpeg killed by a signal) is made again for the request that waited on it, and
-- does not refuse its spec for a while as a failure of the file does (JS-088).
ALTER TABLE transcode_jobs ADD COLUMN again boolean NOT NULL DEFAULT false;
