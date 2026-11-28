# Delivery: for the lead

How a task gets from the plan to `main`. The lead is the session a person talks to, which runs tasks and starts other agents. No other agent needs this file: a builder or reviewer follows `AGENTS.md` and its role file.

## A task

Tasks are `tasks/tasks/<id>.json`, built in `tasks/order.txt` order, each in its own worktree. A task is one capability with its checks and its documents. Independent tasks are built side by side.

**A small task** adds no behaviour a viewer, an operator or Octave can see: a fix with a failing test, a removal, a mechanical change, documents. One builder writes the failing test (if any) and the change; the whole affected packages are run; the lead reads the diff. Nothing touching sign-in, playback, players, access or stored data is small. When unsure, it isn't.

**Every other task** runs the `build` workflow (`.claude/workflows/build.js`):

1. **Spec.** A spec writer turns the criteria into checks and shows each fails today. It may sharpen a criterion, never weaken one. The checks are then locked.
2. **Check.** One blind reviewer reads the design and the checks before anything is built: would each check fail on a wrong build and pass on a right one? A blocker or major sends the checks back once. A blocker left after that stops the run.
3. **Build.** One implementer works until the checks and the whole of every package it touched pass. Changing or disputing a locked check stops the run; the lead settles it from evidence.
4. **Verify,** side by side:
   - **Run:** the checks and the whole affected packages once, then each criterion tried by hand.
   - **Blind review:** one adversarial reviewer tries to break it, and says whether it is in the right place and what it costs. It sees the task, the checks, the diff and the documents, never the implementer's reasoning.
   - **UX review,** for a task with `ux`.
5. **Fix.** One round, for findings with evidence, at their cause. Only the reviewers whose findings were fixed look again. Evidenced findings left after that send the task back to Spec.

A blocker or major needs evidence that runs: a failing test, or a command with its output. Without it the finding is a minor. Minors never start a fix round; they go to the product owner, batched.

**Flags on a task**

