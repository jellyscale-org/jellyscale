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
- **Cut point:** the first audio packet at or after the keyframe's DTS. x264 sees whole frame numbers, so the DTS is the keyframe's time rounded to a frame, minus 2 frames of reorder delay, which ties it to the encoder settings. Phase 3 corrected this: taking 2 frames off the unrounded time passed every case here, but duplicated one AAC packet at the seam when the source's start time sat off the frame grid (a 7.5 s start at 24000/1001 fps).
- **`force_key_frames`:** `t` stays output-relative under `-copyts`.

## Remux

ffmpeg's input seek lands up to about a second before the target, and output `-ss` under `-copyts` didn't correct it. So don't try to control the start: let ffmpeg begin early, identify each output by its first keyframe using the keyframe index, and discard what isn't needed. `remux.py` does this. Tested only with regular 5s GOPs, so irregular GOPs are still open.

## Limits

- **fMP4 can't mix encoder families.** Init segments carry the encoder's parameters, and spliced x264 and NVENC lost about half the frames. MPEG-TS can mix because parameters are in-band.
- **MPEG-TS seams break RFC 8216 section 3,** because continuity counters reset. Treat MPEG-TS as a fallback.
- **Not tested:** AVPlayer, ExoPlayer, Intel QSV (no Intel GPU here), AV1, HDR, real film content, multiple audio tracks, and burned-in subtitles.

## Phase 3: what the product runs

`internal/transcode/ffmpeg.go` builds the command, and `splice_test.go` runs this spike's check on it: frame times within 1 ms, audio packets within 0.5 ms, identical init segments and no decoder warnings, for restarts at segments 1, 5 and 9 of 24000/1001 fps, 25 fps, 44.1 kHz audio and a source starting at 7.5 s. It passes on ffmpeg 9.0.2 (this repository's host build) and, in containers, on 6.1.1 (`ubuntu:24.04` with `apt-get install ffmpeg`, what CI installs) and 8.1.3 (`alpine:3.24` with `apk add ffmpeg`, what the Dockerfile ships). Those two ran as `CGO_ENABLED=0 go test -c ./internal/transcode`, mounted into the container, with `-test.run 'TestRestartSplices|TestAudioIndex|TestInterlaced'`. No other versions were run.

Where the product differs from the table above:

- x264 settings are `-preset veryfast -profile:v high -level 51 -crf 23 -maxrate .. -bufsize .. -x264-params bframes=2:b-adapt=0:open-gop=0:scenecut=0 -forced-idr 1`.
- No `-to`: a job runs to the end of the file or until it is stopped. The one effect is at the end of the file, where a run that started later flushes one more padded AAC frame, past the picture. The test leaves the last segment out of its audio comparison for that reason.
- `-hls_flags temp_file`, so a finished segment is the one without the `.tmp` suffix.
- The restart needs the source's start time, frame rate and first frame at the restart point, from two `ffprobe` calls. `-read_intervals` takes an absolute end time, not a duration, when the start lands inside a GOP.

Jellyfin 12.4's own restarts (`compat/captures/transcode-12.4.md`) use the same ingredients: `-ss` before `-i`, `-start_number`, `-copyts`, `noise=drop` on the audio and `movflags=+frag_discont`. They differ in 3 s segments, `+skip_sidx` and no `-output_ts_offset`. We keep the 4 s the spike validated.
