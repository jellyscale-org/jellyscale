# Re-encoding, tone mapping and chapter pictures

Facts about the re-encode path in `internal/transcode` and the chapter code in `internal/library`. See [README.md](README.md) for how to read this.

## What a re-encode makes today

- **H.264, SDR, at most 1080 lines, in software** (x264 `veryfast`, CRF 23, with a ceiling at the device's bitrate). A 4K or HDR film that must be re-encoded comes out 1080p SDR. This is the engine's largest gap against the product ([gaps.md](gaps.md)).
- **The 1080 cap exists because** software x264 at 4K needs more CPU than a worker usually has, and the player stalls. Size is in the spec, so segments of different sizes never mix.
- **The bitrate ceiling is 6 × width × height bits per second**, whatever the device asks: 12.4 Mbps at 1920×1080. It ignores frame rate. Stored, that is at most about 5.7 GB per hour at 1080p 16:9, and 0.45 GB per hour for each megabit of a lower cap.
- **Interlaced video is deinterlaced**, one frame out per frame in.
- **A worker that falls behind makes the player stall.** There is no lighter step to fall back to yet.

## Picture quality of the preset

Measured in `spikes/picture-quality` (one workstation, relative numbers):

- x264 `faster` gains about 2.5 VMAF on average and 3.5 to 5 on the worst frames over `veryfast`, for 1.2 to 1.6 times the CPU and about 45% longer to the first segment. `medium` adds about 0.5 more for 2.2 times the CPU.
- **Two presets cannot share a stream.** Segments of two presets under one init segment failed to decode in Chromium. A preset would have to be part of the spec.

## Tone mapping

An HDR picture that is re-encoded is mapped to SDR, so it doesn't look washed out.

- **Decided from the file alone**, by the transfer function it declares: PQ or HLG. Dolby Vision is mapped by its base layer (8.1 as PQ, 8.4 as HLG); its own dynamic data is ignored.
- **The transfer function is part of the spec**, so a washed-out segment stored earlier is never served for a mapped one.
- **Software filters every supported ffmpeg has:** `zscale` to linear light and to BT.709, and `tonemap=hable:desat=0`, run after scaling, so the work is on at most 1080 lines, and before a subtitle is drawn, so the subtitle is not dimmed. The input is declared BT.2020 limited range, because `zscale` refuses a file that leaves its primaries unsaid.
- **It keeps no state between frames**, so a restart on any worker makes the same frames, with no step in brightness at the join (tested on ffmpeg 6.1.1, 8.1.3 and 9.0.2).
- **`libplacebo` is not used:** it needs a GPU device a plain worker doesn't have.
- **A copy is never tone mapped.**
- **Cost:** 2.7 times a plain re-encode from a 1080p source and 2.0 times from 4K, about 3.2 cores to keep one 4K HDR stream at real time. The output is 11 to 38% bigger than an unmapped one at the same settings.
- **Not observed:** whether a re-encoded Dolby Vision profile 5 film's colours come out right.

## Hardware, measured once

On one cluster's small nodes with Intel integrated graphics (figures for those nodes only):

- A 4-core i5-6500 made the 4K HDR re-encode at 0.81 to 0.87 times real time in software. Its GPU does not decode 10-bit HEVC.
- A newer integrated GPU (UHD 630) decoded, tone mapped and encoded on the GPU at 3.9 to 5.2 times real time for 0.16 to 0.27 CPU-seconds per second, at 0.97 to 1.47 times x264's bytes for equal quality.
- **No GPU tone map matched the software picture.** Brightness on one scene: software 18.7; the GPU's video processor 6.5 and 25.6 by driver; OpenCL 13.4.
- A GPU encoder is a different encoder family, so a stream can't mix it with x264 ([segments.md](segments.md)).

## Chapters

- **Read by the same probe that lists a file's streams**, never a second read. Stored with a marker that tells "never read" from "has none", so a film without chapters is not probed again.
- **Times are the file's own clock and are not shifted** by when its streams start, because a transcode keeps the file's clock too. A film whose streams start 2 s late seeks to the right place either way.
- An untitled chapter is named by its number. MP4 chapters kept in a text track are read as chapters and never listed as a subtitle.

## Chapter pictures

- **Opt-in per place films are kept, and made by a worker job, never by the scan.** A library of 1000 films with 30 chapters each is 30,000 seeks.
- **One job per film does everything:** it makes missing pictures and removes those that are no longer wanted. Its state has no link to the film's own rows, because a scan rewrites those and the job must still know what to delete after a film is gone.
- **Slots of their own (2):** a library's backlog must never make a viewer's subtitle wait.
- **At most 3 attempts per revision of a file**, counted before the store is touched and kept apart from the job row, so a job taken over after a lost lease is held to it too. A job that reaches the cap deletes what it made.
- **Failures are told apart:** a decode error skips that chapter for good; a store that can't be read, or a tool killed by a signal, fails the job without recording a skip.
- **A frame is read at the chapter's start on the file's clock**, scaled to at most 400 pixels wide, and tone mapped for an HDR film with the same filters playback uses.
- **Nothing is served while pictures are being made**, or for a revision of the file that has changed.
- **Cost per film:** one ffmpeg run per chapter (a few ranged reads and one decoded frame) and a picture of 9 to 43 kB. Measured on synthetic sources only.
