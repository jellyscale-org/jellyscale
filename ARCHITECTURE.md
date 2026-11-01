# Architecture

What the system is meant to look like, and why. [PRODUCT.md](PRODUCT.md) says what to build and [DOMAINS.md](DOMAINS.md) says how it behaves today. Where the code departs from this file, either the code is wrong or this file needs a recorded decision.

## Goal

One library on a highly available cluster, serving tens to hundreds of concurrent streams, with each part scaling on its own. Postgres is the only required service, plus object storage or POSIX.

## Roles

One binary, chosen with `--roles`, run as separate Deployments. Roles version together, so a schema or segment change never has to span separately built services.

| Role | Job | State | Scales on | Runtime needs |
|---|---|---|---|---|
| `api` | REST, WebSocket, PlaybackInfo | Connections | CPU, connections | Postgres |
| `stream` | Sends bytes: direct play, playlists, segments, images, subtitles | None | Bandwidth | Postgres, stores |
| `worker` | Runs media tools: transcodes, burn-in, scans, probes | Scratch only | Slots, queue depth | Postgres, stores, ffmpeg |

## Rules

1. **Only workers run media tools.** `api` and `stream` move bytes and coordinate. A viewer waiting for a result doesn't put the work in the role answering the request: segments already work this way.
2. **Roles never call each other.** Work goes through Postgres and results through the store, so either side can be replaced or scaled alone.
3. **Work is pulled.** A worker takes a job only when it has a free slot, so overload is a queue, and the queue is the autoscaling signal.
4. **Workers are replaceable.** Jobs are leased, and a segment's video and timing depend on the spec, not on which worker made it. Encoded audio can differ in its bytes after a restart, inaudibly (DOMAINS 48).
5. **No required service beyond Postgres.** Anything added must earn its place against a measurement.
6. **Cost is more than CPU.** A design states its network, storage and database cost at the target scale. Copying a film is cheap in CPU and expensive in S3 reads, for example.
7. **Images follow runtime needs.** Split images by what a role needs, not per service.

## Known departures

- `stream` runs ffmpeg to extract subtitles (DOMAINS 43), against rule 1. Extraction moves to workers in the next slice.
- A copy job has no read-ahead limit (DOMAINS 48): it reads to the end of the film or until 60 seconds after the last request. That cost has to be bounded in a later slice.
