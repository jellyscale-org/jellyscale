#!/usr/bin/env bash
# Runs one e2e gate against a stand-in browser, from a directory holding exactly the modules
# test/e2e/run.sh copies beside the gates (so a gate that imports another fails here, as it would there).
# Usage: run-gate.sh <repo> <workdir> <gate> [env VAR=value ...]; prints the gate's output, exits as it did.
set -uo pipefail
repo=$1 work=$2 gate=$3
shift 3
mkdir -p "$work/play/node_modules" "$work/state" "$work/scratch"
# FAKE_PLAYWRIGHT=playwright-play: a page that plays a film (for the failover gate's roll scenarios); the default one refuses everything but the sign-in.
cp -r "$repo/test/e2e/fake/${FAKE_PLAYWRIGHT:-playwright-core}" "$work/play/node_modules/playwright-core"
for f in $(grep -E '^cp ' "$repo/test/e2e/run.sh" | grep -oE 'test/e2e/[A-Za-z0-9_-]+\.mjs'); do cp "$repo/$f" "$work/play/"; done
export FAKE_STATE=$work/state PATH="$repo/test/e2e/fake/bin:$PATH" TMPDIR=$work
node "$repo/test/e2e/fake/server.cjs" >"$work/state/port" 2>/dev/null &
server=$!
for _ in $(seq 1 50); do [[ -s $work/state/port ]] && break; sleep 0.1; done
# The worker a scenario may stop: a process of its own, so that stopping it harms nothing else.
sleep 3600 &
worker=$!
export E2E_API_REPLICA="$repo/test/e2e/fake/replica" E2E_STREAM_REPLICA="$repo/test/e2e/fake/replica"
export E2E_SCRATCH="$work/scratch" E2E_WORKER_PID=$worker E2E_PG=fake-pg E2E_LOG="$work/server.log" E2E_RESPONSES="$work/responses.jsonl"
export E2E_MEDIA="$work/media" E2E_ASSETS="$work/assets" E2E_WORKER="$repo/test/e2e/fake/replica" E2E_NOSTORE="$repo/test/e2e/fake/replica"
: >"$E2E_RESPONSES"
base="http://127.0.0.1:$(cat "$work/state/port")"
export E2E_BASE_NOSTORE=$base
(cd "$work/play" && env "$@" node "$gate.mjs" "$base" admin pass 2>&1)
status=$?
kill "$server" "$worker" 2>/dev/null
exit $status
