#!/usr/bin/env bash
# test/ffmpeg61.sh <command...> runs a command, usually go test, in Ubuntu
# 24.04 with its ffmpeg 6.1: the oldest ffmpeg jellyscale supports and the one
# CI runs. A newer ffmpeg hides some defects (a muxer that writes what the
# arguments allow, an x265 built with options Ubuntu's lacks), which otherwise
# show up first in the merge queue.
#
#   test/ffmpeg61.sh go test -tags integration -count=1 ./internal/transcode -run TestX
#
# The host's Go toolchain and module cache are mounted, and JELLYSCALE_TEST_*
# is passed through, so a test can reach a Postgres or S3 the caller started
# on a Docker-assigned port (the container shares the host network for that).
# The image is built once, as jellyscale-ffmpeg61.
set -euo pipefail
cd "$(dirname "$0")/.."
image=jellyscale-ffmpeg61
if ! docker image inspect "$image" >/dev/null 2>&1; then
  docker build -q -t "$image" - >/dev/null <<'DOCKERFILE'
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg fontconfig fonts-dejavu-core gcc libc6-dev ca-certificates && rm -rf /var/lib/apt/lists/*
DOCKERFILE
fi
goroot=$(go env GOROOT)
cache=${XDG_CACHE_HOME:-$HOME/.cache}/jellyscale/go-build-ffmpeg61
mkdir -p "$cache"
pass=()
for name in $(compgen -e | grep '^JELLYSCALE_TEST_' || true); do pass+=(-e "$name"); done
exec docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD:$PWD" -w "$PWD" -v "$goroot:$goroot:ro" \
  -v "$(go env GOMODCACHE):/gomod" -v "$cache:/gocache" \
  -e GOMODCACHE=/gomod -e GOCACHE=/gocache -e HOME=/tmp -e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false \
  -e PATH="$goroot/bin:/usr/sbin:/usr/bin:/sbin:/bin" "${pass[@]}" "$image" "$@"
