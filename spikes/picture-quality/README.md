# Picture quality of a re-encode

> A measurement report, kept as written. Where it names a web client, another server, a task id or a document that is no longer in the tree, see [docs/engine/README.md](../../docs/engine/README.md), "Reading the code's comments". Its recommendations were for the earlier plan and are not decisions.

**Recommendation: move x264 from `veryfast` to `faster`, as one cluster-wide setting that is part of the spec hash, and reprice the x264 rows from a real film before it ships.** `faster` is where the picture stops improving quickly: it gains about 2.5 VMAF on average and 3.5 to 5 on the worst frames for 1.2 to 1.6 times the CPU (1.4 on live action), at nearly the same bitrate. It starts a film about 45% later on the same CPUs. `medium` adds about 0.5 more for 2.2 times the CPU. CRF 20, a higher bitrate ceiling and AV1 are separate proposals below, each with something to lose, for the owner to decide.

Measured 2026-11-19 on one workstation (Ryzen 7 7800X3D, 8 cores, 16 threads) that other work was using. Numbers are relative, not production capacity.

## Question

Every re-encode uses x264 `-preset veryfast -crf 23`, with `-maxrate` at the client's video bitrate and `-bufsize` twice that (`internal/transcode/ffmpeg.go`). What gives the best picture while the start, seeks and playback stay as fast as they are?

## What the app does today, found on the way

- **"No cap" does not exist.** `playback.NewSpec` holds the video bitrate to 6 × width × height bits per second. jellyfin-web's Auto (it sends 2147483647) becomes `-maxrate` 12.44 Mbps at 1920x1080 and 9.88 Mbps at 1920x858. A 12 or 20 Mbps cap is the same command as Auto, so the tables have three caps: 4 Mbps, 8 Mbps and Auto.
- **The ceiling ignores frame rate.** A 50 fps film gets the same bits per second as a 24 fps one.
- **x264 costs the same on ffmpeg 6.1 and 9.** The "about twice" in docs/transcoding.md is SVT-AV1's, not x264's (cost table below).

## Method

Scripts are indexed at the end. Each number comes from a CSV in `results/`; `results/tables.md` has every table, including per-clip ones not repeated here.

- **Encodes** use the app's video arguments (scale filter, `-profile:v high -level 51`, `bframes=2:b-adapt=0:open-gop=0:scenecut=0`, a forced keyframe every 4 s, the app's `-maxrate` rule), with only the preset, CRF or one x264 setting changed. Video only: no audio.
- **Sources.** Each clip is cut from an openly licensed 4K master, scaled once to 1920 wide and coded near lossless (x264 `slow`, CRF 10), which stands for a Blu-ray remux. The picture is measured against that 1080p source, as the server would see it.
- **VMAF** (model v0.6.1, libvmaf in ffmpeg 9.0.2), with PSNR and SSIM in the CSVs. Every fifth frame is scored, to make the matrix affordable; the splice test scores every frame. "Worst 5%" is the 5th percentile frame, which is what a viewer notices in a hard scene.
- **CPU** is user plus system seconds of the ffmpeg process per second of film, one encode at a time, pinned to 3 cores with their hyperthreads (6 logical CPUs), the least of two runs. The CPU column in the picture CSVs was taken with several encodes side by side and is not used for cost.
- **Latency** is the time from starting ffmpeg to a segment file being complete, with the app's HLS arguments, on a 60 s action clip, pinned to 1, 2 or 3 cores. It leaves out the claim, the upload and the player.

### Clips

| Id | Scene | Size, rate, length | Source bitrate |
|---|---|---|---|
| `tos_dark` | Live action, dim interior, slow | 1920x858, 24 fps, 20 s | 28 Mbps |
| `tos_action` | Live action with effects, fast motion, debris | 1920x858, 24 fps, 20 s | 41 Mbps |
| `tos_grain` | The dim interior with synthetic temporal noise added (`noise=alls=10:allf=t`), standing for film grain | 1920x858, 24 fps, 20 s | 183 Mbps |
| `sintel_dark` | Animation, dark cave | 1920x818, 24 fps, 30 s | 29 Mbps |
| `sintel_motion` | Animation, a chase | 1920x818, 24 fps, 25 s | 22 Mbps |
| `bbb_flat` | Flat, bright animation | 1920x1080, 30 fps, 21 s | 14 Mbps |
| `crowd_run` | Camera footage, a crowd and a pan, sensor noise | 1920x1080, 50 fps, 10 s (all there is) | 213 Mbps |

