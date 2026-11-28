# The video engine: what is known about the carried-over code

The rebuild brings over one thing from the earlier server: the video engine, with its tests. These pages hold what was learned and measured while building it. They are **facts about code**, not product rules: a rule lives in `DOMAINS.md` and `docs/domains/`, and what the product should do lives in `PRODUCT.md`.

Use them like this:

- Before changing a part of the engine, read its page. Each fact was paid for with a defect or a measurement.
- A fact stays true only while the code it describes stays. When a task changes the code, it changes the fact in the same change.
- A number here was measured on the machine and versions named beside it. It is not sizing guidance for another machine.
- Where a page says "not measured" or "not observed", nobody knows.

## What the engine is

| Part | Today's package | What it does |
|---|---|---|
| Segments and jobs | `internal/transcode` | Makes segments with ffmpeg, as jobs that workers claim, lease, pause, hand over and share |
| The decision and the layout | `internal/playback` | Decides between sending a film as it is, copying and re-encoding; names a spec; cuts playlists |
| Subtitles and fonts | `internal/subtitles` | Extracts text tracks, converts files beside a film, stores fonts and image tracks |
| Stores | `internal/store` | One interface over local disk and S3-compatible storage |
| Reading a file | `internal/library` (probe, keyframes, cue points, chapters, chapter pictures), `internal/mediatool` | What a file holds, where its keyframes are, where its video ends |

What does not come over: the HTTP routes and their answers, sign-in and sessions, the stored item answers, the scanner's naming and identity, and everything shaped for another server's clients. The engine today is entangled with some of that; untangling it is part of bringing it over ([gaps](gaps.md)).

## Pages

- [segments.md](segments.md): what a segment is, why any worker can make it, joins between runs, read-ahead, sharing a slow encode.
- [workers.md](workers.md): leases, knowing a worker is dead, what a job costs, how a worker learns what it can hold.
- [copy.md](copy.md): sending the film's own video and audio in segments; the keyframe index.
- [picture.md](picture.md): re-encoding, tone mapping, chapters and chapter pictures.
- [subtitles.md](subtitles.md): text tracks, files and fonts beside a film, image tracks, drawing into the picture.
- [stores.md](stores.md): what local disk and S3 do differently, what reads cost, cleaning up.
- [scale.md](scale.md): what one Postgres carries.
- [gaps.md](gaps.md): what the engine does not do yet, or does only for some films.
- [test-history.md](test-history.md): the test tooling of the code in the tree, and every past test failure that taught something.
- [../players.md](../players.md): what players were seen to do.

## Reading the code's comments

Comments and test names in the carried-over code cite records that are no longer in the tree. The tag `pre-rebuild` keeps them all (`git show pre-rebuild:DOMAINS.md`, `git show pre-rebuild:docs/transcoding.md`).

| A comment says | It means |
|---|---|
| `DOMAINS 22`, `invariant 22` | A numbered rule of the earlier `DOMAINS.md`. The table below says which page restates it |
| `JS-088`, `BL-245`, `D-021` | A task, backlog item or decision of the earlier plan. None is current |
| `PlaybackInfo`, `DeviceProfile`, `TranscodingUrl`, `jellyfin-web` | The request, client statement, link and web client of the earlier API. The new API replaces each |
| `the dogfood` | The owner's test installation |
| `compat/captures/…` | A recorded observation of another server and its web client, kept only because tests in the tree still read it |

| Earlier rule | Now in |
|---|---|
| 1 to 4, 7 to 10, 27 | `docs/domains/platform.md` |
| 11 to 15, 39, 42, 56 | [stores.md](stores.md) |
| 21 to 24, 34 to 38, 40, 57 to 59 | [segments.md](segments.md) |
| 25, 37 (worker loss), 64 | [workers.md](workers.md) |
| 45 to 48, 52 | [copy.md](copy.md) |
| 41, 53, 54, 55 | [picture.md](picture.md) |
| 43, 44, 50, 51 | [subtitles.md](subtitles.md) |
| 5, 6, 26, 28 to 33, 49, 60 to 63, 65 to 67 | Not carried: they describe the earlier API, sign-in and stored answers. The rules that replace them are in `docs/domains/` |
| 16 to 20 | Not carried: another server's client conventions |
