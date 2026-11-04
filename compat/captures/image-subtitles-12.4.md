# What Jellyfin 12.4 and jellyfin-web 12.4 do with PGS, VobSub and DVB tracks inside a film

Observed, not read from source (the jellyfin-web code was read only to find which setting turns PGS rendering on). Server: `jellyfin/jellyfin:12.4` (12.4.0, jellyfin-ffmpeg 8.1.3), a disposable container on a Docker-assigned port, 2026-11-03. Client: jellyfin-web 12.4.0 in Chrome 154, driven with playwright. The exchanges are in `image-subtitles-12.4.json` (same directory); ids and the token are placeholders there.

The films are 120 s black H.264 and AAC Matroska files, 640x360. ffmpeg cannot write PGS, so the PGS track is a hand-made `.sup` (`test/fixtures/pgs`, `internal/testenv/pgs.go`: yellow rectangles on a 1920x1080 canvas, cues at 20, 60 and 100 seconds) muxed with `-c:s copy`; Jellyfin and ffmpeg both decode it, and jellyfin-web draws it. VobSub (`dvd_subtitle`) and DVB (`dvb_subtitle`) are ffmpeg's own `dvdsub` and `dvbsub` encoders, fed from the PGS track (bitmap to bitmap). The files are real tracks of those codecs, though not from a disc. A Mixed film has a SubRip track and two PGS tracks (one forced and default, French, 1280x720).

## Listing

| Codec in the file | `Codec` | `DisplayTitle` | `SupportsExternalStream` | `Width`, `Height` |
|---|---|---|---|---|
| `hdmv_pgs_subtitle` | `PGSSUB` | `English - PGSSUB` (`French - Default - Forced - PGSSUB`) | true | the canvas, 1920 x 1080 |
| `dvd_subtitle` | `DVDSUB` | `French - DVDSUB` | true | the canvas |
| `dvb_subtitle` | `DVBSUB` | `Undefined - DVBSUB` | false | absent: the stream has none |

`IsTextSubtitleStream` is false, and `IsDefault`, `IsForced`, `IsExternal` and `IsHearingImpaired` are as for text. The other fields (`TimeBase`, `VideoRange`, `IsOriginal`, the `Localized*` names) are not given by jellyscale, as for its text tracks. The Mixed film numbers its tracks as the file does (SubRip 2, PGS 3 and 4).

## PlaybackInfo

Delivery depends on the client's `SubtitleProfiles`, as it does for text:

- **Chrome's, as jellyfin-web sends it by default** (`vtt`, `ass`, `ssa`, all `External`): every image track is `DeliveryMethod: Encode` with no `DeliveryUrl`, even with no track chosen. Choosing one gives a transcode: `SupportsDirectPlay` false, a `TranscodingUrl` with `SubtitleStreamIndex={n}`, `SubtitleMethod=Encode` and `TranscodeReasons=SubtitleCodecNotSupported`. This holds for PGS, VobSub and DVB alike. With no `SubtitleStreamIndex` in the request but a `UserId`, Jellyfin chose the track flagged default (the only PGS track of the first film, the forced French track of the Mixed film) and burnt it in; jellyscale chooses nothing (DOMAINS 32).
- **With `{Format: pgssub, Method: External}` and `{Format: vobsub, Container: mks, Method: External}` added** (what jellyfin-web sends when its viewer turns on PGS rendering): PGS and VobSub tracks are `External` with `IsExternalUrl: false` and `DeliveryUrl: /Videos/{item}/{source}/Subtitles/{index}/0/Stream.pgssub?ApiKey=...` (PGS) or `.../Stream.vobsub?ApiKey=...` (VobSub), and picking one makes no transcode. DVB stays `Encode`: Jellyfin does not deliver it as a file at all.

jellyfin-web 12.4 sends those two profiles only when `localStorage.subtitlerenderpgs` is `"true"` and the page has a 2d canvas, and when its burn-in setting is not "all", "allcomplexformats" or "onlyimageformats".

