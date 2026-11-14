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

## Rolling update

Replace the `stream` replicas first, then the `api` replicas. An `api` replica of this release offers a copy of HDR10, HDR10+ and HLG video, and a `stream` replica of the previous release refuses that copy, so a viewer who is sent to it fails to play. A new `stream` replica serves every copy the previous `api` offered, so it is safe to be first.

Every job records the output version of the replica that queued it, and a worker claims only jobs of the version it makes, so an upgrade must keep a running worker for every version a running replica queues. To upgrade, start the new workers first, then replace `stream`, then `api`, and stop the old workers last, so that at each step every version a running replica queues has a running worker that makes it. To roll back, start the old workers first, then put the old `api` back, then the old `stream`, and stop the new workers last, again with a worker for each version at every step: the old `stream` must never run while no worker of the old version does. When a release does not change the output version, the order matters only for the HDR10 rule above.

When that order is not kept, a viewer whose job no running worker of its version takes waits `queueTimeout` (6 seconds) and no less, then gets a 503 with `Retry-After`, and the server log says that only workers of another output version run. Segments made by another version are never served under a spec of this one. A worker built before this release makes no such check, since it claims any queued job, so every such worker must be stopped before a replica of a later output version starts.

## Configuration

A key the server does not know, at the top of the config or in a library, is refused at startup with its name, so a misspelt `chapterImages` can't silently leave the option off; so is anything after the config. Upgrading: a config that loaded before because the server ignored a stray key now stops the start, naming the key to remove.

A library whose POSIX media folder is missing fails its scan with an error naming the folder, and so does one whose media store lists no video at all while the library has films (an empty mount point, a volume unmounted under a folder that is still there, or an empty bucket): the films and their watch state are kept, the log says why, and the next scan after the volume is back carries on as before. The cost is that a library emptied on purpose keeps its last films listed until a video is in it again.

A film or episode whose file a scan no longer lists is hidden, not deleted: viewers see what they would if it were gone, but its item, media source and every viewer's position, played flag and favourite stay in the database. A file that is listed again inside the grace, at its old key or moved or copied to another (matched by its size and ETag, or by its size and what the probe finds in it), is shown again as the same item. `missingGrace`, a Go duration such as `72h`, is how long that grace is; left out it is `168h` (seven days), and a value that is not a duration or is negative stops the start, naming the key. The grace is counted by Postgres's clock from the first scan that missed the file and kept in the database, so it holds across scans, workers and restarts. The first scan after it has passed removes the item with its watch state, and then any season or series left with no episode; a file that returns after that is a new item.

`maxLostShare`, a number from 0 to 1 (left out, `0.2`), is the most of a library's stored files a scan may find missing. A scan that would lose more fails, like the empty-mount case above, with an error that names the library, how many files are lost and how many are stored; it hides and removes nothing (not even an item whose grace has passed), `jellyscale_scan_last_success` is 0 for the library, and every later scan fails the same way until the files are back, when the first one succeeds with every item and watch state as before. A file moved to another key and matched to its item is not lost, and the files already hidden count, so several small losses can't add up unseen. After removing that many files on purpose (more than the share of a library), set `maxLostShare` to `1` for one scan, or higher than the share you are removing, and set it back.

A library is identified by its media store and kind, not only by its name: a config that changes only a library's `name` leaves one library with the same id, items and watch state, and the new name in the views. A library that is new to the database gets the hash of its name as its id, which is what every library has had, so an upgrade keeps all ids. A library left out of config is hidden with its items (it is gone from the views, and its items from every listing) and is removed, with its items and everyone's watch state, by the first worker tick after `missingGrace` has passed since the first worker start without it; putting it back in config before then shows it again with its id and everything in it. A worker removes libraries on every scan tick, so no restart is needed. Every worker must list the same libraries: a worker whose list leaves a library out hides it from every viewer and starts its grace, whatever the other workers list. The `libraries` of api and stream replicas are not read (a bad entry there stops only a worker), and a worker listing none hides and removes nothing, so emptying the list does not remove the last library. A library's grace runs from the first worker start without it, no longer from any start.

