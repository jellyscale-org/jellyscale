#!/usr/bin/env bash
# test/release-before.sh <output> builds the transcode integration test binary of
# the release before JS-030 (its worker), for the checks of a fleet of both
# releases when git is out of reach, as in test/ffmpeg61.sh's container:
#
#   test/release-before.sh /tmp/jellyscale-old.test
#   JELLYSCALE_TEST_RELEASE_BEFORE=/tmp/jellyscale-old.test test/ffmpeg61.sh go test -tags integration ./internal/transcode -run TestAWorkerOfTheReleaseBefore
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(realpath -m "$1")
rev=$(sed -n 's/^const releaseBefore = "\(.*\)"/\1/p' internal/transcode/one_reaper_integration_test.go)
dir=$(mktemp -d "${TMPDIR:-/tmp}/jellyscale-release-before.XXXXXX")
trap 'rm -rf "$dir"' EXIT
git archive "$rev" | tar -x -C "$dir"
# At this checkout's output version: workers of another one share no jobs (see releaseBefore).
version=$(sed -n 's/^	OutputVersion  = \([0-9]*\)$/\1/p' internal/playback/spec.go)
sed -i "s/OutputVersion  = [0-9]*/OutputVersion  = $version/" "$dir/internal/playback/spec.go"
(cd "$dir" && CGO_ENABLED=0 go test -tags integration -c -o "$out" ./internal/transcode)
