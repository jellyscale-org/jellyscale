# Workers: leases, loss and capacity

Facts about how `internal/transcode` knows a worker is alive, what a job costs and what a worker can hold. See [README.md](README.md) for how to read this.

## Knowing a worker is dead

- **Two signals.** Each worker holds a Postgres session advisory lock on a connection of its own, which dies with the process, and each job has a lease (10 s) the worker renews.
- **The lock is exact on process death.** A killed process, an out-of-memory kill and a stopped container all close the connection, so a takeover comes 1.0 to 1.25 s after the kill. With real processes, from a kill to the replacement's first segment took 4.0 to 4.3 s; with the lease alone it took 13 s.
- **The lease is the fallback for what closes no connection:** a host that loses power, a kernel panic, a partition, or a proxy that keeps the server's end open. Then a job is taken over when its lease runs out, up to 10 s after the last renewal.
- **A shorter lease alone was not chosen.** It costs a write per job per second, takes a slow database or a stopped worker for dead, and still leaves the takeover a lease plus ffmpeg's start behind.
- **One reaper.** One worker at a time forgets dead workers, chosen by a one-row lease (2.5 s) that another worker takes when the reaper stops renewing. Every other worker only reads its own row. With every worker reaping, 50 workers cost 400 lock-table snapshots a second and 400 workers 3,200; with one reaper it is 8 a second at any size. Each snapshot briefly holds every lock-manager partition, so other queries feel it.
- **The reaper forgets a worker only when all three hold:** its lock is gone in two looks 0.5 s apart; the reaper holds its own lock in the same snapshot; the reaper has not lost its own lock in the last 10 s. A process that holds no lock (an `api` or `stream` replica) forgets nobody: it can miss an outage entirely and would forget live workers that are retaking their locks.
- **Losing the lock is not losing the job.** When Postgres restarts, every worker's lock connection ends at once. Each goes on with its jobs, claims nothing, takes the lock again, then looks for its own row. Only a row found gone stops its ffmpeg. So a Postgres restart or failover restarts no job.
- **A forgotten worker's jobs get half a second**, its running subtitle and picture jobs included. No replacement starts while the old ffmpeg runs.
- **A worker that lost a job stores nothing more for it.** The check before every write to the store is a renewal of the job's lease, sent after the ask and answered by Postgres; a database that does not answer counts as no. Every write also checks the worker's own clock against the renewal, so an answer that came late, or a worker stopped past its lease (SIGSTOP, a frozen VM), stores nothing when it goes on. What remains: a write already in flight can land. Harmless for video, whose content is the spec's; audio bytes can differ inaudibly.
- **A slow database is not a lost one.** A look or registration Postgres doesn't answer within its bound is no event: nothing is stopped or forgotten.
- **Every job has an end.** A worker that loses its lease, can't renew it or shuts down kills its ffmpeg, half a second before the lease can lapse (9.5 s after the last renewal). The kernel kills it if the worker is killed.

The timings are expressions of one lease, the same for the whole cluster:

| Name | Of the lease | At 10 s |
|---|---|---|
| heartbeat | 3/10 | 3 s |
| counted (a worker only its lease vouches for) | 1 | 10 s |
| forgotten worker's jobs, cut margin, confirm gap, look bound | 1/20 | 500 ms |
| look period | 1/40 | 250 ms |
| reaper's lease | 1/4 | 2.5 s |
| retry of the lock | 1/200 | 50 ms |
| hold (after losing its own lock, a reaper forgets nobody) | 1 | 10 s |

## Connections and poolers

- **Each worker opens two connections beside its pool:** the lock's, and one for the lease keeper, which a slow query of the pool's cannot block. N workers use 2N direct connections.
- **The lock's connection must be direct and session-mode.** Behind a transaction-mode pooler the lock belongs to whichever server connection the pooler chose, so a killed worker is not noticed (observed with pgbouncer 1.26.1). A worker that finds its lock missing while its connection stands says so in its log.
- **The main connection may sit behind a session-mode pooler, not a transaction-mode one:** hint listening and advisory locks taken in one statement and released in another need one server connection.
- **Scans and sweeps hold their locks on a connection of their own.** Held on a pooled connection, a small pool waited for itself.
- **Postgres's TCP keepalives, if set, drop a vanished host's lock sooner.** Then a worker that is alive but cut off can't be told from a dead one: two ffmpegs can run for one job for some seconds. The write check above is what keeps the second from storing.

## What a job costs

A job costs a fixed number of reference cores, decided by what it does to which source, the same on every replica and worker. The reference is a 16-thread AMD Ryzen 7 7800X3D with ffmpeg 9.0.2. A re-encode is priced by its source, not its output: the output is at most 1080 lines, but decoding and tone mapping a 4K source is the cost.