`transcodeHelpers` set to `false` stops this worker's slow transcodes from asking other workers to help (parallel transcoding): a slow job is then one ffmpeg and inserts no helper rows. Left out or `true`, helpers are on. Set it on every worker: a worker with it on still asks for help, and every worker still runs helper jobs other workers asked for. Helpers never keep a viewer waiting: a new viewer who finds every slot taken stops a helper (one that is queued, else the one furthest ahead) and the helper gives its slot back at once. What the helper made is kept, and the rest of its chunk is made when a viewer reaches it, which costs that viewer about what a seek does. A stopped helper's job records `a new viewer needed its slot` as its error, and a running one logs it as the reason its ffmpeg stopped.

`databaseMaxConns` sets the most Postgres connections the process opens for its queries, shared by every role in it. Left out or `0`, it is 16; a negative value, 1 (the hint listener keeps one connection, so 1 leaves none for queries) or more than 10000 is refused at start. The default holds about 800 viewers' traffic in one process; a pool of 4 (pgx's own default on a host with 4 CPUs or fewer, as the dogfood's: pgx uses the larger of 4 and the CPU count) queued every call at 400 (ARCHITECTURE.md, Control-plane ceiling), and 64 held 1600. Each worker also holds one more connection for its liveness lock, outside the pool, so Postgres's `max_connections` must cover every process's pool plus its workers: a stock server's 100 is reached at about 90 workers.

A Postgres switchover or restart is survived without interrupting playback: every replica logs `Postgres is away` and later `Postgres is back`, requests that need the database wait for it (up to 90 seconds for a segment) rather than answer an error, workers keep their transcodes and store the segments they made once it answers, and `GET /healthz` answers from the process alone, so a liveness probe on it does not restart every pod together. Run the database with synchronous replication (CNPG `synchronous` settings): a switchover to a replica that lacks the last committed rows can lose a worker's `next_segment` or its `transcode_nodes` row, which the worker answers by stopping its jobs and starting over, so playback stalls for a few seconds.

## Metrics

Set `metricsListen` in the config (for example `":9090"`) and every role in the process serves Prometheus text exposition at `/metrics` on that address, on a listener of its own: the public port never has `/metrics`, so the ingress cannot reach it. Left out or empty, nothing listens. A worker-only process serves it too. There is no authentication, so bind it to an address only the operator's network reaches (`127.0.0.1:9090`, or a pod port no Service or Ingress exposes). Nothing in it carries a title or a file path.

To read it from a pod without a Prometheus server, forward the port of a pod and not of the Deployment:

```sh
kubectl -n media get pods -o wide
kubectl -n media port-forward pod/<pod> 9090:9090 &
curl -s localhost:9090/metrics | grep ^jellyscale_
```

Each pod serves only its own process. A transcode job, its speed, paused state, slots and its `jobs_running` exist only on the worker pod that runs it, and `deploy/...` forwards to one arbitrary pod, so with several workers it may show every slot idle while the job you want runs on another. Find the pod with the job: `psql -c "select id, worker from transcode_jobs where state = 'running'"` gives the worker's ID, which is `<pid>-<start time in nanoseconds>` and not the pod's name. A worker logs `worker started worker=<id> host=<hostname>` when it starts (the hostname of a pod is its name), so `kubectl logs` of each worker pod, searched for that ID, finds the pod. Segment waits are on the stream pod that served the request, and scans on the worker that won the library's lock; queue depth is the same on every worker.

Every metric starts with `jellyscale_`, and the Go runtime and process metrics (`go_*`, `process_*`) come with it.