None of the open films has real grain, so the grain clip is synthetic. None comes from the owner's library.

| Clips | Download | Licence |
|---|---|---|
| `tos_*` | https://download.blender.org/demo/movies/ToS/tearsofsteel_4k.mov.zip (3840x1714 H.264, 73 Mbps) | CC BY 3.0 (https://mango.blender.org/about/) |
| `sintel_*` | https://download.blender.org/durian/movies/Sintel.2010.4k.mkv.zip (4096x1744 H.264, 41 Mbps) | CC BY 3.0 (https://durian.blender.org/about/) |
| `bbb_flat` | https://download.blender.org/demo/movies/BBB/bbb_sunflower_2160p_30fps_normal.mp4.zip (3840x2160 H.264, 8 Mbps) | CC BY 3.0 (https://peach.blender.org/about/) |
| `crowd_run` | https://media.xiph.org/video/derf/y4m/crowd_run_1080p50.y4m (uncompressed) | SVT multi-format test set; its terms are in the readme linked at https://media.xiph.org/video/derf/, which was not read here: unverified |

## Picture by preset

CRF 23, mean of the 7 clips: VMAF mean / worst 5% / Mbps (`results/presets.csv`).

| Preset | 4 Mbps cap | 8 Mbps cap | Auto |
|---|---|---|---|
| superfast | 83.1 / 74.2 / 4.14 | 89.1 / 84.0 / 6.53 | 91.1 / 86.1 / 7.56 |
| **veryfast (today)** | 83.6 / 75.5 / 3.49 | 88.7 / 82.9 / 5.24 | 90.6 / 85.0 / 6.22 |
| **faster** | 86.5 / 80.6 / 3.66 | 91.3 / 86.7 / 5.50 | 93.1 / 88.5 / 6.47 |
| fast | 87.0 / 81.1 / 3.71 | 91.7 / 87.2 / 5.66 | 93.3 / 88.9 / 6.63 |
| medium | 87.7 / 82.4 / 3.67 | 92.0 / 87.9 / 5.55 | 93.6 / 89.6 / 6.52 |
| slow | 88.1 / 82.6 / 3.62 | 92.2 / 87.8 / 5.50 | 93.9 / 89.6 / 6.46 |

- **veryfast to faster is the step.** +2.5 to +2.9 on the mean and +3.5 to +5.1 on the worst frames at every cap, for 4% more bits. On the action clip at 4 Mbps the worst frames go from 73 to 82.
- **After faster, the preset nearly stops mattering.** fast adds 0.2 to 0.5, medium 0.5 to 1.2, slow 0.8 to 1.6 over faster.
- **Where the cap binds.** At CRF 23 the 4 Mbps cap binds on five of seven clips; the 8 Mbps cap and Auto bind only on the grain and the 50 fps clips. Where the cap binds, the preset is the only lever, and it is worth the most there.
- **Hard content stays poor at any preset.** The 50 fps noise clip scores 44 to 54 at 4 Mbps and 79 to 85 at Auto; the grain clip 80 to 85 and 84 to 88. That is the bitrate, not the preset.

### Settings beyond the preset

One setting changed at a time (`results/settings.csv`).

On `fast`, CRF 18 under a 4 Mbps cap, so every variant spends the same bits (3 clips):

| Setting | VMAF | Worst 5% | CPU |
|---|---|---|---|
| `aq-mode=2` / `aq-mode=3` | +0.0 / -0.5 | +0.3 / -0.5 | ±1% |
| `rc-lookahead=40` / `60` (fast has 30) | +0.0 / +0.1 | +0.3 / +0.5 | +1 to 3% |
| `ref=3` / `ref=5` (fast has 2) | +0.0 / +0.0 | +0.1 / +0.2 | +9% / +18% |
| `bframes=3` / with `b-adapt=2` | -0.4 / -0.2 | +0.0 / -0.1 | +2% / +13% |
| `subme=7` | +0.5 | +0.9 | +11% |
| `trellis=2` | +0.2 | +0.1 | +12% |
| `-tune film` | -0.0 | +0.1 | 0% |
| `-tune grain` | +0.5 | +1.4 | +1% |
| `psy-rd=0` | +0.5 | +0.8 | 0% |

