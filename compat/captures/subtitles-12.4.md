# What Jellyfin 12.4 does with subtitle files and attached fonts

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0, jellyfin-ffmpeg 8.1.3). The exchanges are in `subtitles-12.4.json` (same directory); ids and the token are placeholders there. The film is a 30 s Matroska file with an embedded SubRip track (eng, default), an embedded ASS track (jpn) and one attached font (`LiberationSerif-Regular.ttf`, `font/ttf`), with 24 files beside it named `Ext Film (2024).<words>.<ext>`, and `Other.en.srt`, which has another name. jellyfin-web 12.4.0's behaviour was read from `web-dist` and is checked in `test/e2e/subtitles.mjs`.

## How a file's name is read

`Film.<words>.<ext>` is a track of `Film.mkv`; `Other.en.srt` is not. Words are separated by dots and may come in any order:

| Word | Result |
|---|---|
| `default` | `IsDefault` |
| `forced`, `foreign` (any case) | `IsForced` |
| `cc`, `hi`, `sdh` | `IsHearingImpaired` |
| a two or three letter code (`en`, `eng`, `deu`, `de`), a code with a region (`pt-BR`), or an English name (`English`) | `Language`: ISO 639-3 (`eng`, `deu`, `spa`, `fra`, `nld`, `jpn`, `kor`, `ita`, `zho`), or the tag as written, lower-cased, with a region (`pt-br`) |
| anything else (`commentary`, `en_US`, `zz`) | `Title` |
| none | `Language` absent |

Which words are languages is Jellyfin's own list (its ISO 639-2 table, in `languageWords` of the JSON): a code it has no name for is a word of the title. `chs`, `cht`, `cmn`, `yue`, `qaa`, `scc`, `iw`, `jp` and `xyz` read as titles (`cht - Undefined - SUBRIP - External`; jellyscale shows them the same way, as the label Jellyfin shows), while `mis` is "Uncoded languages", `mul` "Multiple languages", `und` "Undetermined", `zxx` "No linguistic content", `fil` "Filipino" and `tl` is `tgl`, "Tagalog". `zh-tw` and `zh-hant` are both "Chinese (Traditional)". jellyscale reads a code as a language when the Go language tables have an English name for it, so it follows Jellyfin for these except `cmn`, `yue` and `iw` (Hebrew; languages here, titles there), `tl` (stored as `fil` and labelled "Filipino", where Jellyfin stores `tgl` and says "Tagalog"), `zh-tw` ("Chinese (Taiwan)" here), `zh-hant` (a title here) and a few names ("Norwegian Bokmål", "Chinook Jargon").

`DisplayTitle` is `[Title - ]Language - [Hearing Impaired - ][Default - ][Forced - ]CODEC - External`, with `Undefined` for no language and `Portuguese (Brazil)` for `pt-BR`. `.srt` is `subrip`, `.vtt` is `webvtt`, `.ass` and `.ssa` are both `ass`. Jellyfin also lists `.sub`, `.smi` and `.sup`; jellyscale reads `.srt`, `.ass`, `.ssa` and `.vtt`.

## Numbering

Jellyfin numbers the external files first, in file name order ignoring case (`cc`, `commentary.es`, `de.sdh`, ... `pt-BR`, then the file with no words last), from 0, and the film's own streams after them: with 14 files, the video was 14, the audio 15 and the embedded subtitles 16 and 17. The shift moves every embedded index when a file is added. jellyscale keeps the film's own indices and numbers the files after them in the same order, which jellyfin-web accepts because it reads `Index` from the list.

## Delivery

