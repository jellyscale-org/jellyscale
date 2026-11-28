# What one Postgres carries

A measurement of the engine's coordination load alone, with no video: how many streams one Postgres primary carries for the job, lease and viewer bookkeeping described in [segments.md](segments.md) and [workers.md](workers.md). See [README.md](README.md) for how to read this.

`test/stress.sh <workdir>` starts a disposable Postgres and drives the engine's real calls, with only ffmpeg faked: per viewer, a segment request every 4 s; per running job, a heartbeat every 3 s, a look at its viewers every 500 ms and a stored segment every 4 s; per worker, a claim every second and the liveness looks. V viewers and V/4 workers, 60 s per step. It fails if any operation completes under 90% of its intended calls.

**Result: one primary carried about 1600 streams with 400 workers** at p99 latencies of 14 to 22 ms. 3200 streams was just over the line; 6400 collapsed. The product's target, tens to hundreds of viewers, is about 8 times below it, and 200 viewers cost 16% of one core.

Measured on a 16-thread AMD Ryzen 7 7800X3D with 62 GB of memory, Postgres 18.6 in Docker with `max_connections=2000`, the load generator on the same cores, a pool of 64 connections per process. One run past 1600.

| Viewers | Workers | Request p99 | Heartbeat p99 | Postgres CPU (% of one core) | Connections |
|---|---|---|---|---|---|
| 200 | 50 | 12 ms | 13 ms | 16% | 117 |
| 400 | 100 | 15 ms | 16 ms | 36% | 173 |
| 800 | 200 | 13 ms | 13 ms | 71% | 273 |
| 1600 | 400 | 21 ms | 22 ms | 195% | 473 |
| 3200 | 800 | 55 ms | 58 ms | 499% | 873 |
| 6400 | 1600 | 5.0 s | 5.0 s | 796% | 1673 |

This run predates the single reaper and the lease keeper's connection ([workers.md](workers.md)), so it overstates the liveness load and understates connections by one per worker.

## The pool is the first wall, not Postgres

The same ladder by pool size (p99 of a request):

| Pool | 200 viewers | 400 | 800 | 1600 |
|---|---|---|---|---|
| 4 (the library's default on a host with 4 CPUs or fewer) | 57 ms | 1.7 s | 5 s, half the calls time out | not reached |
| 16 | 11 ms | 14 ms | 17 ms | 490 ms |
| 64 | 12 ms | 15 ms | 13 ms | 21 ms |

- With a pool of 4, every call queued for a connection at 400 viewers while Postgres used a quarter of one core.
- The engine's default is 16, which holds 800 viewers and 200 workers' traffic per process.
- The next wall is `max_connections`: a process opens up to its pool, plus two connections per worker. A stock Postgres (100) is reached at about 45 busy workers.

## What follows

- A second service for short-lived state (a message bus, a cache) is not needed at the product's scale, and adding one would break "no required service beyond Postgres" for a ceiling nobody is near.
- If the target ever moves past a few thousand streams, slow the polling before moving the state: the liveness looks, the job's looks at its viewers and the once-a-second claims are what Postgres is asked most.

`go test -tags integration -run TestStressDriver ./internal/transcode` runs the same driver for 6 seconds as a check that it still works. The full run is behind the build tag `stress` and in no CI tier.
