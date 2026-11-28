# What players were seen to do

Observations of real players, each with the version it was seen on. A design that depends on a player's behaviour cites a line here or adds one. Nothing here is a requirement, and a line about a player Octave doesn't use is kept only because the mechanism may recur.

**Not observed at all:** Apple's AVPlayer. Android's Media3 beyond the one test below. Any player on a real TV or phone.

## Android: Media3 ExoPlayer 1.11.1

One run on an Android TV emulator (API 36) against a stand-in server (`spikes/pathsecret`).

- **A link with its secret in the path, and relative links inside its playlists, needs no header or query credential.** Every media request arrived with neither.
- **It survived a real 30-minute pause and then a seek.** The player made no request at all during the pause.
- **A refused link does not surface as a player error.** After the server answered 410, the player reported two load errors and then stayed buffering with no picture, with no player error within 90 s. An app must watch load errors, or its own deadline, to notice that a playback has ended.

## Browsers: hls.js and Chromium

- **A request with no first byte after 10 s is abandoned and asked again**, up to 4 more times, then it is fatal (hls.js 1.6.16 defaults). So a server that is slow gets a retry; the buffer drains meanwhile.
- **An error answer is retried up to 6 times by hls.js itself**, but an app's own error handler can end playback on the first one. The earlier web client did: any answer of 400 or above destroyed its player. So never answer an error for something that will pass.
- **A failed segment before playback began** made that client ask for the stream again with no limit: a failing stream became a request storm. **After playback began** it dropped the stream: a frozen picture and no message.
- **A media segment that begins with its own init data is taken as a new init** (hls.js 1.6.16, Chrome). This is what lets a stream change encoder settings mid-stream.
- **The buffer is typed by the codec the playlist names.** Chrome refused an H.264 segment appended to a buffer typed AV1, as a fatal error.
- **Segments of two x264 presets under one init segment failed to decode** in Chromium.
- **A 10-bit HDR AV1 stream switched to a re-encode of the same kind and back** without error in Chromium.
- **Chromium plays AV1 in software, and HEVC only with a hardware decoder.**
- **A `<video>` element can't send headers**, and a browser playing a file directly plays only its default audio track.
- **A browser's fetch has no timeout of its own:** Chromium held a subtitle request for 200 s and then took the answer.
- **Joins between two encoder runs** played with a continuous buffer and no dropped frames in hls.js 1.7.4 and Shaka, five streams each (`spikes/seam`).

## A worker lost mid-film, as a viewer sees it

In a browser, with the worker killed 1.6 s into playback (the least buffer there is): the picture of a re-encode stood still 2.5 to 2.8 s, a copy's 0.3 s. Later in a film, with a full buffer, nothing was seen. The player kept going because a silent request is retried.

## Other players

- **mpv and GStreamer** played five joined fMP4 streams each without error. mpv reported corrupt packets on MPEG-TS joins.
- **Redirects:** ffmpeg, mpv, GStreamer and Chromium all followed a redirect to a second origin. ffmpeg and mpv forwarded the `Authorization` header to it (`spikes/redirect`).
- **Players built on ExoPlayer in several third-party apps sent no credential on media requests.** A link that carries what it needs avoids depending on the player for that.
