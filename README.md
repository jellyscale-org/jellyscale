# jellyscale

A cloud-native video server that speaks the Jellyfin client API. Each role scales on its own, and each library can live on a filesystem or S3-compatible storage. Not affiliated with the Jellyfin project.

**Status: Phase 3, third slice.** Unmodified jellyfin-web logs in, browses, and plays films from POSIX and S3 libraries, with seeking, resume and Continue Watching. A film the browser can't play as it is, or that exceeds the quality menu's cap, is transcoded to HLS (H.264 and AAC in fragmented MP4) by a worker, which copies the video instead of re-encoding it when the client plays the video but not the audio or the container (H.264 with AC3 in MKV, say), with seek, audio track switching and the stop paths working. Text subtitles inside the file (SubRip, ASS, SSA, WebVTT, mov_text) show in jellyfin-web, as the client's own text or burnt into a transcode. Not built yet: image subtitles (PGS, VobSub and DVB tracks are not listed), external subtitle files, copying HEVC for clients that play it, copying the audio while the video is re-encoded, hardware encoders, HDR tone mapping, and recovery when a worker dies mid-film (Phase 4; see Transcoding). Phase 0 findings are in [`spikes/README.md`](spikes/README.md).

## Scope

Video only. No music, photos, Live TV or .NET plugins. Routes Jellyfin marks obsolete aren't implemented, apart from the aliases flagged `alias` in [`compat/endpoints.yaml`](compat/endpoints.yaml), which jellyfin-web still needs.

## Roles

One binary, chosen with `--roles`:

| Role | Serves |
|---|---|
| `api` | REST API, WebSocket |
| `stream` | Images, direct-play media, file downloads, HLS playlists and segments, subtitles |
| `worker` | Library scans, transcodes, subtitle extraction |

`--roles=all` runs everything in one process. Paths under `/Videos/`, `/Items/*/Images/` and `/Items/*/Download`, and `/Playback/BitrateTest` (case-insensitive), belong on `stream`, and everything else on `api`. [`deploy/compose/Caddyfile`](deploy/compose/Caddyfile) and `test/e2e/router.mjs` do this.

## Transcoding

Add `transcodeStore` to the config, the same for every role. With none anywhere, nothing transcodes and every source plays as it is. Every process records its roles and `transcodeStore` in the database with a heartbeat, and a replica compares them: when they differ, or one process has none (a worker-only process with none is left out, since it only extracts subtitles), an `api` replica refuses the transcode at PlaybackInfo and its log names both. A spec that already has a live job is offered anyway, by any replica, and its viewer joins it; the log warns and names both stores, and if the replica that serves the segments has no `transcodeStore`, it answers 503 and jellyfin-web asks PlaybackInfo again, which offers the live job again, so before playback the viewer retries without a dialog until the job ends, and after playback has begun the picture freezes; if it has a store it can't read the segments in, the request times out. A store URI that carries credentials (a user or password, or a parameter named like a key, secret, access, token, pass, sig or credential) is refused at startup; credentials come from the environment. A process with only the `worker` role and no store starts and extracts subtitles, but runs no transcodes. Set it, to the same value, on all of them:

| Key | Meaning | Default |
|---|---|---|
| `transcodeStore` | Store URI for segments: `file:///var/lib/jellyscale/segments`, or `s3://bucket/segments?endpoint=...` with credentials from the environment | none |
| `transcodeScratch` | Where a worker's ffmpeg writes before uploading. Workers on one host may share it; each sweep removes job directories untouched for two minutes, which a crash leaves behind | `$TMPDIR/jellyscale-transcode` |
| `transcodeSlots` | Concurrent transcodes per worker, apart from up to four subtitle extractions; a negative number is refused at startup | 2 |
| `transcodeIdle` | How long without a request before a spec's ffmpeg stops. Closing a tab sends no stop | 60s |
| `transcodeRetention` | How long a spec's segments stay after its last request | 24h |

`stream` replicas and workers share the segments only through that store and Postgres, so the store must be reachable from both: a shared volume, or S3. The worker needs `ffmpeg` and `ffprobe` on its path, as the image has; `api` and `stream` need neither. Segments of a re-encode are 4 seconds, and a restart after a seek splices with the segments before it.

