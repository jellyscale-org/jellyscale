#!/usr/bin/env bash
# Runs the differential tier twice and checks what only a real run shows:
#   1. test/differential/run.sh passes on this tree, ends with the line
#      "shard 0/1: ran k of T" (k = T > 0), leaves <logs>/shard-differential-0of1.txt for
#      test/check-shards.sh, and leaves none of its containers behind;
#   2. on a copy of the tree with each of three bugs put back in turn, it fails, reports a
#      difference on the request named below (not only on its route), and leaves no container:
#        (a) GET /UserItems/Resume ignores mediaTypes (PR #32): "continue watching, Book";
#        (b) GET /Items/Latest drops one film: "latest films in the Movies library";
#        (c) GET /Items sorted by SortName in the wrong order: "the films library, by name".
# Usage: acceptance.sh <workdir>. Needs Docker; a missing dependency fails.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${1:?usage: acceptance.sh <workdir>}
mkdir -p "$work" && work=$(cd "$work" && pwd)
fails=0
check() { if [[ $2 == "$3" ]]; then echo "ok    $1"; else echo "FAIL  $1: want '$2', got '$3'"; fails=$((fails + 1)); fi; }
# The tier starts jellyfin/jellyfin:12.4 and postgres; none may outlive it.
tier_containers() { docker ps -aq --filter ancestor=jellyfin/jellyfin:12.4 --filter ancestor=postgres:18-alpine | sort; }
leaked() { comm -13 <(echo "$1") <(tier_containers) | tr '\n' ' '; }

before=$(tier_containers)
JELLYSCALE_LOG_DIR=$work/logs-good bash "$repo/test/differential/run.sh" "$work/good" >"$work/good.out" 2>&1
check "the tier passes on the tree" 0 $?
check "a passing run ends with its shard line" "0 1" "$(bash "$repo/test/check-shards.sh" line "$work/good.out" | cut -d' ' -f1,2)"
check "a passing run leaves a shard record for check-shards.sh" 1 "$(grep -c '^differential 0 1 [1-9][0-9]* [1-9][0-9]*$' "$work/logs-good/shard-differential-0of1.txt" 2>/dev/null)"
check "no container is left after a passing run" "" "$(leaked "$before")"
rm -rf "$work/good"

# mutate <name> <file> <sed script> <grep -c pattern for the edit> <request that must differ>
mutate() {
  local name=$1 file=$2 script=$3 marker=$4 request=$5
  local mutant=$work/mutant-$name status
  rm -rf "$mutant" && mkdir -p "$mutant"
  (cd "$repo" && git ls-files -co --exclude-standard -z | xargs -0 cp --parents -t "$mutant") || exit 1
  [[ -e $repo/web-dist ]] && ln -s "$repo/web-dist" "$mutant/web-dist"
  sed -i "$script" "$mutant/$file"
  check "($name) the mutation is in place" 1 "$(grep -c -F -- "$marker" "$mutant/$file")"
  before=$(tier_containers)
  JELLYSCALE_LOG_DIR=$work/logs-$name bash "$mutant/test/differential/run.sh" "$work/bad-$name" >"$work/$name.out" 2>&1
  status=$?
  check "($name) the tier fails" 1 "$([[ $status -ne 0 ]] && echo 1 || echo 0)"
  check "($name) the report names the request '$request'" 1 "$(grep -c -x -F "      request: $request" "$work/$name.out")"
  check "($name) no container is left after a failing run" "" "$(leaked "$before")"
  rm -rf "$mutant" "$work/bad-$name"
}
mutate mediatypes internal/api/items.go '/^func mediaTypeFilter(/a\	return include, true // mutant' 'return include, true // mutant' "continue watching, Book"
mutate latest internal/api/items.go 's/writeJSON(w, result.Items)$/writeJSON(w, result.Items[:max(len(result.Items)-1, 0)])/' 'result.Items[:max(len(result.Items)-1, 0)]' "latest films in the Movies library"
mutate sortname internal/items/query.go 's/"sortname": *"i.sort_name"/"sortname": "i.name"/' '"sortname": "i.name"' "the films library, by name"
exit $((fails > 0))
