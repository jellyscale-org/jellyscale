# Spikes

Small experiments that answered a question with a measurement before something was built on the answer. Each directory holds the harness that made the measurement, so it can be run again.

Measured on one workstation (16 threads, local NVMe, Postgres and stores in Docker). Numbers are relative, not production capacity. What the engine does with each result is in [docs/engine/](../docs/engine/README.md).

| Spike | Question | Result |
|---|---|---|
| [seam](seam/README.md) | Can a fresh ffmpeg, restarted at segment N, replace segments from an earlier run with no break? | Yes with fMP4, within one encoder family: 23 of 25 cases clean, the 2 failures mix CPU and GPU encoders |
| `notify` | Can Postgres wakeups be the only message path? | Yes, sent outside the data transaction. Inside it, commits capped at about 189 a second; outside, at least 1,767 a second |
| `objstore` | Can object stores keep up with segment writes? | Yes: 2 MB writes under 170 ms at p95 with 64 writers, on SeaweedFS and Garage. Presigned HEAD is refused by both |
| `s3cost` | What does reading a film from object storage cost? | A probe under 6 MB. A seek about 22 MB, or 325 MB for a Matroska file with no index |
| `redirect` | Do players follow a redirect to storage? | ffmpeg, mpv, GStreamer and Chromium all do. ffmpeg and mpv forward the `Authorization` header |
| [picture-quality](picture-quality/README.md) | Which x264 preset gives the best picture without slowing a start? | `faster`: +2.5 VMAF on average for 1.2 to 1.6 times the CPU. Two presets can't share one stream |
| `pathsecret` | Can a player hold a link with its secret in the path across a long pause, and what does it report when the link is refused? | Media3 1.11.1: yes across a 30-minute pause and a seek. A refused link raises load errors, not a player error ([docs/players.md](../docs/players.md)) |

## Not tested by any spike

- Apple's player, and Android's beyond the one `pathsecret` run.
- Real film content at scale: sources were synthetic or short clips.
- Production infrastructure: replicated Postgres or storage, object storage over a wide-area network, a load test with video.
- AWS S3 itself.
