# What jellyfin-web does when an HDR copy goes on as another stream, and what that stream is (JS-134)

JS-088 and JS-133 send an encode of the copy's codec in place of a copy whose keyframe index was found wrong, each segment under
its own init in front. Their picture is always `format=yuv420p`, 8 bits. The question: for an HDR10 or HLG copy, copied to a
client whose profile shows the range (master `VIDEO-RANGE=PQ` or `HLG`), what is that picture today, what should it be, and does
the player take it? Numbers are in hdr-fallback-12.4.json; the commands are below.

## What ffprobe says (ffmpeg 9.0.2, x265 and SVT-AV1 4.2.0; the same on Ubuntu 24.04's ffmpeg 6.1.1 unless said)

`compat/captures/hdr-fallback-observe.sh <empty workdir>` makes a 12 s 1080p HDR10 HEVC film (x265, `master-display`, `max-cll`) and
an HDR10 AV1 one (SVT-AV1, the same mastering display), copies the first 4 s as the worker's CopyCommand does, and makes the
fallback of that segment with the arguments of `recodeArgs`, `fragmentCommand` and `AV1SegmentCommand` (`internal/transcode/ffmpeg.go`).
`FALLBACK=tonemapped` (the default) is today's, `FALLBACK=plain` is the same without the tone map, `FALLBACK=10bit` is the 10-bit
picture, `format=yuv420p10le` in place of `format=yuv420p` and no colour flag. Under 6.1: `docker run ... jellyscale-ffmpeg61 bash compat/captures/hdr-fallback-observe.sh <dir>`
(the image test/ffmpeg61.sh builds). Each is probed for the init segment alone and for the frames of the segment with its init in front.

| | init segment alone | frames |
|---|---|---|
| HEVC copy | Main 10, yuv420p10le, bt2020 / smpte2084 / bt2020nc, tv | all 10 bit PQ; mastering display and content light level (1000, 400) |
| HEVC, today's fallback | Main, yuv420p, bt709 / bt709 / bt709, tv | all 8 bit BT.709; mastering display and content light level still there on ffmpeg 9 (describing a picture that is no longer that), none on 6.1 |
| AV1 copy | Main, yuv420p10le, bt2020 / smpte2084 / bt2020nc, tv | all 10 bit PQ; mastering display and content light level |
| AV1, today's fallback | Main, yuv420p, bt709 / bt709 / bt709, tv | all 8 bit BT.709; metadata as for HEVC |
| either, `FALLBACK=plain` (no tone map) | 8 bits, bt2020 / smpte2084 / bt2020nc | 8-bit samples tagged PQ: the failure the task feared, not what today does |
| either, `FALLBACK=10bit` | as the copy's (ffmpeg 9.0.2 and 6.1.1) | 10 bit with the copy's four tags, no colour flag given; mastering display and content light level kept on ffmpeg 9 and lost on 6.1 |

Today's fallback of an HDR copy is tone mapped: `internal/api/transcode.go` `goOn` gives `Recoded` the source's tone map, so
the command carries `zscale`, `tonemap=hable` and `format=yuv420p` (playback.ToneMapFilter). The picture is SDR, 8 bits, BT.709,
under a master playlist that says PQ and a SourceBuffer typed for a 10-bit codec. A copy's own Spec.ToneMap is empty; the
fallback's is not.

## Observed: jellyfin-web 12.4's hls.js 1.6.16 in Chrome

