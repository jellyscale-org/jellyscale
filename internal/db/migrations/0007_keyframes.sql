-- Where the video's keyframes are, in 100 ns ticks, read from the file's own
-- index (Matroska Cues, MP4 stss) at scan time. A copied stream is cut on
-- them. NULL: not looked at yet, so a scan reads it. Empty: the file has no
-- usable index, and is re-encoded. video_end is where the last video frame
-- ends on the same clock, read with them.
ALTER TABLE media_sources ADD COLUMN keyframes bigint[];
ALTER TABLE media_sources ADD COLUMN video_end bigint NOT NULL DEFAULT 0;
