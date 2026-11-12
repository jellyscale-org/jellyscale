#!/usr/bin/env bash
# Splits the integration tier into shards that CI runs side by side.
#   shard.sh args  <i/n|rest>   the arguments for `go test`, one per line
#   shard.sh count <i/n|rest>   how many tests that shard has to run
# internal/transcode holds most of the tier's time, so its tests are dealt out
# by measured time (test/shard-times.txt, made by test/shard-times.sh): longest
# first, each to the shard with the least so far, as -run '^(A|B)$'. A test not
# in the file counts as the median, so a new test lands somewhere on its own.
# The same inputs always give the same split. The shard "rest" runs every other
# package. The lists come from the compiler (`go test -list`), so a new test or
# package lands in a shard on its own; test/check-shards.sh checks that the
# shards add up to the whole tier.
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

# Prints "<shard> <test>" for each test: the longest first, to the shard with the least total.
deal() {
  local times=${SHARD_TIMES:-test/shard-times.txt} median
  median=$(awk '!/^#/ { print $2 }' "$times" | sort -n | awk '{ v[NR] = $1 } END { print NR ? v[int((NR + 1) / 2)] : 1 }')
  awk -v n="$1" -v times="$times" -v median="$median" '
    BEGIN { while ((getline line < times) > 0) if (line !~ /^#/ && split(line, f, " ") == 2) t[f[1]] = f[2] + 0 }
    { name[NR] = $1; w[NR] = ($1 in t) ? t[$1] : median + 0 }
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
mine=$(names $heavy | LC_ALL=C sort | deal "$n" | awk -v i="$i" '$1 == i { print $2 }' | LC_ALL=C sort)
[[ -n $mine ]] || { echo "shard.sh: shard $shard has no tests" >&2; exit 1; }
if [[ $mode == count ]]; then
  echo "$mine" | wc -l | tr -d ' '
else
  printf '%s\n' -run "^($(echo "$mine" | paste -sd'|'))\$" $heavy
fi
