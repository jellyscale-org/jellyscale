# What Jellyfin 12.4 does with HEVC and AV1 files

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (jellyfin-ffmpeg), one library of eight 20 s fixtures (640x360, 24 fps, keyframe every 72 frames) in `hevc-av1-12.4.json` (same directory), names as there. Ids, tokens and session ids are placeholders in the JSON.

Fixtures: HEVC Main 8 bit (MKV with AC3, MP4 with AAC), HEVC Main 10 SDR (MKV), HEVC Main 10 HDR10 (MKV); AV1 Main 8 bit (MKV with AC3, MP4 with AAC), AV1 10 bit SDR (MKV), AV1 10 bit HDR10 (MKV); and an H.264 MKV with AC3 as the control. Made by ffmpeg with libx265 and libsvtav1.

The PlaybackInfo body is the one jellyfin-web sends (`IsPlayback`, `AutoOpenLiveStream`, `AudioStreamIndex: "1"`, `SubtitleStreamIndex: "-1"`, `MaxStreamingBitrate: 1440987881`), because Jellyfin does not check the audio of a file when the body names no `AudioStreamIndex`: with a body that omits it, the AV1 and HEVC MKV files with AC3 reported `SupportsDirectPlay: true` under every profile, H.264 with AC3 too. jellyfin-web always names one.

## Profiles

- `chrome`: jellyfin-web 12.4 in Chrome 154, from `transcode-12.4.json`. Lists AV1 (and no HEVC) in the mp4, mkv and webm direct-play profiles and in the fMP4 HLS transcoding profile (`av1,h264,vp9`); HEVC has a CodecProfile (main, level 120) but no profile to play it in.
- `hevc-client`: `chrome` plus `hevc` in the mp4 and mkv direct-play profiles and the fMP4 HLS transcoding profile, with the HEVC CodecProfile widened to `main|main 10` and level 183. What a client with a hardware HEVC decoder sends, a profile constructed here from Chrome's: no browser that sends one was available to capture.
- `hevc-direct-only`: `hevc-client` with the HLS transcoding profile listing `h264` only.

## Decisions

| File | chrome | hevc-client | hevc-direct-only |
|---|---|---|---|
| HEVC Main, MP4, AAC | transcode, `VideoCodecNotSupported` | direct play | direct play |
| HEVC Main, MKV, AC3 | transcode, `VideoCodecNotSupported,AudioCodecNotSupported` | transcode, `AudioCodecNotSupported`, **video copied** | transcode, `VideoCodecNotSupported,AudioCodecNotSupported`, re-encoded |
| HEVC Main 10 SDR, MKV, AC3 | as HEVC Main | as HEVC Main, copied | re-encoded |
| HEVC Main 10 HDR10, MKV, AC3 | as HEVC Main | transcode, `AudioCodecNotSupported`; master has two variants, the HDR copy (`VIDEO-RANGE=PQ`, `hvc1.2.4.L63.B0`) and an SDR H.264 variant (`AllowVideoStreamCopy=false`, tone mapped with `tonemapx`) | re-encoded |
| AV1 Main 8 bit, MP4, AAC | direct play | direct play | direct play |
| AV1 Main 8 bit, MKV, AC3 | transcode, `AudioCodecNotSupported`, **video copied** | same | `VideoCodecNotSupported,AudioCodecNotSupported`, re-encoded |
| AV1 10 bit SDR, MKV, AC3 | as AV1 8 bit, copied | same | re-encoded |
| AV1 10 bit HDR10, MKV, AC3 | transcode, `AudioCodecNotSupported`; HDR copy (`PQ`, `av01.0.01M.10`) plus an SDR H.264 variant | same | re-encoded |
| H.264, MKV, AC3 (control) | transcode, `AudioCodecNotSupported`, copied | same | same |

So: `VideoCodecNotSupported` is reported when the direct-play profile lacks the codec **or** the client's HLS transcoding profile lacks it for a stream that needs a transcode anyway (the `hevc-direct-only` column: HEVC and AV1 are in the direct-play profile there, and Jellyfin still says `VideoCodecNotSupported` because it can't copy). jellyscale reports the same (`DeviceProfile.hlsMP4`, `TestDecideMatchesJellyfinForHEVCAndAV1`).

## What the copy looks like

ffmpeg for the copies (`FFmpeg.DirectStream-*` logs, command lines in the JSON): `-codec:v:0 copy -tag:v:0 hvc1 -bsf:v hevc_mp4toannexb` for HEVC and `-codec:v:0 copy` for AV1, the audio to `libfdk_aac`, `-f hls -hls_segment_type fmp4`, `-hls_time 6`. Jellyfin pins `hvc1` as jellyscale does (DOMAINS 48).

Master playlists of the copies: `CODECS="hvc1.1.4.L63.B0,mp4a.40.2"` (Main), `hvc1.2.4.L63.B0` (Main 10), `av01.0.01M.08`, `av01.0.01M.10`, with `VIDEO-RANGE=SDR` for SDR films. The level is ffprobe's number. jellyscale writes `hvc1.1.6.L63.B0` for Main (the compatibility flags of a Main stream written by x265 and by ffmpeg's muxer; both decode the same) and the same strings otherwise.

The HDR films: Jellyfin offers the copy with `VIDEO-RANGE=PQ` and a second, tone-mapped H.264 variant. **jellyscale does not copy HDR**: its master playlist has one variant labelled SDR, so a copy would be washed out, and tone mapping is not built. HDR films behave as they did before HEVC and AV1 were added: direct play if the profile allows it, else the H.264 re-encode. This is the one place the decision differs from Jellyfin's.

## Chromium

Run by `test/e2e/transcode.mjs` against jellyscale (Chrome stable on the fixture host, a headless run with `--disable-gpu`, as CI):

- AV1 decodes in software (dav1d): `MediaSource.isTypeSupported('video/mp4; codecs="av01.0.08M.08"')` is true, jellyfin-web's profile lists `av1`, and an AV1 MP4 with AAC direct plays (picture decoded, seeks), an AV1 film with AC3 plays as a copy through HLS (fMP4 with the `av01` sample entry), from POSIX and S3, in MKV and MP4.
- HEVC: the run printed `Chrome says: {"hevc":false,"av1":true}` for `MediaSource.isTypeSupported` on `hvc1.1.6.L120.B0` and `av01.0.08M.08` (google-chrome-stable, headless, `--disable-gpu`, on a machine that has a GPU). The HEVC MP4 with AAC was re-encoded (`VideoCodec=h264`, `TranscodeReasons=VideoCodecNotSupported`) and the HEVC MKV with AC3 as well (`VideoCodecNotSupported,AudioCodecNotSupported`), and both played. Chrome plays HEVC only through a platform hardware decoder, so on CI jellyfin-web's profile has no `hevc` and the film is re-encoded to H.264 with `VideoCodecNotSupported` (and `AudioCodecNotSupported` for AC3). **HEVC playback in a browser is not exercised on CI**: the HEVC direct-play and copy paths are tested in `playback_test.go` (`TestDecideMatchesJellyfinForHEVCAndAV1`, `TestCopyOfHEVCAndAV1`) and in an integration PlaybackInfo test (`TestPlaybackInfoOffersACopyOfHEVCAndAV1`) with the `hevc-client` profile above, and in the splice tests that run the copy on real HEVC. A developer machine with a hardware decoder runs the browser side with `E2E_HEVC=1`.
