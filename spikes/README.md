# Phase 0 report

**Recommendation: build it.** Everything testable on this machine supports the architecture, with the design changes below. Four things can't be tested here; see Remaining risks.

Measured on one workstation: 16 threads, local NVMe, Postgres 18.6 and stores in Docker, RTX 4070 Ti, AMD iGPU. Numbers are relative, not production capacity.

## Design changes from Phase 0

1. **fMP4 is the default segment format.** Jellyfin's own web client already uses it, and it's the only format that passes seams with no player tolerance. MPEG-TS is a fallback.
2. **Encoder family is part of `spec_hash`.** A job fails over only within its family. If none is available, the client restarts playback rather than splicing.
3. **`pg_notify` never goes in a data transaction.** Hints use a separate `synchronous_commit=off` transaction.
4. **Keyframe index is mandatory for S3.** It's also what identifies remux segments.
5. **Serve-side HEAD and proxying stay.** Presigned URLs reject HEAD, and ffmpeg and mpv forward `Authorization` across redirects.
6. **Obsolete routes become a tracked exception list,** not a blanket ban, because jellyfin-web needs some of them.

## Results

| Question | Result | Decision |
|---|---|---|
| Can a restarted job replace segments? | 23 of 25 fMP4 cases clean; the 2 failures mix encoder families. See [seam](seam/README.md) | **Go** on fMP4 within a family |
| Can Postgres NOTIFY be the only bus? | Inline about 189 commits/s; async hints at least 1,767/s | **Go** with async hints |
| Can object stores keep up? | 2MB puts p95 under 170ms at 64 writers | **Go** |
| What do S3 reads cost? | Probe under 6MB; seek about 22MB, or 325MB for MKV with no Cues | **Go** with keyframe index |
| Are redirects safe? | All 4 tested players follow them, but proxying is safer | **Proxy by default** |
| What do clients really call? | Captured 34 routes from jellyfin-web 12.4 | See below |

## NOTIFY

`spikes/notify`, 8 listeners, 15s per step, commit latency p50:

| Target rate | No NOTIFY | Inline | Separate async transaction |
|---|---|---|---|
| 100/s | 5.55ms | 5.58ms | 5.74ms |
| 500/s | 8.34ms | **343.64ms, 189/s** | 8.63ms |
| 2000/s | 8.44ms, 1133/s | **341.24ms, 189/s** | 9.05ms, 1171/s |
| 5000/s | 8.26ms, 1538/s | **347.95ms, 185/s** | 9.97ms, 1767/s |

The async ceiling is a lower bound, since the harness also limits the baseline. A lost hint is safe because consumers poll. Failover and synchronous replicas are untested.

## Object stores

`spikes/objstore`, single-node SeaweedFS and Garage in Docker. Not representative of a cluster or a network path.

| Operation | SeaweedFS | Garage |
|---|---|---|
| 2MB put, 16 writers, p50 / p95 | 21.4 / 30.4ms | 30.9 / 44.5ms |
| 2MB put, 64 writers, p50 / p95 | 95.6 / 161ms | 134.7 / 168.9ms |
| Put throughput at 16 writers | 696.9/s | 473.6/s |
| List 2,000 objects, size and ETag included | 32ms | 28ms |
| Presigned GET with range | 206 | 206 |
| Presigned HEAD | **403** | **403** |
| GET with `Authorization` header added | 206 | 206 |

200 streams need about 50 puts/s. Neither store rejected a duplicate `Authorization`; AWS S3 itself wasn't tested.

## Read cost

`spikes/s3cost`, 606MB 600s 1080p file, range-logging server with 20ms injected latency.

| File | Probe requests | Probe MB | Seek requests | Seek MB |
|---|---|---|---|---|
| MP4, moov at front | 1 | 2.69 | 2 | 22.02 |
| MP4, moov at end | 3 | 5.74 | 4 | 24.09 |
| MKV with Cues | 1 | 2.69 | 4 | 21.43 |
| MKV without Cues | 1 | 2.69 | 3 | **325.26** |

## Redirects

`spikes/redirect`, 302 to a second origin that rejects HEAD like a presigned URL. ffmpeg, mpv, GStreamer and Chromium `<video>` all followed it. Seeks worked in ffmpeg, mpv and Chromium; GStreamer read only. ffmpeg and mpv forwarded `Authorization` to the origin when set. ExoPlayer and AVPlayer are untested.

## Client compatibility

From a real Jellyfin 12.4.0 with jellyfin-web, covering login, browsing, direct play, forced transcode and seek. Routes are in `compat/captures/jellyfin-web-12.4.json`.

- Transcoding uses `/videos/{dashed-id}/master.m3u8`, `main.m3u8`, then `hls1/main/-1.mp4` for init and `{n}.mp4` for media.
- Paths arrive in mixed case: `/videos/`, `/Videos/`, `/users/public`, `/Users/authenticatebyname`.
- Direct play uses `Videos/{id}/stream.mkv?Static=true`.

v12.4 hides 55 routes from the spec, 41 of them marked `[Obsolete]` and 34 of those under `/Users/{userId}/`; see `compat/hidden-routes-12.4.txt`. It's a text parse of `Jellyfin.Api/Controllers`.

Client use of those obsolete routes, from source at current `HEAD`:

| Client | Obsolete `/Users/{id}/...` use |
|---|---|
| jellyfin-web, and apps that wrap it | **Yes**, through `jellyfin-apiclient`; `Items/{id}` and `Items/{id}/Intros` were captured live |
| Kodi add-on | **Yes**, core: `Items`, `Views`, `PlayedItems`, `FavoriteItems`, `Suggestions` |
| Android TV, Swiftfin, Findroid, Streamyfin | None found in source |

Only jellyfin-web was captured. The others are source reads, and Android TV, Swiftfin, Findroid and Streamyfin weren't run.

## Remaining risks

1. **AVPlayer, and so Swiftfin and Apple TV.** fMP4 removes the known spec violation, but it still needs a run on an Apple device.
2. **ExoPlayer, and so Android clients.** Android tooling isn't installed here.
3. **Real workloads.** No real film content, HDR, AV1, multiple audio tracks or burned-in subtitles. No time-to-first-frame measurement.
4. **Production infrastructure.** No CloudNativePG, replicated storage, WAN object storage, or concurrent-stream load test.
