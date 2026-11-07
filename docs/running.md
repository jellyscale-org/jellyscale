# Running jellyscale

## Roles

One binary, chosen with `--roles`:

| Role | Serves |
|---|---|
| `api` | REST API, WebSocket |
| `stream` | Images, direct-play media, file downloads, HLS playlists and segments, subtitles |
| `worker` | Library scans, transcodes, subtitle extraction |

`--roles=all` runs everything in one process. Paths under `/Videos/`, `/Items/*/Images/` and `/Items/*/Download`, and `/Playback/BitrateTest` (case-insensitive), belong on `stream`, and everything else on `api`. [`deploy/compose/Caddyfile`](../deploy/compose/Caddyfile) and `test/e2e/router.mjs` do this.

## Run

```sh
docker create --name jf jellyfin/jellyfin:12.4
docker cp jf:/jellyfin/jellyfin-web web-dist && docker rm -v jf

MEDIA_DIR=/path/to/media JELLYSCALE_ADMIN_PASSWORD=change-me \
  docker compose -f deploy/compose/compose.yaml up --build
```

`MEDIA_DIR` needs `movies/` and `shows/` folders. Segments are kept in the `transcode` volume. Open http://127.0.0.1:8096/web/ and sign in as `admin`. The stack's Caddy ingress sends media and images to `stream` and the rest to `api`. Libraries on S3 need `JELLYSCALE_S3_ACCESS_KEY` and `JELLYSCALE_S3_SECRET_KEY` exported on the host; compose passes them to every service. Add one to `deploy/compose/config.json` such as `{"name": "Cloud", "kind": "movies", "media": "s3://bucket/movies?endpoint=https://s3.example.com", "assets": "s3://bucket/assets?endpoint=https://s3.example.com"}`. For a single process, run `--roles=all`.

## Configuration

A key the server does not know, at the top of the config or in a library, is refused at startup with its name, so a misspelt `chapterImages` can't silently leave the option off; so is anything after the config. Upgrading: a config that loaded before because the server ignored a stray key now stops the start, naming the key to remove.

A library whose POSIX media folder is missing fails its scan with an error naming the folder, and so does one whose media store lists no video at all while the library has films (an empty mount point, a volume unmounted under a folder that is still there, or an empty bucket): the films and their watch state are kept, the log says why, and the next scan after the volume is back carries on as before. The cost is that a library emptied on purpose keeps its last films listed until a video is in it again.
