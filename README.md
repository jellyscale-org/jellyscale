# jellyscale

A video server in Go that speaks the Jellyfin client API and runs on more than one machine. Unmodified Jellyfin apps sign in, browse and play, while each part of the server (`api`, `stream`, `worker`) runs as many copies as it needs, coordinated through Postgres. It is for someone who runs a Jellyfin library, has outgrown one process, and wants a lost machine to be invisible to viewers. Video only: films and shows. Not affiliated with the Jellyfin project; [PRODUCT.md](PRODUCT.md) says what it should become.

What it does better than Jellyfin, today:

- **No single worker whose loss stops a film:** a killed worker is replaced within seconds and the picture goes on, and playback continues through a Postgres switchover.
- **A slow transcode is shared** across workers, and reads only as far ahead as the viewer needs.
- **Films on disk or in S3-compatible object storage,** per library.
- **Media and personal data always need a token,** where Jellyfin serves some to anyone ([docs/access.md](docs/access.md)).

## Status

A summary; [docs/status.md](docs/status.md) is the record of every feature.

| Phase | | Status |
|---|---|---|
| 1 | Browse | done |
| 2 | Direct play | done |
| 3 | Transcode | mostly done; not built: encoding to HEVC or AV1, GPU tone mapping |
| 4 | High availability | workers and Postgres done; API replica failover not built |
| — | Identity | partial; user management, Quick Connect and OIDC not built |
| 5 | Library and metadata | partial (scan, chapter images); metadata from files, trickplay, GPU encoders not built |
| 6 | Kubernetes | partial (metrics, published images); manifests, Helm, autoscaling not built |
| 7 | SyncPlay | not built |
| — | Beyond Jellyfin (`/jellyscale/v1/`) | track languages: saved preferences and opt-in hiding of other languages' tracks done; starting in the viewer's language and a settings page coming |

## Deploy

From the repository root, with Docker Compose:

```sh
docker create --name jf jellyfin/jellyfin:12.4
docker cp jf:/jellyfin/jellyfin-web web-dist && docker rm -v jf

MEDIA_DIR=/path/to/media JELLYSCALE_ADMIN_PASSWORD=change-me \
  docker compose -f deploy/compose/compose.yaml up --build
```

`MEDIA_DIR` needs `movies/` and `shows/` folders. Open http://127.0.0.1:8096/web/ and sign in as `admin`. For more than one machine, the roles, the ingress and configuration, see [docs/running.md](docs/running.md).

## Contribute

Read [CONTRIBUTING.md](CONTRIBUTING.md) first: behaviour is observed on a real Jellyfin 12.4, no Jellyfin code is ported, and a bug fix comes with a test that fails without it. `go test ./...` needs only Go; the integration, e2e and differential tiers are in [docs/testing.md](docs/testing.md).

## Documentation

- [Running jellyscale](docs/running.md): roles, the Compose stack, ingress, rolling updates, configuration, metrics.
- [Transcoding](docs/transcoding.md): settings, worker failover, read-ahead, video copy, tone mapping, storage costs.
- [Subtitles](docs/subtitles.md): text and image tracks, files and fonts beside the film.
- [Chapters](docs/chapters.md): chapters and chapter pictures.
- [Access](docs/access.md): who may call each route, and why.
- [jellyscale's own API](docs/api.md): the `/jellyscale/v1/` routes.
- [Status](docs/status.md): every feature, client coverage and the route audit.
- [Testing](docs/testing.md): the test tiers, CI shards, the fixture cache.
- [Testing a client](docs/client-testing.md): the published image with test films, for a client's CI.
- [Architecture decisions](docs/architecture-decisions.md): the superseded first design, kept for history.

## License

GPL-3.0-or-later. Written from observed behavior and public API documentation; no Jellyfin code is ported. jellyfin-web is a separate GPL-2.0 program and isn't included here.
