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
- **Outside scope but called anyway:** answer with a valid empty response if the client breaks otherwise; otherwise 404.
- **Kodi** isn't a stated target. It works if it happens to, and no work goes in solely for it.

## Quality bar

- Behavior is observed from a real Jellyfin, not guessed. No code is ported from Jellyfin.
- A claim of working means a real client or a real store exercised it.
- Failure modes are designed, tested and reported. Nothing fails silently.
- Code is simple, readable and maintainable by people. The simplest thing that works wins.

## Roadmap

Phases and gates are in the project plan. In short: read-only browse (done), direct play (done), transcode, HA, library and metadata, Kubernetes, SyncPlay.
