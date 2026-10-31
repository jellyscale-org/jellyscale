# What Jellyfin 12.4 does for jellyfin-web when it transcodes

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0, jellyfin-ffmpeg 8.1.3), 3 CPUs. Client: jellyfin-web 12.4.0 in headless Chrome 154. Exchanges are in `transcode-12.4.json` (same directory); scenario names below match its keys. Item, user, token and device ids are placeholders there.

Caveats up front:

- **The DeviceProfile is Chrome's.** Headless Chrome 154 reports no HEVC and no AC3 (`MediaSource.isTypeSupported` false for both), so those two force transcodes. Safari, Android and native clients send different profiles. The one profile is in the JSON under `deviceProfile`; jellyfin-web sent it byte-identical in every PlaybackInfo call.
- Fixtures are 90 s, 24 fps, keyframes at 0,3,7,13,18,26,31,39,44,52,57,65,70,78,83 s (irregular on purpose). The seek and stop runs use a noisy 1080p HEVC file with the container limited to the CPU it needs, so the encode runs at about 1x.
- Default server settings were not changed. `GET /System/Configuration/encoding` (fetched by jellyfin-web when it renders ASS) shows `EnableThrottling:false`, `EnableSegmentDeletion:false`, `HardwareAccelerationType:none`, `H264Crf:23`, `EncoderPreset:auto`, `HlsAudioSeekStrategy:TrimCopiedAudio`.

## Scenarios

1. **Direct play (control, H.264+AAC MP4).** `POST /Items/{id}/PlaybackInfo`, then `GET /Videos/{id}/stream.mp4?Static=true&mediaSourceId=..&deviceId=..&ApiKey=..&Tag=..` with `Range: bytes=0-` -> 206. A seek is a new ranged GET (`bytes=46530560-`); no new PlaybackInfo. `POST /Sessions/Playing` has `PlayMethod:"DirectPlay"`. No `/videos/` HLS routes.
2. **HEVC in MKV.** Chrome cannot play it. PlaybackInfo returns `SupportsDirectPlay:false`, `SupportsDirectStream:false`, a `TranscodingUrl`, `TranscodeReasons=VideoCodecNotSupported`. ffmpeg: `libx264 -preset veryfast -crf 23 -maxrate .. -profile:v high -level 51`, scaled to fit the max resolution, audio `copy` (the source was AAC). Playlist is VOD, 3 s segments, 31 entries for 90.021 s.
3. **AC3 audio in MKV (H.264 video).** Video is copied (ffmpeg log is `FFmpeg.DirectStream-*`: `-codec:v:0 copy -bsf:v h264_mp4toannexb`), audio is `libfdk_aac -ac 1 -ab 128000`. `TranscodeReasons=AudioCodecNotSupported`, `AudioCodec=aac,opus,flac` in the master URL and `AudioCodec=aac` in main. The playlist segment lengths follow source keyframes (below). `PlayMethod` in `/Sessions/Playing` is still `Transcode`. Also run with an MP4 container (`3b`): see the surprises.
4. **Low Maximum Streaming Bitrate.** Quality menu offers Auto, 8, 6, 4, 3, 1.5 Mbps, 720 kbps, 420 kbps (the item ids in the menu are the bitrates in bps, `Auto` is 0). Choosing 720 kbps: new `POST PlaybackInfo` with `MaxStreamingBitrate:720000`, `StartTimeTicks` = current position (44550970 = 4.455 s), `AudioStreamIndex` and `SubtitleStreamIndex` as numbers. Reply: transcode with `TranscodeReasons=ContainerBitrateExceedsLimit`, `VideoBitrate=650544&AudioBitrate=69456` (they sum to the limit), master `BANDWIDTH=720000`, `RESOLUTION=960x540`, ffmpeg `-maxrate 650544 -profile:v baseline -level 31`, width capped to 960, audio copied. No `DELETE ActiveEncodings` here because the previous stream was direct play.
5. **Seek during a transcode** (details below).
6. **Switch audio during a transcode** (AC3 file, English -> Spanish): `POST PlaybackInfo` with `StartTimeTicks:41406190` and `AudioStreamIndex:2`, then `DELETE /Videos/ActiveEncodings?deviceId=..&PlaySessionId=<old>` (204), then a full new `master.m3u8` -> `main.m3u8` -> `hls1/main/-1.mp4` -> `0.mp4` with the new `PlaySessionId` and `AudioStreamIndex=2`. A second identical `DELETE` (same old PlaySessionId) is sent about 300 ms later, after the new segments start. No new `/Sessions/Playing`; `/Sessions/Playing/Progress` just continues with the new `PlaySessionId`. `StartTimeTicks` is only in the POST body, never in a URL: the new playlist starts at segment 0 and the player picks the segment for its position (segment 0 was requested again at 4.1 s).
7. **Subtitles.** See the dedicated section.
8. **Stop.** See the dedicated section.