A viewer whose transcode can't start (no worker running, every slot busy, or the same one just failed) gets a "cannot be played at this time" dialog from jellyfin-web instead of waiting, answered within seconds; the server log says which. A segment that fails before playback has begun ends in that dialog too, because jellyfin-web asks PlaybackInfo again and is refused. There are two exceptions: a spec with a live job, which is offered again, and a copy whose keyframe index was found wrong, which is dropped so that the next PlaybackInfo re-encodes the film with no dialog. After playback has begun, a failed segment makes jellyfin-web drop the stream: the picture freezes, with no dialog. A worker that stops gracefully and is replaced within six seconds is not noticed, because a request for a segment waits that long for a worker (`test/roles.sh` does this with real processes). A worker that is killed is another matter: its job keeps its lease, which lasts 10 seconds from each renewal (7 to 10 seconds after the kill, longer if ffmpeg had only just started), before a request can restart it, and one measurement answered a waiting segment after 12.7 seconds, past the player's 10, so the picture freezes whether or not a replacement is quick. A worker that is killed, or not replaced in time, freezes the picture, and recovering from that without losing the picture or the resume position is Phase 4. A viewer on a film whose default track plays as it is, who picks another audio track while no slot is free, keeps playing the default track. In a re-encode, interlaced H.264 is deinterlaced, and the output is H.264 at no more than 1080 lines, in software with `veryfast`; a copy does neither (see Video copy). A worker that falls behind makes the player stall, and there is no lower-quality fallback yet. A job whose ffmpeg makes no segment for 30 seconds is failed, so a hung encode ends in a dialog and not a spinner when it hangs before playback begins; after playback has begun the failed segment freezes the picture as above. A source scanned before the scanner recorded level, field order and range plays directly where its conditions allow it, and is probed again at the next scan, which it does without the file changing.

Storage: segments are kept for `transcodeRetention` (24 hours) after a spec's last request, one set per spec, so a film watched in full at a quality cap of *B* bits per second takes up to *B* × 3600 / 8 bytes per hour of film: about 0.45 GB per hour for each megabit of the cap. The encoder also caps video at 6 × width × height bits per second, and the width follows the source's shape: at 1080 lines a 16:9 spec stays under about 12.63 Mbps (about 5.7 GB per hour) and a 2.39:1 one, 2578 wide, under about 16.9 Mbps (about 7.6 GB per hour); at 720 lines 16:9 stays under about 5.72 Mbps (about 2.6 GB per hour). These are ceilings for a re-encode, with the 192 kbps audio included; none of them applies to a copied stream (see Video copy); how far real film at crf 23 falls below them has not been measured. Each distinct audio track or quality is its own spec. Workers also hold a job's unuploaded segments in `transcodeScratch`, a few at a time.

### Video copy

When the video can play in the client as it is and only the audio or the container can't (H.264 with AC3 in MKV, or in MP4), the worker copies the video and re-encodes the audio to AAC. A stream then costs almost no CPU and keeps the source's picture exactly. Nothing to configure. It happens when:

- the video is H.264, and passes the client's `CodecProfiles` for it (profile, level, range, interlacing, resolution);
- no bitrate cap is below the file's bitrate, and no subtitle is to be burnt in;
- the file has a keyframe index, which the scan reads from the file's own index (Matroska Cues, or the MP4 sample tables) and keeps in the database. A Matroska file written without Cues, a fragmented MP4 and any other container are re-encoded, as before;
- no gap between keyframes is longer than 12 seconds, counting the stretch from the last keyframe to the end of the film. x264 and x265 default to a keyframe every 250 frames, which is under 12 seconds at 21 fps and up. A file with longer gaps is re-encoded.

The playlist is cut on the file's keyframes: a segment ends at the first keyframe at least 4 seconds after it began, or earlier if waiting would make it longer than 12 seconds, so no segment is over 12 seconds, and the playlist says their exact lengths.

A copy is not capped, and at Auto it carries the file's whole bitrate and size, 4K included, exactly as direct play of the same file does and as Jellyfin's copy does. A viewer on a slow link picks a cap in the quality menu, which re-encodes. A copy stores and sends the picture at the file's own bitrate: a 40 Mbps remux takes about 18 GB per hour (computed) of film for each spec, kept for 24 hours, against the 5.7 GB per hour at most of a 1080p re-encode. None of the storage ceilings above applies to it.

