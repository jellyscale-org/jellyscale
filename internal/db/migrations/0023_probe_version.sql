-- The version of the probe that last wrote each media source (library.ProbeVersion).
-- A scan probes a source again, though its file is the same, when this is below the
-- binary's. Every source stored before this is 0, and so is a row that the previous
-- release inserts during a rolling upgrade without naming the column.
ALTER TABLE media_sources ADD COLUMN probe_version integer NOT NULL DEFAULT 0;
