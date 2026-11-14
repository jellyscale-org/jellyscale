# Status

Where jellyscale stands against [PRODUCT.md](../PRODUCT.md), which says what it should be. This file is the record of what is built, partial and not built, and of the route audit against Jellyfin 12.4; `go test ./compat` checks it against `compat/coverage-12.4.tsv`. The work still to do is in [tasks/prd.json](../tasks/prd.json).

## Roadmap and slices

Subtitles inside the file are delivered or burnt in (done for text tracks, extracted by workers so that `api` and `stream` need no ffmpeg); external subtitle files beside the film (SubRip, ASS, SSA, WebVTT, and PGS and VobSub image files) are done the same way; image tracks inside the film (PGS, VobSub, DVB) are listed as Jellyfin lists them, and PGS is sent as the file to a client that draws it (jellyfin-web with its PGS setting on) and, like VobSub and DVB, drawn into a transcode otherwise, with `api` and `stream` still running no media tools; a run that begins mid-film draws the cue showing at its cut (DOMAINS 51). External `.sup` and `.sub`/`.idx` files are built, and a VobSub pair is drawn into a transcode, never sent as a file, as Jellyfin does for a file. When the client plays a film's video but not its audio or container, the video is copied and the audio is copied too if the client takes its codec, else re-encoded to stereo AAC (done for H.264, HEVC and AV1 in MKV and MP4), so the picture keeps its quality exactly and the operator spends almost no CPU. A copy at Auto carries the file's whole bitrate and size, 4K included, as direct play does, and a viewer on a slow link picks a cap. Its segments are stored at the file's own bitrate, not under the re-encode's ceiling, and a worker opens the file once when it copies both streams and reads it at 5 times the film's speed, so five times the film's bitrate while the job runs and its bytes once for the whole film (twice each when the audio is re-encoded), until the film ends or a minute after the last request, and never more than `transcodeLead` (60 seconds of film by default) ahead of the viewer; a seek waits for a whole segment, up to 12 seconds of film. When the video is re-encoded, the audio is copied if the client's fMP4 HLS profile lists its codec (AAC, MP3, AC3, E-AC3, Opus or FLAC) (a real client has played only AAC, Opus and FLAC; the rest are checked with ffmpeg only), its channels fit the profile's limit and it takes no more than a quarter of the bitrate cap (a deliberate choice: a copy that took more would starve the picture); a copy costs a worker no audio encode and the network and storage of the track's own bitrate (up to 640 kbps for AC3, 1 to 4 Mbps for FLAC) in place of 192 kbps. A restart on another worker puts the same audio packets in the same segments: for an MP4 source the audio in each segment is byte for byte the same, and for a Matroska source the packets are identical but their times can differ by under 1 ms, because Matroska stores whole milliseconds (accepted, not a defect). A missing, non-numeric or out-of-range `TranscodingMaxAudioChannels` is 2. When the client plays both the video and the audio codec but not the container, both are copied (copying both is built, DOMAINS 48 and 52): the picture and sound are the file's own packets and the audio is the track the viewer picked. The scan reads a keyframe index from each H.264 file (a Matroska file's Cues, an MP4's moov, which for a large file is megabytes), and reads where the video ends from the last stretch of the file. Phases and gates are in the project plan. In short: read-only browse (done), direct play (done), transcode (re-encode, subtitles and video copy done), HA, library and metadata, Kubernetes, SyncPlay.

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
- Video copy with audio re-encoded (SDR H.264, HEVC and AV1, in MKV and MP4; HDR10, HDR10+ and HLG HEVC and AV1 for a client that shows the range, else tone mapped): **done**.
- Parallel transcoding: a re-encode running below 1.2x real time gets helper jobs on other workers for the 4-segment chunks ahead of the viewer, and a new viewer who finds every slot taken stops a helper rather than waiting; `transcodeHelpers: false` turns it off (`docs/running.md`): **done** (a burnt image subtitle gets helpers too, as its chunks draw the cue showing; helpers use a fixed threshold, not the cluster's spare capacity).
- Local accounts with bcrypt passwords and per-device tokens; the first administrator comes from configuration: **partial** (no user management, see `identity`).
- Sessions list and playback reporting: **partial** (listing and progress done, no operator view of devices).
- Items' multiple audio tracks: **done**.
- Search by term (jellyfin-web's search page calls `GET /Items` with `searchTerm`): **done**.

### `phase3`

- HEVC and AV1 to clients that play them: **done** as direct play and as a copy into fMP4 HLS (DOMAINS 45), for HEVC Main and Main 10 and AV1 profile 0 at 8 or 10 bits, SDR, when the client's DeviceProfile lists the codec. Anything else is re-encoded to H.264 as before. Dolby Vision with an HDR10, HLG or SDR base layer is copied to a client that plays the base layer (JS-023). HDR10, HDR10+ and HLG are copied to a client whose profile shows the range (JS-025). Not built: encoding to HEVC or AV1. CI's Chrome decodes AV1 but not HEVC, so a browser plays HEVC only on a machine with a hardware decoder (`E2E_HEVC=1`).
- Copying audio while the video is re-encoded: AAC, Opus and FLAC **done** (played and sought in jellyfin-web in Chromium); AC3, E-AC3 and MP3 **partial**: copied for clients whose profile lists them, checked with ffmpeg only. Copying both the video and the audio: **done** (a client that takes the track's codec in fMP4 HLS, within its channel limit and a quarter of the cap; otherwise the audio is re-encoded and the video still copied); played in jellyfin-web in Chromium with AAC, and for AC3, E-AC3, MP3, Opus and FLAC checked with ffmpeg only. A copy of both opens the file once.
- HDR tone mapping to SDR: **done** (DOMAINS 53; jellyfin-web plays HDR10 and HLG HEVC and 10-bit H.264 films through it in Chromium). A re-encoded film in PQ or HLG (HDR10, HLG, and Dolby Vision with an HDR10 or HLG base layer, whose own data is ignored) is tone mapped to SDR BT.709 by a worker in software, with `zscale` and `tonemap` (hable), which every supported ffmpeg has; Dolby Vision profile 5, which has no HDR10 base, is not mapped. An HDR film is never copied to a client that was told the video is SDR, H.264 included. It costs 2.7 times the CPU of an SDR re-encode from a 1080p source and 2.0 times from a 4K one (3.2 cores to keep a 4K source in real time, measured, DOMAINS 53), and a worker's slots do not weigh it. Not built: HDR direct play and copy with `VIDEO-RANGE=PQ` and an SDR fallback variant, GPU tone mapping.
- All text tracks inside a film extracted in one pass (JS-028): **done**. The first request for any text track has one worker run one ffmpeg over the film that writes every text track, and each is kept in the asset store, so a later pick of another track starts no job and reads nothing (on S3, the range proxy shows the film read once). Two viewers on different api processes asking for different tracks of one film at the same moment cause one read. The job is the film's (`subtitle_jobs` key `subtitles/{source}/{etag hash}/text`), leased and taken over after a worker's death as a track job was. Tracks ffmpeg can't extract (it names the streams) are left out, the pass is run again without all of them (two reads of the film in all, however many; tracks the file lacks are dropped first, before ffmpeg reads the film), each run with the whole time limit, and each is marked unreadable in the film's streams, only if the file is still the version the pass read, until the file changes, so it answers 500 on its own and costs no read afterwards; a failure that names no stream (a file ffmpeg can't open, a timeout) is remembered for 30 seconds for all the film's text tracks and marks nothing. PGS tracks (`sup`), fonts and files beside the film keep their own jobs. See `docs/subtitles.md`.
- Image subtitles inside the file (PGS, VobSub, DVB): **done**. They are listed as Jellyfin lists them (`Codec` `PGSSUB`, `DVDSUB`, `DVBSUB`), and any other bitmap codec is not listed. jellyfin-web 12.4 draws PGS itself from the file when its viewer turns on PGS rendering (a setting that is off by default): the track is `External`, a worker copies it out of the film once (the whole film is read once for each PGS track) and `stream` sends it from the asset store. For every other viewer, and for VobSub and DVB always, choosing the track is a transcode with the track drawn into the picture, which a worker's ffmpeg reads from the film in the same pass as the video. A run that begins mid-film (a seek, a failover, a helper's chunk) draws the cue showing at its cut: the scan keeps where the track's packets lie from the Matroska Cues (no read of the whole film), and a run reads the packet of the cue showing and opens the film at its first packet, so a cue's age is paid for in extra reading by a run inside it (DOMAINS 51). What can still miss a showing cue is a film with no cue points for the track (MPEG-TS, a Matroska file written without Cues for the track): a run there opens a 10 second look-back early and an older cue is the one remaining gap. VobSub inside the film is not sent as a file (`.mks`) to the clients that draw it; it is drawn into a transcode (a deliberate difference, DOMAINS 51). Not built: choosing a track by the viewer's subtitle mode (above).
- External subtitle files beside the film (`Film (2024).en.forced.srt`, `.ass`, `.ssa`, `.vtt`, from POSIX and S3): **done**. They are listed after the film's own tracks, in file name order, with the language (ISO 639-3), `Forced`, `Default` and hearing-impaired flags (`forced`, `foreign`, `default`, `cc`, `hi`, `sdh`) and any other word as `Title` that Jellyfin 12.4 gives them, and are delivered and burnt in through the path of an embedded track (`DOMAINS.md` invariant 50). The scan only lists them; a worker converts a file to UTF-8 when it is first asked for (UTF-8, UTF-16, and the legacy sets chardet detects, with the language in the file's name breaking a near-tie; Windows-1250 text and Thai are known to be misread, and the remedy is to convert the file to UTF-8), and a file that none of chardet's results decodes, or that ffmpeg rejects, is hidden with a warning until it changes, once the first request for it has found that out (that viewer gets an error, and a burn-in of it freezes the picture until a seek or reload); a failure that says nothing about the file (ffmpeg killed by a signal, a full disk, the store not answering) leaves it listed to be tried again. A rescan notices a file added, changed or removed without probing the film again; a file keeps its index while it exists and the index of a removed file is never reused, so open sessions are not shifted. It differs from Jellyfin in numbering the film's own streams first, where Jellyfin numbers the external files first and shifts the film's indices, and in reading only `.srt`, `.ass`, `.ssa`, `.vtt`, `.sup` and `.sub` with `.idx` (Jellyfin also takes `.smi`, and lists a `.sub` with no `.idx`). Which track a viewer starts with is not decided by the server (see `identity`).
- External image subtitle files beside the film (`.sup` for PGS, and a `.sub` with its `.idx` for VobSub, from POSIX and S3): **done**, as `compat/captures/external-image-subtitles-12.4.md` observed on Jellyfin 12.4. They are listed in file name order with the text files (a pair at its `.idx`), with the flags from the file name and the canvas from the file; a pair's `Language` is the one inside the `.idx` (`en`, two letters, as Jellyfin lists it), not ISO 639-3. A `.sub` with no `.idx`, an `.idx` with no `.sub` and an `.idx` that holds no `id:` line are not listed and do not stop the scan. The scan reads only these small files (the `.sup`'s first segment and the `.idx`), never the film. jellyfin-web with its PGS setting on gets a `.sup` as `External` and `stream` sends the file itself from the media store (as `Stream.pgssub` or `Stream.sup`, with ranges) with no worker job; every other client, and VobSub always, gets `Encode`, and the worker copies the file (or both files of a pair) to its scratch directory and draws it as a second ffmpeg input, read from its start so a cue showing at a seek is drawn.
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
- One typed media model (JS-086, `internal/media`): **done**: the probe, the scan, the playback decision, the subtitle code, the transcode worker and every answer share one `MediaSource`, `MediaStream` and `MediaAttachment` type, which sends each field `compat/schema-12.4.json` requires and only the others a source has. A source stores the integer version of the probe that wrote it (`media_sources.probe_version`, `library.ProbeVersion`); a scan probes an unchanged file again when its source is older, keeping the item, the subtitle files' indexes and a stored keyframe index. Raise `ProbeVersion` when a release probes more or differently. A probe that fails leaves the source as it was, and the next scan tries again.
- Genres, studios, persons and their pages and images: **not built**. In scope. The pages list them through `GET /Items` filters; `GET /Persons/{name}` and `GET /Studios/{name}` are non-goals, because nothing calls them.
- Collections and playlists of films and episodes: **not built**. In scope.
- Similar items (`/Items/{id}/Similar` and the movie, show and trailer variants): **not built**. In scope.
- Next up and filters: **not built**. In scope.
- Movie recommendations on the Movies page (`/Movies/Recommendations`; today a 404 hides its rows): **not built**. In scope, with an empty list as the first answer.
- TV Shows Upcoming tab (`/Shows/Upcoming`): **not built**. In scope (decision).
- Search hints (`/Search/Hints`, Fluster #102): **done**: one hint for each film, series or episode whose name holds the term, in Jellyfin 12.4's shape (names that start with the term first, episodes ahead of the rest, then by name); `limit` cuts the matches and `TotalRecordCount` counts the cut, as Jellyfin does.
- Suggestions (`/Items/Suggestions`): **partial** (#91): random items of the types asked for, which Findroid's home screen needs; Jellyfin 12.4's own selection is not observed.
- Multi-part films (`/Videos/{id}/AdditionalParts`): **not built**. In scope (decision).
- Parental ratings (maximum rating per user, set under `identity` user management): **not built**. In scope here because ratings come from the scan: an item's official rating is read from the file's embedded tags or a sidecar `.nfo`, never fetched. `GET /Localization/ParentalRatings`, which fills the user-edit page's rating list, is built here too. Until then every item is unrated, so a maximum rating filters nothing and `BlockUnratedItems` would hide every film; `identity` therefore answers `POST /Users/{userId}/Policy` with 400 when it sets `MaxParentalRating` or `BlockUnratedItems`, and the control is never presented as working (decision).
- Multiple versions of a film (4K and 1080p files grouped at scan by file name): **not built**. In scope; merging by hand through the API is a non-goal (decision).
- Hardware encoding (NVENC and VAAPI): **not built**. In scope; the encoder family is already part of `spec_hash`, per the plan.
- Empty answers for the music routes global search calls (`GET /Artists`): **done**, as a stub, because music is a non-goal and an empty list is the final answer.
- Media segments and intro skip: **not built**. In scope. Segments come only from data an operator provides or from chapter markers; there is no audio or video fingerprinting (decision).
- Trickplay: **not built**. In scope; workers generate it.
- Artwork and synopses from files beside the film (poster and fanart images, `.nfo`) and from embedded tags and cover art: **not built**. In scope; this is the only metadata source (decision). Fetching from remote providers, and editing items, are non-goals below.

### `phase6-k8s`

- Kubernetes deployment, as example manifests for `kubectl` and as a Helm chart, with readiness and liveness: **not built**. Both are supported ways to install; there is no operator (decision).
- Metrics (Prometheus): **not built**. In scope.
- Postgres failover (JS-029): **done**. A switchover or restart keeps the transcodes (workers take their lock again and look for their row before stopping anything), segments finished meanwhile are stored when it answers, and stream and api replicas hold a request for the database instead of answering 500 (`GET /healthz` no longer asks it). Direct play and stored segments go on through an outage (JS-085): every stream replica refreshes a snapshot of recently active sessions' tokens and serves them from it while Postgres is away, and everything that needs Postgres to decide waits; `DOMAINS.md` invariants 25 and 37.
- Backup and restore: **not built**. In scope as documented procedure: the operator backs up Postgres and the object stores, so Jellyfin's Backup API is a non-goal (decision).
- Migration from Jellyfin: **not built**. In scope as a one-way import of users, watch state and libraries, so that viewers keep their history. Users arrive without passwords: they set one, or sign in through OIDC (decision).
- Upgrades without downtime (workers replaced before `stream` and `api`, migrations that old replicas survive): **partial**. Every job and every worker records its `OutputVersion`, and a worker claims only jobs of its own version, so an older worker never stores segments made the old way under a newer spec's hash; `docs/running.md` gives the upgrade and rollback order, and a worker from before this release must be stopped before a later output version starts. Not built: a check that a migration is survivable by the previous release's replicas.

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
- Routes no viewer-facing page calls and no client in scope needs, answered 404: `/FallbackFont/*`, `/Items/Counts`, `/Items/Root` (the user-scoped alias is served), `/System/Info/Storage`, `/Persons/{name}`, `/Studios/{name}`, `/Years` and `/Years/{year}`, `/Items/{itemId}/File` (the download link is `/Items/{itemId}/Download`), likes and ratings (`/UserItems/{itemId}/Rating`), `GET /UserItems/{itemId}/UserData` (decision).
- Kodi is not a gate.

## Client coverage

`compat/clients.yaml` lists, for Findroid, Android TV, Swiftfin and Streamyfin and for each of startup, sign-in, home, browse, detail, playback, progress and stop, every request the client sends: method, route, the query names as the client's SDK spells them, the body, the form of the Authorization header, the client's commit and whether the request was read from source (in the client's code, the route taken from the SDK operation it calls) or seen in traffic (on the dogfood server; only Findroid's startup, sign-in, home, detail and playback requests of #44, #47, #91, #94 and #95). The differential tier (`test/differential/run.sh`, part of CI's `e2e` check) sends each one to a running jellyscale with a scanned library and a signed-in user and fails on any answer that is not 2xx, naming the client, the path and the route. Every JSON answer, there and in the four e2e gates, is also held to `compat/schema-12.4.json`, the types and response schemas of Jellyfin 12.4.0's OpenAPI document that `compat/gen-schema.sh` makes from the `jellyfin/jellyfin:12.4` container (the tier fails when the checked-in file differs from what the container it started gives). A field the document requires, whether it lists the field in `required` or leaves it non-nullable with no default, that is absent or null at any depth fails, as does a value of the wrong type, enum, uuid or date-time; a field the document does not list does not. Besides the schema, the tier decodes answers taken from the running jellyscale with jellyfin-sdk-kotlin 1.9.4's own serializers (`test/sdkjvm`, jars pinned by sha256) and asks `@jellyfin/sdk`'s server discovery (`test/sdknode`, pinned) which server it recommends.

What this is evidence of: the routes answer, the answers fit the schema, and the Kotlin SDK decodes the answers it was given, for the requests as the list spells them, which come from source or from traffic as marked. It is not evidence that a client works. No client is called working until it has played on a device; Findroid has been tried on the dogfood and stopped on a missing route or a field its SDK requires (#44, #47, #91, #94, #95), and the other three clients have not been tried. One request all four clients send before playback, `GET /MediaSegments/{itemId}`, answers an empty list until JS-052 finds intros and credits, and is in the list like every other.

Whether every listed request answers and passes the schema check (`yes`), and each route left unserved with its decision (`-` when none). `compat/clients_test.go` fails when this table disagrees with the list.

| Client | Path | Every request answers | Unserved, with the decision |
|---|---|---|---|
| Findroid | startup | yes | - |
| Findroid | sign-in | yes | - |
| Findroid | home | yes | - |
| Findroid | browse | yes | - |
| Findroid | detail | yes | - |
| Findroid | playback | yes | - |
| Findroid | progress | yes | - |
| Findroid | stop | yes | - |
| Android TV | startup | yes | - |
| Android TV | sign-in | yes | - |
| Android TV | home | yes | - |
| Android TV | browse | yes | - |
| Android TV | detail | yes | - |
| Android TV | playback | yes | - |
| Android TV | progress | yes | - |
| Android TV | stop | yes | - |
| Swiftfin | startup | yes | - |
| Swiftfin | sign-in | yes | - |
| Swiftfin | home | yes | - |
| Swiftfin | browse | yes | - |
| Swiftfin | detail | yes | - |
| Swiftfin | playback | yes | - |
| Swiftfin | progress | yes | - |
| Swiftfin | stop | yes | - |
| Streamyfin | startup | yes | - |
| Streamyfin | sign-in | yes | - |
| Streamyfin | home | yes | - |
| Streamyfin | browse | yes | - |
| Streamyfin | detail | yes | - |
| Streamyfin | playback | yes | - |
| Streamyfin | progress | yes | - |
| Streamyfin | stop | yes | - |

## Feature audit against Jellyfin 12.4

`compat/coverage-12.4.tsv` has one row for each operation in Jellyfin 12.4.0's stable OpenAPI document (`https://repo.jellyfin.org/files/openapi/stable/jellyfin-openapi-12.4.json`, 364 operations: 346 plus 18 HEAD operations the document lists apart), each of the 55 routes in `compat/hidden-routes-12.4.txt`, and two routes outside the document: `/socket`, the WebSocket Jellyfin serves and jellyfin-web opens, and `/System/Configuration/encoding`, the one key of `/System/Configuration/{key}` that clients read, which jellyscale serves by its own route. `compat/spec-operations-12.4.txt` lists its operations, one `METHOD path` per line, generated from the document; `compat/schema-12.4.json` keeps its types and response schemas (see Client coverage). Columns are method, path, class, slice, served_by, web_caller and evidence.

- Class is `done` (implemented or an alias in `compat/endpoints.yaml`), `stub-ok` (an empty answer is the final behaviour), `needed` (a client or feature above needs it) or `out-of-scope` (a non-goal above, or nothing calls it).
- Slice is the slice that delivers a `needed` route, or a `stub-ok` route that is not built yet; otherwise `-`.
- web_caller is `-` (no call found), `sdk` (a name match, see the search rule) or `ApiClient.<name>`, the legacy call a page makes.
- served_by is `-`, or for a `done` row that a different pattern answers, that route as `METHOD path`. The method must be the row's, except that a HEAD row may be served by the GET pattern.

`go test ./compat` fails when: a spec operation or hidden route has no row, or a row is in neither list (so a dropped or mistyped row fails); the file and the API's route list disagree about what is built; a `served_by` names the wrong method or a path that cannot serve the row; a `web_caller` is malformed; an obsolete route is `needed` without an `ApiClient.<name>` web_caller; the parental-ratings route leaves `phase5-library`; a slice has work and no group in Features; or the counts below are wrong.

Routes still to build, by slice (checked against the rows):

- `phase3`: 0 routes to build
- `phase4-ha`: 10 routes to build
- `identity`: 34 routes to build
- `phase5-library`: 45 routes to build
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