`compat/captures/hdr-fallback-streams.sh av1 <dir>` makes A (the copy: 10-bit PQ AV1, a keyframe every 4 s, AAC), B (today's
fallback of it: tone mapped, 8 bits BT.709) and C (the candidate: 10 bits, the copy's tags, no tone map). `hdr-fallback-spike.mjs`
serves a master playlist `CODECS="av01.0.04M.10",VIDEO-RANGE=PQ` and a 12 segment playlist of 4 s whose segments 0 to 4 are A and 5
to 11 are B or C, each fallback segment under its own init, and A's segments under A's init once a fallback segment was answered
(as the server does). Played forward from 14 s across the change at 20 s, then seeked back to 10 s (a copy segment: a 10-bit init
after an 8-bit one) and forward to 30 s across it again. Run:
`STREAMS=<dir> CODEC=av1 HLS_CHUNK=web-dist/node_modules.hls.js.<hash>.chunk.js PW=<dir with playwright-core> CHROMIUM_PATH=/usr/bin/google-chrome-stable node compat/captures/hdr-fallback-spike.mjs`.
The colours are the mean RGB of a 64x36 canvas draw of the video before (16 to 19.9 s) and after (20.1 to 24 s) the change.

| the later segments are | direction | frames shown | video.error | a dialog | currentTime keeps its place | colours Chrome draws, before and after |
|---|---|---|---|---|---|---|
| the copy all through (control) | forward | 394 | none | no | yes | 119, 105.7, 126.1 and 117.1, 105.7, 127.5 |
| | back, forward | 487 | none | no | yes | the same |
| today's fallback (8 bits, tone mapped) | forward | 392 | none | no | yes | 119, 105.7, 126 and 122.5, 107.8, 129.1 |
| | back, forward | 487 | none | no | yes | 119, 105.7, 126 and 122.5, 107.8, 129.1 |
| 10 bits, the copy's tags | forward | 393 | none | no | yes | 119, 105.7, 126 and 117.1, 105.7, 127.4 |
| | back, forward | 487 | none | no | yes | 119, 105.7, 126.1 and 117, 105.8, 127.5 |

The player takes today's 8-bit fallback and the 10-bit one in both directions, under a 10-bit codec string, with each segment
under its own init. What differs is the picture: the control and the 10-bit fallback draw the same colours before and after the
change to within a unit of noise, the tone-mapped one shifts by 3 to 5 units. Chrome here draws HDR video through its
own tone mapping to the screen, so the difference is the stream's, not a display's.

HEVC: this Chromium does not play `hvc1.2.4.L120.B0` (`MediaSource.isTypeSupported` is false), so HEVC is not observed; it plays only with a hardware
decoder (E2E_HEVC=1, where the e2e scenario observes it).

## The tags need no flags

The 10-bit recipe is `format=yuv420p10le` and nothing else: the decoded frames carry the copy's tags and libx265 and libsvtav1 write
them, on ffmpeg 9.0.2 and on Ubuntu 24.04's 6.1.1 (the init alone and the first frame, both codecs, for PQ with BT.2020 primaries, PQ
with `smpte432` primaries, and for `tv` and `pc` range; ffmpeg 6.1's encoders write `tv` whatever the frames say unless given the
encoder's own flag: x265 `range=full`, SVT-AV1 `color-range=1`, which the checks' films use). So a build that names bt2020 / bt2020nc / tv
itself, or derives the tags from the transfer, changes the primaries or range mid-film for a PQ film that has others (the range type is
derived from the transfer alone, internal/library/probe.go transferRangeType, so such a film is HDR10, is copied and falls back). The
checks hold the build to the copy's own tags with films of those kinds.

## The format

The fallback of an HDR copy is the picture at 10 bits with the copy's primaries, transfer, matrix and range, no tone mapping
(`choice` in the JSON): the player was not seen to refuse it, in either direction, so the rule gives it. A refusal would go to
the owner and would not be settled by sending another format.

## Not a finding of the player, a finding for the build: video timescale

The first runs had A at timescale 16000 (what ffmpeg gives a copy from Matroska) and B and C at 12288 (what an encode gets), and
the player stalled at 19.97 s with `bufferStalledError`: the fallback's segments were buffered from 24.64 s, 4.64 s late. With
AAC in the streams as without (an H.264 pair with audio went on, without it stalled alike). The streams script now gives all three
one timescale (`TIMESCALE`, default 12288) so the observation above is of the codec and the bit depth. The worker's own commands
give a copy from Matroska 16000 and a fallback 12288 (`ffmpeg -c copy` and `-c:v libx264` to the hls muxer, both from one mkv),
and the e2e gate plays the real stack: it is the arbiter whether this meets the player (unverified here).