On `veryfast`, CRF 23 at Auto (4 clips), to see what carries the step to faster:

| Setting | VMAF | Worst 5% | Bits | CPU |
|---|---|---|---|---|
| `trellis=1` | +1.4 | +1.2 | +1% | +8% |
| `subme=4` | +0.5 | +1.0 | +3% | +34% |
| `rc-lookahead=20` | +0.4 | +0.4 | +3% | +1% |
| `subme=4` and `rc-lookahead=20` | +0.9 | +1.5 | +6% | +33% |
| `ref=2`, with or without `mixed-refs` | -0.1 | -0.1 to -0.4 | 0% | +6 to 11% |

- **Nothing beats choosing the preset.** More reference frames, more B-frames, adaptive B-frames, longer lookahead and the AQ modes are worth nothing measurable. So the fixed B-frames and closed GOP that restarts need (invariant 22) cost no picture.
- **`psy-rd=0` and `-tune grain` cannot be judged with VMAF.** VMAF rewards a smoother picture and penalises what the psychovisual settings do on purpose. Neither row justifies a setting; that needs eyes.
- **A cheaper tier may exist.** `veryfast` with `trellis=1` got more than half of faster's gain for 8% more CPU. It was measured on four clips at Auto only, not under a cap and not together with `rc-lookahead=20`: unverified as a default.

## CRF, and the ceiling

Mean of the 5 clips run at every CRF: VMAF mean / worst 5% / Mbps.

| Preset | CRF | 4 Mbps cap | Auto |
|---|---|---|---|
| veryfast | 23 | 89.6 / 82.1 / 3.28 | 92.0 / 86.9 / 5.12 |
| veryfast | 20 | 90.7 / 82.6 / 3.78 | 93.8 / 89.5 / 6.63 |
| veryfast | 18 | 90.9 / 82.4 / 3.97 | 94.6 / 90.7 / 7.66 |
| fast | 23 | 92.5 / 87.2 / 3.55 | 94.2 / 90.4 / 5.60 |
| fast | 20 | 93.0 / 86.9 / 3.94 | 95.3 / 91.9 / 7.15 |
| fast | 18 | 93.1 / 87.0 / 4.06 | 95.8 / 92.6 / 8.18 |
| medium | 23 | 93.1 / 88.5 / 3.50 | 94.6 / 91.0 / 5.48 |
| medium | 20 | 93.6 / 88.6 / 3.91 | 95.6 / 92.4 / 6.98 |

`faster` was not run at CRF 20 or 18; `fast` stands for it (they differ by 0.2 at CRF 23).

- **At Auto, CRF 20 adds 1.1 VMAF (1.5 on the worst frames) for 28% more bits and about 7% more CPU.** CRF 18 adds 1.6 for 46% more bits. The dim live-action clip gains most (93.0 to 95.4); the grain clip gains nothing, since it is already at the ceiling.
- **Under a 4 Mbps cap CRF does nothing** (+0.5, worst frames unchanged): the cap decides.
- **The preset is the cheaper lever in bits.** `fast` at CRF 23 (94.2 at 5.6 Mbps) matches `veryfast` at CRF 18 (94.6 at 7.7 Mbps) with 27% fewer bits. A preset costs CPU; a CRF costs every viewer's bandwidth and the transcode store.
- **A real 20 Mbps cap in place of the 6 bits per pixel ceiling** (`fast`, CRF 23): the 50 fps clip goes from 84.2 / 78.8 at 13.4 Mbps to 92.7 / 88.0 at 21.5 Mbps; the grain clip from 86.7 to 88.6 for twice the bits; the action clip does not change. The ceiling costs picture only on high frame rate and very noisy films.

## Cost

