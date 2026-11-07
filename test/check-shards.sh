#!/usr/bin/env bash
# Checks that parallel shards added up to the whole, from the records they leave
# (CI downloads each shard's file into <dir>). A test or scenario that no shard
# ran, a shard that ran nothing, and a shard that is missing all fail.
#   check-shards.sh integration <dir> <total>
#       Records are lines "integration <shard> <ran>". The ran counts must sum
#       to <total>, the tests `go test -list` finds for the tier.
#   check-shards.sh line <gate output file>
#       Prints "<i> <n> <ran> <total>" from the file's last line, "shard i/n: ran
#       k of T", and fails if there is no such line (the gate stopped partway) or
#       it ran no scenario. test/e2e/run.sh runs it on every gate.
#   check-shards.sh e2e <dir> <gates>
#       Records are lines "<gate> <i> <n> <ran> <total>", from each gate's last
#       line "shard i/n: ran k of T". Per gate, every shard 0..n-1 must be there
#       once, agree on n and T, run something, and run T between them. Every gate
#       named in <gates> (run.sh's default list, in quotes) must have records, so
#       a gate that no job ran fails.
set -uo pipefail
kind=${1:-} dir=${2:-}
if [[ $kind == line ]]; then
  last=$(tail -n 1 "$dir" 2>/dev/null)
  if [[ ! $last =~ ^shard\ ([0-9]+)/([0-9]+):\ ran\ ([0-9]+)\ of\ ([0-9]+)$ ]]; then
    echo "FAIL  the gate printed no \"shard i/n: ran k of T\" line at the end, so it stopped partway" >&2
    exit 1
  fi
  if [[ ${BASH_REMATCH[3]} -eq 0 ]]; then
    echo "FAIL  the shard ran no scenario (E2E_SHARD=${E2E_SHARD:-none}, E2E_ONLY=${E2E_ONLY:-none})" >&2
    exit 1
  fi
  echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]} ${BASH_REMATCH[4]}"
  exit 0
fi
[[ -d $dir ]] || { sed -n '2,/^set /p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2; }
shopt -s nullglob
files=("$dir"/*.txt)
[[ ${#files[@]} -gt 0 ]] || { echo "FAIL  no shard left a record in $dir" >&2; exit 1; }
status=0
bad() { echo "FAIL  $*" >&2; status=1; }

case $kind in
integration)
  total=${3:?check-shards.sh integration <dir> <total>}
  sum=0 seen=" "
  while read -r tier shard ran; do
    [[ $tier == integration ]] || { bad "unexpected record: $tier $shard $ran"; continue; }
    [[ $seen == *" $shard "* ]] && bad "shard $shard reported twice"
    seen+="$shard "
    (( ran > 0 )) || bad "shard $shard ran no test"
    sum=$((sum + ran))
  done < <(cat "${files[@]}")
  (( sum == total )) || bad "the shards ran $sum tests between them, and the tier has $total"
  [[ $status -eq 0 ]] && echo "integration: ${#files[@]} shards ran $sum of $total tests"
  ;;
e2e)
  gates=${3:?check-shards.sh e2e <dir> <gates>}
  # gate -> "i:n:k:T ..." in file order.
  declare -A records
  for gate in $gates; do records[$gate]=; done
  while read -r gate i n k t; do
    records[$gate]+="$i:$n:$k:$t "
  done < <(cat "${files[@]}")
  for gate in "${!records[@]}"; do
    [[ -n ${records[$gate]} ]] || { bad "$gate: no shard ran this gate"; continue; }
    n= t= sum=0 seen=" "
    for record in ${records[$gate]}; do
      IFS=: read -r ri rn rk rt <<<"$record"
      : "${n:=$rn}" "${t:=$rt}"
      [[ $rn == "$n" ]] || bad "$gate: shards disagree on how many there are ($rn and $n)"
      [[ $rt == "$t" ]] || bad "$gate: shards disagree on how many scenarios there are ($rt and $t)"
      [[ $seen == *" $ri "* ]] && bad "$gate: shard $ri/$rn reported twice"
      seen+="$ri "
      (( rk > 0 )) || bad "$gate: shard $ri/$rn ran no scenario"
      sum=$((sum + rk))
    done
    for ((s = 0; s < n; s++)); do [[ $seen == *" $s "* ]] || bad "$gate: shard $s/$n left no record"; done
    (( sum == t )) || bad "$gate: the shards ran $sum scenarios between them, and the gate has $t"
    [[ $status -eq 0 ]] && echo "e2e $gate: $n shard(s) ran $sum of $t scenarios"
  done
  ;;
*)
  echo "check-shards.sh: unknown kind '$kind'" >&2
  exit 2
  ;;
esac
exit $status
