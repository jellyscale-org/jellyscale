# Domains

The terms, boundaries and invariants of the system. Reviewers check changes against this file. If a change needs one of these to bend, update this file in the same change and say why.

## Vocabulary

- **Role:** what a process does, chosen by `--roles`: `api`, `stream` or `worker`. One binary, many Deployments.
- **Library:** a configured collection with a media store and an asset store. Clients see it as a `CollectionFolder`.
- **Item:** a Movie, Series, Season or Episode. Identified by a stable UUID.
- **Media source:** the playable object behind an item.
- **Store:** a POSIX or S3-compatible backend, reached only through `internal/store`.
- **Session:** a device's token, plus what it's playing.
- **Alias:** an obsolete route kept only for jellyfin-web.
- **Hint:** a NOTIFY wakeup. Lossy by design.
- **Spec hash:** identifies one transcode output: item, source and its ETag, audio track, video and audio bitrate and resolution as ffmpeg will use them, segment length, encoder family and the output settings version. A burned-in subtitle joins it when subtitles are built.
- **Segment:** a 4 second piece of a transcode, stored as `segments/{spec_hash}/{n}.mp4`, with `init.mp4` beside them.
- **Transcode store:** the store segments live in, configured once for every role.
- **Job:** one ffmpeg run that makes segments from N on. Workers claim jobs from a Postgres table.
- **Viewer:** one play session watching a spec. Jobs live while viewers do.
- **Seam:** where segments from two jobs meet.

## Boundaries

| Package | Owns | Must not |
|---|---|---|
| `internal/api` | HTTP, auth, DTO shaping at the edge | Touch a store except through `internal/store` |
| `internal/items` | Item queries and hydration | Know about HTTP |
| `internal/library` | Scanning, naming, probing | Serve requests |
| `internal/store` | Backend differences | Leak backend types or credentials |
| `internal/playback` | The decision between direct play and transcode, the spec and its hash, and the playlists | Do I/O |
| `internal/transcode` | Jobs, viewers, ffmpeg and the segment store | Know about HTTP |
| `internal/playstate` | Watch state: position, played, favorite, and its write rules | Know about HTTP or sessions |
| `internal/api` session persistence (`sessions`, `session_sockets`) | What each device is playing and which sockets it holds open | Keep that state in process memory |
| `internal/compat` | Client conventions | Hold domain logic |
| `internal/db` | Pool and migrations | Hold queries for other domains |

## Invariants

### Scaling

1. No process keeps state another replica needs. In-memory state must be safe to lose: connections, caches, running ffmpeg.
2. Anything a replica does at startup is safe when every replica does it at the same instant. Upserts use one conflict target, and a table must not have a second unique index that can race it.
3. Work that must happen once uses a lease or an advisory lock.
4. Routes belong to one role. Bandwidth-heavy routes go on `stream`.

### Data

5. Item IDs survive renames. A moved file keeps its item and watch state.
6. Filtering and sorting use indexed columns. Response shaping happens in memory from `items.dto`.
7. Client input reaches SQL only as bound parameters. Sort keys come from an allowlist.
8. Migrations are forward-only, and an applied migration is never edited. Dropping a column breaks replicas still running the old code, so after the first release, removals ship one release after the code stops using them.

### Messaging

9. Never call `pg_notify` inside a data transaction. Commit durably, then send the hint in a separate `synchronous_commit=off` transaction. Inline, it capped at about 189 commits/s.
10. Every hint consumer also polls, so a lost hint only adds delay.

### Storage

11. POSIX and S3 behave the same at the `Store` interface. The conformance suite runs against POSIX, SeaweedFS and Garage.
12. S3 credentials come from the environment. Store URIs are persisted and logged, so they never hold secrets: `store.Open` refuses a URI with userinfo or a parameter named like a key, secret, access, token, password, signature or credential, at startup, with an error that names the problem and not the value. The parameters are searched after `;` as well as `&`, in the fragment, and inside the query of a URL-valued parameter such as `endpoint`, plain or percent-encoded. A value with no such name, such as a token in an endpoint's path, can't be told from a gateway's path prefix, so it is accepted and reaches logs like the rest of the URI.
13. ffmpeg reads S3 through the loopback input, never a presigned URL.
14. Direct play is proxied by default, and we always answer HEAD ourselves, because presigned URLs reject it.
15. `http.ServeContent` callers set `Content-Type`, or it reads the object to sniff one.