CPU seconds per second of film, CRF 23, on 6 logical CPUs (`results/cost-crf23.csv`). ffmpeg 9.0.2; ffmpeg 6.1.1 in brackets where it differs by more than 0.05.

| Preset | 1080p action, 24 fps | 1080p flat animation, 30 fps | 4K H.264 action | 4K 10-bit HEVC action | 4K flat animation |
|---|---|---|---|---|---|
| superfast | 0.88 | 0.70 | 1.65 | 2.76 (2.69) | 1.20 |
| veryfast | 1.36 | 0.94 | 2.40 (2.46) | 3.47 | 1.49 |
| faster | 1.92 | 1.48 | 3.27 | 4.28 (4.16) | 1.95 |
| fast | 2.52 | 1.81 | 4.18 | 5.12 | 2.27 (2.41) |
| medium | 3.14 | 2.19 | 5.07 | 5.94 (6.51) | 2.79 |
| slow | 4.35 (4.52) | 2.90 | 7.26 | 8.09 (7.78) | 3.48 |

- **Against veryfast:** faster 1.2 to 1.6 times (1.4 on the 1080p action clip), fast 1.5 to 1.9, medium 1.7 to 2.3, slow 2.3 to 3.2. From a 4K source the ratio is smaller, because decoding and scaling do not change with the preset.
- **CRF 20 costs 0 to 16% more** than CRF 23, 7% on the action clip (`results/cost-crf20.csv`): faster on the 1080p action clip 2.05, on the flat animation 1.71, from the 4K HEVC source 4.27.
- **The grain and 50 fps clips cost far more** (veryfast 2.3 and 4.0), but their sources are 183 and 213 Mbps, which no library film is, so part of that is decoding. They are in the CSV and not used for pricing.
- **These figures are not on the basis of today's prices.** docs/transcoding.md measured 0.76 CPU seconds per second for veryfast at 1080p, unpinned on 16 threads on another clip, and prices it at 0.8. Here veryfast needs 0.94 to 1.36 on six busy hyperthreads. The unpinned calibration did not run, so how much of the gap is the film and how much is hyperthreads sharing cores is unverified. The ratio between presets has the same basis on both sides and is the solid result.

## Time to the first segment

Seconds from ffmpeg's start to the first segment / to the fourth (a whole 16 s chunk), CRF 23 at Auto (`results/latency-crf23.csv`). "Seek" starts 32 s into the clip. The least of two runs; three of the 4K rows on 3 cores were measured later under other load.

| Source | Preset | 1 core (2 threads): start | seek | 2 cores (4 threads): start | seek | 3 cores (6 threads): start | seek |
|---|---|---|---|---|---|---|---|
| 1080p | superfast | 1.76 / 5.47 | 2.12 / 6.99 | 1.05 / 3.00 | 1.21 / 3.69 | 0.75 / 2.01 | 0.88 / 2.55 |
| 1080p | veryfast | 2.56 / 8.06 | 3.24 / 10.58 | 1.52 / 4.61 | 1.83 / 5.73 | 1.04 / 2.95 | 1.27 / 3.72 |
| 1080p | faster | 3.82 / 12.67 | 4.51 / 15.29 | 2.25 / 7.06 | 2.62 / 7.97 | 1.48 / 4.46 | 1.67 / 5.18 |
| 1080p | fast | 4.78 / 15.76 | 5.83 / 19.45 | 2.69 / 8.54 | 3.28 / 10.64 | 1.91 / 5.62 | 2.24 / 6.80 |
| 1080p | medium | 5.48 / 17.84 | 7.02 / 26.94 | 3.23 / 9.63 | 3.97 / 12.42 | 2.26 / 6.43 | 2.90 / 8.39 |
| 4K | superfast | 3.78 / 12.69 | 4.29 / 14.59 | 2.13 / 6.54 | 2.25 / 7.26 | 1.42 / 4.28 | 1.55 / 4.72 |
| 4K | veryfast | 5.44 / 17.14 | 5.71 / 19.29 | 2.70 / 8.60 | 3.17 / 10.18 | 2.00 / 5.78 | 2.16 / 6.63 |
| 4K | faster | 6.97 / 23.45 | 7.97 / 29.57 | 3.79 / 12.52 | 4.28 / 13.63 | 2.62 / 8.18 | 3.04 / 9.22 |
| 4K | fast | 8.83 / 31.41 | 10.66 / 35.37 | 4.68 / 15.16 | 5.62 / 17.90 | 3.47 / 10.44 | 3.72 / 11.41 |
| 4K | medium | 10.13 / 33.03 | 12.64 / 41.15 | 5.65 / 17.56 | 6.37 / 20.14 | 3.82 / 11.15 | 4.44 / 13.49 |

