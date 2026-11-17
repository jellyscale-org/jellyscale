#!/usr/bin/env bash
# Writes test/shard-times.txt, which test/shard.sh uses to deal the tests of
# internal/transcode and internal/api to shards: for each package and top-level
# test, the mean seconds over the tests.json files in which that test passed (a
# failed result and a subtest are not counted). Each argument is a directory named
# for a merge-queue run of CI, holding the shard files that
#   gh run download <run> --pattern 'integration-shard-*' -D <dir>
# leaves (tests.json at any depth). The times are CI's, not a developer machine's.
#   shard-times.sh <run-id-dir>...
# SHARD_TIMES_OUT picks another output file. The same inputs give the same file.
set -euo pipefail
(( $# > 0 )) || { sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2; }
out=${SHARD_TIMES_OUT:-$(dirname "$0")/shard-times.txt}
ids=()
files=()
for dir in "$@"; do
  ids+=("$(basename "$dir")")
  while IFS= read -r f; do files+=("$f"); done < <(find "$dir" -name tests.json | LC_ALL=C sort)
done
{
  echo "# Mean seconds of each top-level test, by package, for test/shard.sh to deal internal/transcode and internal/api by."
  echo "# Made by test/shard-times.sh from the tests.json files of CI runs ${ids[*]} (passed tests only), on CI's runners."
  echo "# It only decides which shard a test is dealt to, never whether it passes."
  jq -R -r 'fromjson? | select(.Action == "pass" and .Test != null and (.Test | contains("/") | not)) | "\(.Package | sub("^jellyscale/"; "")) \(.Test) \(.Elapsed)"' "${files[@]}" |
    awk '{ k = $1 " " $2; sum[k] += $3; n[k]++ } END { for (k in sum) printf "%s %.1f\n", k, sum[k] / n[k] }' | LC_ALL=C sort
} >"$out.tmp"
mv "$out.tmp" "$out"
