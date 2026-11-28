# What the engine does not do yet

Known gaps of the carried-over code, as facts for whoever plans and designs. Nothing here is a plan or a priority: the product owner decides what is closed and when. See [README.md](README.md) for how to read this.

## Against the product

- **No lighter steps.** A re-encode makes one output: H.264, SDR, at most 1080 lines. There is no ladder of qualities, no stepping with the connection, and nothing lighter to fall back to when a worker falls behind. A viewer on a 4K or HDR screen gets 1080p SDR from any film that must be re-encoded.
- **Audio is not its own stream.** Each audio track is a spec of its own with the picture, so a language switch makes the picture again. The join proof for separate audio has not been made.
- **A subtitle can still cost the picture.** An image track is drawn into the picture for any player that can't draw it, and a styled or plain text track too when the player shows none.
- **Software only.** No GPU decode, tone map or encode; HEVC and AV1 are encoded only as a copy's fallback, never as a first choice.
- **One x264 preset, `veryfast`**, not part of the spec.
- **Disc images and disc folders** are not read. Format support is unresearched.
- **Nothing is made before it is asked for:** no next episode ready before Play, no versions made ahead.
- **Nobody is told why a film stalls.** A film that runs under real time shows as freezes; the viewer gets no reason.

## Entangled with the earlier server

Bringing the engine over means cutting these:

- **A spec is built from a request's query string**, validated on every request. The product wants it built from a stored plan.
- **The decision reads another server's device statement** and answers in its shapes and reason names.
- **Viewers, stops and the "who is watching" bookkeeping are keyed by the earlier API's sessions and tokens.** The stream role's outage snapshot holds tokens.
- **The scan writes answers shaped for the earlier API** beside the facts the engine needs (streams, keyframes, cue points, chapters).
- **Tests replay recorded observations in `compat/captures/`**, including the device statement most transcode tests use.
- **The engine's log lines, metrics and refusal reasons** use the earlier API's words.

## For some films or some moments

- **Dolby Vision profile 5** is always re-encoded, and whether its colours come out right has never been observed.
- **A film whose image subtitles can't be copied out within an hour** keeps seeks that cost more the older the showing cue is ([subtitles.md](subtitles.md)).
- **A film with no cue points** (MPEG-TS, some Matroska files) can miss a showing image cue after a seek until its track is stored.
- **A film whose audio outlasts its video by 12 s or more** is re-encoded where it could be copied.
- **A film whose audio alone ends early** is not caught by the early-end check.
- **A subtitle file beside a film that is out of sync stays out of sync.**
- **During a Postgres switchover or restart** some requests are answered with a server error, and a subtitle can be refused for 30 s after a read that hit the outage.
- **A worker removed while films play** can cost a viewer a stall.
- **A store hiccup the reader recovers from** can still fail a subtitle, font or chapter-picture job.
- **Whether a text track is offered** can depend on the asset store's health at that moment.
- **A cheap second play can be refused** on a cluster that one shared heavy film fills.

## Capacity

- **A worker learns what it holds only from whole, dedicated cores.** On a shared host the figure can't see the competition.
- **The first heavy play after each worker start runs at the worker's own speed** until the viewer leaves: the learned figure is not kept across a restart, and lowering it ends no job.
- **A worker's own extraction slows its transcodes** (16 to 18% measured) and is not learned from. A lone worker can freeze a film while one runs.
- **A lone slow worker refuses what it can't hold.** A 4-core worker of an older generation holds about 2.2 to 2.3 reference cores against the 3.2 a 4K HDR re-encode costs.

## Never tried

- **Apple's player** has played none of the engine's streams. Nor has Android's, beyond one stand-in test of a link ([../players.md](../players.md)).
- **Real film content at scale:** the prices and most measurements come from synthetic sources.
- **A real cluster:** replicated Postgres, replicated or remote object storage, and a load test with video.
- **AWS S3 itself.**

## In the tests

- No test in the merge queue fails if subtitle files beside a film on S3 are refused, or if a drawn image subtitle draws nothing.
- A fresh install logs an error on every asset sweep because the assets location does not exist yet.
- [test-history.md](test-history.md) lists failures seen once and never explained.
