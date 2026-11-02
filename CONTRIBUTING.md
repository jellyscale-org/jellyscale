# Contributing

Thanks for helping. Read [PRODUCT.md](PRODUCT.md) for what's in scope and [DOMAINS.md](DOMAINS.md) for the invariants before starting anything large, and open an issue first for substantial changes.

## Setup

You need Go. Docker, and ffmpeg with libx264, libx265 and libass (and a font), are for the integration tier. See the README for running the stack and the test suites.

```sh
go test ./...        # Tests that need nothing outside Go
test/integration.sh  # Everything, with disposable Postgres, SeaweedFS and Garage and real ffmpeg
```

A test that needs ffmpeg, Postgres, S3 or a font goes in a file named `*_integration_test.go` that starts with `//go:build integration`, and fails when the dependency is missing (`internal/testenv` says so by name). Never skip for a missing dependency.

## Unexplained test failures

These were each seen on a developer machine, have not recurred on demand, and have no known cause. If you see one, keep the logs.

- `ffmpeg made no segment in 30s` on the copied 1080p film. The e2e log also shows this message from a scenario that freezes ffmpeg on purpose; that one is expected.
- `a bitrate cap re-encodes a film that would be copied` timed out in `leave()`. jellyfin-web sends no Stopped while changing stream, and the test now waits for the switch, but the failure was not reproduced, so that isn't shown to be the cause.

## Rules

- **Don't copy Jellyfin code.** Jellyfin is GPL-2.0, and jellyscale is written from observed behavior and public API documentation. Observe a real server instead.
- **Show that it works.** Include the commands you ran. For client behavior, test a real client.
- **A missing dependency fails a test.** A CI failure is never fixed by adding a skip.
- **Tests must be able to fail.** A bug fix needs a test that fails without it. Concurrency claims need real contention.
- **Scope:** new aliases for obsolete routes are only accepted for jellyfin-web.
- If a route changes, regenerate the manifest: `go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml`.

Pull requests are squash-merged, so the title becomes the commit message.

By contributing, you agree your work is licensed under GPL-3.0-or-later.
