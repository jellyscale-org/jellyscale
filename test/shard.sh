#!/usr/bin/env bash
# Splits the integration tier into shards that CI runs side by side.
#   shard.sh args  <i/n|rest>   the arguments for `go test`, one per line
#   shard.sh list  <i/n|rest>   one line "<package> <test>" for each test the shard runs, sorted
#   shard.sh count <i/n|rest>   how many tests that is
# internal/transcode and internal/api hold most of the tier's time, so their tests
# are dealt out together by measured time (test/shard-times.txt, made by
# test/shard-times.sh from CI's runs): longest first, each to the shard with the
# least so far. A shard's `args` are one go test run for each package it holds,
# as -run '^(A|B)$' <package>, a blank line between two runs, so a test name in
# both packages runs once in each. A test not in the file counts as its median, so a
# new test lands somewhere on its own. The same inputs always give the same split.
# The shard "rest" runs every other package, whole. The lists come from the compiler
# (`go test -list`), so a new test or package lands in a shard on its own;
# test/check-shards.sh checks that the shards add up to the whole tier.
set -euo pipefail
cd "$(dirname "$0")/.."
dealt=(internal/transcode internal/api)
mode=${1:-} shard=${2:-}
[[ $mode == args || $mode == list || $mode == count ]] && [[ $shard == rest || $shard =~ ^[0-9]+/[0-9]+$ ]] || {
  sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

# "<package relative to the module> <test>" for each test of the given packages.
names() {
  go test -tags integration -list '.*' "$@" | awk '
    /^ok/ { p = $2; sub(/^[^\/]*\//, "", p); for (i = 0; i < n; i++) print p, b[i]; n = 0; next }
    /^(Test|Example|Fuzz)/ { b[n++] = $1 }' | LC_ALL=C sort
}

if [[ $shard == rest ]]; then
  packages=$(go list -tags integration ./... | grep -vE "/($(IFS='|'; echo "${dealt[*]}"))\$")
  if [[ $mode == args ]]; then echo "$packages"; exit 0; fi
  # shellcheck disable=SC2086
  mine=$(names $packages)
else
  # Prints "<shard> <package> <test>" for each test: the longest first, to the shard with the least total.
  deal() {
    local times=${SHARD_TIMES:-test/shard-times.txt}
    awk -v n="$1" -v times="$times" '
      BEGIN {
        while ((getline line < times) > 0) if (line !~ /^#/ && split(line, f, " ") == 3) { t[f[1] " " f[2]] = f[3] + 0; v[++k] = f[3] + 0 }
        # The median of the file: the middle of the seconds in order.
        for (a = 2; a <= k; a++) { x = v[a]; b = a - 1; while (b >= 1 && v[b] > x) { v[b + 1] = v[b]; b-- } v[b + 1] = x }
        median = k ? v[int((k + 1) / 2)] : 1
      }
      { name[NR] = $0; w[NR] = ($0 in t) ? t[$0] : median }
      END {
        # Order by weight, heaviest first, then by name; the name makes ties deterministic.
        for (k = 1; k <= NR; k++) order[k] = k
        for (a = 2; a <= NR; a++) {
          x = order[a]; b = a - 1
          while (b >= 1 && (w[order[b]] < w[x] || (w[order[b]] == w[x] && name[order[b]] > name[x]))) { order[b + 1] = order[b]; b-- }
          order[b + 1] = x
        }
        for (k = 1; k <= NR; k++) {
          best = 0
          for (s = 1; s < n; s++) if (total[s] < total[best]) best = s
          total[best] += w[order[k]]
          print best, name[order[k]]
        }
      }'
  }
  i=${shard%/*} n=${shard#*/}
  (( n >= 1 && i < n )) || { echo "shard.sh: $shard is not i/n with 0 <= i < n" >&2; exit 2; }
  mine=$(names "${dealt[@]/#/./}" | deal "$n" | awk -v i="$i" '$1 == i { print $2, $3 }' | LC_ALL=C sort)
  [[ -n $mine ]] || { echo "shard.sh: shard $shard has no tests" >&2; exit 1; }
  if [[ $mode == args ]]; then
    first=1
    for p in "${dealt[@]}"; do
      tests=$(awk -v p="$p" '$1 == p { print $2 }' <<<"$mine" | paste -sd'|')
      [[ -n $tests ]] || continue
      (( first )) || echo
      first=0
      printf '%s\n' -run "^($tests)\$" "./$p"
    done
    exit 0
  fi
fi
if [[ $mode == count ]]; then echo "$mine" | wc -l | tr -d ' '; else echo "$mine"; fi
