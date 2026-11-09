# What Jellyfin 12.4 does with PGS and VobSub files beside a film

Observed, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0), a disposable container on a Docker-assigned port, 2026-11-09, one movies library of synthetic films mounted read-only. The exchanges are in `external-image-subtitles-12.4.json` (same directory); ids and the token are placeholders there. jellyfin-web was not driven for these files: it asks for a `DeliveryUrl` of an external `.sup` the way it asks for one of an embedded PGS track (`image-subtitles-12.4.md`), and `test/e2e/subtitles.mjs` checks that on jellyscale.

The films are 120 s black H.264 and AAC Matroska files. The `.sup` is `test/fixtures/pgs` (yellow bars on a 1920x1080 canvas). ffmpeg cannot write a VobSub pair, so the `.sub` and `.idx` are from `test/fixtures/vobsub` (`internal/testenv/vobsub.go`: the same bars on a 720x480 canvas, in MPEG program stream sectors, which ffmpeg decodes and draws). The first film has a SubRip track of its own, and beside it `.de.default.sup`, `.en.sup`, `.fr.forced.sup`, a bare `.sup`, `.en.srt`, `.es.sub` + `.es.idx`, `.ja.forced.sub` + `.ja.forced.idx`, a lone `.it.sub` and a lone `.ko.idx`.

## Listing

| File | `Codec` | Language | `DisplayTitle` | Size |
|---|---|---|---|---|
| `Film.de.default.sup` | `PGSSUB` | `deu` | `German - Default - PGSSUB - External` | 1920 x 1080 |
| `Film.fr.forced.sup` | `PGSSUB` | `fra` | `French - Forced - PGSSUB - External` | 1920 x 1080 |
| `Film.sup` | `PGSSUB` | none | `Undefined - PGSSUB - External` | 1920 x 1080 |
| `Film.es.sub` + `Film.es.idx` | `DVDSUB` | `en` | `English - DVDSUB - External` | 720 x 480 |
| `Film.ja.forced.sub` + `.idx` | `DVDSUB` | `en`, forced | `English - Forced - DVDSUB - External` | 720 x 480 |
| `Film.it.sub` alone | `DVDSUB` | `ita` (from the name) | `Italian - DVDSUB - External` | none |
| `Film.ko.idx` alone | not listed | | | |

- All are `IsExternal` true, `IsTextSubtitleStream` false, `SupportsExternalStream` true (also for VobSub), flags `IsDefault` and `IsForced` from the words of the file name as for text files (`.default`, `.forced`, `.sdh` as hearing impaired; any other word is a `Title`, as `en.commentary.sup` is `commentary - English - PGSSUB - External`).
- A `.sub` and `.idx` pair is one track, listed at the place of the `.idx` in file name order, with the `.idx` as its `Path`. Its language is the one inside the `.idx` (`id: en, index: 0`, which the generator writes), not the one in the file name: `Film.es.idx` is English and `Film.fr.idx` with `id: ja` is `ja`. The language is the two-letter code as the `.idx` has it, not ISO 639-3.
- A pair whose `.idx` ffprobe cannot read (no `id:` line, or text that is no index) is not listed, and neither is its `.sub`. A lone `.sub` is listed (`DVDSUB`, no size) though nothing can be drawn from it; a lone `.idx` is not. A `.sup` that is not PGS (`garbage` in it) is listed, with the language from its name and no size.
- Order: all the files first, in file name order (`.de.default.sup`, `.en.srt`, `.en.sup`, `.es.idx`, `.fr.forced.sup`, `.it.sub`, `.ja.forced.idx`, then the bare `.sup`), then the film's own streams, renumbered after them (video 8, audio 9, its SubRip 10). jellyscale numbers the film's own streams first, as it does for text files (`subtitles-12.4.md`).

## PlaybackInfo

With the profile's `SubtitleProfiles` as Chrome sends them (`vtt`, `ass`, `ssa`, all `External`): every image file is `DeliveryMethod: Encode` with no `DeliveryUrl`, and picking one gives `SupportsDirectPlay` false and a `TranscodingUrl` with `SubtitleStreamIndex={n}`, `SubtitleMethod=Encode` and `TranscodeReasons=SubtitleCodecNotSupported`. (`SubtitleStreamIndex` in the query was ignored; in the body it is read.)

With `{Format: pgssub, Method: External}` and `{Format: vobsub, Container: mks, Method: External}` added: every `.sup` is `External`, `IsExternalUrl` false, `DeliveryUrl: /Videos/{item}/{source}/Subtitles/{index}/0/Stream.pgssub?ApiKey=...`, and picking one is a direct play with no transcode. A VobSub file stays `Encode`, and picking it is a transcode as above: unlike a VobSub track inside the film (`image-subtitles-12.4.md`, sent as `.mks`), Jellyfin sends a VobSub file to no client.

## The file

`GET .../Subtitles/{index}/0/Stream.pgssub` and `Stream.sup` of a `.sup` answer the file's own bytes (200, `application/octet-stream`, `Accept-Ranges: bytes`, `Content-Length`; `Range: bytes=0-0` answers 206 `Content-Range: bytes 0-0/N`), `HEAD` 405. `.vtt` of it is 400, `subtitles.m3u8` 404, a SubRip track as `.pgssub` 400. For a VobSub file: `Stream.vobsub`, `.mks` and `.sub` answer 400; `Stream.idx` answers the `.idx` text (200) and `Stream.vtt` an empty cue list (200), which are quirks jellyscale does not copy.

## Where jellyscale answers differently

- It lists only a `.sub` with its `.idx` and an `.idx` with its `.sub`; a lone `.sub` is no track (Jellyfin lists it, without a way to draw it).
- It numbers the film's own streams first (above).
- `.sub` and `.idx` as a track in `.vtt` are 400, not 200.
- Everything `image-subtitles-12.4.md` lists for tracks inside the film: no server-chosen default, `HEAD` allowed, the `DisplayTitle` of a track inside the film with the code.
- A VobSub track inside the film is drawn into a transcode where Jellyfin sends it as `.mks` to a client that draws it (DOMAINS 51); the file case is the same as Jellyfin's.
