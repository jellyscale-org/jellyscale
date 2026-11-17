#!/usr/bin/env bash
# Splits the integration tier into shards that CI runs side by side.
#   shard.sh args  <i/n|rest> [<k>/<m>|alone]   the arguments for `go test`, one per line
#   shard.sh list  <i/n|rest> [<k>/<m>|alone]   one line "<package> <test>" for each test the shard runs, sorted
#   shard.sh count <i/n|rest> [<k>/<m>|alone]   how many tests that is
# A job can run its shard as m processes at once (INTEGRATION_PROCS, test/integration.sh):
# <k>/<m> is the share of process k, the shard's tests dealt out again by time, longest
# first, with the tests of test/alone.txt left out (SHARD_ALONE names another file). Those
# run on their own after the shares and are the shard's `alone`. The shares and `alone` hold
# every test of the shard once. A share's `args` are one go test run for each package, as for
# a dealt shard; for rest, one run over all the packages of the share.
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
mode=${1:-} shard=${2:-} part=${3:-}
[[ $mode == args || $mode == list || $mode == count ]] && [[ $shard == rest || $shard =~ ^[0-9]+/[0-9]+$ ]] && [[ -z $part || $part == alone || $part =~ ^[0-9]+/[0-9]+$ ]] || {
  sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
  exit 2
}

# "<package relative to the module> <test>" for each test of the given packages.
names() {
  go test -tags integration -list '.*' "$@" | awk '
    /^ok/ { p = $2; sub(/^[^\/]*\//, "", p); for (i = 0; i < n; i++) print p, b[i]; n = 0; next }
    /^(Test|Example|Fuzz)/ { b[n++] = $1 }' | LC_ALL=C sort
}

# Reads "<package> <test>" lines and prints "<shard> <package> <test>" for each test: the longest first, to the shard with the least total.
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
if [[ $shard == rest ]]; then
  packages=$(go list -buildvcs=false -tags integration ./... | grep -vE "/($(IFS='|'; echo "${dealt[*]}"))\$")
  if [[ $mode == args && -z $part ]]; then echo "$packages"; exit 0; fi
  # shellcheck disable=SC2086
  mine=$(names $packages)
  names_rest=$mine
else
  i=${shard%/*} n=${shard#*/}
  (( n >= 1 && i < n )) || { echo "shard.sh: $shard is not i/n with 0 <= i < n" >&2; exit 2; }
  mine=$(names "${dealt[@]/#/./}" | deal "$n" | awk -v i="$i" '$1 == i { print $2, $3 }' | LC_ALL=C sort)
  [[ -n $mine ]] || { echo "shard.sh: shard $shard has no tests" >&2; exit 1; }
fi
if [[ -n $part ]]; then
  alone_file=${SHARD_ALONE:-test/alone.txt}
  # The lines of $mine that test/alone.txt names (want 1), or the rest (want 0).
  pick() {
    awk -v file="$alone_file" -v want="$1" '
      BEGIN { while ((getline line < file) > 0) if (line !~ /^#/ && split(line, f, " ") >= 2) a[f[1] " " f[2]] = 1 }
      (($1 " " $2) in a) == want { print }'
  }
  if [[ $part == alone ]]; then
    mine=$(pick 1 <<<"$mine")
  else
    k=${part%/*} m=${part#*/}
    (( m >= 1 && k < m )) || { echo "shard.sh: $part is not k/m with 0 <= k < m" >&2; exit 2; }
    mine=$(pick 0 <<<"$mine" | deal "$m" | awk -v k="$k" '$1 == k { print $2, $3 }' | LC_ALL=C sort)
    [[ -n $mine ]] || { echo "shard.sh: share $part of shard $shard has no tests" >&2; exit 1; }
  fi
fi
if [[ $mode == args && $shard == rest ]]; then
  # A share of rest is one go test run over its packages. A name in two packages would then
  # run in both, whichever share holds it, so that is refused.
  dup=$(awk '{ print $2 }' <<<"$names_rest" | sort | uniq -d | head -1)
  [[ -z $dup ]] || { echo "shard.sh: $dup is a test of two packages of rest, which a run of one share would run in both" >&2; exit 1; }
  [[ -z $mine ]] || {
    printf '%s\n' -run "^($(awk '{ print $2 }' <<<"$mine" | paste -sd'|'))\$"
    awk '{ print "./" $1 }' <<<"$mine" | sort -u
  }
  exit 0
fi
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
if [[ $mode == count ]]; then
  if [[ -z $mine ]]; then echo 0; else echo "$mine" | wc -l | tr -d ' '; fi
else
  [[ -z $mine ]] || echo "$mine"
fi
