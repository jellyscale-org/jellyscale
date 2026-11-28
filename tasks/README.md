# tasks

The plan, the backlog and the decisions. [PRODUCT.md](../PRODUCT.md) says what to build; [docs/status.md](../docs/status.md) says what is built.

jellyscale is being rebuilt (D-20261128-start-net-new), to the plan here (D-20261128-first-plan). The earlier plan, backlog and decisions are in the tag `pre-rebuild`, as history and never as direction.

## Layout

One JSON file per record, `jq --indent 2`, the file's `id` equal to its name. No generated index.

- `tasks/tasks/<id>.json`: one task per file, open or finished. A file is never renamed or moved; finishing a task sets its `status`.
- `tasks/order.txt`: open task ids in priority order, one per line. Only the product owner changes it.
- `tasks/backlog/BL-<YYYYMMDD>-<slug>.json`: one finding kept for later.
- `tasks/decisions/D-<YYYYMMDD>-<slug>.json`: one decision.

`tools/plan.sh` lists the open tasks in order and the open backlog; `tools/plan.sh <id>` prints one record.

Ids are never reused. The earlier plan used `JS-001` to `JS-227`, and comments in the carried-over code still cite them, so a new task is numbered from `JS-300`.

## Tasks

A task is one capability a viewer or operator would notice, with its checks and its documents.

| Field | Meaning |
|---|---|
| `id`, `title` | The title says what a viewer or operator gets |
| `phase` | The stage of PRODUCT.md's road it belongs to |
| `description` | The line of PRODUCT.md it serves, quoted, and what is missing today |
| `acceptanceCriteria` | Outcomes, never mechanisms: what a viewer, an app or an operator can observe |
| `domains` | The domains it touches (DOMAINS.md) |
| `ux`, `core`, `strong`, `small` | How it is built and reviewed ([docs/delivery.md](../docs/delivery.md)) |
| `status` | `todo`, `in_progress`, `done` or `dropped` |
| `dependsOn`, `notes` | |

A task that makes media states what it costs in storage.

## Backlog

Findings judged real and not fixed by the task that found them, each described by what a viewer or operator would notice. The product owner manages it. It is no part of any task's brief: nothing points an agent that builds here.

`status`: `open`, `promoted` (to the task named in `notes`), `wontfix` or `closed`, with the reason.

## Decisions

Who decided what, and what was given up. **Only these files are decisions.** A sentence in any other document is a description or a rule.

| Field | Meaning |
|---|---|
| `id`, `date` | The date is when it was decided |
| `by` | `owner` or `product-owner` |
| `task` | What it is about |
| `brief` | The question as it was put, or where it came from |
| `tradeoff` | What is given up, and by whom |
| `decision` | `accept`, `keep`, `change`, `defer`, or `open` for a question recorded as unanswered |
| `serves` | The line of PRODUCT.md it rests on |
| `entry` | What was decided. The owner's own words are quoted exactly; anything else is marked as someone's record of them |
| `replaces` | Earlier records this one stands in for |

- A decision that gives something up is cited where the loss shows: in the task, the changed rule or the document.
- A record that is replaced is removed, so nothing superseded can be read as current.
- This directory is public: no names, credentials, film titles, paths or details of anyone's servers.

## The task log

After each task, append one line per task and one per defect that got past the reviews to `.results/tasks.jsonl` (not in git):

```json
{"type": "task", "task": "JS-300", "date": "2026-12-01", "attempts": 1, "lead_corrections": 0, "minutes": {"spec": 20, "build": 50, "verify": 30}, "models": {"build": "opencode/beam-free"}, "findings": [{"reviewer": "adversarial", "round": 1, "severity": "major", "evidence": "test", "real": true, "fixed": true, "title": "..."}]}
{"type": "escape", "task": "JS-300", "date": "2026-12-01", "caught_by": "merge-queue", "defect": "...", "why_missed": "..."}
```

`reviewer`: `check`, `qa`, `adversarial`, `ux` or `lead`. `evidence`: `test`, `command` or `reading`. `caught_by`: `merge-queue`, `ci`, `test-installation` or `user`. A model swapped for a fallback is named in `models`.
