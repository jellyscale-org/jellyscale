# Subtitles and fonts

Facts about `internal/subtitles`, the subtitle jobs in `internal/transcode`, and the cue-point code in `internal/library`. See [README.md](README.md) for how to read this.

## Text tracks inside a film

- **Read out of the file once, by a worker**, whatever the number of requests and replicas. The first request queues one job and waits on the result in the asset store. Only workers run media tools.
- **One ffmpeg writes every text track of the film** into every format, stored as one object per track, so a reader never sees half of one. The key includes the file's revision, so a changed file gets fresh text.
- **Its own table and slots (4 per worker)**, never the transcode slots: an extraction must proceed while every transcode slot is busy, because a player may fetch a subtitle once and not retry.
- **A killed worker's extraction is taken over** about a second later, up to three attempts.
- **A failed read is remembered for 30 s**, so a bad track isn't read again by every request; then the next request tries afresh.
- **A request waits while a worker is on the job**, which for a big file on object storage is as long as reading it: up to one hour per run. With no worker, or a job nobody claims for 6 s, the request is refused.
- **A track ffmpeg hangs on costs the film's other tracks nothing:** after a run that hit a time or stall limit (a minute without reading or writing), each remaining track gets a run of its own.
- **Any output from ffmpeg at error level fails the extraction**, exit code 0 included: that is how it reports a file cut short.
- **Cue times count from the film's start.** On ffmpeg 6.1 a negative audio start is also applied, so cues can be 23 ms late there.
- **Cost:** reading a text track reads the whole film once from the media store. A 40 GB film is 40 GB of reads.

## Subtitle files beside a film

- **The scan only lists them.** It reads names from the listing it already makes, converts nothing and writes nothing to the asset store.
- **From the name:** language, forced, default, for the deaf or hard of hearing, and a title from the remaining words.
- **A worker converts a file when it is first asked for**, one job per version of the file.
- **Text encoding:** UTF-16 is converted by the engine (ffmpeg reads it unreliably); UTF-8 passes; anything else goes to a detector, and the first candidate that decodes the bytes wins. The language in the file's name breaks a near-tie only. Stray control bytes are removed first: ffmpeg refuses a file that starts with one and loses the cues after one.
- **The detector is wrong for some files**, accepted and not worked around: Central European text is read as Western, and Thai as a CJK set. The remedy is converting the file to UTF-8.
- **A file that can never be read is remembered** against its version, so it costs one job, and is no longer offered. A failure that is not the file's (the store, a timeout, a signal, a full disk) marks nothing.
- **Track numbers are never reused.** The film's own tracks keep theirs; a file takes the next number from a counter that only grows, and keeps it while it exists. A viewer who holds a number never finds another file's text under it.
- **A new or changed subtitle file costs nothing from the film:** the scan re-lists the files without probing the film again.

## Fonts

- **Fonts attached to a film are stored once per revision of the film**, by one job whose ffmpeg reads little more than the file's header. Each font is stored under the hash of its bytes, so a season's shared fonts are one object. A font over 64 MB is left out.
- **A font is offered only once it is stored.** Until then a player draws styled subtitles in its fallback font.
- **A stored font that has gone missing or is cut short is answered empty and re-made**; an error answer stalled a browser's subtitle renderer.
- **Drawing styled subtitles into the picture waits for the fonts**, so the picture uses the film's own. A failure that may pass fails the job, to be asked for again: the segments are stored under a hash that doesn't name the fonts, so a picture drawn without them would be what every viewer gets for a day. Only the film's own failure draws without them.

## Image tracks (PGS, VobSub, DVB)

- **Listed from what the scan already probed.** A bitmap kind nothing can deliver is listed nowhere.
- **PGS can be sent as a file** to a player that draws it: a worker copies the track out once (`-c:s copy -f sup`), which reads the whole film.
- **Everything else is drawn into the picture**, straight from the film as it is encoded: no extraction, file or font comes first.
- **The overlay doesn't hold the video for a track with few packets.** Never run such an encode with ffmpeg's `-readrate`: it then waits for the track.

### A run that begins mid-film

An image cue is one packet when it appears, so a run that begins after it would not draw it.

- **Cue points are read at scan time** from a Matroska file's own index: a few hundred KB of a film of any size, never the whole film. A design that listed the track's packets on the first seek read 86% of a 547 MB film before the first segment.
- **A run reads the last point before its cut** (two ranged reads of a few hundred bytes), tells whether a picture is showing, and for a picture built by several packets reads back to the one that starts it.
- **Inside a cue, it reads and decodes the film from the cue's first packet to the cut.** The cost grows with the cue's age and has no cap: capping it would drop a cue that is on screen. Measured: a cue 8 s old cost 13.3 MB and 1.2 s to the first segment; 50 s old, 24.4 MB and 2.8 s; no cue showing, 9.3 MB and 0.9 s.
- **A muxer can write a packet well before the video of its time**, so each point also keeps the last video keyframe at or before it.
- **Frames before the cut are dropped with a filter, not `-ss`**, whose first frame differs between ffmpeg 6 and 9.

### The stored track

- **A play that draws an image track asks, without waiting, for every image track of the film to be stored:** one job reads the film once and stores each track as a small file of its own, at the film's own times.
- **A later mid-film run feeds ffmpeg the stored track as a second input** and opens the film at its cut. A cue's age then costs kilobytes of the track and no film read: 6.2 MB and 0.8 s for a cue 8 s or 50 s old alike.
- **The two inputs align by construction**, because the stored packets keep the film's times. ffmpeg refuses such a stream when its declared size is wrong or it holds no cluster; both are handled.
- **Fed through a small pipe, not a read-ahead server:** a loopback server read about twice the track for each start of ffmpeg.
- **Slots of their own (2)** and a one-hour limit. A read that runs out the hour fails until the next day or until the file changes, so a slow film costs one read a day, not one per seek.
- **Still missed until the track is stored:** a film with no cue points for the track (MPEG-TS, Matroska written without them). Its runs open 10 s early and miss an older cue.

## Drawing text into the picture

- **Only when the player can show a subtitle no other way.** A different drawn track, or none, is a different spec.
- **The cue times are moved later by the film's start time**, because the encode keeps the film's clock and extraction does not. A restart at any segment then draws the same text at the same moments, including for a film that starts 7.5 s late.
- **No viewer's text reaches ffmpeg's arguments:** the track is a number and the file name is fixed.
- **The first draw of a track waits for its extraction**, holding its room on the worker; it gives up if nobody is watching.
