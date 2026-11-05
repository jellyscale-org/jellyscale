-- A film's chapters as its item DTO lists them (name and start position). NULL:
-- scanned before chapters were read, or by an older version's scan, which
-- rewrites the row without them; the next scan probes the film again (not its
-- keyframe index) to read them. An empty array is a film with none. It is on
-- the media source and not on the item for the same reason as sidecars.
ALTER TABLE media_sources ADD COLUMN chapters jsonb;
