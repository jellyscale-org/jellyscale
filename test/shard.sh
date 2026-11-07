#!/usr/bin/env bash
# Splits the integration tier into shards that CI runs side by side.
#   shard.sh args  <i/n|rest>   the arguments for `go test`, one per line
#   shard.sh count <i/n|rest>   how many tests that shard has to run
# internal/transcode holds most of the tier's time, so its tests are dealt out
# by rank: sorted by name, shard i of n takes those whose rank % n == i, as
# -run '^(A|B)$'. The shard "rest" runs every other package. The lists come from
# the compiler (`go test -list`), so a new test or package lands in a shard on
# its own; test/check-shards.sh checks that the shards add up to the whole tier.
set -euo pipefail
cd "$(dirname "$0")/.."
heavy=./internal/transcode
mode=${1:-} shard=${2:-}
[[ $mode == args || $mode == count ]] && [[ $shard == rest || $shard =~ ^[0-9]+/[0-9]+$ ]] || {
  sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

names() { go test -tags integration -list '.*' "$@" | grep -E '^(Test|Example|Fuzz)' || true; }

if [[ $shard == rest ]]; then
  packages=$(go list -tags integration ./... | grep -v '/internal/transcode$')
  if [[ $mode == args ]]; then echo "$packages"; else names $packages | wc -l | tr -d ' '; fi
  exit 0
fi

i=${shard%/*} n=${shard#*/}
(( n >= 1 && i < n )) || { echo "shard.sh: $shard is not i/n with 0 <= i < n" >&2; exit 2; }
mine=$(names $heavy | LC_ALL=C sort | awk -v n="$n" -v i="$i" '(NR - 1) % n == i')
[[ -n $mine ]] || { echo "shard.sh: shard $shard has no tests" >&2; exit 1; }
if [[ $mode == count ]]; then
  echo "$mine" | wc -l | tr -d ' '
else
  printf '%s\n' -run "^($(echo "$mine" | paste -sd'|'))\$" $heavy
fi
