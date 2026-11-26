# jellyscale

A horizontally scalable, Jellyfin-compatible video server in Go.

- Read [PRODUCT.md](PRODUCT.md) for what to build, [docs/status.md](docs/status.md) for what is built, [DOMAINS.md](DOMAINS.md) for the invariants, [ARCHITECTURE.md](ARCHITECTURE.md) for the design, and [spikes/README.md](spikes/README.md) for Phase 0 evidence.

## Commands

```sh
go test ./...                               # Unit tier
test/integration.sh                         # Every test, with Postgres, SeaweedFS, Garage and real ffmpeg
INTEGRATION_SHARD=0/4 test/integration.sh   # One slice: i/4 of internal/transcode and internal/api, or rest
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web in Chromium
E2E_GATES=transcode E2E_SHARD=1/3 test/e2e/run.sh ...  # One slice of a gate; E2E_ONLY=regex picks scenarios
E2E_REPEAT=5 test/e2e/run.sh ...            # Warm reruns on one stack
test/differential/run.sh <workdir>          # Jellyfin 12.4 and jellyscale asked the same requests
test/ffmpeg61.sh go test -tags integration ./internal/transcode -run X  # Under ffmpeg 6.1, as CI
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
compat/gen-schema.sh                        # compat/schema-12.4.json from the jellyfin/jellyfin:12.4 container
```

- Put tests that need ffmpeg, Postgres, S3 or a font in `*_integration_test.go` files tagged `//go:build integration`.
- Install for the integration tier: Docker, ffmpeg with libx264, libx265, libsvtav1 and libass, and a font.
- Give the test scripts a workdir; they clean up after themselves and record each run in `.results/<tree>/`.
- Copy `web-dist` from the Jellyfin image into the checkout before running `test/e2e/run.sh`. See [docs/testing.md](docs/testing.md).

## Rules

- Prefer retrieval-led reasoning over training-led reasoning.
- Write the simplest code that works. Add an abstraction only when something real needs it.
- Observe a real Jellyfin 12.4 before implementing a protocol.
- Don't port Jellyfin code.
- Match the surrounding code. Write comments that explain why, not what.
- Test only application behaviour: prove what the server does for a viewer or operator. Never write a test for documentation, prose, static content, generated manifests, CI or workflow YAML, source or AST shape, fixture inventories, capture-evidence completeness, coverage tables, or the test harness and test scripts themselves. Test tools prove themselves by running. Record historical test failures in [docs/testing.md](docs/testing.md).
- Write tests that can fail. Show that a bug's regression test catches it.
- Test concurrency with real processes or real contention.
- Never change application code to fit a test.
- Fail a test on a missing dependency; never skip it.
- Never fix a CI failure with a skip, a retry or a longer timeout.
- Use Docker-assigned ports for disposable infrastructure.
- Never connect to a service already running on this machine.
- Wait for a long run with one command: foreground with a timeout of at most 10 minutes, or one background run and its notification.
- Never start a polling loop.
- Don't commit or push unless asked.

## Delivery

- Build each task in [tasks/prd.json](tasks/prd.json) with the `build` workflow (`.claude/workflows/build.js`), highest priority first.
- Build independent tasks side by side, each in its own worktree under `~/.cache/jellyscale/` (`args.root`).
- Keep each task to one capability, its checks and its docs.

The workflow's steps:

0. **Refine.** Opus 5.5 at high effort sharpens the acceptance criteria against the code. It may make a criterion more exact, never weaker. The lead records the refined criteria in `tasks/prd.json`.
1. **Spec.** A spec writer turns the criteria into checks (tests, e2e scenarios, capture-based tests) and shows each fails today.
2. **Check.** An architecture reviewer reviews the design and the checks.
   - A blocker or major sends the run back to Spec once.
   - Majors left after that bind the implementer.
   - A blocker left after that stops the run. When two locked requirements contradict each other, the lead corrects a check and resumes with `resolved.settled`.
3. **Build.** One implementer works until the checks and its changed packages' tests pass.
   - The checks are locked: changing or disputing one stops the run.
   - The lead settles a dispute from evidence, corrects a wrong check, and resumes with `resolved` (files and why).
   - `resolved.unlock` names files locked by accident that hold no check.
   - `resolved.light` has only QA and the adversarial reviewer look again.
4. **Verify.**
   - QA runs the checks and the touched packages' tests once.
   - One blind adversarial reviewer tries to break the build.
   - Architecture reviews again for `core: true`.
   - UX reviews for `ux: true`.
   - A blocker or major without executable evidence counts as minor.
5. **Fix.** One round for evidenced findings, at their cause.
   - Only the reviewers whose findings were fixed look again.
   - Evidenced findings left after that send the task back to Spec.
   - Minors go to the product owner.

- Run the roles on these models (D-076, D-086):
  - Refine and Check: `claude-agent/opus` (Opus 5.5), high effort.
  - Spec, architecture, adversarial and UX: `claude-agent/sonnet` (Sonnet 5.5), high effort.
  - Product owner and advisor: `claude-agent/fable` (Fable 5.2), high effort.
  - Build/Fix: `opencode/deepseek-v4.1-flash-free`, high effort. QA: `opencode/beam-free`, high effort. Snapshot helpers: `opencode/mimo-v2.6-flash-free`.