### Compatibility

16. Routing is case-insensitive and query keys are lowercased once, in middleware. Route wildcards keep their case.
17. `TranscodingUrl` stays Jellyfin-shaped and unsigned. Signing the URLs inside playlists is still to do: today each line carries the client's own token as `ApiKey`, as Jellyfin's do.
18. `PlaySessionId` can be empty in reports, so progress keys on user and item. Coalescing happens in SQL, never in process memory; see invariant 28.
19. `<video>` can't send headers, so media routes accept `ApiKey` in the query.
20. `compat/endpoints.yaml` is generated from `Routes()`, and every alias must be a real obsolete route. Tests enforce both.

### Transcoding

21. Segments are fMP4 by default. MPEG-TS seams break RFC 8216.
22. Segment contents depend on the spec hash and segment number, not on which worker made them.
23. fMP4 can't splice across encoder families, so failover stays within a family.
24. The exact ffmpeg settings in `spikes/seam/README.md` are required, including fixed B-frames and a closed GOP. For x264 that is `bframes=2:b-adapt=0:open-gop=0:scenecut=0`; the seam test in `internal/transcode` fails if a restart drifts from a run from zero. The code leaves out `-to`: a job runs to the end of the file or until it is stopped, because a job's end is decided by who is still watching, not by a time. The cost is one padded AAC frame past the picture at the very end of a run that started late, which the seam test excludes. Frames the decoder marks interlaced go through `yadif` (one frame out per frame in, so timing is untouched) because the output is progressive; `OutputVersion` was bumped for it.
25. Every job has a kill path, and no ffmpeg outlives its lease. The paths are: `Sessions/Playing/Stopped`, `DELETE Videos/ActiveEncodings` (sent twice, so idempotent), and no request for the spec in `transcodeIdle` (60 seconds), because closing a tab sends nothing. A worker that loses its lease, can't renew it, or shuts down kills its ffmpeg, and the kernel does if the worker is killed outright.

### Security

26. Tokens are random and stored per device. Passwords use bcrypt. Login failures don't reveal which usernames exist.
27. Paths built from keys can't leave the store's root.

### Play state and sessions

28. Coalescing reads `user_item_data.progress_at`, which only Progress and Stopped write. Playing, favorite and played toggles never touch it, so they can't suppress a report. The first Progress after Playing is stored, unless a Stopped or Progress landed within the last 30 seconds.
29. Runtime comparisons use `numeric`, and `ActiveWithinSeconds` is clamped to about eight years, so no client value can overflow a bigint or an interval. `PositionTicks` is clamped to `playstate.MaxTicks` so stored positions stay sane.
30. A late Stopped or Progress can't move a session: Stopped clears the row and Progress updates it only when its `item_id` matches. Playing always sets the item, so a late Playing can still move it.
31. A device can hold several sockets. Each is a `session_sockets` row, and `sessions.ws_replica` is the newest row's replica, or NULL. Connect and close update both under the session row's lock, in one transaction. Any frame a socket receives refreshes its row, and detach deletes the device's rows quiet for two minutes, so a crashed replica's row stops being a candidate for the claim once any of that device's sockets next closes. Until then it can still become the claim. A device that never closes a socket again keeps its stale row until Phase 4 cleanup.
32. Item DTOs carry only the video and default audio streams, and no subtitles, until subtitles are built, so the details page offers no track that can't be delivered. `media_sources.streams` keeps the full list. PlaybackInfo lists every audio stream when transcoding is on, because a stream change can then deliver any of them.
33. `Content-Type` for direct play comes from the stored container, never from the URL. Matroska and WebM share a container, so only that case also reads the store key's extension.

### Transcoding, continued