- **On the same CPUs, a 1080p film starts about 45% later at faster than at veryfast, and about 115% later at medium** (from 4K, 30% and 90%). On 3 cores that is 1.04, 1.48 and 2.26 s; on 1 core 2.56, 3.82 and 5.48 s.
- **The start follows the CPUs a job gets, which on a full worker is its price.** `transcodeCores` counts the processors a worker may use, so a price of 2.0 is the "1 core (2 threads)" column. A 1080p job priced at 2.0 with `faster` gets its first segment in 3.8 s and a chunk in 12.7 s. Today's job, priced at 0.8, has less than that column's 2.6 s and 8.1 s; nothing was measured at 0.8 CPUs, and scaling by the CPUs gives about 3.2 s: unverified. So on a full worker `faster` starts at about the time veryfast does today or up to a second later, and on an idle worker of 6 threads 0.45 s later (1.04 to 1.48 s).
- **Real time needs a 16 s chunk in under 16 s; helpers come below 1.2 times real time (invariant 58), so in under 13.3 s.** By the fourth-segment times, a 1080p job keeps that on 2 logical CPUs at veryfast (8.1 s) and faster (12.7 s) but not at fast or medium; on 4 at every preset. A 4K job needs 4 at veryfast and faster (8.6 and 12.5 s) and 6 at fast and medium.
- A seek's first segment comes 5 to 30% later than a start's.

Not measured: a start through a real worker and a player, and anything with helpers running. What is said about helpers here is derived from invariant 58 and the table: unverified.

### Designs for a faster start with a slower preset

| Design | What it gives | What it costs or breaks |
|---|---|---|
| **Price the job at what the preset needs** (recommended) | A full worker keeps the job at real time without helpers, and starts it in 3.8 s at 1080p, against today's 2.6 to about 3.2 s; no new mechanism | Fewer viewers per worker; up to a second on a start |
| A faster preset for the first chunk after a start or seek | 0.4 s (6 threads) to 1.3 s (2 threads) sooner at 1080p with veryfast under faster | Breaks playback as the arguments stand (next section). Which segments are the fast ones depends on where a job began, so the stored picture differs with who seeked where, against the spirit of invariant 22 |
| A faster preset for segment 0 of every film, by position | Deterministic, so invariant 22 holds | Helps a start at 0 only, not a seek or a resume, which are most starts. Same decoding limit |
| Several workers on the first chunk | Segments 2 to 4 arrive sooner | The first segment is still one encoder's 4 s of film plus its lookahead, so the start does not move. An ffmpeg start on other workers for every start and seek. Unverified: not run |
| A helper queued at once, not after 2 s, when the job's worker cannot give it real time | Fewer slow first chunks under load | Cores on another worker for every start and seek, also those a viewer leaves at once. Unverified: not run |

## Mixed presets under one init segment

A spec has one init segment, which carries the H.264 parameter sets. Segments made at another preset must decode under it (`60-mixed.sh`, `61-mixed-chrome.mjs`; `results/mixed.txt`, `results/mixed-chrome.txt`).

| Init and segment 0, then segments 1 to 3 | ffmpeg 9 | Chromium 158 (MSE) |
|---|---|---|
| veryfast, veryfast (control) | clean | clean |
| veryfast, then faster, fast, medium or slow | errors, VMAF 3 to 35 | `PIPELINE_ERROR_DECODE` at the first such segment (run for fast and medium) |
| medium or slow, then veryfast | errors, VMAF 2 | decode error (run for medium) |
| superfast, then veryfast | clean | not run |
| `ref=3` on both: veryfast then faster | clean | not run |
| `ref=3` on both: fast then medium or slow | clean | exact (run for fast then slow) |
| `ref=3` on both: veryfast then fast or slow | clean by VMAF, which reads luma only | plays, but pixels differ from the control by 1.5 to 3.6 of 255 |
| `repeat-headers=1` on both: veryfast then medium, and the reverse | clean | exact |