| Job | Cores |
|---|---|
| Re-encode of a 1080p SDR source | 0.8 |
| Re-encode of a 4K SDR source | 1.6 |
| Re-encode of a 1080p HDR source, tone mapped | 2.0 |
| Re-encode of a 4K HDR source, tone mapped | 3.2 |
| HEVC fallback of a copy, 1080p / 4K | 3.2 / 4.0 |
| AV1 fallback of a copy, SDR, 1080p / 4K | 2.0 / 2.4 |
| AV1 fallback of a copy, HDR, 1080p / 4K | 3.6 / 4.2 |
| A copy, its audio copied or re-encoded | 0.05 |

- Where a 4K HDR re-encode's CPU goes, per second of film on the reference machine: decode 1.19, scale 0.12, tone mapping 0.66, drawing a subtitle 0.00, encode 0.47.
- The AV1 fallback is priced from ffmpeg 6.1, which uses about twice the CPU of ffmpeg 9 for the same SVT-AV1 arguments. x264 costs the same on both.
- The prices were measured on synthetic sources, which are easier than a film. The 1080p SDR row measured 1.36 CPU-seconds per second on a hard film (`spikes/picture-quality`), so that row is low.
- Copies are bounded apart from cores, at 32 per worker: a copy costs almost no CPU but still reads and writes a stream.
- Subtitle extractions (4 at once), image-track extractions (2) and chapter-picture jobs (2) have slots of their own and never take a transcode's room. A whole-film read must not make a viewer's subtitle wait.

## Admission

- **A job fits a worker** when the costs of the jobs it holds, paused ones included, and this one add up to no more than its capacity.
- **A play that no worker can hold is refused at once**, with the job's cost and the most any worker has free. Before this rule a job was admitted on a worker too slow for it and stalled at about 0.85 times real time.
- **Two workers may hold together a re-encode neither holds alone** when each has 0.65 of its cost free. Capacity is what a worker keeps at 1.2 times real time, so 0.65 of the cost is 0.78 of real time, which is what each worker of the slowest measured pair made alone while the pair held 1.0 together.
- **A queued, unclaimed job counts** against the worker with most room, so two replicas don't both promise the last room.

## How a worker learns what it holds

A worker's capacity is the cost it keeps at 1.2 times real time: **E = sum(cost × speed) / 1.2** over its running jobs.

- **It starts from the CPUs the process may use** (a container's CPU limit as written, fractions too), or the operator's ceiling if that is lower. It is never above its start. It runs no benchmark: a start-up benchmark made workers register late, and requests timed out waiting.
- **It learns only from the jobs it runs**, one sample each heartbeat.
- **Speed is ffmpeg's own progress, never the rate segments reach the store.** With store writes held 5 s and 8 s, ffmpeg's speed read 2 to 3% lower, while the stored rate fell 12% and 44%.
- **A sample counts only when the worker is CPU-bound:** its transcode ffmpeg processes wanted (time on a CPU plus time waiting for one, from the kernel's scheduler figures) at least 0.9 of what the worker may use. Measured: every starved case read 1.86 or more; every case waiting on reads read 0.44 or less. Nothing between was measured.
- **The kernel's run-queue time already includes time a CPU limit throttled the thread.** Adding the throttled time counts it twice.
- **A sample is void** when a job is paused at its lead, is under 15 s old, or just ended; when a copy's fallback encode runs (its CPU is 33 to 52% off its price); when the worker's own extraction runs or ended in the last 10 s; or when the kernel's figures can't be read.
- **It moves only on evidence:** seven CPU-bound samples in a row with a job under 1.2x lower it to their mean, never below a quarter of its start; three that show more raise it; a 10% dead band both ways.
- **A lowered figure no job has tested rises again by time**, the wait doubling each time a job shows the rise wrong.
- **Lowering ends no job.** A running job its worker can no longer hold alone becomes a shared one.
- **The figure counts only once it is stored**, where every replica reads it.
- One learned figure predicted the speeds of the other x264 rows within 10 to 14% on the same worker.

## What an operator has to do

- Run at least two workers, with the cost of the dearest film to spare between them.
- Give a worker a CPU request equal to its limit, in whole cores. A limit above the request lets the kernel throttle the worker when neighbours are busy, which it meets as a slow CPU it cannot plan for.
- A worker that is to hold a 4K HDR re-encode alone needs about 4 to 5 current cores to itself. A worker limited to 2 CPUs counts 2.0, under the 2.08 a share of that job needs: give a worker that is to share one a limit of 3 or more.
