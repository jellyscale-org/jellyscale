# Architecture

What the system is meant to look like, and why. [PRODUCT.md](PRODUCT.md) says what to build and [DOMAINS.md](DOMAINS.md) says how it behaves today. Where the code departs from this file, either the code is wrong or this file needs a recorded decision.

## Goal

One library on a highly available cluster, serving tens to hundreds of concurrent streams, with each part scaling on its own. Postgres is the only required service, plus object storage or POSIX.

## Roles

One binary, chosen with `--roles`, run as separate Deployments. Roles version together, so a schema or segment change never has to span separately built services.

| Role | Job | State | Scales on | Runtime needs |
|---|---|---|---|---|
| `api` | REST, WebSocket, PlaybackInfo | Connections | CPU, connections | Postgres, and the asset store to see which subtitles are cached while no worker runs |
| `stream` | Sends bytes: direct play, playlists, segments, images, subtitles | None | Bandwidth | Postgres, stores |
| `worker` | Runs media tools: transcodes, burn-in, subtitle extraction, chapter pictures, scans, probes | Scratch only | Slots, queue depth | Postgres (plus one direct, session-mode connection for its liveness lock), stores, ffmpeg |

## Rules

1. **Only workers run media tools.** `api` and `stream` move bytes and coordinate. A viewer waiting for a result doesn't put the work in the role answering the request: segments and subtitle extraction both work this way. `test/roles.sh` and `test/e2e/run.sh` run `api` and `stream` with no `ffmpeg` on their PATH, so an `api` and `stream` image can leave ffmpeg out; the Dockerfile has not been split yet.
2. **Roles never call each other.** Work goes through Postgres and results through the store, so either side can be replaced or scaled alone.
3. **Work is pulled.** A worker takes a job only when it has a free slot, so overload is a queue, and the queue is the autoscaling signal. A transcode job paused at its lead holds a slot with almost no CPU, so the signal counts viewers, not load; the autoscaler in `phase6-k8s` has to weigh that.
4. **Workers are replaceable.** Jobs are leased, and a segment's video and timing depend on the spec, not on which worker made it. Encoded audio can differ in its bytes after a restart, inaudibly (DOMAINS 48). A worker's life is a Postgres session advisory lock held on a connection of its own, which dies with the process, plus its `transcode_nodes` row: whoever finds the lock gone deletes the row and cuts the worker's leases to a one-second grace (`Reap`), and a worker without its row may not claim, renew or store (DOMAINS 25, 37). The lease stays the fallback for what closes no connection (a host that loses power, a partition). A shorter lease alone (3 seconds, renewed every second) was the simpler alternative and was not chosen: it costs a write per job per second, takes a slow database or a stopped worker for dead, and still leaves the takeover a lease plus ffmpeg's start behind, where the lock is exact on process death. When Postgres's `tcp_keepalives_*` drop a vanished host's backend, a worker that is in fact alive but cut off keeps its ffmpeg until its own lease check, so two ffmpegs can run for one job for some seconds; the check before every store keeps the second from storing, but a store already in flight can still land (harmless for video, whose bytes are the spec's; audio can differ in its bytes, DOMAINS 48).
5. **No required service beyond Postgres.** Anything added must earn its place against a measurement.
6. **Cost is more than CPU.** A design states its network, storage and database cost at the target scale. Copying a film is cheap in CPU and expensive in S3 reads, for example. Liveness costs one Postgres connection per worker process and, per worker, four reaps a second, each one snapshot of `pg_locks` (0.2 ms with 100 nodes on Postgres 17) that writes nothing unless a worker is dead; each replica's PlaybackInfo reaps at most once a second as well. At a few dozen workers that is a few hundred lock-table snapshots a second, and each snapshot briefly holds every lock-manager partition, so other queries feel it too; past about 50 workers, one reaper per cluster (a try-lock) with each worker checking only its own lock is the change to make.
7. **Images follow runtime needs.** Split images by what a role needs, not per service.

## Known departures

- A burnt-in PGS cue that began before a transcode job's restart point is not drawn by that job until the next cue, though a job from zero draws it, so a segment's picture depends on which job made it where a cue spans a restart (DOMAINS 51). Text cues are exact, because their track is a whole file. Opening the track's input earlier would cost another read of the film for as long as the job, so it is accepted until a measurement says otherwise.
- A worker from before a release that adds a spec field (tone mapping, copied audio) stores segments made without it under the new hash if it takes a job with the field, so workers roll first and roll back last (docs/transcoding.md, Tone mapping). The general fix, workers reporting their `OutputVersion` and the api offering only specs every live worker supports, belongs to the upgrades item in `phase6-k8s` (PRODUCT.md).