- **With today's arguments, two presets cannot share a spec.** Chromium stops with a decode error. This is also what happens if an operator changes the preset, or a rolling upgrade changes it, while the store holds segments of the old one under the same hash.
- By the dump in `results/mixed.txt`, the SPS is the same from superfast to medium and differs only for slow; the PPS has four forms (superfast and veryfast; faster; fast; medium). With `ref=3` on all, superfast, veryfast and faster write the same sets, as do fast, medium and slow. So veryfast segments under a `faster` stream are possible with `ref` fixed; that pair decoded cleanly in ffmpeg and was not run in Chromium.
- Repeating the parameter sets in every keyframe decodes exactly in Chromium 158. Other players were not tried: unverified.

## Bigger levers than the preset

Mbps needed for the same VMAF with no cap, interpolated between CRFs, against x264 veryfast (`results/codecs.csv`; 3 clips: action, grain, flat animation).

| Encoder | Bits for VMAF 90 | Bits for VMAF 95 | CPU s per s |
|---|---|---|---|
| x264 veryfast | 0% | 0% | 1.8 |
| x264 fast | -13% | -14% | 3.1 |
| x265 veryfast | -37% | -35% | 5.4 |
| x265 fast | -39% | -38% | 6.3 |
| SVT-AV1 preset 10 | -39% | -32% | 1.6 |
| SVT-AV1 preset 8 | -57% | -51% | 2.8 |

- **AV1 is the larger lever where a client decodes it.** SVT-AV1 preset 10 gave the picture of x264 veryfast with a third fewer bits at about the same CPU, and preset 8 with half the bits at the CPU of x264 fast. x265 saves as much as preset 10 for three times the CPU.
- **Limits of this table.** The CPU column was taken with other encodes running, on ffmpeg 9 with SVT-AV1 4.2 only; docs/transcoding.md has SVT-AV1 1.7 on ffmpeg 6.1 at about twice the CPU. SVT-AV1 ran as one encode with a keyframe every 4 s, not as the app's fallback does (one run for each segment). Some cells are out of the CRF range run (the grain clip never reaches 95). Time to the first AV1 segment, seams between AV1 runs and what decoding AV1 costs a client were not measured.
- **What Jellyfin 12.4 offers.** jellyfin-web in Chrome sends `VideoCodec=av1,h264,vp9` for its HLS transcode, and Jellyfin still encoded with libx264: its defaults are `AllowAv1Encoding:false`, `AllowHevcEncoding:false`, `H264Crf:23`, `H265Crf:28`, `EncoderPreset:auto`, which gave `veryfast` (`compat/captures/transcode-12.4.md`). So a client that asks for AV1 first already exists, and jellyscale would be ahead of Jellyfin's default by answering it.
- **A smarter bitrate.** Capped CRF is what the app already does. CRF 20 and a ceiling that follows frame rate are the two changes with a measured gain (above).
- **Hardware encoders** (nvenc, qsv, vaapi) are in this ffmpeg and were not measured: workers are CPU-only today.

## Recommendation

