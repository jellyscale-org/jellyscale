# What Jellyfin 12.4 does with plain HDR films it has to transcode only for the audio

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (server 12.4.0, jellyfin-ffmpeg 8.1.3), one library of three synthetic films (HEVC Main 10, 640x360, 24 fps, 20 s, MKV, AC3), data in `hdr-copy-12.4.json` (same directory): HDR10 (PQ), HLG, and HDR10+ (the SEI in every picture). Made with `test/fixtures/dovi` and remuxed with ffmpeg. The PlaybackInfo body is the one jellyfin-web sends (`IsPlayback`, `AudioStreamIndex "1"`, `SubtitleStreamIndex "-1"`, `MaxStreamingBitrate 1440987881`, `MediaSourceId`) under the five profiles of `dolby-vision-12.4.json` and the `hevc-client` profile of `hevc-av1-12.4.json`. The master playlist was fetched from the `TranscodingUrl`.

Without `IsPlayback` and `MediaSourceId` in the body Jellyfin direct played the AC3 file under the profiles that list no AC3, which the captures before this one never noticed; the body here is the one that gets the audio reason.

## What Jellyfin does

| Profile | HDR10 / HLG / HDR10+ film | `TranscodeReasons` | Master playlist variants |
|---|---|---|---|
| `sdr` | all three | `AudioCodecNotSupported,VideoRangeTypeNotSupported` | one: `VIDEO-RANGE=SDR`, `avc1.640033,mp4a.40.2` (re-encoded, tone mapped) |
| `hdr10only` (lists HDR10, HDR10Plus) | HDR10, HDR10+ | `AudioCodecNotSupported` | two: the copy, then the SDR H.264 re-encode |
| `hdr10only` | HLG | `AudioCodecNotSupported,VideoRangeTypeNotSupported` | one: SDR re-encode |
| `hdr10`, `dv`, `dvfull`, `hevc-client` (list HLG too) | all three | `AudioCodecNotSupported` | two: the copy, then the SDR re-encode |

The copy's variant: `VIDEO-RANGE=PQ` (HDR10, HDR10+) or `HLG`, `CODECS="hvc1.2.4.L63.B0,mp4a.40.2"`, and for the HDR10+ film `SUPPLEMENTAL-CODECS="hvc1.2.4.L63.B0/cdm4"`. The second variant is `VIDEO-RANGE=SDR`, `avc1.640033,mp4a.40.2`.

So: a profile that lists the range gets the file's own video with only the audio re-encoded; one that does not list it gets the tone-mapped H.264 only, and the reason carries `VideoRangeTypeNotSupported`.

One thing seen and not explained: with 6 channel TrueHD in MKV (the case the task names) every profile, `hevc-client` included, got the SDR re-encode alone, with `AudioCodecNotSupported` as the only reason; AC3 gave the copy. jellyscale deliberately differs: it copies the video whatever the audio is (JS-025).

## Where jellyscale differs

The second (SDR) variant is not required of the copy's master playlist, as for Dolby Vision (`dolby-vision-12.4.md`). Main is written `hvc1.1.6` where Jellyfin writes `hvc1.1.4`; Main 10 is the same.
