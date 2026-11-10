#!/usr/bin/env bash
# tools/tiers.sh <tree> [seconds]: the build workflow's tier gate as one command,
# so the agent that runs it only has to repeat it and copy its answer.
#
# Makes sure .results/<tree>/ holds a record for the unit tier and each
# integration shard (or one record for the whole integration tier). It starts
# what has no record (the unit tier, and two integration shards at a time), waits
# up to <seconds> (default 540, under the Bash tool's 10-minute limit) and prints
# one JSON line: {"done": bool, "records": [{"name", "status", "summary", "log"}]}.
# done is false while a run is still going: run it again to keep waiting. A run
# already going for this checkout is waited on, never started twice. A run that
# ends without writing a record is reported as failed, never started again. If
# the working tree is not <tree>, it prints one record named "tree" that failed.
set -euo pipefail
tree=$1 wait=${2:-540}
repo=$(git rev-parse --show-toplevel)
cd "$repo"
dir=.results/$tree
out=$HOME/.cache/jellyscale/agents/tiers-$tree
names=(unit integration-rest integration-0of3 integration-1of3 integration-2of3)

report() { # report <done> <name:status:summary:log>...
  python3 - "$@" <<'EOF'
import json, sys
done, rows = sys.argv[1] == "true", sys.argv[2:]
records = []
for row in filter(None, rows):
    name, status, summary, log = row.split("\x1f")
    records.append({"name": name, "status": status, "summary": summary, "log": log})
print(json.dumps({"done": done, "records": records}))
EOF
}

if [[ $(bash tools/snapshot.sh) != "$tree" ]]; then
  report true "tree"$'\x1f'"fail"$'\x1f'"the working tree is not $tree: it changed after the build"$'\x1f'""
  exit 0
fi
mkdir -p "$out"

record_of() { # the record file that answers for <name>, if any
  if [[ -f $dir/$1 ]]; then echo "$dir/$1"
  elif [[ $1 == integration-* && -f $dir/integration ]]; then echo "$dir/integration"
  fi
}

running_of() { # prints the name of each test script run going in this checkout
  # Only a whole run writes a record: exactly `bash test/unit.sh` or `bash
  # test/integration.sh`, with no arguments. A shell whose command line merely
  # mentions a script, or a run with -run arguments, is not one.
  local pid argv shard
  for pid in $(pgrep -x bash || true); do
    [[ $(readlink "/proc/$pid/cwd" 2>/dev/null) == "$repo" ]] || continue
    mapfile -d '' argv <"/proc/$pid/cmdline" 2>/dev/null || continue
    ((${#argv[@]} == 2)) || continue
    if [[ ${argv[1]} == test/unit.sh ]]; then echo unit; continue; fi
    [[ ${argv[1]} == test/integration.sh ]] || continue
    shard=$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | sed -n 's/^INTEGRATION_SHARD=//p')
    if [[ -n $shard ]]; then echo "integration-${shard/\//of}"; else echo integration; fi
  done | sort -u
}

is_running() { # is_running <name>: a run of it, or of the whole integration tier, is going
  grep -qx "$1" <<<"$running" || { [[ $1 == integration-* ]] && grep -qx integration <<<"$running"; }
}

start() { # start <name>, detached, so it outlives this command
  local env=
  case $1 in
    unit) ;;
    integration-rest) env=INTEGRATION_SHARD=rest ;;
    integration-*of3) env=INTEGRATION_SHARD=${1#integration-}; env=${env/of//} ;;
  esac
  local script=test/integration.sh
  [[ $1 == unit ]] && script=test/unit.sh
  touch "$out/$1.started"
  setsid nohup env $env bash "$script" >"$out/$1.log" 2>&1 </dev/null &
}

deadline=$((SECONDS + wait))
while :; do
  running=$(running_of)
  pending=() lost=()
  for name in "${names[@]}"; do
    [[ -n $(record_of "$name") ]] && continue
    if is_running "$name"; then pending+=("$name"); continue; fi
    # Started by this script and gone without a record: report it, don't retry.
    if [[ -f $out/$name.started ]]; then lost+=("$name"); continue; fi
    pending+=("$name")
    if [[ $name == unit ]]; then start unit; running+=$'\n'unit; continue; fi
    shards=$(grep -c '^integration' <<<"$running" || true)
    if ((shards < 2)); then start "$name"; running+=$'\n'$name; fi
  done
  if ((${#pending[@]} == 0)) || ((SECONDS >= deadline)); then break; fi
  sleep $(( deadline - SECONDS < 10 ? deadline - SECONDS : 10 ))
done

rows=()
for name in "${names[@]}"; do
  file=$(record_of "$name")
  if [[ -n $file ]]; then
    rows+=("$(basename "$file")"$'\x1f'"$(sed -n 's/^status=//p' "$file")"$'\x1f'"$(sed -n 's/^summary=//p' "$file")"$'\x1f'"$(sed -n 's/^log=//p' "$file")")
  elif printf '%s\n' "${lost[@]}" | grep -qx "$name"; then
    rows+=("$name"$'\x1f'"fail"$'\x1f'"the run ended without writing a record (the tree changed during it, or it was killed)"$'\x1f'"$out/$name.log")
  fi
done
# One record for the whole integration tier answers for every shard: list it once.
mapfile -t rows < <(printf '%s\n' "${rows[@]}" | awk '!seen[$0]++')
if ((${#pending[@]} == 0)); then
  report true "${rows[@]}"
  [[ ${#lost[@]} -gt 0 ]] || rm -rf "$out"
else
  report false "${rows[@]}"
fi
