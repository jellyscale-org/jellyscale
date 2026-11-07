# Product

## What jellyscale is

A self-hosted video server that speaks the Jellyfin client API and runs as a horizontally scalable service. Someone who has outgrown a single Jellyfin process keeps their clients and gains independent scaling, failover, and object storage.

## Who it's for

Operators running one library on Kubernetes for a household or community, with tens to hundreds of concurrent streams. They're comfortable with Helm and Postgres. Their viewers aren't technical and only ever see a Jellyfin client.

## Goals, in priority order

1. **Playback never surprises a viewer.** Starting, seeking, resuming and recovering from a server failure should just work.
2. **Every component scales independently,** vertically or horizontally, with no shared state outside Postgres and the stores.
3. **Current Jellyfin clients work unmodified,** unless supporting one would create lasting technical debt.
4. **Storage is a per-library choice** between POSIX and S3-compatible object storage, with neither a second-class citizen.
5. **Operating it stays boring.** Postgres is the only required service, and configuration is declarative.

## Non-goals

- Music, photos, audiobooks and Live TV. Better tools exist for those.
- .NET plugins, and a plugin system generally.
- Multi-tenancy.
- A jellyscale-branded client. jellyfin-web is the reference UI, unmodified.
- Library or metadata administration through the dashboard. Libraries are configuration.
- Routes Jellyfin marks obsolete, apart from the exception below.

## Compatibility policy

- **Target:** the current Jellyfin release, currently 12.4.
- **Future-facing:** a client that depends on obsolete routes isn't well-suited to jellyscale.
- **The one exception:** thin aliases for routes jellyfin-web still calls, flagged `alias` in `compat/endpoints.yaml`. Each one is removed when the web client stops calling it. Never add an alias for another client.
- **Improve on Jellyfin.** Real Jellyfin behavior shows what clients depend on, not what to copy. Keep what clients need, and do better wherever they allow it.
- **Outside scope but called anyway:** answer with a valid empty response if the client breaks otherwise; otherwise 404.
- **Kodi** isn't a stated target. It works if it happens to, and no work goes in solely for it.

## Quality bar

- Behavior is observed from a real Jellyfin, not guessed. No code is ported from Jellyfin.
- A claim of working means a real client or a real store exercised it.
- Failure modes are designed, tested and reported. Nothing fails silently.
- Code is simple, readable and maintainable by people. The simplest thing that works wins.

## Roadmap

Subtitles inside the file are delivered or burnt in (done for text tracks, extracted by workers so that `api` and `stream` need no ffmpeg); external subtitle files beside the film (SubRip, ASS, SSA, WebVTT) are done the same way; image tracks inside the film (PGS, VobSub, DVB) are listed as Jellyfin lists them, and PGS is sent as the file to a client that draws it (jellyfin-web with its PGS setting on) and, like VobSub and DVB, drawn into a transcode otherwise, with `api` and `stream` still running no media tools; a seek into the middle of a burnt PGS cue misses that cue until the next, and external `.sup` and `.sub` files and VobSub as a file are not built. When the client plays a film's video but not its audio or container, the video is copied and only the audio is re-encoded (done for H.264 in MKV and MP4), so the picture keeps its quality exactly and the operator spends almost no CPU; the audio becomes stereo AAC. A copy at Auto carries the file's whole bitrate and size, 4K included, as direct play does, and a viewer on a slow link picks a cap. Its segments are stored at the file's own bitrate, not under the re-encode's ceiling, and a worker opens the file twice and reads it at 5 times the film's speed each time, so ten times the film's bitrate while the job runs and twice its bytes for the whole film, until the film ends or a minute after the last request, and never more than `transcodeLead` (60 seconds of film by default) ahead of the viewer; a seek waits for a whole segment, up to 12 seconds of film. When the video is re-encoded, the audio is copied if the client's fMP4 HLS profile lists its codec (AAC, MP3, AC3, E-AC3, Opus or FLAC) (a real client has played only AAC, Opus and FLAC; the rest are checked with ffmpeg only), its channels fit the profile's limit and it takes no more than a quarter of the bitrate cap (a deliberate choice: a copy that took more would starve the picture); a copy costs a worker no audio encode and the network and storage of the track's own bitrate (up to 640 kbps for AC3, 1 to 4 Mbps for FLAC) in place of 192 kbps. A restart on another worker puts the same audio packets in the same segments: for an MP4 source the audio in each segment is byte for byte the same, and for a Matroska source the packets are identical but their times can differ by under 1 ms, because Matroska stores whole milliseconds (accepted, not a defect). A missing, non-numeric or out-of-range `TranscodingMaxAudioChannels` is 2. Copying both the video and the audio is not built. The scan reads a keyframe index from each H.264 file (a Matroska file's Cues, an MP4's moov, which for a large file is megabytes), and reads where the video ends from the last stretch of the file. Phases and gates are in the project plan. In short: read-only browse (done), direct play (done), transcode (re-encode, subtitles and video copy done), HA, library and metadata, Kubernetes, SyncPlay.

