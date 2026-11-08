#!/usr/bin/env bash
# Runs the differential tier twice and checks what only a real run shows:
#   1. test/differential/run.sh passes on this tree, ends with the line
#      "shard 0/1: ran k of T" (k = T > 0), leaves <logs>/shard-differential-0of1.txt for
#      test/check-shards.sh, and leaves none of its containers behind;
#   2. on a copy of the tree with each of four bugs put back in turn, it fails, reports a
#      difference on the request named below (not only on its route), and leaves no container:
#        (a) GET /UserItems/Resume ignores mediaTypes (PR #32): "continue watching, Book";
#        (b) GET /Items/Latest drops one film: "latest films in the Movies library";
#        (c) GET /Items sorted by SortName in the wrong order: "the films library, by name";
#        (d) a film carries a PremiereDate Jellyfin does not (timestamps are compared): "a film's details".
# Usage: acceptance.sh <workdir>. Needs Docker; a missing dependency fails.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${1:?usage: acceptance.sh <workdir>}
mkdir -p "$work" && work=$(cd "$work" && pwd)
fails=0
check() { if [[ $2 == "$3" ]]; then echo "ok    $1"; else echo "FAIL  $1: want '$2', got '$3'"; fails=$((fails + 1)); fi; }
# run.sh names its containers jellyscale-differential-{pg,jf}-<its pid>; a run.sh of
# another worktree or reviewer on this host has other pids and is not ours to count.
# run_tier <log dir> <run.sh> <workdir> <out> runs one and sets $pid and $status.
run_tier() {
  JELLYSCALE_LOG_DIR=$1 bash "$2" "$3" >"$4" 2>&1 &
  pid=$!
  wait "$pid"
  status=$?
}
leaked() { docker ps -a --format '{{.Names}}' | grep -E -- "^jellyscale-differential-(pg|jf)-$pid$" | tr '\n' ' '; }

run_tier "$work/logs-good" "$repo/test/differential/run.sh" "$work/good" "$work/good.out"
check "the tier passes on the tree" 0 $status
check "a passing run ends with its shard line" "0 1" "$(bash "$repo/test/check-shards.sh" line "$work/good.out" | cut -d' ' -f1,2)"
check "a passing run leaves a shard record for check-shards.sh" 1 "$(grep -c '^differential 0 1 [1-9][0-9]* [1-9][0-9]*$' "$work/logs-good/shard-differential-0of1.txt" 2>/dev/null)"
check "no container is left after a passing run" "" "$(leaked)"
rm -rf "$work/good"

# mutate <name> <file> <sed script> <grep -c pattern for the edit> <request that must differ>
mutate() {
  local name=$1 file=$2 script=$3 marker=$4 request=$5
  local mutant=$work/mutant-$name
  rm -rf "$mutant" && mkdir -p "$mutant"
  # A copy of the tree, which may be a git archive: no .git, and no big or run-local directories.
  tar -C "$repo" --exclude=./.git --exclude=./.results --exclude=./web-dist -cf - . | tar -C "$mutant" -xf - || exit 1
  [[ -e $repo/web-dist ]] && ln -s "$repo/web-dist" "$mutant/web-dist"
  sed -i "$script" "$mutant/$file"
  check "($name) the mutation is in place" 1 "$(grep -c -F -- "$marker" "$mutant/$file")"
  run_tier "$work/logs-$name" "$mutant/test/differential/run.sh" "$work/bad-$name" "$work/$name.out"
  check "($name) the tier fails" 1 "$([[ $status -ne 0 ]] && echo 1 || echo 0)"
  check "($name) the report names the request '$request'" 1 "$(grep -c -x -F "      request: $request" "$work/$name.out")"
  check "($name) no container is left after a failing run" "" "$(leaked)"
  rm -rf "$mutant" "$work/bad-$name"
}
mutate mediatypes internal/api/items.go '/^func mediaTypeFilter(/a\	return include, true // mutant' 'return include, true // mutant' "continue watching, Book"
mutate latest internal/api/items.go 's/writeJSON(w, result.Items)$/writeJSON(w, result.Items[:max(len(result.Items)-1, 0)])/' 'result.Items[:max(len(result.Items)-1, 0)]' "latest films in the Movies library"
mutate sortname internal/items/query.go 's/"sortname": *"i.sort_name"/"sortname": "i.name"/' '"sortname": "i.name"' "the films library, by name"
# Neither server returns a PremiereDate for the synthetic films, so the mutant adds a wrong one;
# a timestamp mask over every string would hide it as it hid nothing else.
mutate premieredate internal/library/scan.go '/dto\["IsHD"\] = probe.Height >= 720/a\	dto["PremiereDate"] = "2001-01-01T00:00:00.0000000Z" // mutant' 'dto["PremiereDate"] = "2001-01-01T00:00:00.0000000Z" // mutant' "a film's details"
exit $((fails > 0))
