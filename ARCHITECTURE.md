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
4. **Workers are replaceable.** Jobs are leased, and a segment's video and timing depend on the spec, not on which worker made it. Encoded audio can differ in its bytes after a restart, inaudibly (DOMAINS 48). A worker's life is a Postgres session advisory lock held on a connection of its own, which dies with the process, plus its `transcode_nodes` row: whoever finds the lock gone deletes the row and cuts the worker's leases to half a second from now (`Reap`), and a worker without its row may not claim, renew or store (DOMAINS 25, 37). When Postgres is away every connection ends, so the lock goes with it; a worker's jobs go on while it takes the lock again, and only a row found gone stops them (DOMAINS 25). A worker forgets another only when it finds its lock gone twice, 0.5 seconds apart, holds its own lock in the same look, and has not lost its own within 10 seconds; a replica forgets nobody and counts a worker only while its lock is held. The cost is that the lock is exact on process death (a takeover 1.0 to 1.25 seconds after a kill) except for a worker killed during an outage, which is forgotten up to 10 seconds later. The lease stays the fallback for what closes no connection (a host that loses power, a partition). A shorter lease alone (3 seconds, renewed every second) was the simpler alternative and was not chosen: it costs a write per job per second, takes a slow database or a stopped worker for dead, and still leaves the takeover a lease plus ffmpeg's start behind, where the lock is exact on process death. When Postgres's `tcp_keepalives_*` drop a vanished host's backend, a worker that is in fact alive but cut off keeps its ffmpeg until its own lease check, so two ffmpegs can run for one job for some seconds; the check before every store keeps the second from storing, but a store already in flight can still land (harmless for video, whose bytes are the spec's; audio can differ in its bytes, DOMAINS 48).
5. **No required service beyond Postgres.** Anything added must earn its place against a measurement.
6. **Cost is more than CPU.** A design states its network, storage and database cost at the target scale. Copying a film is cheap in CPU and expensive in S3 reads, for example. Liveness costs one Postgres connection per worker process and, per worker, four reaps a second, each one snapshot of `pg_locks` (0.2 ms with 100 nodes on Postgres 17) that writes nothing unless a worker is dead; each replica's PlaybackInfo reaps at most once a second as well. At a few dozen workers that is a few hundred lock-table snapshots a second, and each snapshot briefly holds every lock-manager partition, so other queries feel it too; past about 50 workers, one reaper per cluster (a try-lock) with each worker checking only its own lock is the change to make.
7. **Images follow runtime needs.** Split images by what a role needs, not per service.

## Known departures

- A burnt-in image subtitle cue (PGS, VobSub, DVB) that began before a transcode job's restart point is drawn by that job only if the film lists cue points for the track: Matroska with Cues for it, which the scan keeps (DOMAINS 51). A film with none (MPEG-TS, a Matroska file written without Cues for the track or with Cues for the video only) is read a look-back of 10 seconds early, with no read of the whole film, so a cue that began within the look-back is drawn from the run's first frame and an older one is missed until the next cue. A run inside a showing cue also reads and decodes the film from the cue's first packet to its cut, which grows with the cue's age and has no cap.
- A worker from before a release that adds a spec field (tone mapping, copied audio) stores segments made without it under the new hash if it takes a job with the field, so workers roll first and roll back last (docs/transcoding.md, Tone mapping). The general fix is built: every job row and every worker records an `OutputVersion`, and a worker claims only jobs of the version it makes, so a worker that predates the column is the only one that can still take a newer spec (docs/running.md says to stop it before a later output version starts).

## Control-plane ceiling

How many streams one Postgres primary carries for the control plane alone, no video (JS-020). `test/stress.sh <workdir>` starts a disposable Postgres (`postgres:18-alpine`, 18.6 on the run below, `max_connections=2000`) and drives the real calls, not copied SQL: per viewer a `Jobs.Watch`, `Jobs.Segment` (with its ensure and state reads, waiting on a fake job) every 4 s and a `Jobs.Unavailable` at start and every 30 s; per running job `Worker.beat` every 3 s, a `Worker.furthest` look every 500 ms (as a job at its lead limit makes, so an upper bound) and a segment stored and a `Notify` every 4 s; per worker `Reap` four times a second on top of its own liveness-lock connection, `Worker.claim` every second and on each new-job hint, and `Worker.Sweep` once a minute. Only ffmpeg is fake. V viewers, W = V/4 workers (4 slots each, so about one job per 4 viewers), 60 s per step, calls open loop so a slow database shows as latency, never as a quietly lower rate. The run fails if any operation completes less than 90% of its intended calls without error.