34. A transcode URL's query is the only input to a spec, and every request validates it again, because clients can edit it. A client value reaches ffmpeg only as a bounded integer, never as text, and segment numbers outside the playlist are refused.
35. Segments are 4 seconds, the length the seam spike validated, not the 3 seconds Jellyfin 12.4 uses (`compat/captures/transcode-12.4.md`). Nothing else depends on Jellyfin's length: the client reads durations from the playlist. Segment length is in the spec hash.
36. A playlist is complete VOD with `EXT-X-ENDLIST` and no discontinuity tags, computed from the duration before anything is encoded. A tail under a second joins the segment before it, because the encoder may not cut one. The duration is where the *video* ends (`DurationTicks` on the video stream, else the format duration), because the muxer cuts segments only on video and audio that runs longer would otherwise leave playlist entries nothing can produce.
37. Any `stream` replica serves segment N: from the transcode store, else by waiting on an active job that will reach it (within three segments of its progress), else by starting a job at N. That check-and-insert runs under a per-spec advisory lock, so concurrent requests start one job. A request replaces the same viewer's previous job, whether it then starts a job or joins another viewer's, unless another viewer's latest wait was on that job (`transcode_viewers.job_id`), which is left running; other viewers' jobs are left alone. The init segment's URL carries no number and any job's first segment brings the init with it, so its request joins a live job, else starts one at the segment the viewer starts at (`StartTimeTicks` is in every segment URL), never at 0 for a resume to replace. A media request for segment N starts its job at N, even below the resume point. A request that cannot be served in time fails with an error rather than hang: 500 with the reason logged if the job failed, 503 once a queued job has waited `queueTimeout` (6 seconds) without a worker taking it, whether none is running or every slot is busy (the log says which). That wait is what lets a worker that is stopping gracefully (not one that is killed) be replaced without the viewer noticing, shown with real processes by `test/roles.sh`: its job is cancelled, the next request queues a new one, and a replacement that arrives within the timeout takes it. jellyfin-web's player abandons a request after 10 seconds, so a refusal must come sooner. 504 on timeout, 410 if the viewer was stopped.
    Before playback has begun, jellyfin-web answers a failed segment by destroying its player and asking PlaybackInfo again, with no limit and no dialog of its own, so a failing stream is a request storm, not an error unless PlaybackInfo refuses (below). After playback has begun it answers a failed segment by dropping the stream: the picture freezes, there is no dialog, and it asks for nothing more. The server can do nothing about that second case from the failed response, so a worker that dies mid-film freezes the viewer's picture, and a replacement within `queueTimeout` does not save it, because the dead job keeps its lease until it expires (a lease lasts 10 seconds from each renewal, which is 7 to 10 seconds after a kill, and up to 30 seconds from the claim until the first heartbeat, about 3 seconds after ffmpeg starts), and only then does the waiting request queue a new job (measured: a segment answered after 12.7 seconds, with the replacement started 1 second after SIGKILL); recovering from that without losing the picture or the resume position is Phase 4 (`PRODUCT.md`). The failover that exists is only this: a job whose worker is gone is cancelled and restarted by the next segment request, and a replacement worker that arrives within `queueTimeout` takes the restarted job, which is only fast enough when the old worker stopped gracefully and cancelled its job. The fix for the first case is at PlaybackInfo: it refuses a transcode that can't start now, by answering `ErrorCode: RateLimitExceeded` with no media sources, which jellyfin-web shows as a "Playback Error" dialog saying the media can't be played at this time (`NoCompatibleStream` would blame the client). It refuses when no worker has reported in 10 seconds (`transcode_nodes`, rows with slots), when every worker slot is taken by jobs of other viewers (the asking device's own jobs don't count: jellyfin-web asks for the new stream before it deletes the old), or when the spec's last job failed within 30 seconds of its end (`transcode_jobs.ended_at`), and a segment request in that time gets the failure too rather than start the same job again. A segment failure before playback therefore ends in that dialog: the PlaybackInfo the player asks again is refused, unless the spec has a live job, which is offered (below). A spec that already has a live job is always offered, by every replica, also one with no `transcodeStore`, since a viewer joins it, whatever failed lately and whatever the stores say; if the stores differ, the log warns and names both, and what the viewer then gets depends on the replica that serves the segments. One with no `transcodeStore` answers 503 at once, which sends jellyfin-web back to PlaybackInfo, where the live job is offered again, so before playback the viewer retries with no dialog until the job ends, which `transcodeIdle` bounds only if nobody else is watching it; after playback has begun the 503 freezes the picture as above. One with a store it can't read the segments in waits for them, keeps the job watched, and its request times out. A queued job is live for `queueTimeout` (6 seconds); whoever sees an older one cancels it and workers don't claim it, so a returning worker starts no ffmpeg for a viewer who left. With no live job on the spec, it also refuses when the live `transcode_nodes` rows differ in `transcodeStore`. Every process that takes part, whatever its roles, registers its roles, store URI and (for a worker) slots there with a heartbeat, and a process with no store registers an empty one, so a missing store and a different one are found by the same comparison; the log names both roles and both stores. Nothing else reports a configuration fault. When the viewer's default track plays as it is, a pick of another audio track that can't be transcoded now plays the default instead of refusing, whatever codec the pick has, though the client's menu still shows the track it asked for; when the default track needs the transcode, PlaybackInfo refuses. The one case the comparison cannot see is the same URI on different volumes: a job that ends having stored a segment this replica cannot read is marked failed with that cause, and PlaybackInfo then refuses with it for 30 seconds. A stream replica with no `transcodeStore` answers a transcode request with 503.