Reading the source: a copy opens the file twice, once for the video and once for the audio, and reads each at 5 times the film's speed (`-readrate 5`), so while the job runs it reads ten times the film's bitrate, and a run to the end reads twice the film's bytes. That keeps it from reading the film as fast as the disk allows, but nothing limits how far it gets ahead of the viewer. Every request for a copied stream's playlists and segments reads the film's keyframe array (about 14 KB computed for a 2 hour film with a keyframe every 4 seconds, 8 bytes each) from Postgres; the request rate has not been measured. The job continues until the film ends or 60 seconds after the last request. For a 2 hour film at 40 Mbps (36 GB), a run to the end reads 72 GB from the source (72 GB of S3 egress), stores 36 GB of segments for 24 hours and takes about 24 minutes. A viewer who leaves after a minute still costs the minute watched and the 60 seconds of idle time, 120 seconds of the job running at ten times the film's bitrate: 6 GB at 40 Mbps. The byte figures here are computed from the bitrate and the length, not measured. They assume the store takes uploads at 5 times the film's speed (200 Mbps for a 40 Mbps film), and ffmpeg is not paced by uploads: unuploaded segments sit in `transcodeScratch` a few at a time if the store keeps up, and pile up, up to the whole film (36 GB), if it does not. A seek waits for the whole segment it lands in, up to 12 seconds of film (60 MB at 40 Mbps), read at 5 times the film's speed and downloaded at the file's bitrate.

The audio becomes stereo AAC (192 kbps by default), so only the picture is unchanged.

The scan builds the index, and a few things have to be known about it. A film scanned before this version has no index until the next scan reads it (a scan does so for every H.264 file in MKV or MP4 that has none), so it is re-encoded until then. For a Matroska file the index is the Cues (the test bounds the read to 2% of a small test file); for an MP4 it is the moov, which has an entry for every frame and audio packet, so a large MP4's is megabytes. The scan also reads where the video ends, from the packets of the last stretch from the last keyframe to the end of the file, when that stretch is within 12 seconds of the file's duration (a film with a longer gap can't be copied and is not read): up to 12 seconds of film, read up to twice (120 MB at 40 Mbps), plus what ffmpeg reads to open the file, a few MB that vary with its version. If the index turns out not to match the file (a segment doesn't start or end on a keyframe), the job fails, the file's index is dropped and an error logged with the item and source, and the next PlaybackInfo re-encodes the film (jellyfin-web asks PlaybackInfo again when a segment fails before playback has begun; after it has begun the picture freezes until the viewer restarts). A dropped index stays empty until the file changes; to have the scan read it again, set `keyframes` to NULL on that `media_sources` row. An input cut short (a file truncated in the store) fails the job but keeps the index, unless the cut is within 1.5 seconds of the end of the film, inside the last segment: that job succeeds and its last segment is that much short. The scan logs an unusable index at Info with the reason, and a read that failed at Warn, to be tried again at the next scan; the other reasons a film is re-encoded are in the `TranscodeReasons` of the PlaybackInfo reply. A film whose audio outlasts its video enough to fail the 12 second check against the file's duration is re-encoded.

Upgrading from a version without video copy, roll in this order: workers, then `stream` replicas, then `api` replicas. The API is what offers a copy, so it goes last. A worker from before this release that claimed a copy job would not know it is a copy, and would re-encode the picture and store it under the copy's hash; once every worker is new, none can.

After this release, existing H.264 films whose audio the client can't play (AC3 in MKV, say) become full-bitrate copies once a scan has read their index, where they were re-encoded. A viewer on such a film gets the source's picture, and the storage and bandwidth change with it: a copy stores and sends the file's own bitrate (see Storage above and the figures under Video copy), where a re-encode is at most 1080p.

## Subtitles

A text track in the file is listed with the film and served on the `stream` role as `GET /Videos/{id}/{source}/Subtitles/{index}/{startPositionTicks}/Stream.{vtt,srt,js,ass}`, with the token in the query or a header, as other media. jellyfin-web fetches `js` for SubRip and `ass` for ASS, and `GET /System/Configuration/encoding` before it draws ASS. A non-zero start moves every cue earlier by that much, to zero. Picking another track makes no new PlaybackInfo, no ffmpeg job and no playlist change.

The first request for a track queues a job in Postgres (`subtitle_jobs`, one row per track) and waits for the result. A worker reads the whole file with ffmpeg and keeps the result in the library's asset store, under `subtitles/`, so later requests, other replicas and burn-in jobs read the cache. However many requests and replicas ask, one worker runs one ffmpeg. A worker runs up to four extractions at once, on slots of its own that transcodes don't take (a burn-in job does wait for its track), and renews a lease while it works; a worker killed mid-extraction loses it, and another worker takes the job after at most 10 seconds, up to three attempts in all. A changed file gets a new key and fresh text; the old object is not removed. On a big file on S3 the first request takes as long as reading the file does, and a read is given up after an hour. A track that can't be read answers 500 and is logged, and the failure is remembered for 30 seconds, so a bad track isn't read again for every request.

