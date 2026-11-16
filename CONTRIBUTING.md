# Contributing

Thanks for helping. Read [PRODUCT.md](PRODUCT.md) for what's in scope and [DOMAINS.md](DOMAINS.md) for the invariants before starting anything large, and open an issue first for substantial changes.

## Setup

You need Go. Docker, and ffmpeg with libx264, libx265 and libass (and a font), are for the integration tier. See [docs/running.md](docs/running.md) for running the stack and [docs/testing.md](docs/testing.md) for the test suites.

```sh
go test ./...        # Tests that need nothing outside Go
test/integration.sh  # Everything, with disposable Postgres, SeaweedFS and Garage and real ffmpeg
```

A test that needs ffmpeg, Postgres, S3 or a font goes in a file named `*_integration_test.go` that starts with `//go:build integration`, and fails when the dependency is missing (`internal/testenv` says so by name). Never skip for a missing dependency.

## Unexplained test failures

These were each seen on a developer machine, have not recurred on demand, and have no known cause. If you see one, keep the logs.

- `ffmpeg made no segment in 30s` on the copied 1080p film. The e2e log also shows this message from a scenario that freezes ffmpeg on purpose; that one is expected.
- `a bitrate cap re-encodes a film that would be copied` timed out in `leave()`. jellyfin-web sends no Stopped while changing stream, and the test now waits for the switch, but the failure was not reproduced, so that isn't shown to be the cause. `leave()` also waits for the player page now: going back before it is ready leaves the film playing and sends no Stopped, the same symptom. That failure can be forced by delaying the page's chunk, and a timeout reports where the page and video were. It happened again in CI after two quality changes, in `audio copy: a bitrate cap the track doesn't fit re-encodes the audio, and one it fits copies it`, with the page at `#/details` and no video: the page left the player, and no Stopped came. Both sightings follow a stream change, and jellyfin-web sends no Stopped while it believes a change is still under way, so that is the likeliest cause, still unproven.
- `S3 burn-in of subtitle files beside the film` (test/e2e/subtitles.mjs) timed out once in three full runs at the seek to 30 s after choosing track 2; the server log showed that seek's job stopped as "no viewer is watching" and no later job started. Six runs of that scenario alone (`E2E_GATES=subtitles E2E_ONLY="S3 burn-in of subtitle files"`) and six of all four burn-in scenarios (`E2E_ONLY="burn-in"`) all passed, so it is not reproduced and its cause is unknown. Nothing was changed for it. If it recurs, keep `server.log` and `ffmpeg.log` from `$JELLYSCALE_LOG_DIR`.
- `S3 burn-in: text is in the picture` (test/e2e/subtitles.mjs) timed out once waiting for a PlaybackInfo, in an e2e run on the audio-copy branch, and passed on the rerun. It is the second S3 burn-in timeout in two releases; the two may share a cause.

## Releases

Every commit that lands on `main` gets an image run (.github/workflows/image.yml), which publishes `main-<12 hex>` and `main-<12 hex>-fixtures` to ghcr.io/jellyscale-org/jellyscale once test/image.sh has passed on those images. A release is cut by the owner, in this order:

1. Choose a commit of `main` whose image run passed.
2. Push the tag `vX.Y.Z` on it (`git tag vX.Y.Z <commit> && git push origin vX.Y.Z`). Only a tag of exactly `v<number>.<number>.<number>` publishes anything.
3. Wait for that tag's image run: it tests the images again and publishes `X.Y.Z` and `X.Y.Z-fixtures`.
4. Make a GitHub release for the tag with generated notes.

A version is never published twice: the run fails naming the tag if `X.Y.Z` or `X.Y.Z-fixtures` exists. A mistake gets the next version, and the tag of the mistake stays as it is.

The first time, the owner makes the package public by hand (package settings on GitHub) after the first push, and decides the first version. If that first run stops with "cannot be asked" (HTTP 401 or 403) before any push, ghcr.io would not answer the workflow's token about a package that does not exist yet: the owner publishes the first image once by hand: build and test the commit with `JELLYSCALE_BUILT_AS=<name> test/image.sh`, `docker login ghcr.io` with a token that may write packages, then `GITHUB_SHA=<commit> GITHUB_REF_TYPE=branch GITHUB_REF_NAME=main REGISTRY_USER=<you> REGISTRY_TOKEN=<token> test/publish.sh ghcr.io/jellyscale-org/jellyscale <name>:server <name>:fixtures`. Later runs find the package.

## Rules

- **Don't copy Jellyfin code.** Jellyfin is GPL-2.0, and jellyscale is written from observed behavior and public API documentation. Observe a real server instead.
- **Show that it works.** Include the commands you ran. For client behavior, test a real client.
- **A missing dependency fails a test.** A CI failure is never fixed by adding a skip.
- **Tests must be able to fail.** A bug fix needs a test that fails without it. Concurrency claims need real contention.
- **Scope:** new aliases for obsolete routes are only accepted for jellyfin-web.
- If a route changes, regenerate the manifest: `go run ./cmd/jellyscale --print-routes > compat/endpoints.yaml`.

Pull requests are squash-merged, so the title becomes the commit message.

By contributing, you agree your work is licensed under GPL-3.0-or-later.