## Seek (scenario 5)

On the slowed server (1080p, ffmpeg at about 1x), at about 1.5 s into playback the page set `video.currentTime = 75`.

- The client sends **no PlaybackInfo, no StartTimeTicks, no DELETE**. It requests `hls1/main/25.mp4?...&runtimeTicks=750000000&actualSegmentLengthTicks=30000000`, then 26, 27 as each arrives. 25 = 75 s / 3 s. The URL carries the original `PlaySessionId`.
- The first request for segment 25 got no response; the client's HLS player (media source, `blob:` video src) abandoned it after about 10 s and asked again, which was answered in 2 s. The server held the request open while ffmpeg started. This was not retried more than once.
- On the server, at the moment of that first request a new ffmpeg started and the old one logged `[q] command received. Exiting.` in the same second. The new command line is the original plus `-ss 00:01:15.000` before `-i`, `-start_number 25`, and for copied audio `-bsf:a noise=drop='lt(pts*tb\,75.000)'`. `-copyts` is kept; output names stay `{hash}{n}.mp4`. Seeking back to 40 s repeated this with `-ss 00:00:39.000 -start_number 13` (segment 13 starts at 39 s, so the seek target 40 s was rounded down to the segment start).
- Seeking while the encode has already finished (the fast HEVC fixture) starts nothing: segments 25-28, then 6-9 were just served from disk. Only one ffmpeg ran.

## TranscodingUrl and its parameters

`MediaSource.TranscodingUrl` is relative, lower-case `/videos/`, dashed item id, and is exactly the URL jellyfin-web requests for `master.m3u8`. In the JSON it is `&`-escaped; this is the unescaped HEVC one:

```
/videos/{dashed-id}/master.m3u8?DeviceId=..&MediaSourceId={hex-id}&VideoCodec=av1,h264,vp9&AudioCodec=aac&AudioStreamIndex=1&VideoBitrate=2147291647&AudioBitrate=192000&AudioSampleRate=48000&MaxFramerate=24&SegmentContainer=mp4&MinSegments=1&PlaySessionId=..&ApiKey=<access token>&TranscodingMaxAudioChannels=2&RequireAvc=false&EnableAudioVbrEncoding=true&Tag={ETag}&SubtitleMethod=Encode&hevc-level=93&hevc-videobitdepth=8&hevc-profile=main&hevc-audiochannels=1&aac-profile=lc&av1-profile=main&av1-rangetype=SDR,HDR10,HDR10Plus,HLG&av1-level=19&vp9-rangetype=SDR,HDR10,HDR10Plus,HLG&h264-profile=high,main,baseline,constrainedbaseline,high10&h264-rangetype=SDR&h264-level=52&h264-deinterlace=true&TranscodeReasons=VideoCodecNotSupported
```

