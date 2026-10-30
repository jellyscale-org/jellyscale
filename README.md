# jellyscale

A cloud-native video server that speaks the Jellyfin client API. Each role scales on its own, and each library can live on a filesystem or S3-compatible storage. Not affiliated with the Jellyfin project.

**Status: Phase 2.** Unmodified jellyfin-web logs in, browses, and direct-plays films from POSIX and S3 libraries, with seeking, resume and Continue Watching. Transcoding isn't built yet, so clients that can't play a file as it is won't play it. For the same reason subtitles and alternate audio tracks aren't offered: playback info lists only the default audio stream and no subtitles. Phase 0 findings are in [`spikes/README.md`](spikes/README.md).

## Scope

Video only. No music, photos, Live TV or .NET plugins. Routes Jellyfin marks obsolete aren't implemented, apart from the aliases flagged `alias` in [`compat/endpoints.yaml`](compat/endpoints.yaml), which jellyfin-web still needs.

## Roles

One binary, chosen with `--roles`:

| Role | Serves |
|---|---|
| `api` | REST API, WebSocket |
| `stream` | Images and direct-play media |
| `worker` | Library scans |

`--roles=all` runs everything in one process. Paths under `/Videos/` and `/Items/*/Images/`, and `/Playback/BitrateTest` (case-insensitive), belong on `stream`, and everything else on `api`. [`deploy/compose/Caddyfile`](deploy/compose/Caddyfile) and `test/e2e/router.mjs` do this.

## Run

```sh
docker create --name jf jellyfin/jellyfin:12.4
docker cp jf:/jellyfin/jellyfin-web web-dist && docker rm jf

MEDIA_DIR=/path/to/media JELLYSCALE_ADMIN_PASSWORD=change-me \
  docker compose -f deploy/compose/compose.yaml up --build
```

`MEDIA_DIR` needs `movies/` and `shows/` folders. Open http://127.0.0.1:8096/web/ and sign in as `admin`. The stack's Caddy ingress sends media and images to `stream` and the rest to `api`. Libraries on S3 need `JELLYSCALE_S3_ACCESS_KEY` and `JELLYSCALE_S3_SECRET_KEY` exported on the host; compose passes them to every service. Add one to `deploy/compose/config.json` such as `{"name": "Cloud", "kind": "movies", "media": "s3://bucket/movies?endpoint=https://s3.example.com", "assets": "s3://bucket/assets?endpoint=https://s3.example.com"}`. For a single process, run `--roles=all`.

## Test

```sh
go test ./...                               # Unit tests
test/integration.sh                         # Adds disposable Postgres, SeaweedFS, Garage
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web, two api replicas, S3, a router
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

## License

GPL-3.0-or-later. Written from observed behavior and public API documentation; no Jellyfin code is ported. jellyfin-web is a separate GPL-2.0 program and isn't included here.
