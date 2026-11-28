# What Jellyfin 12.4 lists for a data track, and what jellyfin-web does with it

Observed 2026-11-16, not read from source. Server: `jellyfin/jellyfin:12.4` (image id `fea863d0cd9e`), a disposable container on a Docker-assigned port, no metadata provider (the library was set up as in `original-language-12.4.md`). The film is one synthetic MP4, `Tmcd Menu Film (2021).mp4`, made with
`ffmpeg -f lavfi -i testsrc2=size=320x180:rate=24:duration=30 -f lavfi -i sine=...(x2) -i en.srt -i de.srt -map 0:v -map 1:a -map 2:a -map 3 -map 4 -c:v libx264 -c:a aac -c:s mov_text -timecode 01:00:00:00 -metadata:s:a:0 language=eng -metadata:s:a:1 language=deu -metadata:s:s:0 language=eng -metadata:s:s:1 language=deu`.
ffprobe: 0 video, 1 audio eng, 2 audio deu, 3 subtitle eng (mov_text), 4 subtitle deu, 5 data (`tmcd`, the timecode track `-timecode` adds). The answers, cut to the fields below, are in `data-stream-12.4.json` (same directory), made by a real Jellyfin 12.4.

## What the API lists

Every list holds the data track, at its own index, as the last entry, so position and Index agree (0 to 5):

| Answer | List | Streams, position = Index |
|---|---|---|
| `POST /Items/{id}/PlaybackInfo?UserId=` (body `{}`) | `MediaSources[0].MediaStreams` | Video 0, Audio 1, Audio 2, Subtitle 3, Subtitle 4, **Data 5** |
| `GET /Items/{id}/PlaybackInfo?UserId=` | `MediaSources[0].MediaStreams` | the same |
| `GET /Items/{id}?userId=` and `GET /Users/{userId}/Items/{id}` | `MediaStreams` and `MediaSources[0].MediaStreams` | the same, in both lists |

`DefaultAudioStreamIndex` is 1 and `DefaultSubtitleStreamIndex` is 3 (the first subtitle, which the container flags default).

The data stream holds exactly these fields, the same in all four answers and both lists:
`CodecTag` `tmcd`, `Language` (`eng`: Jellyfin copies a tag from the container, the file did not set one), `TimeBase` `1/12288`, `VideoRange` `Unknown`, `VideoRangeType` `Unknown`, `AudioSpatialFormat` `None`, `IsInterlaced` false, `BitRate` (1), `IsDefault` false, `IsForced` false, `IsHearingImpaired` false, `IsOriginal` false, `Type` `Data`, `Index`, `IsExternal` false, `IsTextSubtitleStream` false, `SupportsExternalStream` false.
It has no `Codec`, no `Title`, no `DisplayTitle`, no `DeliveryMethod`, no `DeliveryUrl`, no `Localized*` field.

## What jellyfin-web 12.4 does with it

Opened in Chromium against the same container (`/web/`), signed in as the administrator:

- **Details page, audio and subtitle selects:** audio `1`, `2`; subtitles `-1` (Off), `3`, `4`. No entry for 5.
- **Details page, More, Media Info:** Video, Audio (1), Subtitle (3), Audio (2), Subtitle (4): five blocks, none for the data stream.
- **Player, audio menu:** `1`, `2`. **Player, subtitle menu:** Secondary Subtitles, Off (`-1`), `3`, `4`. No entry for 5.

The menus build their entries from streams of Type Audio and Subtitle, so a stream of Type Data at any index is never shown, and the details page and the media info dialog skip it the same way.

## What jellyscale does, and where it differs (JS-119)

A track outside an opted-in viewer's languages is listed as a placeholder of this shape: Type `Data` at the track's own Index, with the fields above, except that `Language` is left out and `CodecTag`, `TimeBase` and `BitRate` are left out or the same constant on every placeholder, so one placeholder tells nothing of the track it stands for. Jellyfin has no such stream for a track that has audio or subtitles in it; an opted-in viewer's answers differ from Jellyfin's, as listed in `the named exceptions held in the replay test`.