| Metric | Role | Meaning |
| --- | --- | --- |
| `jellyscale_transcode_jobs_running{kind}` | worker | Jobs running, paused ones included, by `reencode` or `copy`. |
| `jellyscale_transcode_job_speed_ratio{job,kind}` | worker | Seconds of film a running job encodes per second of its own running time, over its last 10 seconds. It comes from ffmpeg's `-progress` output and leaves out the time ffmpeg is paused at its lead limit, so a healthy job that paused is not slow. Below 1.0 a viewer stutters. A copy reads its input at 5 times the film's speed, so it shows about 5 whatever the machine could do. Absent until the job has run a second, and gone when the job ends. `job` is the job's ID. |
| `jellyscale_transcode_job_paused{job}` | worker | 1 while that job's ffmpeg is paused at its lead limit. |
| `jellyscale_transcode_slots`, `jellyscale_transcode_slots_busy` | worker | Slots this worker has, and slots in use (paused jobs hold theirs). |
| `jellyscale_transcode_queue_depth` | worker | Jobs queued for a worker and not yet past the queue timeout. It is the whole cluster's queue, read from Postgres when scraped, so every worker reports the same number: take one worker's, don't sum them. Workers report it, not api or stream, so a scraper sees one source per worker and no query runs for roles that don't need it. |
| `jellyscale_extraction_jobs_running{kind}` | worker | Subtitle extractions (`subtitles`) and chapter-picture jobs (`chapter_images`) running. |
| `jellyscale_transcode_jobs_started_total`, `jellyscale_transcode_jobs_ended_total{result}` | worker | Jobs begun, and ended as `done`, `failed` or `interrupted` (cut short, not failed: the worker stopped, the lease was lost, or no viewer was left). |
| `jellyscale_transcode_reaped_total{what}` | worker, stream, api | Dead workers forgotten (`workers`) and their jobs cut short (`jobs`) by the process that noticed. |
| `jellyscale_segment_wait_seconds{source,kind,outcome}` | stream | Histogram of how long a segment request took. `source`: `store` (already stored), `waited` (on a job that was already queued or running, whoever started it), `started` (a job this request started), `none` (ended before it looked). `kind`: `reencode` or `copy`. `outcome`: `ok`, `timeout` (90 s), `failed`, `refused`, `canceled` (the client left), `error`. The slow cases are the ones that never return a segment, so they are in it. |
| `jellyscale_segment_stalls_total{kind}` | stream | Requests that took over 5 seconds, whatever their outcome. |
| `jellyscale_segment_requests_refused_total{reason}` | stream | Requests refused as `stopped` (a playlist or segment request of a session that ended, answered 410), `no_worker` or `busy` (a segment that needed a job and found none, or no free slot). |
| `jellyscale_playbackinfo_decisions_total{method}` | api | How PlaybackInfo said a source plays: `direct_play`, `copy` or `reencode`. jellyscale has no separate direct stream. |
| `jellyscale_playbackinfo_refusals_total` | api | Sources refused because the transcode they need cannot start. |
| `jellyscale_scan_last_duration_seconds{library}`, `jellyscale_scan_last_success{library}`, `jellyscale_scan_last_timestamp_seconds{library}` | worker | The last scan this process ran of the library (by library ID): how long it took, whether it succeeded (1) or failed (0), and when it ended (seconds since the epoch). These describe the scans this replica ran: a scan another replica holds the lock for is not reported here and leaves the series as they were, so a replica that keeps losing the lock shows an old timestamp. Alert on `time() - max(jellyscale_scan_last_timestamp_seconds)` across workers, and read `jellyscale_scan_last_success` from the series with the newest timestamp. |
| `jellyscale_db_pool_connections{state}`, `jellyscale_db_pool_max_connections` | every role | Postgres pool connections `acquired`, `idle` and `total`, and the limit. |
| `jellyscale_db_pool_acquires_total`, `jellyscale_db_pool_acquire_wait_seconds_total` | every role | Connections acquired, and the time spent waiting for one; the rate of the second over the first is the average wait. |

Counters and histograms count from the process's start. A stream request that is still waiting is not yet in the histogram, so a hung one shows when it ends (at 90 s at the latest).