The `stream` and `api` roles run no ffmpeg or ffprobe, so their image can leave them out (the Dockerfile is not split yet). With no worker running, a track that is already cached still plays, one that isn't is neither listed (in the film's details and in PlaybackInfo, direct play included) nor served: a request for it is answered 503 after six seconds with no worker. The check for what is cached is a request to the asset store for each text track, made only for the details of one film and for PlaybackInfo, and only while no worker runs. A worker that returns lists the tracks again. Extracting uncached subtitles needs a worker, which needs no `transcodeStore` for it. An `api` and `stream` deployment with no worker, upgraded to this version, lists only the tracks already cached, which for a fresh library is none. A request behind busy workers waits its turn. One whose job stays unclaimed for six seconds with no extraction running anywhere, because the only workers are from before subtitle jobs, answers 503 as well.

Upgrading from a version where `stream` extracted subtitles, roll in this order: workers, then `stream` replicas, then `api` replicas. A worker from before this version reports in but never claims a subtitle job, so while only such workers are left, uncached tracks are listed and their requests answer 503 after six seconds.

Limits to know:

- The first read of a track on a big file takes as long as reading the file.
- A proxy or ingress in front needs a read timeout longer than that, or the viewer gets no subtitles until re-picking the track.
- The first burn-in of a track shows no picture for that time.
- A burn-in track that can't be read refuses the film with the "cannot be played" dialog on every attempt while that track is chosen, while the same film plays with that subtitle off.
- Only the worker needs ffmpeg and ffprobe.
- Without a worker, tracks not yet extracted are hidden from the film's details and PlaybackInfo, and a worker that starts again lists them once the page is loaded again. A film listed in a library page never shows its tracks.
- jellyfin-web fetches a subtitle once and sets no time limit of its own that was found in its code or the captures; Chromium held a plain fetch for 200 seconds and then took the answer, so a proxy in front is what needs a long read timeout.
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
go test ./...                               # Tests that need nothing outside Go
test/integration.sh                         # Every test, with disposable Postgres, SeaweedFS, Garage and real ffmpeg
test/roles.sh <workdir>                     # Each role as its own process (makes its own films and config in <workdir>)
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web, two api replicas, S3, a router, a worker
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

There are two tiers of Go tests. `go test ./...` runs the tests that need nothing outside Go and passes on a machine with no ffmpeg, Postgres, S3 or fonts. Tests that need any of them live in files named `*_integration_test.go` with the build tag `//go:build integration`, and `test/integration.sh` runs them (`go test -tags integration`) with everything else. It needs Docker, curl, ffmpeg and ffprobe built with libx264, libx265 and libass, and fontconfig with a font (`fonts-dejavu-core`, or `font-dejavu` on Alpine), and says which is missing when it stops. No test skips for a missing dependency: it fails, naming what it needs. A mistyped build tag would hide a test without any skip, so `TestBuildTags` requires a file's name and tag to agree. `cmd/testreport` fails a run with a skipped test, or with a different number of tests from what `go test -list` finds for the tier, which catches a run that stopped partway. Go tests run through [gotestsum](https://github.com/gotestyourself/gotestsum) (pinned in `go.mod`, run as `go tool gotestsum`), and in CI each job's summary lists passed, failed and skipped counts and each failure's reason, the last line a test located, with an annotation at that file and line. A panic or skip is named but has no line, and an e2e scenario is annotated at the quoted literal of its name, so a scenario keeps any part known only at run time after `: `; a name with no such literal is annotated at the top of the file. `test/roles.sh` and each `test/e2e` gate do the same in CI and print as before locally. A failed job uploads its logs as an artifact: for the Go tiers, `unit.json` or `tests.json` (every test's output, which holds the workers' log lines and the ffmpeg stderr in their errors) and `failures.log` (the full output of each failure, plus the server and worker log of a subtitle test that failed); for roles and e2e, the server, worker and ffmpeg logs; run locally, `JELLYSCALE_LOG_DIR` picks where the logs go. Roles and e2e default to `<workdir>/logs`; the Go tiers keep theirs only when it is set.

`test/e2e` needs `web-dist` (see Run), and a directory with `playwright-core` installed. It drives the browser at `$CHROMIUM_PATH`, `/usr/bin/chromium` if unset. Use Google Chrome (`CHROMIUM_PATH=/usr/bin/google-chrome-stable`) where Chromium is built without H.264 and AAC, which every fixture needs. `E2E_GATES="transcode"` runs one gate.

## License

GPL-3.0-or-later. Written from observed behavior and public API documentation; no Jellyfin code is ported. jellyfin-web is a separate GPL-2.0 program and isn't included here.