| Param | What it seemed to control |
|---|---|
| `DeviceId` | Client device id; used for `DELETE ActiveEncodings`. Base64 of the user agent plus a timestamp in jellyfin-web. |
| `MediaSourceId` | Source within the item (equals the item id here). |
| `VideoCodec` | Allowed output video codecs from the profile (`av1,h264,vp9`). ffmpeg still used libx264. |
| `AudioCodec` | In master/TranscodingUrl: allowed list from the profile (`aac`, or `aac,opus,flac` when the source audio is unsupported). In main.m3u8 and every segment URL it is rewritten to the one decision: `copy` or `aac`. |
| `AudioStreamIndex`, `SubtitleStreamIndex` | Stream indexes chosen. `SubtitleStreamIndex` appears only when a subtitle is selected; absent for off. |
| `VideoBitrate`, `AudioBitrate` | Targets. With Auto, `VideoBitrate` = 2147483647 minus `AudioBitrate` (2147291647 = 2147483647 - 192000; 2147355647 with 128000 audio); with a limit, the limit minus audio (650544 + 69456 = 720000). ffmpeg `-maxrate` was 6044692 for the Auto 720p case and 650544 when limited, so the engine clamps further to something derived from the source. |
| `AudioSampleRate` | Present only when the audio stream's rate is reported and copied. |
| `MaxFramerate` | Source frame rate. |
| `SegmentContainer=mp4`, `MinSegments=1` | From the transcoding profile; mp4 means fMP4 HLS (profile also offers `ts`, never used). |
| `PlaySessionId` | Same id as the PlaybackInfo reply. Changes on every PlaybackInfo (audio switch, quality change, subtitle change). Not used for authentication. |
| `ApiKey` | The access token. Segment requests carry no Authorization header; this query parameter authenticates them. |
| `TranscodingMaxAudioChannels` | Profile limit (2). ffmpeg got `-ac 1` for a mono source. |
| `RequireAvc`, `EnableAudioVbrEncoding` | Profile flags; not visible in ffmpeg args. |
| `Tag` | The MediaSource `ETag`. |
| `SubtitleMethod=Encode` | Present even with no subtitle (see below). |
| `{codec}-profile/-level/-videobitdepth/-rangetype/-audiochannels`, `h264-deinterlace` | Flattened `CodecProfiles` conditions from the DeviceProfile. The same file in different states produced `h264-profile=constrainedbaseline` (once the source was H.264 and Auto) vs the full list (HEVC source). ffmpeg used `baseline` level 31 in the 720 kbps case and `high` level 51 for the HEVC source. |
| `TranscodeReasons` | See below. |

Added by the server on playlist and segment URLs: `runtimeTicks` (start of the segment, in 100 ns ticks) and `actualSegmentLengthTicks` (its length). The init segment line has `runtimeTicks=0&actualSegmentLengthTicks=0`.

`TranscodeReasons` values seen: `VideoCodecNotSupported` (HEVC), `AudioCodecNotSupported` (AC3), `ContainerBitrateExceedsLimit` (720 kbps cap), `SubtitleCodecNotSupported` (burn-in pref, text sub, high bitrate). One value at a time in every run. It exists only in URLs, not as a MediaSource field.

## PlaybackInfo request and reply

Request body (DeviceProfile removed; in the JSON as `deviceProfile`): `UserId, StartTimeTicks, IsPlayback:true, AutoOpenLiveStream:true, AudioStreamIndex, SubtitleStreamIndex, MediaSourceId, MaxStreamingBitrate, AlwaysBurnInSubtitleWhenTranscoding`. `AudioStreamIndex` and `SubtitleStreamIndex` are strings (`"1"`, `"-1"`) on the first call and numbers on later ones. `MaxStreamingBitrate` is 2147483647 for Auto. No query string.

Fields that differ in the `MediaSources[0]` reply, direct play vs transcode (same file, 720 kbps cap):

| Field | Direct play | Transcode |
|---|---|---|
| `SupportsDirectPlay` | true | false |
| `SupportsDirectStream` | true | false |
| `TranscodingUrl` | absent | `/videos/.../master.m3u8?...` |
| `TranscodingSubProtocol` | `"http"` | `"hls"` |
| `TranscodingContainer` | absent | `"mp4"` |
| `Container` | `"mp4"` | `"mov"` (the MP4 file again; MKV files stayed `"mkv"`) |

