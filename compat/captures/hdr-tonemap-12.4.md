# What Jellyfin 12.4 does with HDR films that it re-encodes

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (jellyfin-ffmpeg), one library of three 20 s fixtures (640x360, 24 fps, keyframe every 72 frames, AAC) in `hdr-tonemap-12.4.json` (same directory), names as there. Ids and tokens are placeholders. Made by ffmpeg: an HEVC Main 10 file tagged HDR10 (BT.2020, PQ, mastering display and MaxCLL set), an HEVC Main 10 file tagged HLG, and a 10-bit H.264 file (High 10) tagged HDR10. The PlaybackInfo body is the one jellyfin-web sends, with the Chrome DeviceProfile of `transcode-12.4.json` and Auto bitrate (`MaxStreamingBitrate: 1440987881`).

## What Jellyfin does

| File | Reported | `TranscodeReasons` | Video |
|---|---|---|---|
| HEVC Main 10, HDR10 | `VideoRangeType: HDR10`, not direct play | `VideoCodecNotSupported` | re-encoded, tone mapped |
| HEVC Main 10, HLG | `VideoRangeType: HLG`, not direct play | `VideoCodecNotSupported` | re-encoded, tone mapped |
| H.264 High 10, HDR10 | `VideoRangeType: HDR10`, not direct play | `VideoRangeTypeNotSupported` | re-encoded, tone mapped (not copied) |

- Chrome's H.264 CodecProfile has `VideoRangeType EqualsAny SDR`, so 10-bit PQ H.264 fails it. Its HEVC CodecProfile allows `SDR|HDR10|HDR10Plus|HLG`, but Chrome lists no HEVC to play or to transcode to, so the reason for those is the codec.
- **The tone mapping is always done when the video is re-encoded**, for PQ and HLG alike, and an HDR source is not copied to Chrome. The master playlist of the re-encode says `VIDEO-RANGE=SDR` and `CODECS="avc1.640033,..."` (the 10-bit H.264 file: `avc1.42401E`). The ffmpeg command (`ffmpegCommandLines` in the JSON; the earlier `hevc-av1-12.4.json` has the same one for HEVC HDR10) is `-vf "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc,scale=...,tonemapx=tonemap=bt2390:desat=0:peak=100:t=bt709:m=bt709:p=bt709:format=yuv420p"` with `color_trc=arib-std-b67` for HLG. The scale comes first and the mapping works on the output-sized picture, as it does here. `tonemapx` is jellyfin-ffmpeg's own filter, not in the ffmpeg of any distribution.
- Jellyfin names the transfer function in the stream (`ColorTransfer`: `smpte2084`, `arib-std-b67`) and `ColorSpace` and `ColorPrimaries` (`bt2020nc`, `bt2020`).

## What jellyscale does, and where it differs

`Decide` gives the same reasons (`TestDecideMatchesJellyfinForHDR`), never copies these films, and the master playlist says `VIDEO-RANGE=SDR`. Deliberate differences:

1. **The filter chain is `zscale` and `tonemap=hable`, not `tonemapx` with `bt2390`.** `tonemapx` exists only in jellyfin-ffmpeg. `zscale` and `tonemap` are in the ffmpeg of Ubuntu 24.04 (6.1.1), Alpine 3.24 (8.1.3) and 9.0.2; `bt2390` is not among `tonemap`'s algorithms in any of them (none, linear, gamma, clip, reinhard, hable, mobius), so `hable` is the choice. `libplacebo` is in those builds too but needs a Vulkan device, which a worker without a GPU does not have.
2. **Tone mapping follows the declared transfer function** (PQ or HLG), not the range label. Dolby Vision profile 5, which has no HDR10 base, is not mapped (Jellyfin has its own Dolby Vision handling in `tonemapx` that jellyscale does not).
3. **The input is declared BT.2020 limited range**, which zscale needs when a file leaves its primaries unsaid.
4. **The output is tagged BT.709** (`-colorspace`, `-color_primaries`, `-color_trc`, `-color_range tv`). Jellyfin's command sets none; its filter marks the frames and x264 writes them.
5. **An H.264 HDR file is not copied** even to a client whose profile has no range limit: the master says SDR, so a copy would be shown washed out. Jellyfin copies HDR with `VIDEO-RANGE=PQ` and an SDR variant beside it for clients that say they play HDR (`hevc-av1-12.4.md`); that is not built.
