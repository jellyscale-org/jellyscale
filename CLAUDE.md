# jellyscale

A horizontally scalable, Jellyfin-compatible video server in Go. Read [PRODUCT.md](PRODUCT.md) for what to build and [DOMAINS.md](DOMAINS.md) for the invariants. Phase 0 evidence is in [spikes/README.md](spikes/README.md).

## Commands

```sh
go test ./...                               # Unit tests
test/integration.sh                         # Adds Postgres, SeaweedFS, Garage
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web in Chromium
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

`test/e2e` needs `web-dist` copied from the Jellyfin image and a directory with `playwright-core` installed; see the README.

## Working rules

- Write the simplest code that works. People have to read and maintain this: prefer plain, direct code over clever or general code, and add an abstraction only when something real needs it.
- Observe a real Jellyfin 12.4 before implementing a protocol. Don't port Jellyfin code.
- Match the surrounding code. Comments explain why, not what.
- Tests must be able to fail. When fixing a bug, show the regression test catches it.
- Concurrency claims need a test with real processes or real contention; a single process proves little.
- Disposable infrastructure uses Docker-assigned ports. Never connect to a service already running on this machine.
- Don't commit or push unless asked.

## Delivery

Non-trivial work goes through the `deliver` workflow in `.claude/workflows/deliver.js`: implement, then two blind adversarial reviews, QA, Product, and UX when a change affects what viewers or operators experience. Role definitions are in `.claude/agents/`.

Blind means a reviewer sees the task, the diff and these docs, never the implementer's reasoning or another reviewer's findings. Keep it that way when changing the workflow.

After fixes, both adversarial reviewers, QA, and every other reviewer who raised a blocking finding run again.
