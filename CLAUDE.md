# jellyscale

A horizontally scalable, Jellyfin-compatible video server in Go. Read [PRODUCT.md](PRODUCT.md) for what to build and [DOMAINS.md](DOMAINS.md) for the invariants. Phase 0 evidence is in [spikes/README.md](spikes/README.md).

## Commands

```sh
go test ./...                               # Tests that need nothing outside Go
test/integration.sh                         # Every test, with Postgres, SeaweedFS, Garage and real ffmpeg
test/roles.sh <workdir>                     # Each role as its own process
test/e2e/run.sh <workdir> <playwright_dir>  # Real jellyfin-web in Chromium
go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml
```

Tests that need ffmpeg, Postgres, S3 or a font live in `*_integration_test.go` files tagged `//go:build integration`; the integration tier needs Docker, ffmpeg with libx264, libx265, libsvtav1 and libass, and a font. Go tests run through `go tool gotestsum`. `test/e2e` needs `web-dist` copied from the Jellyfin image and a directory with `playwright-core` installed; see the README.

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

Non-trivial work goes through the `deliver` workflow in `.claude/workflows/deliver.js`: an architecture review of the design, implement, then a blind architecture review, two blind adversarial reviews, QA, Product, and UX when a change affects what viewers or operators experience. Role definitions are in `.claude/agents/`, and the intended design is in [ARCHITECTURE.md](ARCHITECTURE.md).

Blind means a reviewer sees the task, the diff and these docs, never the implementer's reasoning or another reviewer's findings. Keep it that way when changing the workflow.

After fixes, the architecture reviewer, both adversarial reviewers, QA, and every other reviewer who raised a blocking finding run again.

Keep a slice small enough to converge in one or two review rounds: one capability, and its tests and docs. Skip the design review only for a fix pass on work whose design has been reviewed; a slice that touches the scan, a store or job semantics always gets one. A run that stops because the tree changed returns its reviews and both tree hashes; look at the change before going on.
