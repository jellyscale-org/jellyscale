# jellyscale

A video server that speaks the Jellyfin client API and scales out. Unmodified Jellyfin clients log in, browse and play (jellyfin-web is tested on every change), while each part of the server runs as many copies as it needs. Not affiliated with the Jellyfin project.

## Why

Jellyfin is one process: it scans, serves, transcodes and holds the sessions. That is simple, but it means one machine does everything, and when it is busy or down, so is everyone's film. jellyscale keeps Jellyfin's clients and splits the server into three roles that scale on their own, all in one binary (`--roles`): `api` answers the REST API and WebSockets, `stream` serves media, segments and images, and `worker` scans libraries and transcodes. So:

- **More viewers:** add `stream` replicas. They keep no state of their own (it is in Postgres and the stores).
- **More transcodes:** add workers, on any machine with CPU to spare. When a worker is killed mid-film, another worker with a free slot takes over within seconds and the viewer's picture goes on (the API replica half of failover is not built yet; see Status).
- **Storage where you have it:** each library lives on a filesystem or on S3-compatible object storage.
- **One thing to keep safe:** Postgres is the only service jellyscale needs besides its storage. Replicas and workers coordinate through it, and the video itself never passes through it.

What makes failover cheap is that the video and timing of transcoded segments are deterministic: segment *n* of a film at a given quality has the same picture and cuts whichever worker makes it, so a stream started by one worker can be continued by another and the pieces splice. The design and its trade-offs are in [ARCHITECTURE.md](ARCHITECTURE.md), the rules it keeps in [DOMAINS.md](DOMAINS.md), and what is in and out of scope in [PRODUCT.md](PRODUCT.md).

## Status

Video only: films and shows. No music, photos, Live TV or .NET plugins. Routes Jellyfin marks obsolete aren't implemented, apart from the aliases flagged `alias` in [`compat/endpoints.yaml`](compat/endpoints.yaml), which jellyfin-web still needs.

| Phase | | Status |
|---|---|---|
| 1 | Browse: login, libraries, images, jellyfin-web | done |
| 2 | Direct play from POSIX and S3, resume, Continue Watching | done |
| 3 | Transcode: HLS re-encode and video copy, audio switching, subtitles (text and image tracks, burnt in or delivered), tone mapping, chapters | mostly done |
| 4 | High availability: a worker killed mid-film is replaced without losing the picture | worker half done; API replica failover not built |
| — | Identity: user management, Quick Connect, OIDC | not built |
| 5 | Library and metadata, trickplay, GPU encoders | not built |
| 6 | Kubernetes: Helm chart, autoscaling, metrics | not built |
| 7 | SyncPlay | not built |

[PRODUCT.md](PRODUCT.md) lists every feature with its status. Phase 0's findings, which the design rests on, are in [spikes/README.md](spikes/README.md).

## Quick start

```sh
docker create --name jf jellyfin/jellyfin:12.4
docker cp jf:/jellyfin/jellyfin-web web-dist && docker rm -v jf

MEDIA_DIR=/path/to/media JELLYSCALE_ADMIN_PASSWORD=change-me \
  docker compose -f deploy/compose/compose.yaml up --build
```

`MEDIA_DIR` needs `movies/` and `shows/` folders. Open http://127.0.0.1:8096/web/ and sign in as `admin`.

## Documentation

- [Running jellyscale](docs/running.md): the roles, the Compose stack, configuration.
- [Transcoding](docs/transcoding.md): settings, worker failover, read-ahead, video copy, tone mapping, storage costs, upgrade order.
- [Subtitles](docs/subtitles.md): text and image tracks, files and fonts beside the film, known limits.
- [Chapters](docs/chapters.md): chapters and chapter pictures.
- [Testing](docs/testing.md): the test tiers, CI shards, the fixture cache, flake hunting.
- [Architecture decisions](docs/architecture-decisions.md).
- [Contributing](CONTRIBUTING.md).

## License

GPL-3.0-or-later. Written from observed behavior and public API documentation; no Jellyfin code is ported. jellyfin-web is a separate GPL-2.0 program and isn't included here.
