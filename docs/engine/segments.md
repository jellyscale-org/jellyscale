# Segments and jobs

Facts about `internal/transcode` and the layout code in `internal/playback`. See [README.md](README.md) for how to read this.

## What a segment is

- **fMP4, not MPEG-TS.** A join between two runs in MPEG-TS resets continuity counters, which breaks the HLS specification; fMP4 passed joins with no player tolerance (`spikes/seam`).
- **A re-encoded segment is 4 seconds**, the length the join spike validated. A copied segment follows the film's keyframes and is 4 to 12 seconds ([copy.md](copy.md)). Nothing reads a constant: the playlist, the segment holding a position and the worker's checks all read one layout.
- **Stored as** `segments/{spec hash}/{n}.mp4`, with `init.mp4` beside them, in the one transcode store every role shares.
- **A playlist is complete before anything is encoded:** every entry and an end marker, no discontinuity tags, computed from where the *video* ends. The muxer cuts on video, so audio that runs longer would leave entries nothing can produce. A tail under a second joins the segment before it in a re-encode.

## Why any worker can make any segment

- **A segment's frames depend on the spec and the segment number only**, not on which worker made it, where its job began or how fast it ran: the same frames at the same times, starting on a keyframe.
- **A copy's video bytes are identical** from run to run. **A re-encode's are not** (x264's threads and rate control; one thread repeats, every multi-threaded setting with a bitrate cap does not). Segments from different runs therefore meet as a join that players cross cleanly because the times line up. x264 keeps its threads: identical bytes would cost speed nothing needs.
- **Encoded audio bytes can differ after a restart**, because the encoder starts warm and does not continue. Decoded, the difference is at most about 0.36% of full scale (about -49 dB) on the test tone; the test bounds it at 1%. Copied audio is the source's own packets and does not differ ([copy.md](copy.md)).
- **fMP4 can't join across encoder families.** The init segment carries the encoder's parameters; x264 joined with a GPU encoder lost about half the frames. So the encoder family is part of the spec, and a job fails over only within its family. A copy and a re-encode are different families.

## The settings a join needs

From `spikes/seam/README.md`, and enforced by a test that fails if a restart drifts from a run from zero:

- Fixed B-frames and a closed GOP: for x264, `bframes=2:b-adapt=0:open-gop=0:scenecut=0`. x265 lost 3 frames at a join until the same was set.
- `-copyts`, keyframes forced every 4 seconds of output time, `use_editlist=0`, `avoid_negative_ts=disabled`, `+frag_discont`.
- Audio starts early on the 1024-sample AAC grid of a run from zero, and packets before the cut are dropped. The cut is the keyframe's decode time: its time rounded to a frame, less 2 frames of reorder delay. Taking 2 frames off the unrounded time duplicated one audio packet at the join when the film's start sat off the frame grid.
- A restart opens the video a quarter of a frame before its keyframe. ffmpeg 6.1 keeps a frame up to half a frame before `-ss` where ffmpeg 9 drops it, so a cut on the keyframe itself started one frame early on 6.1.
- No `-to`: a job's end is decided by who is watching, not by a time. The cost is one padded AAC frame past the picture at the end of a run that started late.
- Interlaced frames go through `yadif`, one frame out per frame in, so timing is untouched.

## The spec and the output version

- **The spec hash names one output:** the film's file and its revision, the audio track, video and audio bitrate and size as ffmpeg will use them, the segment length, the encoder family, whether video or audio is copied, a burnt-in subtitle track (and, for a file beside the film, that file's fingerprint), the transfer function of an HDR picture that is tone mapped, and the output version.
- **A field is added to the hash only when set**, so adding one orphans no stored segment.
- **Every job and every worker records an output version**, and a worker claims only jobs of the version it makes. Without it, a worker from before a release stored segments made the old way under the new hash, where viewers got them for the whole retention with nothing in any log.
- **A release that makes any output differently must raise the output version.** That changes every hash; old segments are swept after retention.
- **The copy layout is not in the hash.** A change to how a copy is cut, or to the parser that finds keyframes, needs the output version raised, or old segments are served under a new playlist.

## Starting, joining and ending a job

