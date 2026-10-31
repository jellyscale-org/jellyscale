# jellyscale

A cloud-native video server that speaks the Jellyfin client API. Each role scales on its own, and each library can live on a filesystem or S3-compatible storage. Not affiliated with the Jellyfin project.

**Status: Phase 3, first slice.** Unmodified jellyfin-web logs in, browses, and plays films from POSIX and S3 libraries, with seeking, resume and Continue Watching. A film the browser can't play as it is, or that exceeds the quality menu's cap, is transcoded to HLS (H.264 and AAC in fragmented MP4, always re-encoded) by a worker, with seek, audio track switching and the stop paths working. Not built yet: subtitles (playback info lists none), video copy and remux, hardware encoders, HDR tone mapping and recovery when a worker dies mid-film (Phase 4; see Transcoding). Phase 0 findings are in [`spikes/README.md`](spikes/README.md).

## Scope

Video only. No music, photos, Live TV or .NET plugins. Routes Jellyfin marks obsolete aren't implemented, apart from the aliases flagged `alias` in [`compat/endpoints.yaml`](compat/endpoints.yaml), which jellyfin-web still needs.

## Roles

One binary, chosen with `--roles`:

| Role | Serves |
|---|---|
| `api` | REST API, WebSocket |
| `stream` | Images, direct-play media, HLS playlists and segments |
| `worker` | Library scans, ffmpeg |

`--roles=all` runs everything in one process. Paths under `/Videos/` and `/Items/*/Images/`, and `/Playback/BitrateTest` (case-insensitive), belong on `stream`, and everything else on `api`. [`deploy/compose/Caddyfile`](deploy/compose/Caddyfile) and `test/e2e/router.mjs` do this.

## Transcoding

Add `transcodeStore` to the config, the same for every role. With none anywhere, nothing transcodes and every source plays as it is. Every process records its roles and `transcodeStore` in the database with a heartbeat, and a replica compares them: when they differ, or one process has none, an `api` replica refuses the transcode at PlaybackInfo and its log names both. A spec that already has a live job is offered anyway, by any replica, and its viewer joins it; the log warns and names both stores, and if the replica that serves the segments has no `transcodeStore`, it answers 503 and jellyfin-web asks PlaybackInfo again, which offers the live job again, so before playback the viewer retries without a dialog until the job ends, and after playback has begun the picture freezes; if it has a store it can't read the segments in, the request times out. A store URI that carries credentials (a user or password, or a parameter named like a key, secret, access, token, pass, sig or credential) is refused at startup; credentials come from the environment. A process with only the `worker` role and no store does not start. Set it, to the same value, on all of them:

| Key | Meaning | Default |
|---|---|---|
| `transcodeStore` | Store URI for segments: `file:///var/lib/jellyscale/segments`, or `s3://bucket/segments?endpoint=...` with credentials from the environment | none |
| `transcodeScratch` | Where a worker's ffmpeg writes before uploading. Workers on one host may share it; each sweep removes job directories untouched for two minutes, which a crash leaves behind | `$TMPDIR/jellyscale-transcode` |
| `transcodeSlots` | Concurrent ffmpeg processes per worker; a negative number is refused at startup | 2 |
| `transcodeIdle` | How long without a request before a spec's ffmpeg stops. Closing a tab sends no stop | 60s |
| `transcodeRetention` | How long a spec's segments stay after its last request | 24h |

`stream` replicas and workers share the segments only through that store and Postgres, so the store must be reachable from both: a shared volume, or S3. The worker needs `ffmpeg` and `ffprobe` on its path, as the image has. Segments are 4 seconds, and a restart after a seek splices with the segments before it.

