#!/usr/bin/env bash
# Writes test/shard-times.txt, which test/shard.sh uses to balance the
# internal/transcode shards: the mean seconds of each top-level test that
# passed in the gotestsum JSON files given (tests.json, which test/integration.sh
# keeps under .results/<tree>/integration*.d/ and CI uploads for a failed job).
#   shard-times.sh <tests.json>...
set -euo pipefail
(( $# > 0 )) || { sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2; }
out=$(dirname "$0")/shard-times.txt
{
  echo "# Mean seconds of each top-level internal/transcode integration test, for test/shard.sh."
  echo "# Made by test/shard-times.sh on $(date +%F) from $# tests.json files (passed runs only, a 16-CPU developer machine)."
  echo "# It only decides which shard a test is dealt to, never whether it passes."
  jq -R -r 'fromjson? | select(.Action == "pass" and .Package == "jellyscale/internal/transcode" and .Test != null and (.Test | contains("/") | not)) | "\(.Test) \(.Elapsed)"' "$@" |
    awk '{ sum[$1] += $2; n[$1]++ } END { for (t in sum) printf "%s %.1f\n", t, sum[t] / n[t] }' | LC_ALL=C sort
} >"$out.tmp"
mv "$out.tmp" "$out"
