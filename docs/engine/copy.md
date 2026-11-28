# Copying the film's own video and audio

Facts about the copy path in `internal/playback` and `internal/transcode`, and the keyframe index in `internal/library`. See [README.md](README.md) for how to read this.

A copy sends the film's own video in segments, without re-encoding, when the device plays the video but not the container or the audio. It costs about 5 to 8% of the CPU of re-encoding the same 1080p film and keeps the picture exactly.

## When a film is copied

All of these must hold:

- The video is H.264, HEVC or AV1, and passes what the device says it plays (profile, level, range, interlacing, size).
- The picture can be named to the player: SDR; or HDR10, HDR10+ or HLG in HEVC or AV1 for a device that shows that range; or Dolby Vision with a base layer the device plays (profiles 7, 8.1, 8.2, 8.4, 10). HEVC must be Main or Main 10; AV1 profile 0 at 8 or 10 bits. Other profiles are re-encoded, because the codec string would need bytes read from an arbitrary stream.
- No bitrate cap is below the file's bitrate. With a cap and a file of unknown bitrate, the film is re-encoded.
- No subtitle is to be drawn into the picture.
- The file has a keyframe index, and no gap between keyframes, or from the last keyframe to the end, is longer than 12 s.

Not copied: an H.264 film in HDR, and Dolby Vision profile 5 (its base layer is neither HDR10 nor HLG). Both are re-encoded.

A copy is never capped: with no cap it carries the file's whole bitrate and size, 4K included. Every value that asks for a copy is checked again on every request against the film, so an edited link gets a refusal, not a broken stream.

## The keyframe index

- **Read once, when a file is scanned, from the file's own index, and stored**, so every replica and worker agrees. For Matroska it is the Cues of the first video track; for MP4, the sync sample table with one edit list.
- **It reads little:** bounded by tests at 2% of a small file from disk and 5% from S3. A Matroska index has one entry per keyframe. An MP4's lives in a table with an entry for every frame, so a large MP4 costs megabytes and grows with length. Listing keyframes by reading packets reads the whole file.
- **Three states:** never read (the next scan reads it); empty, meaning the file has no usable index (Matroska without Cues, fragmented MP4, tables that disagree); or the list. An empty index is not tried again until the file changes.
- **Where the video ends is read from the packets**, not from the container's duration or tags: those count from the file's first stream and can be the audio's end. The read is the last stretch from the last keyframe, at most 12 s of film read up to twice (120 MB at 40 Mbps).
- **An open-GOP film can list entries that are not clean starts.** Tested for HEVC and AV1 in both containers; a bad entry is caught when a job runs (below).
- **Crafted files end quickly:** tables are walked by runs, and refused unless sync samples ascend within the file's sample count.

## How a copy is cut

- **A segment ends at the first keyframe at least 4 s after it began**, or at the last one before 12 s if waiting would run longer. The last ends where the video does. The playlist says each segment's real length.
- **Keyframes every 2 s give 4 s segments.** x264 and x265 default to a keyframe every 250 frames, which is under 12 s at 21 fps and up, so default encodes pass.
- **A long segment delays every start and seek** by its size at the viewer's bandwidth: 12 s at 40 Mbps is 60 MB.
- **ffmpeg's own cuts depend on where a run began**, so the worker writes one fragment per keyframe (`-hls_time 0.001`) and joins fragments into the playlist's segments itself. Fragments are told apart by their times, read from ffmpeg's output and checked with one probe of the first; a segment that doesn't start and end on fragment boundaries fails the job. The joined segment's sequence number is its start time, so a run begun elsewhere writes the same bytes.
- **The run for segment N begins at the keyframe before it**; what comes before the segment is discarded. Matroska stores no decode times, so nothing depends on one.
- **HEVC is always tagged `hvc1`.** ffmpeg's choice between `hvc1` and `hev1` depends on its version and the input, and workers on different versions wrote different init segments.
- **Restarts splice exactly:** the same video bytes, init segment and audio packet times as a run from the start, at segments 1, 4, 8 and the last, for H.264, HEVC and AV1 in MKV and MP4, on ffmpeg 6.1.1, 8.1.3 and 9.0.2.

## When the index turns out wrong

A segment that doesn't start or end on a keyframe of the file means the index is wrong.

- The job fails, the index is emptied (kept aside), and no new play is offered the copy.
- **A viewer already playing goes on.** Segments already stored are the copy's own; the rest are made by an encode of the same codec (x264, x265 or SVT-AV1) cut on the same layout. Each such segment leads with its own init segment, which players take as a new init. The fallback is logged and counted once per stream.
- **An AV1 copy goes on as AV1.** A browser's player keeps the buffer typed AV1 and refuses an H.264 segment ([../players.md](../players.md)). SVT-AV1 in ffmpeg 6.1 ignores forced keyframes, so each segment is an ffmpeg of its own.
- **An HDR copy's fallback keeps its range:** 10 bits, with the copy's own colour tags, not tone mapped. Lost on the way: lines above 1080, HDR10+ and Dolby Vision dynamic metadata, and on ffmpeg 6.1 the HDR10 mastering display and light level.
- **Only that error empties an index.** A store that fails mid-read is the job's failure, even when the output it cut short looks like a wrong index. An input cut short keeps the index.

## Copied audio

Audio is copied, beside a copied or a re-encoded picture, when:

- the device takes its codec in fMP4 (of AAC, MP3, AC3, E-AC3, Opus, FLAC) and its channels;
- the player can be told its codec (AAC LC and HE-AAC, MP3, AC-3, E-AC-3, Opus, FLAC; AAC Main is encoded);
- it takes no more than a quarter of the bitrate cap, so the picture is not starved. A track of unknown bitrate counts as 6 Mbps.

Otherwise it is encoded to stereo AAC at 192 kbps.

- **A restart splices the same packets.** From MP4 the audio in each segment is byte-identical. From Matroska the packets are identical and their times can move by under 1 ms, because Matroska stores whole milliseconds. Accepted.
- **A copy of both streams opens the film once.** A copy whose audio is re-encoded opens it twice, so it reads twice the film's bytes over a whole run.
- **Cost:** no CPU for audio. A 640 kbps AC3 track in place of 192 kbps AAC adds about 0.45 Mbps per stream and 0.2 GB per stored hour; FLAC up to ten times that.
- **Not measured:** a device that lists a codec its hardware stutters on at a high bitrate.

## What a copy costs

- **ffmpeg reads a copy at 5 times the film's speed**, so it doesn't read, write and upload the whole film as fast as the store allows. [Read-ahead](segments.md#read-ahead) bounds how far it gets.
- **Storage and network are the file's own bitrate:** a 40 Mbps film takes 18 GB per stored hour, against at most 5.7 GB for a 1080p re-encode. The ceilings of a re-encode do not apply.
- **A whole watch of a 2 hour film at 40 Mbps** with re-encoded audio reads 72 GB from the source and stores 36 GB (computed, not measured). From object storage that is 72 GB of egress.
- **A seek waits for the whole segment it lands in.** Resuming a 1080p film two thirds in took 2.6 s from the click to a moving picture, on local disk and on S3.
- **Each request for a copied stream reads the keyframe list** from Postgres: about 14 KB for a 2 hour film with a keyframe every 4 s. The request rate has not been measured.