- `ux`: a viewer or operator experiences it (a refusal's reason, a log line, configuration and documents included). Before Spec, the `designer` designs it and the `ux-reviewer` reviews the design; blockers and majors go back (D-20261127-design-before-build).
- `core`: it touches stored data or its migration, access, or the engine's jobs, leases or stores. Stronger models review it (below).
- `strong`: it touches sign-in, playback or a video player, test players included. The strongest models check, build and review it (D-20261127-strong-models).

**Landing**

- The merge queue is the one full gate. Locally, run what the change touches, whole.
- Fix a queue failure at its cause on a new commit and queue again. Look at the image build after a merge.
- Before landing, finish every item on the run's `unfinished` list, and delete any test whose subject is not what the server does.
- The test installation may be redeployed after a task. Any other change to the owner's servers needs the owner's approval (D-20261128-standing-instructions).

**After each task**

- Set the task's `status` to `done` (or `dropped`) in its file.
- Append one line per task, and one per defect that got past the reviews, to `.results/tasks.jsonl` ([tasks/README.md](../tasks/README.md) has the shape).
- Bring the product owner, in one brief, the findings left unfixed and any untriaged issue from Octave.

## Models

Use a free or discounted model wherever it can do the job; the Claude allowance is scarce (D-20261127-free-models-first, D-20261127-build-models).

| Work | Model |
|---|---|
| Build and Fix | Free first: `opencode/beam-free`, high effort. When the free models can't: `claude-agent/sonnet`, high |
| Run, helpers, reading logs, searching | Free: `opencode/hy4-flash-free`, `opencode/mimo-v2.6-flash-free` |
| Spec, Check, design, blind review, UX review | `claude-agent/haiku`, max effort |
| A `core` task | Check on `claude-agent/opus`, high; blind review on `claude-agent/sonnet`, high |
| A `strong` task | Check, Build, Fix and blind review on `claude-agent/opus`, high |
| Product owner, advisor | `claude-agent/opus`, high |

- Free models, in order: `beam-free`, `hy4-flash-free`, `seaglass-alpha-free`, `deepseek-v4.1-flash-free`, `mimo-v2.6-flash-free`, `iceman-free`, `nemotron-3.5-super-vl-free`, `flint-40b-free`. Not `nemotron-3.5-lightning-free`. When one is unavailable or fails the same step twice, take the next.
- "Free usage limit reached" is shared between the free models. When it appears, go to the paid fallback (Sonnet for a build, Haiku otherwise) and record it in the task's line.
- NVIDIA's free models (`nvidia/...`) are slow: only for jobs that can wait.
- Use Fable 5.2 only when the owner asks for it. Don't use `anthropic/` models (they bill the API). A paid `opencode-go/` model only within the plan's limit; never turn on extra usage. Command Code is not available. Gemini is a reserve, not used unless the owner asks.
- A defect a cheaper review should have caught is recorded in `.results/tasks.jsonl` and brought to the product owner.

**Checking a cheaper model's work.** Free models have edited the wrong checkout, edited locked checks, weakened assertions, added an ignore file to pass a check, restored files they were told to leave, and reported success on all of it.

- Give one a small, well-scoped job with little context: one failing test, a mechanical edit, running tests, reading a log.
- Commit your own work before one works in a checkout. Afterwards run `git status` in every checkout and read the whole diff.
- Look for changed checks, new skips, build tags and ignore files, and assertions made weaker.
- Its "passes" is not evidence: the whole affected packages are run by someone else.
- Check any "missing" or "not found" claim, and any research claim, against the source.
- Never give one a credential or private data, or work on the owner's servers: a free tier may keep what it is sent.

## Running it

- **Roles** are files in `.claude/agents/`: `spec-writer`, `implementer`, `qa`, `adversarial-reviewer`, `designer`, `ux-reviewer`, `product-owner`, `advisor`. The workflow tells each agent to read its role file first; when you start one by hand, do the same. Keep reviewers blind.
- **The workflow** is started with the Workflow tool as `build`, with `{ task, acceptance, tests, id, root, domains, ux, core, strong }`. `root` is the task's worktree, and `models` overrides a role's model for one run, as when the free models are out of allowance. It stops and returns when a check is disputed or a blocker stands; the lead settles it from evidence, corrects a wrong check, and resumes with `resolved`.
- **Model names** above are OpenCode's (`provider/model`), the harness the lead usually runs in. `CLAUDE.md` gives the names for Claude Code alone.
- **Skills:** tell a role to load a skill it needs by name; don't rely on it choosing one.
- **The advisor** is for the lead alone, when stuck after two attempts on the same question, at most once per task. Send only the question, the constraints and the least evidence.

## The product owner

Priorities and the order of the plan are the product owner's alone. The lead never edits `tasks/order.txt`, moves a task or decides what is built next.

- Ask a fresh `product-owner` agent. Never give it the code.
- **Batch.** Put every pending question in one brief. Don't ask what a recorded decision already answers; cite it.
- **A trade-off brief** says what a viewer or operator has today and after, who is affected and how often, the evidence with unverified claims marked, and every option with its cost, keeping the guarantee among them. No recommendation.
- **Plan changes** and findings a run left unfixed go to it. Ask for a direction review after each stage of the road.
- Record each decision in one file, `tasks/decisions/D-<YYYYMMDD>-<slug>.json`, quoting the owner's own words where the owner decided.
- A change in what the product is for goes to the owner. Only the owner and the product owner edit PRODUCT.md.
- The owner is not a go-between for sessions: the issue boards and the decision files are the record.

## Octave

[Octave](https://github.com/jellyscale-org/octave) is a separate product with its own owner, plan and releases (D-20261127-octave-separate-product). jellyscale's contract is the source of truth for the server; jellyscale designs for no other client.

- Change nothing in Octave's repository, plan, issues or releases without its lead agreeing on an issue there.
- Send Octave an issue labelled `from:jellyscale` for each change to the contract, saying what each platform must do, and for anything jellyscale needs from it. Search its issues first. Don't work around it here.
- Octave's requests arrive as issues labelled `from:fluster` (its former name). Every one goes to the product owner before any work, batched into one triage brief after a task. The lead labels each, posts the product owner's comment and closes declined ones.
- In an issue, say what a viewer or operator needs, why, and how to see it.
