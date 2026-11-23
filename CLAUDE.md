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
test/scripts_test.sh                        # Tests of the test scripts
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
- Test only application logic.
- Never write a test that reads docs, Markdown, `tasks/` or `.claude/`.
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

- Run the roles on these models: Refine on Opus 5.5 at high effort; every other workflow role on Sonnet 5.5 at high effort; the product owner on Fable 5.2 at high effort.
- Keep reviewers blind: they see the task, the checks, the diff and the docs, never the implementer's reasoning or another reviewer's findings.
- Treat the merge queue as the one full gate.
- Run locally only what a change touches.
- Fix a queue failure on a new commit and queue again.
- Before landing, read the run's `unfinished` list and finish every item.
- Before landing, delete any test that reads docs, Markdown, `tasks/` or `.claude/`.

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
- Follow its decisions.
- Leave changes to PRODUCT.md, and to what the product is for, to the owner.
