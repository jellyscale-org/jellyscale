# jellyscale

A horizontally scalable, Jellyfin-compatible video server in Go. Read [PRODUCT.md](PRODUCT.md) for what to build and [DOMAINS.md](DOMAINS.md) for the invariants. Phase 0 evidence is in [spikes/README.md](spikes/README.md).

## Commands

```sh
go test ./...                               # Tests that need nothing outside Go
test/integration.sh                         # Every test, with Postgres, SeaweedFS, Garage and real ffmpeg
INTEGRATION_SHARD=0/3 test/integration.sh   # A slice of it: i/3 of internal/transcode, or rest (CI runs the four side by side)
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web in Chromium
E2E_GATES=transcode E2E_SHARD=1/3 test/e2e/run.sh ...  # A slice of a gate; E2E_ONLY=regex picks scenarios
E2E_REPEAT=5 test/e2e/run.sh ...            # Warm reruns on one stack, "passed p of n" per scenario (a flake hunt)
test/differential/run.sh <workdir>          # A real Jellyfin 12.4 and jellyscale asked the same requests (needs Docker, ffmpeg)
test/scripts_test.sh                        # Tests of the test scripts
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

The scripts keep their temp files in the workdir you give them (else under `/tmp/jellyscale-test`) and clean up after themselves, and after killed runs, cache the e2e films (`E2E_FIXTURE_CACHE`), and record each whole run in `.results/<tree>/` for QA to reuse (see [docs/testing.md](docs/testing.md)). `test/e2e/run.sh` needs `web-dist` in the checkout it runs from, and says so at once if it is missing.

Tests that need ffmpeg, Postgres, S3 or a font live in `*_integration_test.go` files tagged `//go:build integration`; the integration tier needs Docker, ffmpeg with libx264, libx265, libsvtav1 and libass, and a font. Go tests run through `go tool gotestsum`. `test/e2e` needs `web-dist` copied from the Jellyfin image and a directory with `playwright-core` installed; see [docs/testing.md](docs/testing.md).

## Working rules

- Prefer retrieval-led reasoning over training-led reasoning.
- Write the simplest code that works. People have to read and maintain this: prefer plain, direct code over clever or general code, and add an abstraction only when something real needs it.
- Observe a real Jellyfin 12.4 before implementing a protocol. Don't port Jellyfin code.
- Match the surrounding code. Comments explain why, not what.
- Tests must be able to fail. When fixing a bug, show the regression test catches it.
- Concurrency claims need a test with real processes or real contention; a single process proves little.
- Disposable infrastructure uses Docker-assigned ports. Never connect to a service already running on this machine.
- A missing dependency fails a test; it never skips one. A CI failure is never fixed by adding a skip.
- Don't commit or push unless asked.

## Delivery

Each task in [tasks/prd.json](tasks/prd.json) goes through the `build` workflow in `.claude/workflows/build.js`, highest priority first. Its shape follows the evidence that agents converge when they loop against checks that can fail, and that reviewer comments are mostly noise unless they come with evidence:

1. **Spec.** A spec writer turns the acceptance criteria into executable checks (tests, e2e scenarios, capture-based tests) and shows each fails today. Protocol behaviour is observed on a real Jellyfin 12.4 first.
2. **Check.** An architecture reviewer reviews the design and whether the checks would catch a wrong build. A blocker sends it back to Spec once.
3. **Build.** One implementer works until the checks and the tiers pass. The checks are locked: changing one, or disputing it, stops the run.
4. **Verify.** QA runs it; one blind adversarial reviewer tries to break it where the checks don't look; architecture reviews it again only for changes to the scan, a store or job semantics (`core: true`); UX when viewers or operators would notice (`ux: true`). A blocker or major needs executable evidence (a failing test, a command and its output, a capture diff), or it counts as minor. Environment failures are reported apart.
5. **Fix.** One round for evidenced findings, then Verify again. Evidenced findings left after that send the task back to Spec: the checks missed something. Minors never start a fix round; they go to a backlog the lead triages.

Blind means a reviewer sees the task, the checks, the diff and these docs, never the implementer's reasoning or another reviewer's findings. Keep it that way when changing the workflow. Role definitions are in `.claude/agents/`, and the intended design is in [ARCHITECTURE.md](ARCHITECTURE.md).

After each task, record what the run found in `.results/tasks.jsonl` (gitignored): the task id, minutes and tokens per phase, each finding with whether it was real, its evidence kind and whether it was fixed, and later any defect that escaped to CI, the dogfood or a user, with the task it came from. Then mark the task done in `tasks/prd.json`, consider what the whole application now needs, and amend `tasks/prd.json` by pull request with the reason.

Keep a task small enough to converge in one build and one fix round: one capability, its checks and docs.
