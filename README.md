# jellyscale

A cloud-native video server that speaks the Jellyfin client API. Each role scales on its own, and each library can live on a filesystem or S3-compatible storage. Not affiliated with the Jellyfin project.

**Status: Phase 3, second slice.** Unmodified jellyfin-web logs in, browses, and plays films from POSIX and S3 libraries, with seeking, resume and Continue Watching. A film the browser can't play as it is, or that exceeds the quality menu's cap, is transcoded to HLS (H.264 and AAC in fragmented MP4, always re-encoded) by a worker, with seek, audio track switching and the stop paths working. Text subtitles inside the file (SubRip, ASS, SSA, WebVTT, mov_text) show in jellyfin-web, as the client's own text or burnt into a transcode. Not built yet: image subtitles (PGS, VobSub, DVB: a file lists none of them), external subtitle files, video copy and remux, hardware encoders, HDR tone mapping and recovery when a worker dies mid-film (Phase 4; see Transcoding). Phase 0 findings are in [`spikes/README.md`](spikes/README.md).

## Scope

Video only. No music, photos, Live TV or .NET plugins. Routes Jellyfin marks obsolete aren't implemented, apart from the aliases flagged `alias` in [`compat/endpoints.yaml`](compat/endpoints.yaml), which jellyfin-web still needs.

## Roles

One binary, chosen with `--roles`:

| Role | Serves |
|---|---|
| `api` | REST API, WebSocket |
| `stream` | Images, direct-play media, HLS playlists and segments, subtitles |
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

## Subtitles

A text track in the file is listed with the film and served on the `stream` role as `GET /Videos/{id}/{source}/Subtitles/{index}/{startPositionTicks}/Stream.{vtt,srt,js,ass}`, with the token in the query or a header, as other media. jellyfin-web fetches `js` for SubRip and `ass` for ASS, and `GET /System/Configuration/encoding` before it draws ASS. A non-zero start moves every cue earlier by that much, to zero. Picking another track makes no new PlaybackInfo, no ffmpeg job and no playlist change.

The first request for a track reads the whole file with ffmpeg (the `stream` role needs it, as the image has) and keeps the result in the library's asset store, under `subtitles/`, so later requests and other replicas read the cache; concurrent first requests run one ffmpeg, within a process by sharing one extraction (at most four run at once, a request waiting on another replica not counted) and across replicas through a Postgres advisory lock. A changed file gets a new key and fresh text; the old object is not removed. On a big file on S3 the first request takes as long as reading the file does, and a read is given up after an hour. The lock is held on its own Postgres connection, so slow extractions never use up the pool. A track that can't be read answers 500 and is logged.

Limits to know:

- The first read of a track on a big file takes as long as reading the file.
- A proxy or ingress in front needs a read timeout longer than that, or the viewer gets no subtitles until re-picking the track.
- The first burn-in of a track shows no picture for that time.
- A burn-in track that can't be read refuses the film with the "cannot be played" dialog on every attempt while that track is chosen, while the same film plays with that subtitle off.
- Only the `stream` role and the worker need ffmpeg, and both do.
- A host with no fonts draws no burnt text.
- A text track positioned after an unlisted image or audio track loses automatic selection, and can switch off on a quality or audio change, until image subtitles are delivered.
- Cached subtitle objects of a replaced file are not deleted.
- After a client-rendered subtitle is picked in a transcode, switching audio leaves the track disabled with no cues until it is picked again; Jellyfin 12.4 does the same (`compat/captures/transcode-12.4.md`), so it is a jellyfin-web limitation.

A viewer who sets jellyfin-web's burn subtitles to "all" gets the chosen track drawn into the picture instead: the client sends no subtitle profile, the track is marked `Encode`, and a worker re-encodes with the text. "Always burn in subtitle when transcoding" does the same for a film that is transcoded anyway. Changing the track replaces the job, as an audio switch does, and a seek or restart draws the same text at the same time. When the server can't transcode, burn-in has nothing to offer and no track is listed. Deployments need a font: the image has `font-dejavu`, which libass falls back to when a style names one that isn't there.

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
