# What a player does when a segment fails after playback has begun (JS-088)

The viewer must not see a failed segment (PRODUCT.md: failures invisible to viewers). What the recovery may count on
depends on what the player does with the failure, so it was watched first. The numbers are in
[segment-errors-12.4.json](segment-errors-12.4.json), and the e2e scenarios that made them hold jellyfin-web to them.

## jellyfin-web 12.4 (hls.js 1.6.16): observed

Reproduce: `E2E_GATES=transcode E2E_ONLY='jellyfin-web after a segment' CHROMIUM_PATH=/usr/bin/google-chrome-stable test/e2e/run.sh <workdir> <playwright_dir>`
(`test/e2e/transcode.mjs`, one scenario for each row). A copied film (180 s, a keyframe every 2 s, segments of 4 s) is
played from the start by the real jellyfin-web in Chrome through `test/e2e/router.mjs`, which answers segment 12
(48 s), the first one the player asks for that is not in its buffer, as the row says, once or every time.
Each case was watched 30 s (once) or 45 s (every time) from the first failed request.

| the segment is | asks for it again | asks PlaybackInfo again | a dialog | the picture goes on |
|---|---|---|---|---|
| answered 500, once | no (1 request in all) | no | no | no: it plays to the end of what it has buffered, 48 s, and stops |
| answered 500, every time | no | no | no | no, the same |
| answered 404, once | no | no | no | no, the same |
| answered 404, every time | no | no | no | no, the same |
| no first byte (the router holds the request), once | once, after about 10 s | no | no | yes: the second request is answered |
| no first byte, every time | 3 to 7 times in 45 s (5 seen), about every 10 s | no | no | no |
| connection closed in the middle of the body, once | once | no | no | yes |
| connection closed in the middle of the body, every time | 5 to 9 times in 45 s (7 seen) | no | no | no |