Everything else matched, including `SupportsTranscoding:true`, `Bitrate`, `HasSegments:false`, `DefaultAudioStreamIndex`, `DefaultSubtitleStreamIndex`, `RequiresOpening/Closing:false`. Top level of the reply is only `MediaSources` and `PlaySessionId` (32 hex). No `ErrorCode`. In `MediaStreams` for a transcode with subtitles, the DeliveryMethod fields are described under Subtitles.

## master.m3u8, main.m3u8

Content type for both: `application/vnd.apple.mpegurl`, no cache headers. HEVC case, master verbatim except `<..>`:

```
#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=6172692,AVERAGE-BANDWIDTH=6172692,VIDEO-RANGE=SDR,CODECS="avc1.640033,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=24
main.m3u8?<TranscodingUrl query, with AudioCodec=copy>
```

One variant only. Relative URI. `CODECS` for the 720 kbps case: `avc1.42401F,mp4a.40.2`, `RESOLUTION=960x540`. `BANDWIDTH` for HEVC = ffmpeg `-maxrate` + 128000 (6044692 + 128000).

main.m3u8, same case (31 entries; the `<Q>` stands for the same query used in master with `runtimeTicks`/`actualSegmentLengthTicks` appended; the first line shows the real thing):

```
#EXTM3U
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:3
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-MAP:URI="hls1/main/-1.mp4?DeviceId=..&MediaSourceId=..&...&TranscodeReasons=VideoCodecNotSupported&runtimeTicks=0&actualSegmentLengthTicks=0"
#EXTINF:3.000000, nodesc
hls1/main/0.mp4?<Q>&runtimeTicks=0&actualSegmentLengthTicks=30000000
#EXTINF:3.000000, nodesc
hls1/main/1.mp4?<Q>&runtimeTicks=30000000&actualSegmentLengthTicks=30000000
... (3.000000 each)
#EXTINF:0.021000, nodesc
hls1/main/30.mp4?<Q>&runtimeTicks=900000000&actualSegmentLengthTicks=210000
#EXT-X-ENDLIST
```

Notes:
- The whole playlist exists immediately, as a finished VOD playlist (`ENDLIST`), before any segment is produced. No `EXT-X-DISCONTINUITY`, no `BYTERANGE`, no `PROGRAM-DATE-TIME`, no `INDEPENDENT-SEGMENTS`.
- `#EXTINF` text is `<duration>, nodesc` (note the space and `nodesc`).
- Full queries repeat on every line (34 KB for 31 entries).
- Re-encoded video: fixed 3 s (`-hls_time 3`, `-force_key_frames:0 expr:gte(t,n_forced*3)`, `-sc_threshold 0`; last entry is the remainder).
- Video **copy** (AC3 in MKV): `-hls_time 6` and the lengths follow the source keyframes. With keyframes at 0,3,7,13,18,26,... the playlist read 7,6,5,8,5,8,5,8,5,8,5,8,5,7.005 s (`TARGETDURATION:8`). The 3 s keyframe was skipped; every later keyframe was a cut. This is exact, `runtimeTicks` for each entry accumulates those lengths.
- Segments: media segments start with a `styp` box (`....stypmsdh`), init is `ftypiso5` (1350 bytes, `video/mp4`, `accept-ranges: bytes`). Segment sizes about 0.9 MB per 3 s at 720p.

## Segment URL shape

```
/videos/{dashed-id}/hls1/main/-1.mp4?{same query as main.m3u8}&runtimeTicks=0&actualSegmentLengthTicks=0     init
/videos/{dashed-id}/hls1/main/{n}.mp4?{same}&runtimeTicks={start}&actualSegmentLengthTicks={len}              media
```

`playlistId` is the literal `main`; `segmentId` is `-1` for init; extension is `mp4` (SegmentContainer). Request carries no body or special headers. The client often requested the first media segment twice at start (seen for segments 0 and 1). Latency for a segment the server already has: 4-15 ms.