1. **Default: x264 `faster`, CRF 23, the other arguments as today.** +2.5 VMAF on the mean and +3.5 to +5 on the worst frames at every cap, for 4% more bits and 1.2 to 1.6 times the CPU (1.4 on live action). `medium` is not worth 2.2 times the CPU for 0.5 more, and `slow` less so.
2. **One operator setting for the whole cluster, in the spec hash.** For example `transcodePreset`: `veryfast`, `faster` (default), `medium`. It must be the same on every process, checked as `transcodeStore` is, and part of `Spec.Hash`, because segments of two presets under one init segment fail in Chromium. A worker-by-worker or load-dependent preset is ruled out for the same reason. It must reach both x264 sites: the `x264` encoder and `recodeArgs`.
3. **Start and seeks stay fast by pricing first.** A job that gets the CPUs its preset needs keeps real time on a full worker without helpers, and starts at most about a second later than today (3.8 s against 2.6 to about 3.2 s for a hard 1080p film; 0.45 s later on an idle worker). If that second matters, the one design that fits is veryfast for the first segment after a start or seek under a `faster` stream with `ref` fixed (task 8): it must be shown to decode in Chromium first, and it makes the stored picture depend on where viewers seeked.
4. **Reprice the x264 rows from a real film, measured through a worker.** The measured ratio is the firm part: `faster` needs 1.4 to 1.6 times veryfast's CPU from a 1080p source and 1.2 to 1.4 times from a 4K one. As a proposal, from the fourth-segment times and the 1.2 times real time that keeps helpers away:

   | Row | Today | Measured need at real time, faster | Proposed |
   |---|---|---|---|
   | 1080p SDR | 0.8 | about 1.6 logical CPUs (12.7 s a chunk on 2) | 2.0 |
   | 4K SDR | 1.6 | about 2.9 to 3.1 (23.5 s a chunk on 2, 12.5 s on 4, H.264 source; a 10-bit HEVC source costs about 1.0 CPU s per s more to decode) | 3.6 to 4.4 |
   | Tone mapped rows | 2.0, 3.2 | not measured | today's plus the encode's increase: unverified |

   These come from one hard action clip on busy hyperthreads. They also say today's 0.8 is low for such a film at veryfast (about 1.0 at real time, 1.2 with the helper margin). A 4-core worker would hold two 1080p viewers where it holds five. Of that fall, 1.4 times is the preset; the rest corrects today's price, on a basis that is unverified until task 2 measures it. So this is the owner's decision; if cores are short, `veryfast` stays available as a setting.
5. **Owner decisions, each a proposal with what it costs:**
   - **CRF 20** for +1.1 VMAF at Auto: 28% more bits for every viewer on Auto and in the transcode store, 7% more CPU. Nothing under a low cap.
   - **A ceiling that follows frame rate** (or 20 Mbps when the client allows it): +8 VMAF on 50 fps noise, +2 on grain, for up to twice the bits on those films, and a change to the storage ceilings in docs/transcoding.md.
   - **AV1 re-encodes for clients that list AV1**: a third to a half fewer bits. A larger build, with its own seam, start-time and pricing work.

## Follow-up tasks

Each is one capability, small enough for one build.

1. **A cluster-wide x264 preset setting, in the spec hash.**
   - `transcodePreset` accepts `veryfast`, `faster` and `medium`; any other value stops the start, whatever the role.
   - Both x264 commands (a re-encode's and an H.264 copy's fallback) carry the configured preset; a test reads the arguments of each.
   - Two specs that differ only in the preset have different hashes, and a store holding segments of one never serves them for the other (a test with real segments of both, played through as one stream, fails today in Chromium with a decode error).
   - Processes whose presets differ are refused as differing `transcodeStore` values are, and the log names both.
   - The default stays `veryfast` in this task.
2. **Price x264 by preset, from a film measured through a worker.**
   - A test logs CPU seconds per second of film for each preset from a 1080p and a 4K real-film source through a worker's own command, on ffmpeg 6.1 and 9, as the AV1 rows are measured.
   - `Spec.Cost` returns a row for each preset, each above its measured figure; docs/transcoding.md's table and sizing text match.
   - With real processes: a worker holding jobs that add up to its cores keeps each at 1.2 times real time or more without a helper.
   - The capacity change (viewers per worker) is listed in the pull request.
3. **Make `faster` the default.**
   - Depends on 1 and 2.
   - An e2e start and a seek in jellyfin-web, timed before and after on the same worker: the first segment comes no more than 1.5 times later than with `veryfast` (this table: 1.42 to 1.49), and playback does not stall.
   - The picture check: VMAF of a real-film clip against its source is at least 2 higher than with `veryfast` at a 4 Mbps cap. It needs libvmaf; whether CI's ffmpeg 6.1 has it is unverified, and a tier without it must fail, not skip.
4. **CRF as part of the setting (if the owner accepts the bits).**
   - CRF 20 at Auto and under caps; the hash changes with it.
   - docs/running.md's transcode store sizing states the new average and the unchanged ceiling.