So a status of 400 or more on a segment is final for hls.js in jellyfin-web: it does not ask again, does not ask
PlaybackInfo again and shows no dialog, and the film freezes where the buffer ends. A request that is abandoned or cut
is asked again, and the film goes on if one of those asks is answered. The recovery therefore has to answer the request
that is waiting with the segment, not with an error (the server holds a failed job's request while a fresh job
makes the segment, and the player's own retry after about 10 s is the second chance).

The counts of the persistent cases depend on the player's timers within the window and are held to the range
in the json, not to one number.

## ExoPlayer (Media3 1.5.1): from the source, NOT OBSERVED

No ExoPlayer client is on this machine (JS-075 covers real players on the dogfood). Read from the 1.5.1 tag of
`androidx/media` (`libraries/exoplayer/.../upstream`, `libraries/exoplayer_hls/.../hls`, `libraries/datasource`), for a
player with the default load error handling policy and `DefaultHttpDataSource`, which is what an app gets that does not
replace them. What the app (Findroid and the other clients) does with the error the player finally raises is not read here.

All four cases take the same path. A failed segment load ends in `HlsSampleStreamWrapper.onLoadError`
(`exoplayer_hls/.../HlsSampleStreamWrapper.java` l. 927-1000): a track exclusion is tried first (l. 968-975), which
needs a second track and a 403, 404, 410, 416, 500 or 503 (`DefaultLoadErrorHandlingPolicy.isEligibleForFallback`,
`DefaultLoadErrorHandlingPolicy.java` l. 145-157; `getFallbackSelectionFor` l. 96-109); with one rendition there is none, so
the load is retried after `getRetryDelayMsFor`, `min((errorCount - 1) * 1000, 5000)` ms (l. 118-127), and
`Loader.LoadTask` counts the errors (`Loader.java` l. 523-536). The error is thrown to the player when the count passes
`getMinimumLoadableRetryCount`, 3 (`DefaultLoadErrorHandlingPolicy.java` l. 36 and 134-139; `Loader.maybeThrowError`,
`Loader.java` l. 389-393). So, for each case:

- answered 500: an `InvalidResponseCodeException` (`DefaultHttpDataSource.java` l. 385-402); retried, so 4 requests in
  all, after 0, 1, 2 s; the error is then raised to the player. It asks no new playlist.
- answered 404: the same path: only `FileNotFoundException` and `ParserException` are not retried (l. 118-127), and a 404 is an
  `InvalidResponseCodeException`. `HlsSampleStreamWrapper` retries a 404 or 410 at once (l. 934-942) only for a chunk
  that is not yet `isPublished()`, a low-latency part; a segment of a VOD playlist is published, so it takes the path above.
- no first byte: `DefaultHttpDataSource` reads with an 8 s connect and 8 s read timeout (`DEFAULT_CONNECT_TIMEOUT_MILLIS`,
  `DEFAULT_READ_TIMEOUT_MILLIS`, l. 241 and 244); the timeout is an `HttpDataSourceException`, retried as above, 4 requests, 8 s each,
  and then raised.
- connection closed in the middle of the body: an `IOException` from the read (`DefaultHttpDataSource.java` l. 442-470), also
  retried as above (it is neither of the exceptions that are not), from the start of the segment.

A dialog is the app's: the player raises the error through `Player.Listener.onPlayerError` and stops. Unlike hls.js in
jellyfin-web, ExoPlayer therefore asks 3 more times, so a job that fails and is made again within about 3 s (the
delays add up to 0 + 1 + 2 s, plus the loads) is not seen by it either. Unverified: whether a given client's app
retries after the error, and the 8 s timeouts under a client's own `DataSource.Factory`.

## A re-encode served under a copy's init (JS-088 spike, jellyfin-web's hls.js 1.6.16 in Chrome stable)

jellyfin-web loads the copy's init segment (EXT-X-MAP) once, and asks PlaybackInfo again in none of the cases above, so a
fallback to a re-encode has to say how the player gets a decoder for the new bytes. The spike
(`compat/captures/segment-init-spike.mjs`, numbers in the json) played a playlist whose first segments are one x264
stream and the rest another with other parameters (baseline and CAVLC, then high and CABAC), under the first's init:

- The second stream's bare segments under the first's init: the player stops with `PIPELINE_ERROR_DECODE` after the
  last good segment (144 frames shown of 290), `currentTime` goes on, `corruptedVideoFrames` stays 0, and hls.js reports
  nothing fatal. So the position, the dialog and the corrupted counter say nothing of it: `video.error` and the frames
  shown do (test/e2e/recovery.mjs `quality`).
- The second stream's own init in front of its first segment, or of each of them: the player plays on, 290 frames, the
  same picture as the control (mean luma difference 2.6 of 255). hls.js appends the segment as it is (its passthrough remuxer
  is handed the bytes and the init it parsed from EXT-X-MAP), and the browser takes a moov in a media segment as a new
  init segment. That settles the delivery for an H.264 copy re-encoded as H.264: a moov at the start of the first re-encoded
  segment (or of each) is enough, and no new playlist or PlaybackInfo is needed.
- Not observed: HEVC and AV1 copies (this Chrome has no HEVC decoder here, and no AV1 test was made), and any moov of a codec
  other than the SourceBuffer's. The SourceBuffer is typed by the master playlist's CODECS when the player adds it, and MSE
  needs `changeType` for another codec; hls.js has `appendChangeType` (in the chunk, called on codec changes it learns from its
  own track events, not traced further), and nothing there reads a segment's own moov for a codec. The check of the build
  (internal/api/recovery_integration_test.go) therefore requires the init the player ends up holding to be of the master
  playlist's codec; for an HEVC or AV1 copy that means the fallback encodes HEVC or AV1, not x264, and this is the
  lead's to confirm (DOMAINS invariant 45 says nothing of the codec of a re-encode that stands in for a copy).

## A seek after playback has begun into a segment the server cannot make (JS-152, 2026-11-20: observed, jellyscale's own answer)

Observed with the e2e scenario "a seek after playback has begun into a segment that cannot be made" (`test/e2e/transcode.mjs`):
a re-encoded film (interlaced, 180 s) is played from the start in jellyfin-web 12.4, its file is cut at 55 % once the scanner has
read it, and the player seeks to 80 %. The job for the seek fails ("no video frame at 144.000s") and the segment is answered 500.
The player's console says `HLS Error: Type: networkError Details: fragLoadError Fatal: false` and then `hls.js response error code: 500`;
there is no `[playbackmanager] onPlaybackError` line, no dialog, PlaybackInfo is asked once (before the play), 6 segment requests
are made in all, and the picture is at 0 s with the video not seeking. Same as the table above, for a re-encode and a real seek.
A reading of the 12.4 web client (`web-dist/main.jellyfin.bundle.js`, the hls.js error handler `EK`, which `htmlVideoPlayer`'s
`setSrcWithHlsJs` calls as `(0, S.EK)(instance, hls, element, instance.onError, resolve, reject)`): a segment answered 400 or more
destroys the player and calls `reject` if it still is set, else triggers the player's `error` event. `reject` is never cleared
once `play()` has resolved, so after playback has begun that call looks like a no-op on a settled promise, which would explain
the missing `onPlaybackError` line. This is a reading of minified code, not an observation, and the `error` event of the video
element itself was not traced: whether any answer of the server gives the dialog after playback has begun is unverified.