## Subtitles (scenario 7)

All with the Chrome profile (`SubtitleProfiles`: `vtt`, `ass`, `ssa`, all `External`).

- **Direct play and transcode alike**, default prefs: both text tracks are `DeliveryMethod:"External"`, `IsExternalUrl:false`, `IsTextSubtitleStream:true`, `SupportsExternalStream:true`. `DeliveryUrl` is `/Videos/{dashed-id}/{hex-source-id}/Subtitles/{index}/0/Stream.vtt?ApiKey=..` for SRT (`subrip`) and `.../Stream.ass?ApiKey=..` for ASS. No `SubtitleStreamIndex` is sent to the transcoder for a client-rendered subtitle: the transcode in 7b started with `SubtitleStreamIndex:-1`, and switching subtitles caused **no new PlaybackInfo, no new ffmpeg, no playlist change** (only `/Sessions/Playing/Progress` with the new `SubtitleStreamIndex`). `SubtitleMethod=Encode` is in every TranscodingUrl regardless.
- jellyfin-web did not use the SRT `DeliveryUrl` as given: it requested `.../Subtitles/2/0/Stream.js?ApiKey=..` instead (`application/json`, `{"TrackEvents":[{"Id","Text","StartPositionTicks","EndPositionTicks"}]}`), and rendered it as a `<track>`. For ASS it fetched `GET /System/Configuration/encoding` and then `Stream.ass` (`text/x-ssa`) for its libass worker. Responses (via curl, same token): `Stream.vtt` text/vtt, `Stream.srt` application/x-subrip, `Stream.js` application/json, `Stream.ass` text/x-ssa (UTF-8 BOM, the font name `Arial` rewritten to `Arial Unicode MS`), `Stream.vtt` from an ASS source works (tags stripped). `.../Subtitles/2/0/Stream` with no extension: 404. A non-zero `startPositionTicks` (`300000000`) shifts cue times by that amount and clamps at 0.
- The initial `SubtitleStreamIndex` in the first PlaybackInfo varied between runs (-1, 2, 3) and the matching `Stream.*` was fetched at start. I did not work out what it was keyed on; it was not a profile setting I changed.
- **Burn-in** only happened when I changed two user settings (Subtitles page: "Burn subtitles" = All, and "Always burn in subtitle when transcoding" on). Then the DeviceProfile sent by jellyfin-web has `SubtitleProfiles: []` (the only difference from the profile in the JSON), PlaybackInfo carries `AlwaysBurnInSubtitleWhenTranscoding:true`, and the reply marks **every** subtitle stream `DeliveryMethod:"Encode"` with no `DeliveryUrl`, even when subtitles are off in a transcode. Selecting any subtitle (or off, or changing quality) is a new PlaybackInfo (`StartTimeTicks` = position), then `DELETE ActiveEncodings`, then a new master with `SubtitleStreamIndex={n}` and `SubtitleMethod=Encode`. With a subtitle burnt in, `TranscodeReasons=SubtitleCodecNotSupported` even at Auto bitrate and the video is re-encoded (`libx264`, no `-map -0:s`), with `-vf ...,subtitles=f='/config/data/subtitles/{n}/{dashed-id}/{index}.srt':fontsdir='/config/data/attachments/{n}/{dashed-id}'` and `-start_at_zero`. Off while transcoding: a fresh PlaybackInfo with `SubtitleStreamIndex:-1`, no subtitle filter.
- **PGS: not captured.** ffmpeg cannot encode PGS and I found no cheap way to build one. Image subtitles are expected to take the burn-in path above, but I did not see it.

## Stop and navigate away (scenario 8)

