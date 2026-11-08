# What Jellyfin 12.4 puts in a session's TranscodingInfo

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0) in Docker on a Docker-assigned port, over copies of the synthetic e2e films (`test/fixtures/e2e.sh`). The requests are what jellyfin-web sends: `POST /Items/{id}/PlaybackInfo` with the Chrome DeviceProfile of `transcode-12.4.json`, then (for a transcode) `master.m3u8`, `main.m3u8`, the init segment and segment 0, then `POST /Sessions/Playing` and `/Sessions/Playing/Progress` with the `PlayMethod` the client chose, then `GET /Sessions?deviceId=..`. Ids, tokens and play session ids are removed from `transcoding-info-12.4.json` (same directory); scenario names below are its keys.

| Scenario | Film | `PlayState.PlayMethod` | `TranscodingInfo` |
|---|---|---|---|
| `reencode` | HEVC + AC3, 640x360 | Transcode | `VideoCodec h264`, `AudioCodec aac`, `Container mp4`, `IsVideoDirect false`, `IsAudioDirect false`, `Bitrate 3371345`, `Width 640`, `Height 360`, `AudioChannels 1`, `HardwareAccelerationType none`, `TranscodeReasons [VideoCodecNotSupported, AudioCodecNotSupported]` |
| `copy` | H.264 + AC3 (and AAC), 640x360 | Transcode | the same keys, `VideoCodec h264`, `IsVideoDirect true`, `IsAudioDirect false`, `Bitrate 3609330`, `TranscodeReasons [AudioCodecNotSupported]` |
| `directplay` | H.264 + AAC MP4 | DirectPlay | none: the key is absent |
| `slow` | 1080p H.264 + AC3, one CPU, `MaxStreamingBitrate 1500000` | Transcode | as `reencode`, and while ffmpeg runs also `Framerate 55` and `CompletionPercentage 42.77`; `Bitrate 1500000` (the cap); `TranscodeReasons [AudioCodecNotSupported, ContainerBitrateExceedsLimit]` |

What the capture shows:

- **Reasons are the PlaybackInfo's.** `TranscodeReasons` is the list in the `TranscodeReasons=` parameter of the `TranscodingUrl` the same request was given, in the same order (`TranscodeReasonsInUrl` in the JSON).
- **`Framerate` and `CompletionPercentage` exist only while ffmpeg runs.** The three runs that finished within a second or two never had them, and `slow` lost them when its encode ended (`sessionsAfterTheEncodeEnded`). Jellyfin's `Framerate` is the encode's frames per second, 55 for a 24 fps film; jellyfin-web divides it by the source's frame rate and shows "55 fps (2.29x)", so it is also the encode speed.
- **It stays while the session does, and goes with it.** `TranscodingInfo` is still there after the encode ended, and gone (`sessionsAfterStop`) after `Sessions/Playing/Stopped` and `DELETE /Videos/ActiveEncodings`.
- **Jellyfin does not say how fast the encode runs against real time, or on which machine.** That is what jellyscale adds (`EncodeSpeed`, `Worker`, `IsBelowRealTime`; `compat/differences.yaml`).

## What jellyfin-web reads

From the 12.4.0 bundle (`web-dist`), not from a run:

- The **Playback info panel** (player menu, "Playback Info") reads the session (`GET /Sessions`) and shows, under "Transcoding Info" ("Remuxing Info" when `IsVideoDirect` and `IsAudioDirect` are both true or the video is absent, "Direct Streaming Info" when only `IsVideoDirect` is): "Target codecs" from `VideoCodec` and `AudioCodec`, each followed by " (direct)" when its `Is...Direct` is true, then `AudioChannels` and `Bitrate`; "Progress / speed" from `CompletionPercentage` and `Framerate` ("N fps (Mx)", M being `Framerate` over the source's frame rate); "Reasons" from `TranscodeReasons`.
- The **dashboard's session card** draws `CompletionPercentage` as the buffer of its progress bar, and its info button opens a dialog with the play method (from `IsVideoDirect`, `IsAudioDirect`, `VideoCodec` and `PlayState.PlayMethod`), `Framerate`, `Bitrate`, `Container`, `VideoCodec`, `AudioCodec` and the `TranscodeReasons`.
- Neither reads `Width`, `Height` or `HardwareAccelerationType`, and neither knows `EncodeSpeed`, `Worker` or `IsBelowRealTime`.