38. A job lives while a viewer on its spec who hasn't been stopped has asked for something in the last `transcodeIdle`. Stopping a viewer marks it stopped rather than deleting it, so a request still in flight can't bring it back (`Segment` checks before it starts a job, and answers 410), and cancels at once every job no unstopped viewer is left watching (a viewer watches the job it last waited on, or any of the spec's if it has only read stored segments), so their slots are free for the stream that replaces them. A job whose ffmpeg finishes no segment in 30 seconds (`Worker.Stall`) is failed: the lease is renewed whatever ffmpeg does, and viewers would otherwise wait on it for good. A Stopped report with no `PlaySessionId` stops the device's viewers of that item (invariant 18). Two viewers on one spec share segments and jobs, and stopping one leaves the other's. A job cut short because its worker shut down or died (its lease ran out and a sweep found it), or lost to a seek, is `cancelled`: viewers restart it, and the spec is not refused. Only a job that ran and broke is `failed`.
39. Segments of a spec nobody has asked for in `transcodeRetention` (24 hours) are deleted by a worker's sweep.
40. A segment the encoder flushes when its input ends is cut wherever the input stopped, and ffmpeg exits 0 either way, so the worker stores that last segment only if it is the playlist's last one and its length is within 1.5 seconds of the playlist's. A job whose input ended early otherwise fails with "the input ended early". `Reader` resumes a cut S3 body (invariant 42), but whether ffmpeg's own reads are exposed to the cut is unproven; this check remains for a source file that is itself cut short. It judges the last segment's length, so a source whose audio alone ends early is not caught; that stays open.
41. A transcode never exceeds 1080 lines (`playback.MaxHeight`), whatever the source and the cap, because software x264 at 4K needs more CPU than a worker usually has and the player stalls. Size is in the spec hash, so segments of different sizes never mix.
42. An S3 read is one open-ended GET with the caller's context, so cancelling the caller ends the transfer. SeaweedFS can end the body of a GET early, with a clean EOF and no Content-Length error, when a neighbouring GET of the same object is dropped while it is being read, and can answer a resumed range GET with `200`, `Content-Length: 0` and no `Content-Range`; Garage never cut a body in the same tests; whether it can answer that way was not tested. ffmpeg drops a request whenever it seeks. So `S3.Open` turns a body that ends before its Content-Length into `io.ErrUnexpectedEOF`, and a GET sent with a Range that comes back without a `Content-Range` is not that range and reads as `io.ErrUnexpectedEOF` whatever its body; `store.Reader` resumes at its current offset on that error, and also when a known-size object ends cleanly short of its size. Segments are read through a `Reader` of unknown size (`math.MaxInt64`), which cannot tell a clean short end, so `readSegment` treats an empty segment (never legitimate), or one still cut once its `Reader` gives up, as not there yet, and the wait loop reads again. A `Reader` gives up after three reopens without progress. Evidence: on `chrislusf/seaweedfs:latest` (weed 4.50, image created 2026-10-25, not pinned) a probe of raw GETs held while neighbours were dropped found 67 of 200 bodies cut, at 256 KiB multiples, and 0 of 200 on Garage; that probe is not in the tree. `TestDroppedReadDoesNotCutAnother` (100 rounds) drops three neighbours during a held `Reader` read and fails on SeaweedFS with the resume removed (10 of 10 runs, `unexpected EOF`), and passes with it on SeaweedFS and Garage (10 of 10). The cases of an empty 200, a clean short body, an empty segment and a segment that stays cut have tests that fail with each fix removed. No test in the tree shows the cut harming real ffmpeg: a repro of 30 encodes per target kept every frame with the resume on and also with it off, so ffmpeg's own exposure is unproven, and an earlier loopback-input run that lost its input in 17 of 80 encodes was not reproduced. `checkLast` (invariant 40) remains for a source that is itself cut short.
