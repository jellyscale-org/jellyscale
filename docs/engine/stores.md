# Stores: local disk and S3

Facts about `internal/store`, what reads cost, and how made things are cleaned up. See [README.md](README.md) for how to read this.

## One interface

- **Local disk and S3 behave the same at the store interface.** A conformance suite runs against local disk, SeaweedFS and Garage. AWS S3 itself was not tested.
- **Credentials come from the environment.** Store locations are stored and logged, so one that carries a user, a password or a parameter named like a key, secret, token or signature is refused at startup, with an error that names the problem and not the value. A secret hidden in a path can't be told from a path and is not caught.
- **A path built from a key can't leave the store's root.**
- **Listing a missing root is an error on local disk**, so an unmounted volume is never read as an empty collection. A scan that finds no video at all, where some is stored, fails without removing anything. The cost: a collection emptied on purpose keeps its last films until a video is back.
- **`Stat` returns only a size**: nothing looks up one object together with its version (ETag) without listing. One credential pair serves every store.

## What S3 does

- **A read is one open-ended request tied to the caller**, so cancelling the caller ends the transfer.
- **SeaweedFS can end a response early with a clean end**, with no error, when a neighbouring read of the same object is dropped while it is being read: 67 of 200 bodies were cut, at multiples of 256 KiB. ffmpeg drops a request every time it seeks. Garage never did in the same test.
- **SeaweedFS can answer a resumed ranged read with an empty success** and no range header.
- **So the reader resumes at its offset** when a body ends before its stated length, when a ranged answer carries no range, and when a known-size object ends short; it gives up after three reopens without progress. A segment read through it that is empty or still cut is treated as not there yet.
- **A connection reset while a job is paused** is resumed the same way.
- **Presigned links reject HEAD** (both stores answered 403). The server answers HEAD itself and proxies media by default.
- **ffmpeg reads S3 through a loopback input the worker serves**, never a presigned link, so no credential reaches ffmpeg's arguments. ffmpeg and mpv forward an `Authorization` header across redirects.
- **Serving a file needs its content type set**, or the server reads the object to guess one. Matroska and WebM share a container, so only that case also reads the file name.

## What reads and writes cost

Measured on single-node stores in Docker, so relative only (`spikes/objstore`, `spikes/s3cost`):

| | SeaweedFS | Garage |
|---|---|---|
| 2 MB write, 16 writers, p50 / p95 | 21 / 30 ms | 31 / 45 ms |
| 2 MB write, 64 writers, p50 / p95 | 96 / 161 ms | 135 / 169 ms |
| List 2,000 objects with size and tag | 32 ms | 28 ms |

200 streams need about 50 segment writes a second.

| File | Probe | Seek |
|---|---|---|
| MP4, index at the front | 1 request, 2.7 MB | 2 requests, 22 MB |
| MP4, index at the end | 3 requests, 5.7 MB | 4 requests, 24 MB |
| Matroska with an index | 1 request, 2.7 MB | 4 requests, 21 MB |
| Matroska without an index | 1 request, 2.7 MB | 3 requests, **325 MB** |

So a keyframe index matters most on object storage ([copy.md](copy.md)).

## Cleaning up

- **One worker lists the stores per sweep period (a minute)**, however many workers run, chosen in Postgres.
- **Segments nobody asked for in the retention time (24 h) are removed.** With a budget set and the store over it, whole specs go, least recently asked for first, and only as many as bring it under.
- **A spec being watched is never removed for the budget:** one with a queued or running job, or asked for in the last 10 minutes. If the watched specs alone are over the budget, the store stays over and the log says so.
- **Each delete checks, under the spec's lock, that the spec is still unwatched.** A viewer who asks for a spec being removed waits for one delete and is then served by a job the request starts. A sweeper killed partway leaves a spec that plays.
- **A listing that fails removes nothing.**
- **Made assets** (subtitle text, stored tracks, fonts, pictures) are removed when no current revision of a file names them. Kept: everything of a file that is only missing for a while, anything a queued or running job is writing, and a font any remaining film still names. Only objects of known shapes are touched, but the whole store is listed, so assets need a location of their own, not the media's.

## Wakeups through Postgres

Measured in `spikes/notify`, 8 listeners:

| Target rate | No wakeup | Wakeup inside the data transaction | Wakeup in its own transaction |
|---|---|---|---|
| 500/s | 8 ms | **344 ms, 189/s reached** | 9 ms |
| 5000/s | 8 ms, 1538/s | **348 ms, 185/s** | 10 ms, 1767/s |

A wakeup sent inside a data transaction capped commits at about 189 a second. Sent after the commit, in its own transaction that doesn't wait for the disk, it costs nothing measurable. A lost wakeup is safe because every consumer also polls.