- Use an OpenCode Zen free model (`opencode/...-free`) for any other work that doesn't judge quality: searching, reading logs, running checks, captures, test diagnostics.
- Replace a free model with `claude-agent/sonnet` when it is unavailable or fails the same step twice, and record it in the task record. Check a free model on a task with a known answer before routing a role to it.
- Never give a free model a credential, or work on the homelab or the dogfood: a free tier may keep what it is sent.
- The `claude-agent/` models use the weekly Claude agent allowance, not the Anthropic API balance. Don't use `anthropic/` or paid `opencode-go/` models.
- Keep reviewers blind: they see the task, the checks, the diff and the docs, never the implementer's reasoning or another reviewer's findings.
- Treat the merge queue as the one full gate.
- Run locally only what a change touches.
- Fix a queue failure on a new commit and queue again.
- Before landing, read the run's `unfinished` list and finish every item.
- Before landing, delete any test whose subject is not application behaviour.

## After each task

- Append one line per task, and one per escape, to `.results/tasks.jsonl`:

```json
{"type": "task", "task": "JS-024", "date": "2026-11-08", "attempts": 1, "lead_corrections": 2, "minutes": {"spec": 31, "build": 160}, "tokens": {"spec": 186143, "build": 400000}, "findings": [{"reviewer": "architecture", "round": 1, "severity": "major", "evidence": "reading", "real": true, "fixed": true, "title": "..."}]}
{"type": "escape", "task": "JS-023", "date": "2026-11-08", "caught_by": "merge-queue", "defect": "...", "why_missed": "..."}
```

  - `reviewer`: `check`, `qa`, `adversarial`, `architecture`, `ux` or `lead`.
  - `evidence`: `test`, `command`, `capture` or `reading`.
  - `caught_by`: `merge-queue`, `ci`, `dogfood` or `user`.
- Move the finished task from `tasks/prd.json` to `tasks/done.json`.
- Bring the product owner the run's unfixed findings and what the application now needs.
- Amend `tasks/prd.json` by pull request as the product owner decides.

## Octave

[Octave](https://github.com/jellyscale-org/octave) is jellyscale's own client. Its web app is to become jellyscale's default web client (owner, D-085). The two projects ask each other for work through GitHub issues.

- Octave files an issue on this repository when it wants a feature or finds a gap, labelled `from:fluster` (its former name).
- When jellyscale wants a feature or a change from Octave, or finds a bug in it, file an issue on `jellyscale-org/octave`, labelled `from:jellyscale` (create the label if it is missing). Don't work around it here.
- In such an issue, say what a viewer or operator needs, why, and how to see it. Search Octave's issues first. Never include a credential, a name, a real film title, a path or a homelab address.
- Send every issue that comes from Octave to the product owner before any work on it. Batch all untriaged ones into one triage brief after a task; with none, make no call. D-084's once-per-phase cap doesn't hold back this triage.
- The lead labels each triaged issue, posts the product owner's comment and closes declined ones.

## Product owner

- Ask a fresh `product-owner` agent (`.claude/agents/product-owner.md`) for trade-offs, the plan and direction reviews.
- Never give it the code.
- **Trade-off.** When a build would give up something a viewer or operator has, write a brief with:
  - what they have today and after;
  - who is affected and how often;
  - the evidence, with unverified claims marked;
  - every option, with its cost, the guarantee kept among them;
  - no recommendation.
- **Plan.** Send it every plan amendment and every finding a run left unfixed.
- **Direction.** Ask for a direction review after each phase.
- Record each brief and decision in `tasks/decisions.json`.
- Cite a decision id in every Changed guarantees entry, amended check, and weakened DOMAINS.md or docs statement.
- **Advice when stuck.** When the lead is consistently stuck, or a decision's real cost is unclear, consult the `advisor` role (Fable 5.2 at high effort) blind. Send only the question, the constraints, and the minimum evidence needed; no whole diff, logs or tree. Tell it to use OpenCode Zen free or Sonnet agents through Task to find and read anything it needs, so its own input and output stay small. It returns a short recommendation, the risk and the check that would disprove it. Record material advice as a decision id. This is not a substitute for the product owner's trade-off briefs.
- **Fable is a scarce phase-gate reviewer (D-084).** Call the product owner on Fable 5.2 at most once per phase, and only for a new product trade-off, a direction review, or a decision that cannot be derived from recorded decisions. Batch every pending question into that one brief. Do not call it for routine priority confirmations, a single decision already covered by a decision id, or a per-task check that Opus 5.5 and the blind reviewers can settle. The advisor uses the same budget: one call per task only after two attempts have failed on the same question. If a Fable call would only confirm existing policy, cite the decision and do not call.
- **Per-task quality comes from cheaper checks.** Opus 5.5 is the per-task Check and Refine model; Sonnet 5.5 carries architecture, adversarial and UX review; OpenCode Zen's free models carry Build/Fix, QA and helpers. Escalate to Fable only when those disagree or a product trade-off remains.
- **Priorities and plan order are the product owner's alone.** The lead never edits a task's priority, moves a task in `tasks/prd.json`, or decides what is built next without the product owner's decision. Bring a brief with the options and costs (D-080).
- Follow its decisions.
- Leave changes to PRODUCT.md, and to what the product is for, to the owner.