Order when leaving the player with `history.back()` (transcode or direct play): `POST /Sessions/Playing/Stopped` (204, no query, JSON body with `PositionTicks`, `PlaySessionId`, `PlayMethod`, `NowPlayingQueue`...), with `GET /Items/{id}` and `GET /Items/{id}/ThemeMedia` sent at the same millisecond by the details page. **No `DELETE /Videos/ActiveEncodings` is sent on a normal stop.** The server still ended ffmpeg: it logged `[q] command received. Exiting.` and the process was gone at the first poll, 3 s after the click (the Stopped request itself took about 1.5-3.6 s to return).

`DELETE /Videos/ActiveEncodings?deviceId={DeviceId}&PlaySessionId={old session}` -> 204, empty body, is sent only when jellyfin-web replaces a transcode: audio change, subtitle change with burn-in, quality change from a transcode. It is sent twice (before the new master is requested, and again about 0.3 s later), both with the old `PlaySessionId`.

Closing the tab (`page.close(runBeforeUnload)`) sent no Stopped (the request was aborted) and no DELETE. ffmpeg kept running (still alive at 40 s in one run) and exited with `q` between 62 and 65 s after the tab closed (second run, 3 s polling). That suggests an idle timer of about a minute, but it is one measurement.

`about:blank` navigation (audio test) also produced no Stopped.

## Routes

Seen, with the casing jellyfin-web used:

| Route | Seen for |
|---|---|
| `POST /Items/{id}/PlaybackInfo` | every start and every stream change |
| `GET /videos/{dashed-id}/master.m3u8` | transcode start |
| `GET /videos/{dashed-id}/main.m3u8` | via master |
| `GET /videos/{dashed-id}/hls1/main/-1.mp4` and `/{n}.mp4` | init, media |
| `DELETE /Videos/ActiveEncodings?deviceId&PlaySessionId` | stream replacement |
| `GET /Videos/{id}/stream.{ext}?Static=true&mediaSourceId&deviceId&ApiKey&Tag` | direct play |
| `GET /Videos/{dashed-id}/{hex-source-id}/Subtitles/{index}/{startPositionTicks}/Stream.{vtt,js,ass}` | subtitles |
| `GET /System/Configuration/encoding` | before ASS rendering |
| `POST /Sessions/Playing`, `/Progress`, `/Stopped` | as usual; `PlayMethod` is `DirectPlay` or `Transcode` |
| `GET /Sessions?deviceId=..` | with burn-in stream changes |

Cross-check with `compat/hidden-routes-12.4.txt`: `DynamicHls master.m3u8`, `main.m3u8`, `hls1/{playlistId}/{segmentId}.{container}` and `HlsSegment DELETE /Videos/ActiveEncodings` are all in that file as `current` (hidden from the spec, not obsolete). Seen live: those four. Listed there but **never requested**: `HEAD master.m3u8`, `live.m3u8`, `GET /Videos/{id}/hls/{playlistId}/stream.m3u8`, `.../hls/{playlistId}/{segmentId}.{segmentContainer}`, and the `/Audio/...` equivalents. The subtitle routes are not in the hidden file (they are in the public spec); they were seen.

Against `compat/endpoints.yaml` (and `internal/api/stream.go`, where `/Videos/{itemId}/{file}` answers 404 unless `file` is `stream` or `stream.*`). This is how it stood when captured: the first transcode slice has since added `master.m3u8`, `main.m3u8`, `hls1/{playlistId}/{segmentId}`, `DELETE /Videos/ActiveEncodings` and a PlaybackInfo that reads its body. Subtitle routes and `GET /System/Configuration/encoding` are still absent.

