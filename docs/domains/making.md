# Making

## Words

- **Made version:** anything the server makes from a film: segments, extracted tracks, fonts and pictures. All of it can be made again.
- **Job:** one ffmpeg run that makes segments from N on. Workers claim jobs from a Postgres table.
- **Spec:** identifies one transcode output (today's spec hash), unchanged.
- **Segment:** a piece of a transcode, deterministic as today (invariants 21 to 24, 35, 36, 47, 48).
- **Budget:** one storage budget over all made kinds.
- **Machine:** a worker, one `transcode_nodes` row. Its capacity is learned from its jobs (invariant 64). Making reports capacity; Playback refuses.

## Owns

Everything made from a film, the one storage budget, jobs, workers and capacity. Owns the tables `transcode_*`, `subtitle_jobs`, `chapter_images`, `sweeps` and `sweep_evictions`. Today `internal/transcode`, `internal/subtitles` and `internal/mediatool`. The track stays with Collection; the spec hash stays as today.

## Must not

Know HTTP, viewers' accounts or access. Refuse a viewer: Making reports capacity, and Playback refuses.

## Rules

Rules at the cutover (JS-194); today's invariants apply until then.

- **Budget (extends invariant 39):** never evict what a live playback needs; evict made-ahead first.
- **Order of work:** each job has a class: a waiting viewer, a running playback's lead, helpers, ready-before-Play, made-ahead, or background. A higher class takes cores from a lower one, as `makeRoom` does for helpers. Today's separate slot pools are this rule in fixed form; they unify only when JS-196 or JS-201 needs it.
- **Rewritten 25, at the cutover, names only (invariant 25's mechanism is unchanged until then).**
- **Rewritten 38, at the cutover, names only (invariant 38 is unchanged until then):** a job ends with its playback, idleness or its lease. A playback lives for hours; a job lives while segments are asked for. Resuming asks a segment, which restarts a job as a seek does.
- **Rewritten 43, 50, 51, 54 and 55, at the cutover (delivery shapes only):** the mechanism of each invariant is unchanged.
