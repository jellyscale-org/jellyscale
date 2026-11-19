# What jellyfin-web does when an AV1 copy goes on as another stream (JS-133)

JS-088 sends a re-encode of a copy under a fresh init in front of each segment and the player plays on (compat/captures/segment-errors-12.4.md,
x264 under x264). For an AV1 copy the question left open there is the codec: may the fallback be H.264, which x264 makes
restart-safe already, or must it be AV1 (SVT-AV1)? The master playlist says CODECS av01 and jellyfin-web asks no new
playlist (segment-errors-12.4.md), so the SourceBuffer is typed AV1 when the player adds it.

## Observed: jellyfin-web 12.4's hls.js 1.6.16 in Chrome 157

`compat/captures/av1-fallback-spike.mjs` (numbers in av1-fallback-12.4.json, which holds the second run only: there the H.264 forward direction showed no error and the failure came on the seek back, while the first run failed forward at once; a rerun in review matched the first. The H.264 back direction failed with the error and the dialog in every run, and the choice rests on it. The 10-bit section's numbers come from a run that was not stored). A master playlist with CODECS `av01.0.04M.08`, a
12 segment playlist of 4 s whose EXT-X-MAP names the AV1 init, segments 0 to 4 AV1 (SVT-AV1, a keyframe every 4 s) and 5 to 11
another stream, each with its own init in front. Played forward from 14 s to 30 s across the change at 20 s, then seeked back
to 10 s (an AV1 segment) and played forward to 30 s across it again. The ffmpeg lines that make the streams:

    ffmpeg -f lavfi -i testsrc2=size=640x360:rate=24:duration=48 -pix_fmt yuv420p -c:v libsvtav1 -preset 10 -g 96 -force_key_frames "expr:gte(t,n_forced*4)" -an -f hls -hls_time 4 -hls_segment_type fmp4 -hls_fmp4_init_filename init.mp4 -hls_segment_filename 'A/%d.m4s' A/index.m3u8
    (B: -c:v libx264 -profile:v high -preset veryfast -g 96 -sc_threshold 0; C: libsvtav1 -preset 8)

| the later segments are | direction | frames shown | video.error | a dialog | currentTime keeps its place |
|---|---|---|---|---|---|
| AV1, no change at all (control) | forward | 389 of 384 | none | no | yes |
| | back, forward | 486 | none | no | yes |
| AV1 (a second AV1 stream, its own init) | forward | 391 | none | no | yes (never back, no jump) |
| | back, forward | 485 | none | no | yes |
| H.264 (its own init) | forward | 0 past 16 s: the player stalls at 16.04 s, where the buffer ends before the change | CHUNK_DEMUXER_ERROR_APPEND_FAILED: "Video stream codec h264 doesn't match SourceBuffer codecs." (first run: 1 frame, at once; second run: on the seek back) | yes, a fatal bufferAppendError (jellyfin-web raises its dialog on a fatal error) | no: it stops at the change |
| | back, forward | 0 | the same error | yes | no |

So an H.264 segment cannot follow an AV1 SourceBuffer, whatever init leads it: Chrome refuses the append and ends the
stream. An AV1 stream under its own init goes on, in both directions, with frames, no error and no dialog.

## The choice

The fallback of an AV1 copy is AV1 (SVT-AV1), for every AV1 copy. H.264 would have needed both directions to show frames with no
video.error and no dialog, and it shows none. Each fallback segment, and each stored copy segment answered after the
fallback, leads with the init segment of its own stream (the copy's own AV1 init for a copy segment).

## A 10-bit AV1 copy (same script, CODECS `av01.0.04M.10`)

A 10-bit AV1 copy (yuv420p10le, SVT-AV1) whose segments from 20 s are another AV1 stream of 8 bits (yuv420p), each with
its own init, played the same two directions: forward 389 frames, back and forward 486, no video.error, no dialog, the
position kept. So an 8-bit AV1 fallback is taken under a SourceBuffer typed with a 10-bit AV1 codec string; whether a 10-bit
fallback is taken too was not observed. (Run: the script with `av01.0.04M.08` replaced by `av01.0.04M.10` and A/ made with `-pix_fmt yuv420p10le`.)
