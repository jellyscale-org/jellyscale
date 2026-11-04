# What Jellyfin 12.4 does with the audio of a re-encoded film

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (jellyfin-ffmpeg), one library of nine 20 s fixtures in `audio-copy-12.4.json` (same directory), names as there. Each is 640x360, 24 fps, **interlaced H.264** (tff, keyframe every 72 frames), so Jellyfin has to re-encode the picture whatever the audio is: AAC stereo, AAC 5.1 (MKV and MP4), AC3 stereo and 5.1, E-AC3 5.1, Opus, FLAC and MP3 stereo. Two more films, progressive H.264 with AAC 5.1 in MKV and MP4, were asked for their decision only. Item, token and session ids are placeholders in the JSON.

Profiles (in the JSON as `deviceProfiles`):

- `chrome`: jellyfin-web 12.4 in Chrome 154, from `transcode-12.4.json`. Its fMP4 HLS transcoding profile lists the audio `aac,mp2,opus,flac` with `MaxAudioChannels: 2`; its MPEG-TS one `aac,mp3,mp2`.
- `surround`: `chrome` with the fMP4 HLS profile listing `aac,mp3,ac3,eac3,flac,opus` and `MaxAudioChannels: 6`. A profile constructed here, of the shape Safari and Android clients send; no browser that sends one was available to capture.
- `ac3-max2`: `surround` with `MaxAudioChannels: 2`.

Each film was asked at three caps for `chrome` (Auto as jellyfin-web measures it, 1440987881; 1.5 Mbps; 720 kbps), two for `surround` and one for `ac3-max2`; the master, the main playlist and segment 0 were fetched, and the ffmpeg command each run started is from Jellyfin's `FFmpeg.Transcode-*.log`, in `ffmpegAudio`.

## What Jellyfin does

- **The audio is copied when the codec is in the fMP4 profile's list and the channels fit its `MaxAudioChannels`**, whatever the DeviceProfile's direct-play list says. ffmpeg then has `-codec:a:0 copy` (`-strict -2` for Opus) and no `-ac`, `-ab` or `-ar`. AAC stereo, Opus and FLAC copy under `chrome`; AC3, E-AC3 and 5.1 AAC copy under `surround`; AC3 stereo copies under `ac3-max2` and AC3 5.1 does not (`AudioChannelsNotSupported`).
- Otherwise it is encoded: `-codec:a:0 libfdk_aac -ac 2 -ab 256000`, with `-af volume=2` after a 5.1 downmix. Jellyfin keeps 5.1 when the profile allows six channels and the cap leaves no room for a copy (`-ac 6 -ab 384000`); jellyscale always writes stereo.
- **The URL.** `AudioCodec` in the master URL is the profile's list (`aac,opus,flac`) when the audio is encoded, and the *source's* codec when it is copied (`AudioCodec=flac`, `ac3`). In `main.m3u8` and every segment URL it is rewritten to `copy` or `aac`. `TranscodingMaxAudioChannels` is the profile's `MaxAudioChannels` (2 or 6). `AudioBitrate` is the copied track's bitrate when copying (192000, 448000, 960000) and 384000 or the track's when encoding. `VideoBitrate` is the cap minus `AudioBitrate` (`1500000 - 192000 = 1308000` for AAC; `1500000 - 128000 = 1372000` for Opus). `AudioSampleRate=48000` is present for a copy.
- **`CODECS` in the master** names the copied codec: `mp4a.40.2` (AAC LC), `ac-3`, `ec-3`, `Opus`, `fLaC`, `mp4a.40.34` (MP3). A FLAC film that Jellyfin re-encoded under a 1.5 Mbps cap has `CODECS="avc1.4D4016"` with **no audio codec at all** (the master was written for a copy that the main playlist then did not make).
- **`TranscodeReasons`** are those of the direct play check, so a copy is not "explained": interlaced AAC stereo has `InterlacedVideoNotSupported` alone, with no audio reason. AC3 under `surround` has `AudioCodecNotSupported,InterlacedVideoNotSupported` and copies, because the direct-play profile does not list AC3 though the HLS one does. `AudioChannelsNotSupported` is added when the HLS profile lists the codec and its channel limit is below the track's (5.1 AAC under `chrome`; AC3 5.1 under `ac3-max2`), and not when the codec is not listed (AC3 5.1 under `chrome`: `AudioCodecNotSupported` only). It comes before `ContainerBitrateExceedsLimit`. Progressive 5.1 AAC direct plays under `chrome`: the transcoding profile's channel limit does not apply to direct play.
- **The cap.** The copied track's bitrate is taken out of the cap (see `VideoBitrate` above), but Jellyfin does not refuse a copy that doesn't fit: AAC stereo (192 kbps reported) copies under a 720 kbps cap, leaving 528 kbps of picture. It re-encoded FLAC (960 kbps) under 1.5 Mbps and AC3 5.1 (448 kbps) and 5.1 AAC from the MP4 (384 kbps) under 1.5 Mbps, but copied 5.1 AAC from the MKV (reported 320 kbps) under the same cap, so its threshold is neither a fixed share nor stated; it is somewhere between a fifth and a fourth.
- **MP3 in Chrome** is delivered in MPEG-TS segments (`SegmentContainer=ts`, `-hls_segment_type mpegts`), because only the TS profile lists MP3; under `surround`, which lists it for fMP4, it is copied into fMP4.
- A Matroska track has no bitrate field of its own and Jellyfin fills one in (128 kbps AAC reported as 192000; FLAC as 960000), so the figures from the MKV fixtures are Jellyfin's guesses. The MP4's are the file's.
- Restart (a seek): `-bsf:a noise=drop='lt(pts*tb\,75.000)'` on copied audio (`transcode-12.4.md`), with the seek time as the threshold; jellyscale uses the same filter with the cut computed from the video's keyframe.

## What jellyscale does, and where it differs

jellyscale reads the same inputs, from the URL and the source alone (DOMAINS 34): `AudioCodec` is the profile's fMP4 list (restricted to AAC, MP3, AC3, E-AC3, Opus and FLAC), `TranscodingMaxAudioChannels` its limit, `AudioBitrate` and `VideoBitrate` the split. The decision is `Source.copyAudio`; `Decide` and `NewSpec` both call it. Deliberate differences, each in `TestAudioCopyMatchesJellyfin`:

1. **The URL's `AudioCodec` is always the list.** Jellyfin names the source's codec in the master and `copy` in the playlists. A spec built from each request has to reach the same answer, so the list stays and the answer comes from the source.
2. **MP3 under `chrome` is encoded.** jellyscale makes fMP4 only (DOMAINS 21); the fMP4 profile doesn't list MP3.
3. **A copy must fit in a quarter of the cap**, and a track of unknown bitrate counts as 6 Mbps (so copies at 24 Mbps and up, Auto included). AAC stereo at 192 kbps under 720 kbps is encoded here, copied by Jellyfin. The rule is stated, tested and the same for every codec.
4. **CODECS is always complete**, including after a re-encode under a cap.
5. **5.1 is downmixed to stereo** when it is encoded; Jellyfin keeps six channels if the profile allows them.
6. **The profile's CodecProfiles apply to the track** (`VideoAudio` limits on `AudioChannels`): a codec whose limit the track fails is taken out of the list in the URL.
7. **The video copy path re-encodes its audio** as before; a copy of both is a later slice.
