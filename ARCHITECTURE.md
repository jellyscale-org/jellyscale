# Architecture

What the new server is meant to look like, and why. [PRODUCT.md](PRODUCT.md) says what to build, [DOMAINS.md](DOMAINS.md) gives the rules, and [docs/engine/](docs/engine/README.md) holds what is known about the carried-over video engine. This file is intent: where the code departs from it, either the code is wrong or this file changes with a reason.

It does not design the new server. Sign-in, the collection, the API and the database are designed task by task, from the product.

## Shape

One binary whose role is chosen at start, run as separate deployments. Roles version together, so a change to the database or to what is made never has to span separately built services.

| Role | Job | Holds | Scales on |
|---|---|---|---|
| `api` | Answers Octave: sign-in, finding, starting and steering a playback | Connections only | Requests |
| `stream` | Sends bytes: films, playlists, segments, subtitles, pictures | Connections; what it needs to keep serving stored bytes while Postgres is briefly away | Bandwidth |
| `worker` | Runs media tools: conversion, extraction, pictures, reading files | Scratch space only | Cores, and the work queued |

Postgres and the stores (local disk or S3-compatible) are the only things the roles share.

## Rules

1. **Only workers run media tools.** `api` and `stream` move bytes and coordinate, so their images need no ffmpeg. A viewer waiting for a result doesn't put the work in the role answering them.
2. **Roles never call each other.** Work goes through Postgres and results through the stores, so either side can be replaced or scaled alone.
3. **Work is pulled.** A worker takes a job only when it can hold it beside the jobs it has, so overload is a queue, and the queue is the signal to add machines.
4. **Workers are replaceable.** Jobs are leased, and what a job makes depends on what was asked for, not on which worker made it.
5. **No required service beyond Postgres.** Anything added must earn its place against a measurement ([docs/engine/scale.md](docs/engine/scale.md) has the one made so far).
6. **Cost is more than CPU.** A design states its network, storage and database cost at the scale PRODUCT.md names. Copying a film is cheap in CPU and expensive in reads from object storage, for example.
7. **Images follow what a role needs,** not one image per service.
8. **No state another replica needs lives in one process.** Any replica answers any request.

## What comes over, and what is new

**Comes over:** the video engine and its tests. It becomes the inside of the Making domain, with the file-reading code the Collection needs (what a file holds, its keyframes, its chapters) and the stores under Platform.

**New, designed for this product:** people and sign-in; the collection's identity, naming and scanning; finding; the playback a server owns, its plan and its link; place; the published contract and the edge; the database.

**The line between them:**

- The engine knows no HTTP, account or viewer. What it is asked for arrives as a stored plan's spec, never as a request's text.
- Nothing shaped for another server's clients crosses the line: not its device statements, link shapes, session keys, answer shapes or reason names.
- The engine's facts ([docs/engine/](docs/engine/README.md)) stay true across the move, or the change that breaks one says so and changes the page.
- The earlier server's code stays in the tree until the new server replaces it. It is not a pattern to follow, and nothing new is built on it.

## Open for the design

Known questions the first designs must answer. None is decided here.

- What Octave's apps say about a device, in place of the device statement the engine's decision reads today.
- How the picture steps with the connection: the engine makes one re-encoded quality today ([docs/engine/gaps.md](docs/engine/gaps.md)).
- Audio as its own stream, so a language switch doesn't make the picture again. The join proof must be made again for it.
- How `stream` keeps serving one playback's stored bytes while Postgres is away, without honouring an account's credential from memory.
- Which pictures need a sign-in, and whether outside metadata is allowed (PRODUCT.md, Open questions).