**Result: one primary carries about 1600 streams with 400 workers (a pool of 64 per process), with Watch, beat and Reap p99 at 22 ms, 22 ms and 14 ms; 3200 streams with 800 workers is just over the line (Watch p99 55 ms); 6400 collapses.** Measured on 16 cores (AMD Ryzen 7 7800X3D, 8 cores with SMT) and 62 GB RAM, Postgres in Docker, with the load generator on the same cores, so the CPU numbers are for the pair and the real ceiling of a dedicated primary is a little higher. A step is degraded when p99 of Watch or beat is over 50 ms or of Reap over 250 ms (a dead worker's jobs get a second). All the calls share one pgx pool, as every role of a process does in the dogfood; its size is `databaseMaxConns` (docs/running.md).

Step table, pool of 64 (CPU is `docker stats`, percent of one core, averaged over the step; conns is the most Postgres connections seen, workers' liveness connections included). The last two rows come from a run of the same driver past the ladder's top step (`JELLYSCALE_STRESS_STEPS=3200,6400`):

| V | W | Watch p99 | beat p99 | Reap p99 | Postgres CPU | conns |
| --- | --- | --- | --- | --- | --- | --- |
| 50 | 12 | 16.4 ms | 10.8 ms | 1.1 ms | 4% | 36 |
| 100 | 25 | 15.1 ms | 14.4 ms | 0.8 ms | 7% | 65 |
| 200 | 50 | 12.3 ms | 13.3 ms | 1.0 ms | 16% | 117 |
| 400 | 100 | 14.7 ms | 15.7 ms | 3.0 ms | 36% | 173 |
| 800 | 200 | 13.3 ms | 13.4 ms | 1.4 ms | 71% | 273 |
| 1600 | 400 | 21.4 ms | 22.1 ms | 13.8 ms | 195% | 473 |
| 3200 | 800 | 54.9 ms | 58.4 ms | 47.6 ms | 499% | 873 |
| 6400 | 1600 | 5.0 s | 5.0 s | 5.0 s | 796% | 1673 |

At 6400 most calls hit the harness's 5 s limit and 77% of Watch calls failed; the run stopped there.

The pgx pool size, same ladder (p99 of Watch / beat / Reap, ms; the pool is shared by every role of the process, and the hint listener holds one connection of it for good):

| pool | V=200 | V=400 | V=800 | V=1600 | first degraded step |
| --- | --- | --- | --- | --- | --- |
| 4 (pgx's default on a host with 4 CPUs or fewer, as the dogfood's) | 57.2 / 62.3 / 49.4 | 1686 / 1676 / 1675 | 5001 / 5001 / 5001 | not reached | V=200; V=400 queues every call; V=800 fails (half its calls time out) |
| 16 | 11.1 / 13.1 / 0.8 | 14.1 / 14.9 / 0.8 | 17.3 / 21.3 / 6.2 | 490 / 498 / 483 | V=1600 |
| 64 | 12.3 / 13.3 / 1.0 | 14.7 / 15.7 / 3.0 | 13.3 / 13.4 / 1.4 | 21.4 / 22.1 / 13.8 | none to 1600; V=3200 |

A pool of 4 is the limit well before Postgres is: at V=400 Postgres used 25% of one core and every call queued for a connection. Pool 4 is on a cliff between V=200 and V=800, so its rows vary from run to run: three runs gave V=400 Watch p99 of 1.5 s, 1.7 s and 4.9 s, the last with 2890 of 6000 Segment calls failing, which stops the ladder there with the rate error; the row is the run of 2026-11-08, and the conclusion, not the digits, is what repeats. The pools of 16 and 64 sit far from their cliffs and repeat in shape. `db.DefaultMaxConns` is 16, which holds 800 viewers and 200 workers' traffic per process without a queue; raise `databaseMaxConns` to 64 for a process that carries more. The cost is `max_connections`: a process opens up to its pool size, plus one connection for every worker it runs, so a stock Postgres (100) is reached at about 90 workers (one connection each) however idle it is. That is the first wall, ahead of CPU, and it moves with one setting (or, for the pools only, a session-mode pooler; the liveness connection must stay direct).

Rerun: `test/stress.sh <workdir>` (about 7 minutes; Docker only; `JELLYSCALE_STRESS_POOL=64` picks the pool, `JELLYSCALE_STRESS_STEPS=3200,6400` the steps; the report is `<workdir>/stress-report-pool<N>.txt`). It is behind the build tag `stress` and in no CI tier. `go test -tags integration -run TestStressDriver ./internal/transcode` runs the same driver for 6 seconds at V=8 as a check that it still works.

**NATS JetStream for ephemeral state after the MVP: not recommended; no task proposed.** The target is tens to hundreds of streams (PRODUCT.md; plan Phase 6: 200 mixed streams). That is 8 times below the 1600 streams one primary carried with p99s of 14 to 22 ms, using about two of 16 cores, and the 200-viewer step (W=50) cost 16% of one core. Postgres stays the only required service (rule 5), and adding NATS would cost a second service to run for a ceiling nobody is near. If the target ever moves past a few thousand streams, move the polling before the state: the `Reap` loops (four a second per worker, the bulk of calls at every step), the `furthest` looks and the 1 s claim ticks are what Postgres is asked most, and the first two can be slowed or served from the NOTIFY hints without a new service. The limits that arrive first are `max_connections` and the pool size, both settings. Unverified: the figures past 1600 are one run, and the load generator's own CPU is not separated from Postgres's.
