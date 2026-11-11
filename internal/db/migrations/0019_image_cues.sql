-- Where the packets of a Matroska film's image subtitle tracks lie (library.ImageCues,
-- from the file's Cues), by stream index, so that a transcode that begins mid-film
-- can read the packet of the cue showing at its cut. NULL: not read yet (a film
-- scanned by an earlier release, or a read that failed); the next scan reads it.
-- {} : read, and the file lists no points for an image track. A column of its own,
-- because it grows with the film and the item reads do not want it.
ALTER TABLE media_sources ADD COLUMN image_cues jsonb;
