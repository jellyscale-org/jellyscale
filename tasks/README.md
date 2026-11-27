# tasks

The plan: tasks, backlog and decisions. [PRODUCT.md](../PRODUCT.md) says what to build; [docs/status.md](../docs/status.md) says what is built.

## Layout

One file per record, JSON, `jq --indent 2`, the file's `id` equals its name. The tree holds no generated index.

- `tasks/tasks/<id>.json` — one task per file, open or finished. A task's file is never renamed or moved; finishing a task sets `status` to `done` or `dropped` in its own file.
- `tasks/order.txt` — task ids, one per line, open tasks in priority order. Only the product owner changes it, by PR. Finishing a task changes only that task's file; the listing skips ids in the order file that are not open, lists open tasks missing from it last marked unordered, and the product owner removes finished ids at their next order change. No task has a `priority` field.
- `tasks/backlog/<id>.json` — one backlog item per file. A new item is `BL-<YYYYMMDD>-<slug>.json`, so tasks finishing side by side never pick the same id; existing ids keep theirs.
- `tasks/decisions/<id>.json` — one decision per file. A new decision is `D-<YYYYMMDD>-<slug>.json`, lowercase kebab-case slug, `YYYYMMDD` equal to its `date`. Existing ids keep theirs.

`tools/plan.sh` prints the open tasks in order (position, id, status, phase, title), then the open backlog (id, severity, title); `tools/plan.sh <id>` prints one record of any kind. It reads files only and exits non-zero, naming the file, when an order id has no file or a file's name is not its `id`. Everything stays under `tasks/`, which `test/changes.sh` classes as plan (no CI tier runs).

## conventions (tasks)

- **status**: done | in_progress | todo | dropped
- **order**: `tasks/order.txt`, open task ids in priority order; the first open one is next; only the product owner changes it
- **delivery**: each task goes through the build workflow (`.claude/workflows/build.js`): checks written first and locked, one implementer until they pass, QA and one blind adversarial review with evidence-gated findings, one fix round; then PR, CI, merge, redeploy the dogfood
- **differential**: a task that adds or changes a route jellyfin-web or another client calls adds its requests to `test/differential/requests.yaml`, and any difference it keeps to `compat/differences.yaml` with a reason

## phases

| id | name | goal | status | source |
|---|---|---|---|---|
| S1 | Watching | Browse, play, convert, subtitles and audio for every film. | done | PRODUCT.md, The road there, 1 |
| S2 | Never interrupted | Any part of the cluster can fail mid-film without the viewer losing their place. | in_progress | PRODUCT.md, The road there, 2 |
| M1 | Its own API, first milestone | Octave signs in, browses Films and Shows with real titles and artwork, and starts playback on jellyscale's own API; then the cutover. | todo | PRODUCT.md, The road there, 3; D-109 |
| S3 | Its own API, for Octave's native apps | Octave's apps are the gates; Octave Web ships; an older Octave keeps working. | todo | PRODUCT.md, The road there, 3 |
| S4 | Watching from anywhere | The picture follows the connection, quick language switches, the shared upload, playback ready before Play. | todo | PRODUCT.md, The road there, 4 |
| S5 | People | Invitations, single sign-on, profiles, per-viewer access, hours and stream limits, devices and activity. | todo | PRODUCT.md, The road there, 5 |
| S6 | A living collection | Notifications, disc images, filters, search, my list, collections, more like this, versions and editions, previews and intro skipping, new films appearing, the operator's hardware. | todo | PRODUCT.md, The road there, 6 |
| S7 | Running on Kubernetes | Manifests and a Helm chart, autoscaling, upgrades without downtime, backup, migration from Jellyfin. | todo | PRODUCT.md, The road there, 7 |
| S8 | Together | Watching in step and moving a film between devices, across the cluster. | todo | PRODUCT.md, The road there, 8 |
| OPS | Operations of the project | Plan records, CI and the tools that keep delivery dependable. | in_progress | AGENTS.md |

## backlog

Findings judged real but not fixed by the task that found them. Read by the lead at each post-task review, and promoted into a task when worth doing. Not part of any task's brief: no role file or doc points subagents here.

One file per item in `tasks/backlog/`. `status`: open | check (may already be fixed) | promoted (to the task named in notes) | wontfix (reason in notes) | closed (reason in notes); `source`: the task whose review found it, or the review that did; `severity`: as the reviewer gave it.

## decisions

Product trade-offs and who accepted them. One file per decision in `tasks/decisions/`. Each entry: id, date, task, trade-off, decision (accept | keep | escalate), by (product-owner | owner), serves (the PRODUCT.md goal or invariant), brief (the brief as given, verbatim). A Changed guarantees entry, an amended check, or a weakened DOMAINS.md or docs statement cites its decision id. No film titles, library paths or personal details: this file is public.