- Absent: `GET /Videos/{id}/master.m3u8` and `main.m3u8` (the `{file}` route would match the shape, but the handler rejects them), `GET /Videos/{id}/hls1/{playlistId}/{segmentId}.{container}` (three segments; no route), `DELETE /Videos/ActiveEncodings`, `GET /Videos/{id}/{mediaSourceId}/Subtitles/{index}/{startPositionTicks}/Stream.{format}` (and the variant without ticks), `GET /System/Configuration/encoding`.
- Stubbed or partial: `POST /Sessions/Capabilities/Full` is a stub (fine). `POST /Items/{id}/PlaybackInfo` is implemented, but `playbackInfo` forces `SupportsTranscoding:false` and `TranscodingSubProtocol:"http"` and never sets `TranscodingUrl`, `TranscodingContainer`, `SupportsDirectPlay`/`SupportsDirectStream` from the profile, or subtitle `DeliveryMethod`/`DeliveryUrl`. It also does not read the request body, so `StartTimeTicks`, `AudioStreamIndex`, `SubtitleStreamIndex`, `MaxStreamingBitrate` and `AlwaysBurnInSubtitleWhenTranscoding` are ignored.
- Path casing: transcode URLs are lower-case `/videos/`; direct play and subtitles use `/Videos/`. The router must be case-insensitive on `videos`.

## Surprises, and where this differs from earlier findings

- **The playlist is built before the encode and is complete.** The client never sees a growing playlist, and there is no `EXT-X-DISCONTINUITY` even though the server restarts ffmpeg per seek. This matches the seam spike's goal.
- **The restart recipe matches `spikes/seam`'s required settings in spirit:** `-ss` + `-start_number N`, `-copyts`, `noise=drop` on copied audio, fMP4 with `movflags=+frag_discont`. It differs in details: the real one adds `+skip_sidx`, uses `-hls_time 3` and `expr:gte(t,n_forced*3)` (spike: 4 s), has no `-output_ts_offset`, and re-encodes with `-sc_threshold 0`, `open_gop=0`. The real command lines (19 logs) are in the JSON under `ffmpegCommandLinesFromServerLogs`.
- **Segment length is 3 s for re-encode, keyframe-driven for video copy.** `spikes/README.md` only says the media segments are `{n}.mp4`. A copy-mode playlist therefore depends on the source's keyframes, which is not a fixed grid.
- **MP4 copy-mode playlists lie.** For an H.264+AC3 *MP4* (`3b`), the playlist advertised fifteen 6.000 s segments, but the segments ffmpeg wrote (checked with ffprobe on init+segment) start at 0, 7, 13, 18, 26, 31 s, i.e. 7,6,5,8,5,8 s. For MKV the server computes the exact keyframe lengths (`AllowOnDemandMetadataBasedKeyframeExtractionForExtensions: ["mkv"]` is the server setting that seems responsible). Players coped in Chrome, but an implementer should not copy this.
- **A normal stop sent no DELETE.** `DELETE /Videos/ActiveEncodings` looks like a stop signal, but for jellyfin-web it is only the "replace this stream" signal. The server stops ffmpeg from `Sessions/Playing/Stopped`, and from an idle timer when no stop arrives.
- **Seek never involves PlaybackInfo.** Segment numbers alone drive it, using the playlist's `runtimeTicks` and a 3 s grid in transcode mode. The ffmpeg restart is triggered by an unproduced segment being requested.
- **The Auto bitrate is 2147483647 plus a bitrate probe** (`GET /Playback/BitrateTest?Size=500000|1000000|3000000`, repeated every few seconds) and the quality menu starts as "Auto 8 Mbps". The measured result never reached the server; `MaxStreamingBitrate` stayed 2147483647.
- `Container` of an MP4 source becomes `"mov"` when it is transcoded (and only then).
- `PlayMethod` is `Transcode`, not `DirectStream`, even when the video is copied.
- Burn-in requires non-default user settings; it is never chosen automatically for text subtitles with this profile.

## Not observed

- PGS/VobSub (no cheap fixture), HDR/Dolby Vision, 10-bit, interlaced, multi-channel audio downmix, hardware encoding, `ts` segments, `HEAD master.m3u8`, `live.m3u8`, audio-only playback, external subtitle files, other clients (Safari/iOS, ExoPlayer).
- What decides the initial `SubtitleStreamIndex` of a play.
- How the idle timer is configured; only one measurement (about 65 s).
- Whether a second `DELETE ActiveEncodings` has a visible effect.
- Time-to-first-frame was not measured (CPU limited and noisy).
