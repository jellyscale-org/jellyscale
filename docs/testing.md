# Testing

```sh
go test ./...                               # Tests that need nothing outside Go
test/integration.sh                         # Every Go test, with disposable Postgres, SeaweedFS, Garage and real ffmpeg
INTEGRATION_SHARD=0/6 test/integration.sh   # One slice, as CI runs it
test/roles.sh <workdir>                     # Each role as its own process
test/ffmpeg61.sh go test -tags integration ./internal/transcode -run X   # Under ffmpeg 6.1, as CI
```

The scripts, shards and CI of the code in the tree are described in [engine/test-history.md](engine/test-history.md), with every past failure that taught something.

## What a test is for

- A test proves what the server does for a viewer or an operator. Nothing else is tested: not documents, static content, generated files, CI configuration, the shape of source code, lists of fixtures, or the test tools themselves. A test tool proves itself by running.
- A test can fail. A bug's test is shown to fail without the fix.
- A missing dependency fails a test, naming what is missing. It never skips.

## Two tiers

- `go test ./...` needs only Go.
- A test that needs ffmpeg, Postgres, S3 or a font lives in a `*_integration_test.go` file tagged `//go:build integration`. The file name and the tag must agree: a mistyped tag hides a test without a skip.
- The integration tier needs Docker, and ffmpeg with libx264, libx265, libsvtav1 and libass, and a font.
- Disposable infrastructure only, on Docker-assigned ports. Never a service already running on the machine.

## Rules that came from failures

These classes of test caused most of the failures that reached the merge queue.

- **Mechanisms, not speeds.** No test passes or fails on a measured duration, a speed, or a ratio of two. Such a test passes on a developer's 16 cores and fails on a shared 2-CPU runner with a right build. Assert what happened: how many runs, which worker made which segment, the order of events. Log timings; don't assert them. A duration may only bound a hang, at ten times what a right build takes or more.
- **Force the interleaving.** A test of a race or an ordering holds the statement, waits for the lock wait, or stops the process at the step it names. A test that runs many rounds and hopes passes on a wrong build here and fails in CI.
- **Wait for your own state.** In a forced interleaving, every wait is for a state only the test's next step can end, asked of the one session it means, never of "any session that waits": background work asks for the same rows.
- **Leave the shared database as the migrations made it.** A trigger, function or setting a test adds is removed in a cleanup registered before it is made.
- **A test that shortens a production timing says what that could hide**, and when the claim is about how soon something happens, one test runs at the production timing.
- **Run whole packages.** A change is verified by the whole of every package it touches, never by a name filter: filters repeatedly missed the test that failed.
- **ffmpeg 6.1 behaves differently.** CI runs the oldest supported ffmpeg, and a newer local one hides what it does differently. Run tests of ffmpeg arguments, probing or fixtures under `test/ffmpeg61.sh` before the queue does.
- **A test that shares a stack with earlier ones** can't assume a first play: stored output is reused. Run it in its whole gate, not alone.
- **Concurrency needs real processes or real contention.**
- **A CI failure is never fixed with a skip, a retry or a longer timeout.** A test that fails beside others and passes alone is fixed at its cause.

## Recording a failure

A failure that taught something is recorded here, under this heading, with the date, what failed, the cause and what changed. A failure seen once and not explained is recorded too, with where its logs are. Never as an executable test of the test.

Nothing recorded yet for the new server.
