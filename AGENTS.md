# jellyscale

A horizontally scalable video server in Go for a private collection of films and shows, with its own API for its own apps (D-107). The Jellyfin API it began with is frozen until the cutover removes it (D-109). The chosen names, Zoetrope for the server and Praxi for the apps, wait for a name check (D-122, JS-200).

- Read [PRODUCT.md](PRODUCT.md) for what to build, [docs/status.md](docs/status.md) for what is built, [DOMAINS.md](DOMAINS.md) for the invariants, [ARCHITECTURE.md](ARCHITECTURE.md) for the design, and [spikes/README.md](spikes/README.md) for Phase 0 evidence.

## Commands

```sh
go test ./...                               # Unit tier
test/integration.sh                         # Every test, with Postgres, SeaweedFS, Garage and real ffmpeg
INTEGRATION_SHARD=0/6 test/integration.sh   # One slice: i/6 of internal/transcode and internal/api, or rest
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
- Write the simplest code that works, readable and maintainable by people. Add an abstraction only when something real needs it.
- Put a capability in the API contract from the viewer's outcome first, and observe the players a design depends on (D-107).
- Do no work on the Jellyfin routes, except to keep the gate green, until the cutover (D-109).
- Write criteria as outcomes, not mechanisms; cite Jellyfin as no requirement; a task that makes media states its storage figure (D-125).
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
- Before building a task a viewer or operator experiences (its `ux` flag; server-only changes such as failure reasons included), have the `designer` design it and the `ux-reviewer` review the design; send blockers and majors back. After Build, the `ux-reviewer` reviews what was built (D-138, D-139). The lead runs these around the workflow.
- Tell a role to load a skill it needs by name; don't rely on it choosing one.

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

- Use a free or discounted model wherever it can do the job: tasks run in parallel, and the Claude allowance is scarce (D-151). Run the roles on these models:
  - Build/Fix: `opencode/beam-free`, high effort. QA: `opencode/hy4-flash-free`, high effort. Snapshot helpers: `opencode/mimo-v2.6-flash-free`.
  - Refine, Check, Spec, design, architecture, adversarial and UX: `claude-agent/haiku` (Haiku 5.5), max effort.
  - Only for a `core: true` task: Check on `claude-agent/opus` (Opus 5.5), high effort, and the adversarial review on `claude-agent/sonnet` (Sonnet 5.5), high effort.
  - Product owner and advisor: `claude-agent/opus` (Opus 5.5), high effort, with questions batched. Fable only as below (D-098).
- Work down the free models in this order (D-106): `beam-free`, `hy4-flash-free`, `seaglass-alpha-free`, `deepseek-v4.1-flash-free`, `mimo-v2.6-flash-free`, `iceman-free`, `nemotron-3.5-super-vl-free`, `flint-40b-free`. Don't use `nemotron-3.5-lightning-free`. Re-rank on a graded task with hidden checks, and on how each does on real work.
- Give a free model a small, well-scoped, low-context job: reading logs, one failing test, running tests, a fix with a clear failing test, a mechanical edit. Keep large builds, test writing and reviews on the models above, and check a free model's key claims.
- Use an OpenCode Zen free model (`opencode/...-free`) for any other work that doesn't judge quality: searching, reading logs, running checks, captures, test diagnostics.
- When a free model is unavailable or fails the same step twice, try the next one down; use `claude-agent/haiku` at max effort when the list runs out or the job doesn't suit a free model, and `claude-agent/sonnet` only after Haiku fails the same step twice. Record it in the task record. Check a new free model on a task with a known answer before ranking it.
- Never give a free model a credential, or work on the homelab or the dogfood: a free tier may keep what it is sent.
- The `claude-agent/` models use the weekly Claude agent allowance, not the Anthropic API balance. Don't use `anthropic/` models. A paid `opencode-go/` model may be used within the plan's limit (D-151); never turn on extra usage. Command Code is no longer available.
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

[Octave](https://github.com/jellyscale-org/octave) is a separate product with its own product owner, plan and releases: the client jellyscale is designed for, with native apps for Android TV, Android phones, the web and Apple's phones and tablets, built side by side (D-129 to D-132). jellyscale's product owner plans jellyscale only. jellyscale's API contract is the source of truth for the server, and jellyscale ships a specific, tested release of Octave Web (D-141). jellyscale designs for no other client (D-107); Android TV and phones come first (D-121).

- Change nothing in Octave's repository, plan, issues or releases without its lead agreeing on an issue there.
- Send Octave an issue labelled `from:jellyscale` for each API contract change, saying what each platform must do, and for anything jellyscale needs from it (create the label if it is missing). Don't work around it here.
- Octave files requests here labelled `from:fluster` (its former name), by platform.
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
- **Advice when stuck.** When the lead is consistently stuck, or a decision's real cost is unclear, consult the `advisor` role (Opus 5.5 at high effort) blind. Send only the question, the constraints, and the minimum evidence needed; no whole diff, logs or tree. Tell it to use OpenCode Zen free or Sonnet agents through Task to find and read anything it needs, so its own input and output stay small. It returns a short recommendation, the risk and the check that would disprove it. Record material advice as a decision id. This is not a substitute for the product owner's trade-off briefs.
- **Use Fable 5.2 only for the one direction review after a phase, or when the owner asks for it by name (D-098).** Every other product-owner call and the advisor run on Opus 5.5, including trade-offs, issue triage and a new owner direction. Don't escalate to Fable because reviewers disagree.
- **Keep product-owner calls few (D-084).** Batch every pending question into one brief. Don't call for a routine priority confirmation or a decision a recorded decision id already covers; cite the id. Consult the advisor once per task at most, and only after two attempts have failed on the same question.
- **Per-task quality comes from cheaper checks.** Opus 5.5 is the per-task Check and Refine model; Sonnet 5.5 carries architecture, adversarial and UX review; OpenCode Zen's free models carry Build/Fix, QA and helpers.
- **Priorities and plan order are the product owner's alone.** The lead never edits a task's priority, moves a task in `tasks/prd.json`, or decides what is built next without the product owner's decision. Bring a brief with the options and costs (D-080).
- Follow its decisions.
- Let the product owner edit PRODUCT.md, which holds only the product and its direction; leave a change in what the product is for to the owner (D-091).
