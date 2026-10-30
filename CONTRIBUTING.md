# Contributing

Thanks for helping. Read [PRODUCT.md](PRODUCT.md) for what's in scope and [DOMAINS.md](DOMAINS.md) for the invariants before starting anything large, and open an issue first for substantial changes.

## Setup

You need Go, Docker and ffmpeg. See the README for running the stack and the test suites.

```sh
go test ./...        # Unit tests
test/integration.sh  # Adds disposable Postgres, SeaweedFS and Garage
```

## Rules

- **Don't copy Jellyfin code.** Jellyfin is GPL-2.0, and jellyscale is written from observed behavior and public API documentation. Observe a real server instead.
- **Show that it works.** Include the commands you ran. For client behavior, test a real client.
- **Tests must be able to fail.** A bug fix needs a test that fails without it. Concurrency claims need real contention.
- **Scope:** new aliases for obsolete routes are only accepted for jellyfin-web.
- If a route changes, regenerate the manifest: `go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml`.

Pull requests are squash-merged, so the title becomes the commit message.

By contributing, you agree your work is licensed under GPL-3.0-or-later.