## The file

`GET .../Subtitles/{index}/0/Stream.pgssub` and `Stream.sup` answer the same bytes: 200, `Content-Type: application/octet-stream`, `Accept-Ranges: bytes`, `Content-Length`, no `Content-Disposition`; the file is a `.sup`, each segment with its `PG` header (the track of a Matroska file, `ffmpeg -c:s copy -f sup`). A `Range` request answers 206 with `Content-Range: bytes 0-0/N` and `bytes 100-N-1/N`. `StartPositionTicks` and a route with no start offset are ignored. `HEAD` answers 405. A track asked for in a format it doesn't have (PGS as `.vtt`, `.srt`, `.ass`, `.js`; a SubRip track as `.pgssub` or `.sup`) answers 400, `subtitles.m3u8` of a PGS track 404, and an index past the end answers 500 (jellyscale: 404).

VobSub: `Stream.vobsub` and `Stream.mks` answer a Matroska file holding only the track (1640 bytes for the three cues), `video/x-matroska`, with ranges; `.sub` and `.idx` answer 400. DVB: every format answers 404.

## jellyfin-web

With the setting on, a PGS track on a **direct play** is drawn by jellyfin-web itself (libbitsub, a WebGL canvas of 1280x720 over the video): picking it makes no PlaybackInfo and no transcode. It fetches `DeliveryUrl` twice, first with `Range: bytes=0-0` (answered 206) and then with no `Range` (200, the whole file), and the bars appear on that canvas at the cue times (checked in screenshots: yellow at the top from 20 s, with the track's position scaled to the screen). VobSub is fetched as the `.mks` the same way (a `0-0` probe, then the whole file) and drawn. With the setting off (the default), picking a track is a new PlaybackInfo and a transcode as above, and nothing is fetched from `/Subtitles/`.

## ffmpeg's command line

Jellyfin's, from its transcode logs, for PGS (the canvas is the stream's size):

```
ffmpeg -analyzeduration 200M -probesize 1G -canvas_size 1920x1080 -i file:"<film>" -map_metadata -1 -map_chapters -1 -threads 0 -map 0:0 -map 0:1 -map -0:0 -codec:v:0 libx264 -preset veryfast -crf 23 ... -filter_complex "[0:2]scale,scale=640:360:fast_bilinear[sub];[0:0]setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709,format=yuv420p[main];[main][sub]overlay=eof_action=pass:repeatlast=0" -start_at_zero -codec:a:0 copy -copyts ... -f hls
```

VobSub is the same with the track as `[0:1]`. DVB has no canvas size, so it has no `-canvas_size`, and Jellyfin scales the track by height and pads and crops it to the picture (`scale=-1:360:fast_bilinear,crop,pad=...,crop=640:360`). The track comes from the same input as the video, so the transcode reads the film once; no separate extraction is made. jellyscale does the same (`internal/transcode/ffmpeg.go`) with `overlay=eof_action=pass:repeatlast=0` after scaling the track to the output's size, and keeps the picture's size for DVB instead of padding.

## Where jellyscale answers differently

- A VobSub track is `Encode` for every client, listed `SupportsExternalStream: false`, and `Stream.vobsub` and `.mks` answer 400. A jellyfin-web viewer who draws VobSub itself gets it drawn into the picture instead (DOMAINS 51).
- An index past the end answers 404, not 500. `HEAD` of the file answers like `GET` without the body (200 or 206).
- The server picks no default track (DOMAINS 32). `DisplayTitle` has the language code, as the film's text tracks do (`eng - PGSSUB`), not its name (`English - PGSSUB`).
- `TimeBase`, `VideoRange`, `VideoRangeType`, `AudioSpatialFormat` and the `Localized*` fields are not sent.
- A burnt PGS cue that began before a transcode job's restart point is not drawn by that job (DOMAINS 51). Jellyfin was not run against a seek into a cue.