Slices, in delivery order. The slice names are the ones in the `slice` column of `compat/coverage-12.4.tsv`, and `go test ./compat` fails if a needed route names another.

1. `phase3`: the remainder of Phase 3, the transcode gaps named above and in Features.
2. `phase4-ha`: Phase 4, high availability, with the gate below. Remote control of sessions rides on its cross-replica delivery.
3. `identity`: user management, hashed tokens, Quick Connect, then OIDC beside local accounts, right after HA.
4. `phase5-library`: Phase 5, library and metadata.
5. `phase6-k8s`: Phase 6, Kubernetes, with metrics and upgrades.
6. `phase7-syncplay`: Phase 7, SyncPlay.

Gate for Phase 4 (HA): a worker, and an API replica, killed in the middle of a film recover without losing the viewer's picture or resume position. The worker half is built: workers hold a Postgres advisory lock that dies with their process, the other workers notice it gone within a quarter second and gives the job to a live worker at the first missing segment, and in jellyfin-web the picture goes on with no dialog and no failed request (killed 1.6 seconds into playback, the least buffer there is, the picture of a re-encode stood still 2.5 to 2.8 seconds and that of a copy 0.3; a Go test with no browser measured about 4 seconds from SIGKILL to the replacement's first segment, and hls.js abandons a request at 10; `DOMAINS.md` invariants 25, 37 and 38). It needs a second worker, or the same one restarted within six seconds. When the lock is not closed with the process (host power loss, a partition, a connection pooler that holds the server's end), the dead job keeps its lease, up to 10 seconds from each renewal, and the picture freezes with no dialog; it is slow, and the worker stores nothing once its lease is gone. A burnt-in subtitle whose extraction was on the killed worker waits for that extraction's own 10 second lease. A viewer already playing a video copy whose keyframe index is found wrong still freezes with no dialog (`DOMAINS.md` invariant 47). The API replica half is not built: an `api` replica killed mid-film is not yet shown to lose nothing.

## Features

Every viewer-visible and operator-visible feature in scope, in one list. Status is **done**, **partial** or **not built**, and a feature is grouped under the slice that delivers it. A feature that is a non-goal is listed in the last group with a reason. Items marked (decision) are choices a maintainer should confirm.

### Built

- Browse libraries, items, seasons and episodes, latest and continue watching, and favourites: **done**.
- Played state through `/UserPlayedItems`, `POST /UserItems/{itemId}/UserData` (a partial write of played, position, last played and favourite; a body whose `LastPlayedDate` is older than the stored one cannot undo a newer position or played state, `DOMAINS.md` invariant 49) and playback reporting: **done**.
- Direct play from POSIX and S3-compatible stores, with ranges and HEAD: **done**.
- Transcode to H.264 with audio track and bitrate choice, seeks, and text subtitles delivered or burnt in: **done**.
- Video copy with audio re-encoded (SDR H.264, HEVC and AV1, in MKV and MP4): **done**.
- Local accounts with bcrypt passwords and per-device tokens; the first administrator comes from configuration: **partial** (no user management, see `identity`).
- Sessions list and playback reporting: **partial** (listing and progress done, no operator view of devices).
- Items' multiple audio tracks: **done**.
- Search by term (jellyfin-web's search page calls `GET /Items` with `searchTerm`): **done**.

### `phase3`

- HEVC and AV1 to clients that play them: **done** as direct play and as a copy into fMP4 HLS (DOMAINS 45), for HEVC Main and Main 10 and AV1 profile 0 at 8 or 10 bits, SDR, when the client's DeviceProfile lists the codec. Anything else is re-encoded to H.264 as before. Not built: encoding to HEVC or AV1, and copying or direct-playing HDR to a client that plays it (an HDR film is re-encoded to SDR with tone mapping unless the client's profile lets the file play as it is). CI's Chrome decodes AV1 but not HEVC, so a browser plays HEVC only on a machine with a hardware decoder (`E2E_HEVC=1`).
- Copying audio while the video is re-encoded: AAC, Opus and FLAC **done** (played and sought in jellyfin-web in Chromium); AC3, E-AC3 and MP3 **partial**: copied for clients whose profile lists them, checked with ffmpeg only. Copying audio with a copied video: **not built**. In scope.
- HDR tone mapping to SDR: **done** (DOMAINS 53; jellyfin-web plays HDR10 and HLG HEVC and 10-bit H.264 films through it in Chromium). A re-encoded film in PQ or HLG (HDR10, HLG, and Dolby Vision with an HDR10 or HLG base layer, whose own data is ignored) is tone mapped to SDR BT.709 by a worker in software, with `zscale` and `tonemap` (hable), which every supported ffmpeg has; Dolby Vision profile 5, which has no HDR10 base, is not mapped. An HDR film is never copied to a client that was told the video is SDR, H.264 included. It costs 2.7 times the CPU of an SDR re-encode from a 1080p source and 2.0 times from a 4K one (3.2 cores to keep a 4K source in real time, measured, DOMAINS 53), and a worker's slots do not weigh it. Not built: HDR direct play and copy with `VIDEO-RANGE=PQ` and an SDR fallback variant, GPU tone mapping.
- Image subtitles inside the file (PGS, VobSub, DVB): **done**. They are listed as Jellyfin lists them (`Codec` `PGSSUB`, `DVDSUB`, `DVBSUB`), and any other bitmap codec is not listed. jellyfin-web 12.4 draws PGS itself from the file when its viewer turns on PGS rendering (a setting that is off by default): the track is `External`, a worker copies it out of the film once (the whole film is read once for each PGS track) and `stream` sends it from the asset store. For every other viewer, and for VobSub and DVB always, choosing the track is a transcode with the track drawn into the picture, which a worker's ffmpeg reads from the film in the same pass as the video. A seek into the middle of a burnt PGS cue misses that cue until the next one (DOMAINS 51). Not built: VobSub as a file for the clients that draw it, external `.sup` and `.sub` files beside the film, and choosing a track by the viewer's subtitle mode (above).
- External subtitle files beside the film (`Film (2024).en.forced.srt`, `.ass`, `.ssa`, `.vtt`, from POSIX and S3): **done**. They are listed after the film's own tracks, in file name order, with the language (ISO 639-3), `Forced`, `Default` and hearing-impaired flags (`forced`, `foreign`, `default`, `cc`, `hi`, `sdh`) and any other word as `Title` that Jellyfin 12.4 gives them, and are delivered and burnt in through the path of an embedded track (`DOMAINS.md` invariant 50). The scan only lists them; a worker converts a file to UTF-8 when it is first asked for (UTF-8, UTF-16, and the legacy sets chardet detects, with the language in the file's name breaking a near-tie; Windows-1250 text and Thai are known to be misread, and the remedy is to convert the file to UTF-8), and a file that none of chardet's results decodes, or that ffmpeg rejects, is hidden with a warning until it changes, once the first request for it has found that out (that viewer gets an error, and a burn-in of it freezes the picture until a seek or reload); a failure that says nothing about the file (ffmpeg killed by a signal, a full disk, the store not answering) leaves it listed to be tried again. A rescan notices a file added, changed or removed without probing the film again; a file keeps its index while it exists and the index of a removed file is never reused, so open sessions are not shifted. It differs from Jellyfin in numbering the film's own streams first, where Jellyfin numbers the external files first and shifts the film's indices, and in reading only `.srt`, `.ass`, `.ssa` and `.vtt` (Jellyfin also takes `.sub`, `.smi` and `.sup`). Which track a viewer starts with is not decided by the server (see `identity`).
- Font attachments of Matroska files for ASS rendering (`GET /Videos/{videoId}/{mediaSourceId}/Attachments/{index}`): **done**. The scan lists them; the first PlaybackInfo or ASS track request has a worker take all the film's fonts out in one job and keep them in the asset store, and `MediaAttachments` lists the fonts once they are stored (until then the list is empty and jellyfin-web draws ASS in its fallback font). The `DeliveryUrl` carries the caller's token as a subtitle's does, where Jellyfin's carries none and its route needs none. A burnt-in ASS or SSA track waits for the same job and is drawn with the film's fonts too (a SubRip or WebVTT burn does not wait on it); if the fonts can't be had for a reason that may pass (the store, a signal, a full disk), the burn fails and is asked for again, because a burn without them would be kept for 24 hours; a film whose attachments ffmpeg rejects is remembered as having none, per film version, and is burnt without them.
- The subtitle HLS playlist (`GET /Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/subtitles.m3u8?segmentLength=N`, for a client whose profile offers the `Hls` subtitle method) and the subtitle route without a start offset (`.../Subtitles/{index}/Stream.{format}`): **done**, as observed on Jellyfin 12.4 (`compat/captures/subtitles-12.4.md`): a VOD playlist with one WebVTT segment per `segmentLength` seconds of the film, each a request for `stream.vtt` with the cues of its window and the caller's token as `ApiKey`. jellyfin-web's profile sends `External` alone and never asks for it. It differs from Jellyfin in answering 404 for a track that does not exist, where Jellyfin answers a playlist.
- File download (`GET /Items/{itemId}/Download`, from POSIX and S3 through the direct-play path, on the `stream` role and the ingress's stream rule; items say `CanDownload` true so jellyfin-web offers it): **done**. `Content-Disposition` carries both `filename` and `filename*`, as Jellyfin does. It differs from Jellyfin in answering HEAD like GET (Jellyfin gives 405) and in taking a token in every form the server reads, including `X-Emby-Token` and `api_key`, which Jellyfin 12.4 refuses with 401 (`DOMAINS.md` invariant 49).
- A transcode job runs at most `transcodeLead` (default 60 s of film) ahead of the furthest segment its viewers have asked for, then its ffmpeg pauses until they come within the lead (DOMAINS 57): **done**, for re-encodes and copies, across replicas, tested with real processes and with a copy from SeaweedFS and Garage paused for 20 s, and in jellyfin-web in Chromium (plays, seeks, switches audio with the bound on). A viewer who leaves after a minute of a 40 Mbps film costs about 1.6 GB of reading instead of 6 GB (computed, assuming the player asks 30 s beyond what it shows; docs/transcoding.md). Jellyfin 12.4 does not bound by default (`EnableThrottling` false).
- Chapters, names and start positions: **done** (DOMAINS 54). The scan reads them from Matroska and MP4 (including QuickTime chapter tracks) in the ffprobe run that lists the streams, and the item DTO carries `Chapters` as Jellyfin 12.4 does: `Name` (`Chapter N` for an untitled one), `StartPositionTicks` on the file's clock. jellyfin-web 12.4 then shows a marker for each on the seek bar and its next and previous chapter buttons seek to them, in direct play and in a transcode (`test/e2e`).
- Chapter images: **done** (DOMAINS 55), opt-in per library (`chapterImages`, off by default as in Jellyfin 12.4; `compat/captures/chapter-images-12.4.md`). A worker takes a frame at each chapter's start after the scan, in a job with slots of its own, keeps it in the asset store, and the item's chapters carry `ImageTag` once it exists; `GET /Items/{id}/Images/Chapter/{n}` serves it from the stream role. jellyfin-web's details page then lists the chapters with their pictures and picking one starts the film there, in direct play and in a transcode (`test/e2e`). With the option off the details page lists none, as Jellyfin's does. Not built: resizing (the pictures are 400 pixels wide, as jellyfin-web asks), `ImagePath`, Jellyfin's split between "during the scan" and a scheduled task.
- `POST /UserItems/{itemId}/UserData`, which writes played state and favourites as one record (Streamyfin's offline sync; a name match, not a trace): **done**. `GET` on the same path is out of scope until something is found that calls it.

### `phase4-ha`

- A worker killed mid-film recovers without losing the picture or resume position, another worker taking its job within seconds: **done**. An API replica killed mid-film: **not built** (the gate above).
- Remote control of another session (play on another device, messages): **not built**. In scope because jellyfin-web offers it (decision).

### `identity`

- User management: create, delete, disable, change a password, preferences: **not built**. In scope.
- Hashed tokens at rest and operator API keys: **not built**. In scope.
- Per-user library access: **not built**. In scope.
- Quick Connect: **not built**. In scope; apps and TVs sign in through it.
- `POST /Users/ForgotPassword`: **not built**. In scope; it answers `Action: ContactAdmin`, because the operator resets passwords (`/Users/ForgotPassword/Pin` is a non-goal).
- OIDC beside local accounts: **not built**. In scope. In jellyfin-web it is a link on the login page, carried by the branding login disclaimer (so `GET /Branding/Configuration` stops being an empty stub); apps and TVs use Quick Connect. Capture real clients before building it (decision).
- Signing the URLs inside playlists, so that they stop carrying the client's own token as `ApiKey` (`DOMAINS.md` invariant 17): **not built**. Known exposure today: anyone who can read a playlist, or a log of the segment requests, holds the viewer's token. In scope.
- Starting on the default subtitle per the viewer's subtitle mode (`SubtitleMode` None, Default, Always, OnlyForced, Smart, and `SubtitleLanguagePreference`): **not built**. In scope. PlaybackInfo reports `DefaultSubtitleStreamIndex` -1 unless the client names a track, as it did for tracks inside the film, and the details page does not carry it, because it needs the user configuration routes that save the mode and the language, which this slice does not have. What Jellyfin 12.4 answers for the mode Default and no language is recorded in `compat/captures/subtitles-default-12.4.md`.
- Sessions and devices view, with revoking a device: **not built**. In scope.
- Activity log: **not built**. In scope.
- User avatars: **not built**. In scope.

### `phase5-library`

- Library scanning from configured libraries with keyframe index and probe: **partial** (done for films and episodes, no metadata providers).
- Genres, studios, persons and their pages and images: **not built**. In scope. The pages list them through `GET /Items` filters; `GET /Persons/{name}` and `GET /Studios/{name}` are non-goals, because nothing calls them.
- Collections and playlists of films and episodes: **not built**. In scope.
- Similar items (`/Items/{id}/Similar` and the movie, show and trailer variants): **not built**. In scope.
- Next up and filters: **not built**. In scope.
- Movie recommendations on the Movies page (`/Movies/Recommendations`; today a 404 hides its rows): **not built**. In scope, with an empty list as the first answer.
- TV Shows Upcoming tab (`/Shows/Upcoming`) and Suggestions (`/Items/Suggestions`): **not built**. In scope (decision).
- Multi-part films (`/Videos/{id}/AdditionalParts`): **not built**. In scope (decision).
- Parental ratings (maximum rating per user, set under `identity` user management): **not built**. In scope here because ratings come from the scan: an item's official rating is read from the file's embedded tags or a sidecar `.nfo`, never fetched. `GET /Localization/ParentalRatings`, which fills the user-edit page's rating list, is built here too. Until then every item is unrated, so a maximum rating filters nothing and `BlockUnratedItems` would hide every film; `identity` therefore answers `POST /Users/{userId}/Policy` with 400 when it sets `MaxParentalRating` or `BlockUnratedItems`, and the control is never presented as working (decision).
- Multiple versions of a film (4K and 1080p files grouped at scan by file name): **not built**. In scope; merging by hand through the API is a non-goal (decision).
- Hardware encoding (NVENC and VAAPI): **not built**. In scope; the encoder family is already part of `spec_hash`, per the plan.
- Empty answers for the music routes global search calls (`GET /Artists`): **done**, as a stub, because music is a non-goal and an empty list is the final answer.
- Media segments and intro skip: **not built**. In scope. Segments come only from data an operator provides or from chapter markers; there is no audio or video fingerprinting (decision).
- Trickplay: **not built**. In scope; workers generate it.
- Artwork and synopses from files beside the film (poster and fanart images, `.nfo`) and from embedded tags and cover art: **not built**. In scope; this is the only metadata source (decision). Fetching from remote providers, and editing items, are non-goals below.

### `phase6-k8s`

- Helm chart, readiness and liveness, Kubernetes deployment: **not built**. The chart is the packaging: no operator and no raw-manifest install (decision).
- Metrics (Prometheus): **not built**. In scope.
- Backup and restore: **not built**. In scope as documented procedure: the operator backs up Postgres and the object stores, so Jellyfin's Backup API is a non-goal (decision).
- Migration from Jellyfin: **not built**. In scope as a one-way import of users, watch state and libraries, so that viewers keep their history. Users arrive without passwords: they set one, or sign in through OIDC (decision).
- Upgrades without downtime (workers replaced before `stream` and `api`, migrations that old replicas survive): **not built**. In scope. This includes the general fix for new spec fields: a worker from before a release that adds one (the tone mapping, copied audio) stores segments made without it under the new hash if it takes such a job, so workers report their `OutputVersion` and the api offers only specs every live worker supports.

### `phase7-syncplay`

- SyncPlay groups, with `GetUtcTime`: **not built**. In scope.

### Non-goals

- Music, photos, audiobooks, channels and Live TV: better tools exist.
- Plugins and the plugin catalogue.
- Client log upload (`POST /ClientLog/Document`): answered 404, so Android TV shows its own "Failed to submit document". Accepting a report nobody can read would tell the viewer it reached the operator, and keeping one could store tokens from a crash log (decision).
- Multi-tenancy.
- Library, metadata and scheduled-task administration through the dashboard, the setup wizard, and API-driven server configuration; these are declarative configuration.
- Forgotten-password flows by email or PIN: the operator resets a password, and `POST /Users/ForgotPassword` says so with `ContactAdmin` (decision).
- Routes Jellyfin marks obsolete, unless a jellyfin-web page calls them through the legacy `ApiClient`: those are aliases of the current route, `needed` in the audit until built and then in `compat/endpoints.yaml`. The `web_caller` column names the call, and `go test ./compat` fails if an obsolete route is `needed` without one. `POST /Users/{userId}` is out of scope: only a library defines `ApiClient.updateUser`, and the page calls the SDK, which posts to `POST /Users`.
- Server administration the operator does outside the API: restart, shutdown and log files (the orchestrator does them), custom device names, branding set through the API (splash screen, custom CSS), and the metadata country and language lists.
- Extras: trailers, theme songs and videos and special features answer empty lists or 404.
- Remote metadata and image providers, remote subtitle download, editing items, uploading images or subtitles and deleting media: an operator supplies metadata as files beside the film (decision).
- Routes no viewer-facing page calls and no client in scope needs, answered 404: `/FallbackFont/*`, `/Items/Counts`, `/Items/Root` (the user-scoped alias is served), `/System/Info/Storage`, `/Search/Hints` (jellyfin-web searches through `GET /Items`), `/Persons/{name}`, `/Studios/{name}`, `/Years` and `/Years/{year}`, `/Items/{itemId}/File` (the download link is `/Items/{itemId}/Download`), likes and ratings (`/UserItems/{itemId}/Rating`), `GET /UserItems/{itemId}/UserData` (decision).
- Kodi is not a gate.

## Feature audit against Jellyfin 12.4

`compat/coverage-12.4.tsv` has one row for each operation in Jellyfin 12.4.0's stable OpenAPI document (`https://repo.jellyfin.org/files/openapi/stable/jellyfin-openapi-12.4.json`, 364 operations: 346 plus 18 HEAD operations the document lists apart), each of the 55 routes in `compat/hidden-routes-12.4.txt`, and two routes outside the document: `/socket`, the WebSocket Jellyfin serves and jellyfin-web opens, and `/System/Configuration/encoding`, the one key of `/System/Configuration/{key}` that clients read, which jellyscale serves by its own route. The document is not kept in the repository; `compat/spec-operations-12.4.txt` lists its operations, one `METHOD path` per line, generated from it. Columns are method, path, class, slice, served_by, web_caller and evidence.

- Class is `done` (implemented or an alias in `compat/endpoints.yaml`), `stub-ok` (an empty answer is the final behaviour), `needed` (a client or feature above needs it) or `out-of-scope` (a non-goal above, or nothing calls it).
- Slice is the slice that delivers a `needed` route, or a `stub-ok` route that is not built yet; otherwise `-`.
- web_caller is `-` (no call found), `sdk` (a name match, see the search rule) or `ApiClient.<name>`, the legacy call a page makes.
- served_by is `-`, or for a `done` row that a different pattern answers, that route as `METHOD path`. The method must be the row's, except that a HEAD row may be served by the GET pattern.

`go test ./compat` fails when: a spec operation or hidden route has no row, or a row is in neither list (so a dropped or mistyped row fails); the file and the API's route list disagree about what is built; a `served_by` names the wrong method or a path that cannot serve the row; a `web_caller` is malformed; an obsolete route is `needed` without an `ApiClient.<name>` web_caller; the parental-ratings route leaves `phase5-library`; a slice has work and no group in Features; or the counts below are wrong.

Routes still to build, by slice (checked against the rows):

- `phase3`: 0 routes to build
- `phase4-ha`: 10 routes to build
- `identity`: 34 routes to build
- `phase5-library`: 46 routes to build
- `phase6-k8s`: 0 routes to build
- `phase7-syncplay`: 23 routes to build

A slice is complete when its count is 0 and its Features group has no **not built** line. Features without a route (HEVC, AV1, tone mapping, image and external subtitles, chapters, hardware encoding, trickplay generation) are not measured by the rows; their status is a claim by hand, to be proved by the integration tests of the slice. `phase6-k8s` has no routes (it is a chart, metrics and migration), so only its Features group counts.

How the evidence was gathered, and its limits:

- Search rule, stated once. A route counts as called by jellyfin-web 12.4.0 (`web-dist`) when its SDK method name appears there as `.name(` (`~N SDK call sites`), or when a page calls the legacy `ApiClient` (`ApiClient.name(`, or `getUrl("Path/...")` followed by `ajax` or `getJSON`), which the SDK count does not see; the row's web_caller names it. A definition inside `node_modules.@jellyfin.sdk` or `node_modules.jellyfin-apiclient` is not a call: only a page chunk calling it counts, so a method nothing calls (`getSearchHints`, `getPerson`, `getStudio`) leaves its route out of scope unless a client in scope matches its name. The SDK count is by method name, so a short or common name (`play`, `getYear`) over-counts; a name that something else in the bundle also uses is a false positive, and the row says so. Either way it says a call exists somewhere in the bundle, often on a dashboard page, not that a viewer reaches it.
- `compat/captures/jellyfin-web-12.4.json` says whether a real session exercised the route.
- The `clients` field is a match of the operation's name in the source of jellyfin-androidtv, Swiftfin and Streamyfin (default branches cloned on 2026-11-02). It is a name match, not a trace of a call, and acceptable as evidence when labelled so; a route no one matches is "none", not proof it is unused.
- A route nothing matched is `out-of-scope` only when its row says what was checked; a reason that is not a Non-goal bullet above is a bug in the row. A route that jellyfin-web calls on a viewer page is `needed`, never out of scope.
- Rows that say `observed on jellyfin/jellyfin:12.4 (empty server)` were requested from a disposable `jellyfin/jellyfin:12.4` container on a Docker-assigned port (2026-11-02). It had no library, so it shows existence and shape, not behaviour with data. Rows that do not say it were not observed.
- Rows in `needed` found only by a client name match carry `phase5-library` as a default slice, and the row says so; a maintainer should confirm those.
