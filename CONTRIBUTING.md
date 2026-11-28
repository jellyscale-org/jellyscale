# Contributing

Thanks for helping. jellyscale is being rebuilt ([docs/status.md](docs/status.md)), so open an issue before starting anything: the plan is not written yet, and code in the tree may be about to go.

Read [PRODUCT.md](PRODUCT.md) for what is in scope, [DOMAINS.md](DOMAINS.md) for the rules a change must keep, and [AGENTS.md](AGENTS.md) for the rules of working here; they hold for people as for agents.

## Setup

You need Go. Docker, and ffmpeg with libx264, libx265, libsvtav1 and libass, and a font, are for the integration tier.

```sh
go test ./...        # Tests that need nothing outside Go
test/integration.sh  # Everything, with disposable Postgres, SeaweedFS and Garage and real ffmpeg
```

[docs/testing.md](docs/testing.md) says what a test is for and which kinds of test have failed us.

## Rules

- **Copy no other server's code.** jellyscale is written for its own product, from its own design and from what real players are observed to do.
- **Show that it works.** Include the commands you ran. For a player's behaviour, observe a real player and record it in [docs/players.md](docs/players.md).
- **Tests must be able to fail.** A bug fix needs a test that fails without it. Concurrency claims need real contention.
- **A missing dependency fails a test.** A CI failure is never fixed by adding a skip, a retry or a longer timeout.
- **Keep the engine's facts true.** A change to the video engine that makes a line in [docs/engine/](docs/engine/README.md) untrue changes that line.
- **Nothing private.** No names, credentials, real film titles or paths in code, tests, fixtures, issues or commits.

Pull requests are squash-merged, so the title becomes the commit message.

## Releases

None while the rebuild is under way. CI still publishes an image of `main` for each commit, built from the earlier server's code; it is not a release of the new server.

By contributing, you agree your work is licensed under GPL-3.0-or-later.
