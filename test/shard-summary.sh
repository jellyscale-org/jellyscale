#!/usr/bin/env bash
# One line for each integration shard: the seconds of the top-level tests it ran,
# summed from its own tests.json (CI downloads each shard's artifact apart, since
# every shard's file has the same name). When the slowest of the dealt shards (the
# ones named <i>of<n>) is more than 1.25 times the fastest, it adds the line
# "refresh test/shard-times.txt": the times that deal the tests are out of date.
# rest is printed, not compared: test/shard.sh does not deal it.
# A shard with no tests.json is named as missing, never counted as 0 seconds.
# It is a notice and always exits 0.
#   shard-summary.sh <dir> <shard name>...     e.g. shard-logs 0of5 1of5 rest
dir=${1:-}
[[ -d $dir ]] || { sed -n '2,/^# *shard-summary/p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 0; }
shift
min= max=
for name in "$@"; do
  files=()
  while IFS= read -r f; do files+=("$f"); done < <(find "$dir" -mindepth 2 -path "$dir/*$name*" -name tests.json 2>/dev/null | LC_ALL=C sort)
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "integration shard $name: no tests.json (missing)"
    continue
  fi
  secs=$(jq -R -r 'fromjson? | select((.Action == "pass" or .Action == "fail") and .Test != null and (.Test | contains("/") | not)) | .Elapsed' "${files[@]}" | awk '{ s += $1 } END { printf "%.6f", s }')
  echo "integration shard $name: $(awk -v s="$secs" 'BEGIN { printf "%.0f", s }')s of tests"
  [[ $name =~ ^[0-9]+of[0-9]+$ ]] || continue
  [[ -z $min ]] || awk -v a="$secs" -v b="$min" 'BEGIN { exit !(a < b) }' && min=$secs
  [[ -z $max ]] || awk -v a="$secs" -v b="$max" 'BEGIN { exit !(a > b) }' && max=$secs
done
if [[ -n $min ]] && awk -v lo="$min" -v hi="$max" 'BEGIN { exit !(hi > 1.25 * lo) }'; then
  echo "refresh test/shard-times.txt: the slowest dealt shard ran $(awk -v s="$max" 'BEGIN { printf "%.0f", s }')s of tests and the fastest $(awk -v s="$min" 'BEGIN { printf "%.0f", s }')s (more than 1.25 times)"
fi
exit 0
