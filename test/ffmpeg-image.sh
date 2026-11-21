#!/usr/bin/env bash
# test/ffmpeg-image.sh <command...> runs a command, usually go test, with the ffmpeg of the server image (the Dockerfile's
# alpine:3.24 and its `apk add ffmpeg`): the ffmpeg viewers' servers run, which is neither CI's (Ubuntu 24.04's 6.1, see
# test/ffmpeg61.sh) nor a developer's. The same mounts, network and environment as test/ffmpeg61.sh. The image is built once,
# as jellyscale-ffmpeg-image, from the Dockerfile's own two lines, so the base's version and package are read from it.
#
#   test/ffmpeg-image.sh go test -tags integration -count=1 ./internal/transcode -run TestX
set -euo pipefail
cd "$(dirname "$0")/.."
image=jellyscale-ffmpeg-image
if ! docker image inspect "$image" >/dev/null 2>&1; then
  base=$(sed -n 's/^FROM \(alpine:[^ ]*\) AS base$/\1/p' Dockerfile)
  packages=$(sed -n 's/^RUN apk add --no-cache \(.*\)$/\1/p' Dockerfile | head -n 1)
  [[ -n $base && -n $packages ]] || { echo "the Dockerfile's base stage has changed shape: no alpine base or apk line found" >&2; exit 1; }
  docker build -q -t "$image" - >/dev/null <<DOCKERFILE
FROM $base
RUN apk add --no-cache $packages gcompat ca-certificates
DOCKERFILE
fi
goroot=$(go env GOROOT)
cache=${XDG_CACHE_HOME:-$HOME/.cache}/jellyscale/go-build-ffmpeg-image
mkdir -p "$cache"
pass=()
for name in $(compgen -e | grep '^JELLYSCALE_TEST_' || true); do pass+=(-e "$name"); done
cpus=()
if [ -n "${JELLYSCALE_TEST_CPUS:-}" ]; then cpus=(--cpuset-cpus "$JELLYSCALE_TEST_CPUS"); fi
exec docker run --rm "${cpus[@]}" --network host --user "$(id -u):$(id -g)" \
  -v "$PWD:$PWD" -w "$PWD" -v "$goroot:$goroot:ro" \
  -v "$(go env GOMODCACHE):/gomod" -v "$cache:/gocache" \
  -e GOMODCACHE=/gomod -e GOCACHE=/gocache -e HOME=/tmp -e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
  -e PATH="$goroot/bin:/usr/sbin:/usr/bin:/sbin:/bin" "${pass[@]}" "$image" "$@"