5. **A bitrate ceiling that follows frame rate.**
   - The ceiling is 6 bits per pixel a second at 24 to 30 fps and scales with the frame rate above that, never over the client's cap.
   - The storage ceilings in docs/transcoding.md are restated; the change is listed as a changed guarantee.
6. **Spike: AV1 re-encodes for clients that ask for AV1.**
   - Time to first segment, seams between runs and CPU on ffmpeg 6.1 and 9 for SVT-AV1 presets 10 and 8 from real film, and whether jellyfin-web plays it with hardware and with software decoding.
7. **Spike: `veryfast` with `trellis=1` and `rc-lookahead=20` as the cheap tier.**
   - The preset table's caps and clips for that one variant, and its CPU, to decide whether it replaces `veryfast` in the setting.

8. **Spike: veryfast for the first segment after a start or seek.**
   - With `ref` fixed, a `faster` stream whose first segment after a start is veryfast plays in Chromium with pixels equal to each segment's own decode; what it saves in a real start; what the store then holds.

## Not measured

- Starts, seeks and helpers in a real cluster, and start time as a viewer sees it.
- CPU on an idle machine or under a cgroup limit, and for tone mapped sources.
- Any player other than Chromium 158 for the mixed-preset splices; three of the splice pairs in Chromium.
- `faster` at CRF 20 and 18 (fast and veryfast stand either side of it).
- AV1 and HEVC cost on ffmpeg 6.1, their start time, and client decoding.
- Whether the psychovisual settings or `-tune grain` look better: VMAF cannot say.
- Real film grain: the grain clip is synthetic.

## Scripts

Run from this directory. Scratch files go under `$SPIKE_SCRATCH` (default `~/.cache/jellyscale/spike-picture`). Every script skips work it has already done.

| Script | What it does | Run as |
|---|---|---|
| `lib.sh` | Shared: the app's x264 arguments and `-maxrate` rule, timing, VMAF | sourced |
| `00-clips.sh` | Downloads the films (about 14 GB) and cuts the clips; writes `results/clips.txt` | `./00-clips.sh` |
| `one.sh` | One encode, measured, as a CSV line | called by the next three |
| `10-presets.sh` | Presets by cap, CRF 20 and 18, the real 20 Mbps cap; `results/presets.csv` | `CPUS=0-4,8-12 JOBS=4 ./10-presets.sh` |
| `20-settings.sh` | One x264 setting at a time; `results/settings.csv` | `CPUS=0-4,8-12 JOBS=4 ./20-settings.sh` |
| `30-codecs.sh` | x264, x265 and SVT-AV1 at several CRFs, no cap; `results/codecs.csv` | `CPUS=0-4,8-12 JOBS=4 ./30-codecs.sh` |
| `gather.sh` | Joins a set's lines into its CSV | called by the three above |
| `40-cost.sh`, `cost-one.sh` | CPU per second of film, ffmpeg 9 and the ffmpeg 6.1 image of `test/ffmpeg61.sh` (needs Docker) | `CPUS=5-7,13-15 ./40-cost.sh 23` and `CPUS=5-7,13-15 PRESETS="veryfast faster fast medium" ./40-cost.sh 20` |
| `50-latency.sh` | Time to the first four segments on 1, 2 and 3 cores; `results/latency-crf23.csv` | `./50-latency.sh 23` |
| `60-mixed.sh` | Parameter sets by preset and spliced streams in ffmpeg; `results/mixed.txt` | `KEEP=1 CPUS=0-4,8-12 ./60-mixed.sh` |
| `61-mixed-chrome.mjs` | The same splices in Chromium's MSE; needs the segments `KEEP=1` leaves in `$SPIKE_SCRATCH/enc/mixed` | `cd <a directory with playwright-core> && node <this directory>/61-mixed-chrome.mjs $SPIKE_SCRATCH/enc/mixed` |
| `summarize.py` | Every table from the CSVs; `results/tables.md` | `python3 summarize.py > results/tables.md` |

The CPU sets are this machine's (`lscpu -e`: CPU n and n+8 share a core). The picture runs and the cost runs were kept on separate cores so that they could run together.
