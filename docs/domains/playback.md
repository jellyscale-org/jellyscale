# Playback

## Words

- **Playback:** one viewing on one device, from Play to stop.
- **Plan:** the stored decision: how the title plays (as it is, converted, re-encoded), why, and the renditions offered (video steps, audio, subtitles), each with its spec. The pure `Decide` produces it. A viewer is shown it as "how this plays and why", never as "plan".
- **Step:** a lighter quality.
- **Media link:** opens one playback only: `…/media/{secret}/…`, with relative URLs inside its playlists and no header or query token. The secret is random, stored only as a hash, names one playback, and is refused with a reason once that playback has ended.
- **Reason:** plain words for a refusal or a conversion, from one list (JS-183).

## Owns

One viewing from Play to stop: the decision, the plan, the link, steps, track switches, refusals, limits and moves. Owns the new tables `playbacks` and `events`; only Playback writes them. Playback owns what a viewer gets, and the upload share. It calls Making (ensure a job, read capacity) and Place (report a position). Today `internal/playback` holds the pure decision; the new `internal/playbacks` holds the lifecycle.

## Must not

Run media tools; write history rules; build a spec from a URL.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **68.** A playback is a row; any replica serves any request of it; nothing about it lives only in memory. Only connections, a cache of plans by `(id, epoch)` and the stream replica's outage snapshot live in memory.
- **69.** A media link opens one playback, is stored hashed, and ends with it. It carries no account credential. Media requests do not hold a playback.
- **71.** Server-sent messages are an append-only log per audience; a reconnect replays from the last seen; a lost hint only delays. The log is `events` (`audience_kind`, `audience_id`, `seq`, `kind`, `body`, `at`), with the audience a playback or a device. It replaces sockets and `api_replicas`.
- **75.** Limits count held playbacks, under one lock per account; a refusal names the devices playing. Limits (hours, streams, upload share) are enforced by Playback at Play and while playing, with a reason. A paused playback whose app holds it counts; a closed app stops counting at once when it can say so, else within the hold window. The refusal lists the devices holding playbacks, naming a title only for the viewer's own profile.
- **76.** A plan is read only by replicas that understand its version. `plan_version` works as `OutputVersion` does (invariant 59): an api replica makes only plans every live stream replica reads. A roll may meet `not-served` from an older replica; treat it as "not yet".
- **77.** A playback lives while its app reports. Holding is the app's report: every 10 seconds while playing, every 30 while paused. Silent for the hold window of 90 seconds, it has ended, with reason `silent`. An ended playback never revives: its link answers `playback-ended` with the action `start-again`. Play without a position starts at the viewer's place, with their picks, with no error; only a refused Play is shown. "Ended" follows from `reported_at` in the statement that reads the row: there is no third state and no sweeper. With Postgres away, reports wait and stored bytes go on; if the window has passed, the app starts again silently (decision D-20261127-domains-followup).
- **Upload share:** Playback sums the nominal rate of remote playbacks by circle, under one advisory lock, and lowers `ceiling` on the lowest circle first, with an event. Stream replicas serve no step above it. No bytes are counted.
- **Rewritten 32, at the cutover (invariant 32 is unchanged until then):** Every track is listed as itself with its id; none is dropped or replaced. A plan offers only what the server can deliver now, and each track it cannot deliver carries a reason. Each track says whether it is hidden for the calling profile (rule 62).
- **Rewritten 34, at the cutover (invariant 34 is unchanged until then):** A playback's stored plan is the only source of a spec. A media URL carries a rendition and a segment number, both bounds-checked.
- **Rewritten 37, at the cutover (serving part kept):** Refusal happens at Play with a reason and a retry time. The outage snapshot holds live playbacks, not tokens: during a Postgres outage a link serves stored bytes of its one playback, and no account token is honoured from memory.
- **Rewritten 44, at the cutover (invariant 44 is unchanged until then):** A subtitle is drawn into the picture only for a player that can show none of it.
- **Rewritten 45, at the cutover (invariant 45 is unchanged until then):** The copy decision reads Octave's device statement; its gates are unchanged.
- **Rewritten 52, at the cutover (invariant 52 is unchanged until then):** Audio is its own rendition; one picture serves every language. Its splice proof is made again (JS-182).