- **Any stream replica serves segment N:** from the store, else by waiting on a running job that will reach it (within three segments of its progress), else by starting a job at N. The check-and-start runs under one lock per spec, so concurrent requests start one job.
- **A request for segment N starts its job at N**, even below the position the viewer resumed at.
- **Viewers of one spec share its segments and its jobs.** A viewer's new request replaces that viewer's previous job unless another viewer is waiting on it.
- **A job lives while someone asks.** With no request for the spec in the idle time (60 s), it ends. Stopping a viewer marks them stopped, so a request still in flight can't bring the job back, and ends at once every job nobody else watches.
- **A job that makes no segment for 30 s is failed.** The lease is renewed whatever ffmpeg does, so without this viewers wait on a hung encode for good.
- **Cancelled is not failed.** A job cut short by a shutdown, a lost worker, a seek or a database outage is cancelled: viewers restart it and nothing is remembered against the spec. Only a job that ran and broke is failed.
- **A failure that is not the file's is retried** (a store refusing a write or cutting a read, ffmpeg killed by a signal): the same job is made again for the waiting request, at most three times. A failure that is the file's ends in the failure, and the spec is refused for 30 s so the same job is not started again at once.
- **A queued job nobody claims is refused after 6 s**, with whether no worker runs or none has room. That wait is also what lets a worker that stops gracefully be replaced without the viewer noticing.
- **A request never fails because Postgres is briefly away.** It waits, up to 90 s. Bytes already stored go on being served meanwhile.

## The end of the input

A segment the encoder flushes when its input ends is cut wherever the input stopped, and ffmpeg exits 0 either way. So a newest segment is stored only if it holds its whole place, or, once ffmpeg has exited, if it is the playlist's last and within 1.5 s of its listed length. Otherwise the job fails with "the input ended early". Limit: a film whose audio alone ends early is not caught.

## Read-ahead

- **A job runs at most the lead (60 s of film) ahead of the furthest segment its viewers asked for, then waits.** The worker stops ffmpeg with SIGSTOP and continues it with SIGCONT. Chosen because it behaves the same on ffmpeg 6.1, 8.1 and 9, needs no pipe, and leaves the input open at its place.
- **A paused job keeps its lease, its room on the worker and its scratch directory.** A worker's capacity therefore bounds the viewers it serves at once, not only the encodes running.
- **A paused job nobody is near any more is cancelled** and gives its room back. What it stored stays.
- **Nothing past the limit is stored.** ffmpeg 6.1 keeps its read-rate budget in wall time while stopped, so after a pause it reads unthrottled until the worker pauses it again (within 100 ms). ffmpeg 8.1 and 9 continue at the set rate.
- **A stopped ffmpeg keeps its input.** A copy from S3 paused for 20 s resumed with byte-equal segments on SeaweedFS and Garage; a connection reset while paused is reopened at its offset.
- Where SIGSTOP does not exist (not Linux), jobs are unbounded and the log says so.

What the bound saves, computed for a 2 hour film at 40 Mbps copied with its audio re-encoded: a viewer who leaves after a minute costs about 160 s of film (1.6 GB read) where an unbounded job cost 6 GB.

## Sharing a slow re-encode

- **A re-encode is cut into chunks of 4 segments (16 s).** A job nobody helps is still one ffmpeg that runs on across chunk starts.
- **A job running below 1.2 times real time asks for a helper** for the first chunk ahead that nobody is making. It is judged only after 2 s of output: ffmpeg's output comes in bursts, and a speed over the first two seconds swung from 0.9x to 1.6x for a job that makes 1.4x.
- **A helper is an ordinary job row** with three limits: it is never claimed by the worker running the viewer's own job of that spec, it never takes a worker over its capacity, and a viewer who has asked is always claimed first.
- **A helper's failure is not the spec's.** The viewer's job makes the chunk instead, and no helper is queued for that chunk for 30 s.
- **A helper whose viewers have gone or seeked away is stopped** within about a second, cancelled and counted on its own. Before this, one held a worker for over five minutes making segments nobody watched.
- **A new viewer never stops a helper a watched film needs** to stay at real time. The newcomer is refused with that reason.
- **Two workers may share a job neither holds alone** when each has 0.65 of its cost free ([workers.md](workers.md)).
- **A copy is not chunked.** A copy's fallback encode is not chunked either.

Measured in a browser, workers held to 2 CPUs each on a 4K HDR re-encode: one worker ran at 0.87 to 0.90x with 39 to 51 freezes in five minutes; two workers sharing ran at 1.00x with 3 freezes.
