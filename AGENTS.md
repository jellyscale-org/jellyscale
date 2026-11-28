# jellyscale

A self-hosted streaming server in Go for a private collection of films and shows, with its own API for its own apps, Octave. It is being rebuilt: new plan, new server code, and only the video engine brought over (D-20261128-start-net-new).

This file is for every agent and holds only what the code can't tell you. `CLAUDE.md` imports it.

## Read first

| For | Read |
|---|---|
| What to build | [PRODUCT.md](PRODUCT.md) |
| What is built, and what in the tree will go | [docs/status.md](docs/status.md) |
| The rules a change must keep | [DOMAINS.md](DOMAINS.md), then `docs/domains/<domain>.md` for each domain the task names, and always `docs/domains/platform.md` |
| The intended design | [ARCHITECTURE.md](ARCHITECTURE.md) |
| The carried-over video engine | [docs/engine/README.md](docs/engine/README.md), then the page for the part you touch |
| What players were seen to do | [docs/players.md](docs/players.md) |
| How to test | [docs/testing.md](docs/testing.md) |
| The plan and the decisions | [tasks/README.md](tasks/README.md) |
| How a task is run: only if you are the lead, the session a person talks to | [docs/delivery.md](docs/delivery.md) |

Only the files in `tasks/decisions/` are decisions. A line in any other file is a description or a rule, never the owner's or the product owner's decision. Text may have been carried over from the earlier server: when something reads like a decision and no decision file records it, say so and ask.

## Commands

```sh
go test ./...                               # Tests that need nothing outside Go
test/integration.sh                         # Every Go test, with Postgres, SeaweedFS, Garage and real ffmpeg
INTEGRATION_SHARD=0/6 test/integration.sh   # One slice, as CI runs it
test/roles.sh <workdir>                     # Each role as its own process
test/ffmpeg61.sh go test -tags integration ./internal/transcode -run X   # Under ffmpeg 6.1, as CI
tools/plan.sh                               # Open tasks in order; tools/plan.sh <id> prints one record
```

The test scripts take a workdir, clean up after themselves and record each whole run in `.results/<tree>/`.

## Rules

**The product**

- Build from the viewer's or operator's outcome. Write criteria as outcomes, never mechanisms (D-20261127-outcomes-not-mechanisms).
- Nothing is built because another server has it (D-20261128-no-jellyfin-features). Copy no other server's code.
- Don't build on the earlier server's code, and don't treat it as a pattern. It stays only until it is replaced.
- When a build would give up something a viewer or operator has, stop and say so. The product owner decides, and decides the order of work; no one else does.

**Code**

- Write the simplest code that works, readable and maintainable by people. Add an abstraction only when something real needs it.
- Match the surrounding code. Comments explain why, not what.
- Before changing the engine, read its page in `docs/engine/`. When the change makes a fact there untrue, change the page in the same change.
- A capability is in the published contract before the server serves it. Observe the players a design depends on; don't guess ([docs/players.md](docs/players.md)).
- Call nothing working until a real player, or a real store, exercised it. Nothing fails silently.

**Tests** ([docs/testing.md](docs/testing.md) has the reasons)

- Test only what the server does for a viewer or operator. Never test documents, static content, generated files, CI configuration, the shape of source, lists of fixtures, or the test tools.
- Write tests that can fail. Show that a bug's test catches the bug.
- Never change application code to fit a test, and never weaken a test to fit the code.
- A missing dependency fails a test; it never skips. A CI failure is never fixed with a skip, a retry or a longer timeout.
- No test passes or fails on a measured duration or speed. Test concurrency with real processes or real contention, and force the interleaving.
- Verify a change with the whole of every package it touches, never a name filter.
- Record a failure that taught something in docs/testing.md, not as a test.

**Safety**

- Work only in the checkout you were given. Don't commit or push unless asked.
- Use disposable infrastructure on Docker-assigned ports. Never connect to a service already running on the machine. Never stop a process or container you did not start.
- Wait for a long run with one command: foreground with a timeout of at most 10 minutes, or one background run and its notification. Never a polling loop.
- This repository and Octave's are public. Write no personal name, credential, real film title, local path or detail of anyone's servers into a file, commit, issue or comment.
- Only the lead touches the test installation, and nobody changes the owner's other servers without the owner's approval. Never give a credential or private data to a free model.

**Your role**

- If you were started for one job, you were given a role file in `.claude/agents/`. Read it first and stay inside it.
- A model's "it passes" is a claim, not evidence. Report each command you ran with its real result, and say plainly what you did not finish.
- Don't change Octave's repository. What jellyscale needs from Octave is an issue there, raised by the lead.
