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
- **Spec hash:** identifies one transcode output: item, source, codecs, bitrate, resolution, audio, burned-in subtitle and encoder family.
- **Seam:** where segments from two jobs meet.

## Boundaries

| Package | Owns | Must not |
|---|---|---|
| `internal/api` | HTTP, auth, DTO shaping at the edge | Touch a store except through `internal/store` |
| `internal/items` | Item queries and hydration | Know about HTTP |
| `internal/library` | Scanning, naming, probing | Serve requests |
| `internal/store` | Backend differences | Leak backend types or credentials |
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
8. Migrations are forward-only, and an applied migration is never edited.

### Messaging

9. Never call `pg_notify` inside a data transaction. Commit durably, then send the hint in a separate `synchronous_commit=off` transaction. Inline, it capped at about 189 commits/s.
10. Every hint consumer also polls, so a lost hint only adds delay.

### Storage

11. POSIX and S3 behave the same at the `Store` interface. The conformance suite runs against POSIX, SeaweedFS and Garage.
12. S3 credentials come from the environment. Store URIs are persisted, so they never hold secrets.
13. ffmpeg reads S3 through the loopback input, never a presigned URL.
14. Direct play is proxied by default, and we always answer HEAD ourselves, because presigned URLs reject it.
15. `http.ServeContent` callers set `Content-Type`, or it reads the object to sniff one.

### Compatibility

16. Routing is case-insensitive and query keys are lowercased once, in middleware. Route wildcards keep their case.
17. `TranscodingUrl` stays Jellyfin-shaped and unsigned. Sign URLs inside playlists.
18. `PlaySessionId` can be empty in reports, so progress keys on user and item. Progress is coalesced in SQL (a write only lands if the row is over 30s old), never in process memory, so it holds across replicas. Stopped always writes.
19. `<video>` can't send headers, so media routes accept `ApiKey` in the query.
20. `compat/endpoints.yaml` is generated from `Routes()`, and every alias must be a real obsolete route. Tests enforce both.

### Transcoding

21. Segments are fMP4 by default. MPEG-TS seams break RFC 8216.
22. Segment contents depend on the spec hash and segment number, not on which worker made them.
23. fMP4 can't splice across encoder families, so failover stays within a family.
24. The exact ffmpeg settings in `spikes/seam/README.md` are required, including fixed B-frames and a closed GOP.
25. Every job has a kill path, and no ffmpeg outlives its lease.

### Security

26. Tokens are random and stored per device. Passwords use bcrypt. Login failures don't reveal which usernames exist.
27. Paths built from keys can't leave the store's root.

### Play state and sessions

28. Coalescing reads `user_item_data.progress_at`, which only Progress and Stopped write. Playing, favorite and played toggles never touch it, so they can't suppress a report. The first Progress after Playing is stored, unless a Stopped or Progress landed within the last 30 seconds.
29. Client numbers are clamped before SQL (`PositionTicks` to `playstate.MaxTicks`, `ActiveWithinSeconds` to about eight years), and runtimes are compared as `numeric`. No client value can overflow a bigint or an interval.
30. A late Stopped for item A doesn't clear a session that has moved on to item B: the session row clears only when its `item_id` matches.
31. A device can hold several sockets. Each is a `session_sockets` row, and `sessions.ws_replica` is the newest row's replica, or NULL. Connect and close update both under the session row's lock, in one transaction. Rows a crashed replica leaves behind are not yet cleaned up (Phase 4).
32. Until transcoding (Phase 3), the scanner writes only the video and default audio streams into item DTOs, so clients offer no track that can't be delivered. `media_sources.streams` keeps the full list.
33. `Content-Type` for direct play comes from the stored container, never from the URL or the store key.