A viewer whose transcode can't start (no worker running, every slot busy, or the same one just failed) gets a "cannot be played at this time" dialog from jellyfin-web instead of waiting, answered within seconds; the server log says which. A segment that fails before playback has begun ends in that dialog too, because jellyfin-web asks PlaybackInfo again and is refused (unless the spec has a live job, which is offered again). After playback has begun, a failed segment makes jellyfin-web drop the stream: the picture freezes, with no dialog. A worker that stops gracefully and is replaced within six seconds is not noticed, because a request for a segment waits that long for a worker (`test/roles.sh` does this with real processes). A worker that is killed is another matter: its job keeps its lease, which lasts 10 seconds from each renewal (7 to 10 seconds after the kill, longer if ffmpeg had only just started), before a request can restart it, and one measurement answered a waiting segment after 12.7 seconds, past the player's 10, so the picture freezes whether or not a replacement is quick. A worker that is killed, or not replaced in time, freezes the picture, and recovering from that without losing the picture or the resume position is Phase 4. A viewer on a film whose default track plays as it is, who picks another audio track while no slot is free, keeps playing the default track. Interlaced H.264 is deinterlaced. Output is H.264 at no more than 1080 lines, re-encoded in software with `veryfast`. A worker that falls behind makes the player stall, and there is no lower-quality fallback yet. A job whose ffmpeg makes no segment for 30 seconds is failed, so a hung encode ends in a dialog and not a spinner when it hangs before playback begins; after playback has begun the failed segment freezes the picture as above. A source scanned before the scanner recorded level, field order and range plays directly where its conditions allow it, and is probed again at the next scan, which it does without the file changing.

Storage: segments are kept for `transcodeRetention` (24 hours) after a spec's last request, one set per spec, so a film watched in full at a quality cap of *B* bits per second takes up to *B* × 3600 / 8 bytes per hour of film: about 0.45 GB per hour for each megabit of the cap. The encoder also caps video at 6 × width × height bits per second, and the width follows the source's shape: at 1080 lines a 16:9 spec stays under about 12.63 Mbps (about 5.7 GB per hour) and a 2.39:1 one, 2578 wide, under about 16.9 Mbps (about 7.6 GB per hour); at 720 lines 16:9 stays under about 5.72 Mbps (about 2.6 GB per hour). These are ceilings with the 192 kbps audio included; how far real film at crf 23 falls below them has not been measured. Each distinct audio track or quality is its own spec. Workers also hold a job's unuploaded segments in `transcodeScratch`, a few at a time. The end-to-end suite's working directory peaks at about 450 MB while it runs (about 330 MB when it ends). It holds two copies of the 127 MB 180 second 720p film: the scenario that rewrites one needs its own, and the rest share the other as hard links (copies would add about 890 MB). The SeaweedFS container holds one more copy of the film and the segments of the S3 plays, and goes with the container.

## Run

```sh
docker create --name jf jellyfin/jellyfin:12.4
docker cp jf:/jellyfin/jellyfin-web web-dist && docker rm jf

MEDIA_DIR=/path/to/media JELLYSCALE_ADMIN_PASSWORD=change-me \
  docker compose -f deploy/compose/compose.yaml up --build
```

`MEDIA_DIR` needs `movies/` and `shows/` folders. Segments are kept in the `transcode` volume. Open http://127.0.0.1:8096/web/ and sign in as `admin`. The stack's Caddy ingress sends media and images to `stream` and the rest to `api`. Libraries on S3 need `JELLYSCALE_S3_ACCESS_KEY` and `JELLYSCALE_S3_SECRET_KEY` exported on the host; compose passes them to every service. Add one to `deploy/compose/config.json` such as `{"name": "Cloud", "kind": "movies", "media": "s3://bucket/movies?endpoint=https://s3.example.com", "assets": "s3://bucket/assets?endpoint=https://s3.example.com"}`. For a single process, run `--roles=all`.

## Test

```sh
go test ./...                               # Unit tests
test/integration.sh                         # Adds disposable Postgres, SeaweedFS, Garage
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web, two api replicas, S3, a router, a worker
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

`test/e2e` needs `web-dist` (see Run), and a directory with `playwright-core` installed. It drives the browser at `$CHROMIUM_PATH`, `/usr/bin/chromium` if unset. Use Google Chrome (`CHROMIUM_PATH=/usr/bin/google-chrome-stable`) where Chromium is built without H.264 and AAC, which every fixture needs. `E2E_GATES="transcode"` runs one gate.

## License

GPL-3.0-or-later. Written from observed behavior and public API documentation; no Jellyfin code is ported. jellyfin-web is a separate GPL-2.0 program and isn't included here.
