# Spike: segment seams

**Question:** can a fresh ffmpeg, restarted at segment N, replace segments from an earlier run with no `EXT-X-DISCONTINUITY`?

**Answer: yes, using fMP4.** It's clean on every encoder family tested, as long as the restart stays in the same family.

ffmpeg n9.0.2, 60s synthetic sources, 4s segments. Run `matrix.py <workdir> [case]`, `remux.py <workdir>`, and `players.mjs` from a directory with `playwright-core` installed.

## Results

| Test | Result |
|---|---|
| fMP4 matrix, 25 cases | 23 pass. The 2 failures are the CPU to GPU mixes |
| MPEG-TS matrix, 19 cases | All pass on timing, including mixes, but with continuity warnings |
| Encoders | x264, x265, NVENC H.264 and HEVC, VAAPI H.264 |
| Sources | 23.976, 25fps, 44.1kHz, variable frame rate, non-zero start |
| Remux, fMP4, restarts at 3 points | Video 1439/1439 frames at 0.00ms, audio within 0.33ms |
| hls.js 1.7.4, Shaka | 5 streams each: continuous buffer, 0 dropped frames, no errors |
| mpv, GStreamer | 5 streams each: exit 0, no errors on fMP4. mpv reports `Packet corrupt` on MPEG-TS |

A pass means: frame times match run A within 1ms, the frame count matches, and the click score is near baseline. Passing fMP4 cases have identical init segments and zero decoder warnings. Click score is the largest sample step within 200ms of the seam over the largest step elsewhere, measured on a 440Hz tone; baseline is about 1. Scores were 0.98 to 1.32, against 4.27 before the audio fix.

## Required settings

```
-copyts -ss <start> -i <src> -ss <audio_start> -i <src>
-map 0:v:0 -map 1:a:0 -to <end>
-force_key_frames expr:gte(t,n_forced*4)
-c:a aac -ar 48000 -bsf:a noise=drop=lt(pts\,<threshold>)
-output_ts_offset 100
-f hls -hls_segment_type fmp4
-hls_segment_options use_editlist=0:avoid_negative_ts=disabled:movflags=+frag_discont
```

- **Encoder:** fixed B-frame count and closed GOP. x265 lost 3 frames until `bframes=2:b-adapt=0:open-gop=0` was set.
- **Audio:** start 4 AAC frames early on the same 1024-sample grid as run A, and drop packets before A's cut point. Output is always 48kHz. Grid and threshold are relative to the source's start time.
- **Cut point:** the first audio packet at or after the keyframe's DTS. The reorder delay used for this is 2 frames, so it's tied to the encoder settings.
- **`force_key_frames`:** `t` stays output-relative under `-copyts`.

## Remux

ffmpeg's input seek lands up to about a second before the target, and output `-ss` under `-copyts` didn't correct it. So don't try to control the start: let ffmpeg begin early, identify each output by its first keyframe using the keyframe index, and discard what isn't needed. `remux.py` does this. Tested only with regular 5s GOPs, so irregular GOPs are still open.

## Limits

- **fMP4 can't mix encoder families.** Init segments carry the encoder's parameters, and spliced x264 and NVENC lost about half the frames. MPEG-TS can mix because parameters are in-band.
- **MPEG-TS seams break RFC 8216 section 3,** because continuity counters reset. Treat MPEG-TS as a fallback.
- **Not tested:** AVPlayer, ExoPlayer, Intel QSV (no Intel GPU here), AV1, HDR, real film content, multiple audio tracks, and burned-in subtitles.
