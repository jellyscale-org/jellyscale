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

Subtitles inside the file are delivered or burnt in (done for text tracks, extracted by workers so that `api` and `stream` need no ffmpeg); image tracks, external subtitle files and burn-in of images are not. When the client plays a film's video but not its audio or container, the video is copied and only the audio is re-encoded (done for H.264 in MKV and MP4), so the picture keeps its quality exactly and the operator spends almost no CPU; the audio becomes stereo AAC. A copy at Auto carries the file's whole bitrate and size, 4K included, as direct play does, and a viewer on a slow link picks a cap. Its segments are stored at the file's own bitrate, not under the re-encode's ceiling, and a worker opens the file twice and reads it at 5 times the film's speed each time, so ten times the film's bitrate while the job runs and twice its bytes for the whole film, until the film ends or a minute after the last request, with no limit on how far it gets ahead of the viewer; a seek waits for a whole segment, up to 12 seconds of film. Copying audio while the video is re-encoded, and HEVC to clients that play it, are not built. The scan reads a keyframe index from each H.264 file (a Matroska file's Cues, an MP4's moov, which for a large file is megabytes), and reads where the video ends from the last stretch of the file. Phases and gates are in the project plan. In short: read-only browse (done), direct play (done), transcode (re-encode, subtitles and video copy done), HA, library and metadata, Kubernetes, SyncPlay.

Gate for Phase 4 (HA): a worker, and an API replica, killed in the middle of a film recover without losing the viewer's picture or resume position. Until then a worker that dies mid-film freezes jellyfin-web's picture with no dialog, even if a replacement starts at once, because its job holds its lease, which lasts 10 seconds from each renewal (7 to 10 seconds after a kill) (`DOMAINS.md` invariant 37). Only a worker that stops gracefully and is replaced within six seconds goes unnoticed. Likewise, a viewer already playing a video copy whose keyframe index is found wrong freezes with no dialog, possibly long before the bad point because the job reads ahead at 5 times the film's speed; this is accepted until the read-ahead has a limit.
