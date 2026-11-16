# What Jellyfin 12.4 says about a film's original language

Observed 2026-11-15, not read from source. Server: `jellyfin/jellyfin:12.4` (12.4.0, image id `fea863d0cd9e`), a disposable container on a Docker-assigned port (`-p 127.0.0.1::8096`). Four 3 second Matroska films (H.264, AAC mono, 160x90) with synthetic names (`Orig A (2020)` to `Orig D (2020)`), mounted read-only at `/media/movies`. The answers, cut to the fields below, are in `original-language-12.4.json` (same directory).

## How the library was set up with no provider

`POST /Library/VirtualFolders?name=Movies&collectionType=movies&refreshLibrary=true` with
`{"LibraryOptions":{"EnableInternetProviders":false,"PathInfos":[{"Path":"/media/movies"}],"TypeOptions":[{"Type":"Movie","MetadataFetchers":[],"ImageFetchers":[]}]}}`.
`GET /Library/VirtualFolders` read it back: `EnableInternetProviders` false and `MetadataFetchers` and `ImageFetchers` empty for `Movie`. The container has no outbound requests in its log for a metadata site. The wizard is as in `differential-12.4.md`; a 3 s wait after the server answers `/System/Info/Public` is needed before `POST /Startup/Configuration`, or the first calls fail.

## The films

| Film | Audio tracks as the file has them | ffprobe |
|---|---|---|
| A | `eng` (default), `deu` | index 1 `eng` default 1, index 2 `deu` default 0 |
| B | `deu`, `eng` (default) | index 1 `deu` default 0, index 2 `eng` default 1 |
| C | untagged (default), `eng` | index 1 no language default 1, index 2 `eng` default 0 |
| D | none | video only |

Each was asked with `GET /Items/{id}?userId=`, `GET /Users/{userId}/Items/{id}`, `POST /Items/{id}/PlaybackInfo?UserId=` with body `{}` and `GET /Items/{id}/PlaybackInfo?UserId=`.

## What was observed

- **`OriginalLanguage` is absent** (not null, not empty) on the item of all four films, on both item routes. It is not on any `MediaSources[]` entry of the item or of PlaybackInfo either.
- **`IsOriginal` is false on every audio stream** of every film, in the item's `MediaStreams`, in `MediaSources[].MediaStreams` of the item and in both PlaybackInfo answers. Jellyfin has no per-file origin of its own: with no provider nothing sets it.
- **`LocalizedOriginal` is the word `Original`** on every stream, whatever `IsOriginal` says.
- **`Language`** is the tag as the file wrote it (`eng`, `deu`), and absent for the untagged track. **`DisplayTitle`** is built from the language name and the codec: `English - AAC - Mono - Default`, `German - AAC - Mono` (default is added to the one flagged), and for the untagged track `AAC - Mono - Default` (no language word).
- Film D (no audio): the item has `MediaStreams` with the video alone and no `OriginalLanguage`.
- PlaybackInfo by `GET` and by `POST` list the same two audio streams with the same fields as the item.

| Film | Audio index: Language, IsDefault, IsOriginal, DisplayTitle |
|---|---|
| A | 1: eng, true, false, `English - AAC - Mono - Default`; 2: deu, false, false, `German - AAC - Mono` |
| B | 1: deu, false, false, `German - AAC - Mono`; 2: eng, true, false, `English - AAC - Mono - Default` |
| C | 1: (absent), true, false, `AAC - Mono - Default`; 2: eng, false, false, `English - AAC - Mono` |
| D | no audio stream; no `OriginalLanguage` |

## What jellyscale does, and where it differs (JS-117)

The original language is a deliberate improvement on this, from the file alone (the owner's decision of 2026-11-15): the language of the default audio track, as the file wrote the tag; `IsOriginal` true on the audio streams in that language. Where Jellyfin sends `OriginalLanguage` absent and `IsOriginal` false, jellyscale sends them, so the differences are listed in `compat/differences.yaml` as improvements, under each request of the differential tier that shows them. `LocalizedOriginal` is not sent by jellyscale (an existing difference listed under JS-068).