PlaybackInfo gives each external track `DeliveryMethod: External`, `IsExternalUrl: false` and `DeliveryUrl: /Videos/{item}/{source}/Subtitles/{index}/0/Stream.vtt?ApiKey=...`, with `Stream.ass` for an ASS track, exactly as for an embedded one. `DefaultSubtitleStreamIndex` was the index of the file flagged `default`. A file in Windows-1252 (`caf\xe9`) is served as UTF-8. `Stream.srt`, `Stream.js` and a start offset work as for embedded tracks. `Subtitles/{index}/subtitles.m3u8` is observed on the same server with a 30.023 s film and a one-cue-per-window SubRip file (every fetch of it with the token as `ApiKey` or a `MediaBrowser` `Authorization` header; `X-Emby-Token` and `api_key` answer 401 here, though `Stream.vtt` accepts them). Without `segmentLength`, or with 0, a negative number or text, it answers 400; `runtimeTicks` is ignored (the length is the item's). With `segmentLength=6` it answers `application/vnd.apple.mpegurl`:

```
#EXTM3U
#EXT-X-TARGETDURATION:6
#EXT-X-VERSION:3
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:6,
stream.vtt?CopyTimestamps=true&AddVttTimeMap=true&StartPositionTicks=0&EndPositionTicks=60000000&ApiKey=<token>
... (one per 6 s)
#EXTINF:0.023,
stream.vtt?CopyTimestamps=true&AddVttTimeMap=true&StartPositionTicks=300000000&EndPositionTicks=300230000&ApiKey=<token>
#EXT-X-ENDLIST
```

`segmentLength=30` gives a 30 s segment and one of 0.023 s; 31 or more gives one segment, `#EXTINF:30.023,`, ending at the run time; `TARGETDURATION` is the number sent. The playlist is answered for a track that does not exist; `Subtitles/{index}/{start}/subtitles.m3u8` answers 404, `HEAD` 405. Each segment is a request for `Subtitles/{index}/stream.vtt` (the route without a start offset). Observed windows: a cue is in the window when it ends at or after `StartPositionTicks` and starts at or before `EndPositionTicks` (a cue ending at 6 s is in 0-6 s and in 6-12 s; one starting at 12 s is in 6-12 s). Without `CopyTimestamps=true` the times are moved earlier by `StartPositionTicks`, to a minimum of zero. With `AddVttTimeMap=true` the body starts `WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:900000,LOCAL:00:00:00.000\n\n`. `StartPositionTicks` and `EndPositionTicks` also work alone and on `Stream.js`. `Stream.vtt` and `stream.vtt` are the same route; `Stream.xyz` answers 400 and a name that is not `Stream.*` 404. jellyfin-web's profile only offers `External`, so it never asks (see the coverage row).

## Attachments

`MediaAttachments` of the item lists `{CodecTag: "[0][0][0][0]", Index, FileName, MimeType}`, and PlaybackInfo adds `DeliveryUrl: /Videos/{item}/{source}/Attachments/{index}` (no token). `GET` of it answers 200 with `Content-Type: font/ttf` and the bytes of the font, with no token at all; an index that is not an attachment answers 404 with a JSON string. jellyfin-web's `renderSsaAss` takes the `DeliveryUrl` of every attachment whose `MimeType` is one of `application/vnd.ms-opentype`, `application/x-truetype-font`, `font/otf`, `font/ttf`, `font/woff` or `font/woff2` and hands them to its ASS renderer as `fonts`.

## Where jellyscale answers differently

- `Subtitles/{index}/subtitles.m3u8` for a track that does not exist answers 404 here (Jellyfin answers the playlist). A format that `Stream.*` does not know is 400, a name that is not `Stream.*` 404, and `HEAD` of the playlist is 405, as observed.
- An attachment index that is no font answers 404 with the plain text `404 page not found` (Jellyfin: 404 with a JSON string). The route is authenticated (above), and its answer carries `ETag` (the SHA-256 of the font) and `Cache-Control: no-cache`; Jellyfin's carries only `Content-Type` and `Content-Length`. A request with the ETag in `If-None-Match` is 304.
- Files are listed after the film's own tracks (Numbering, above), and `.sub`, `.smi` and `.sup` are not read.
- The server chooses no default subtitle: `DefaultSubtitleStreamIndex` is -1 in PlaybackInfo unless the client names a track, and the details page does not carry it at all (not captured for Jellyfin). Jellyfin's PlaybackInfo gave the index of the file flagged `default`. See `subtitles-default-12.4.md`.
