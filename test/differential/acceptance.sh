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
#   3. JS-070: on a copy with each of these put back, it fails and the report names the route, the JSON
#      path and the field (conform_mutate below): each of #47's four sign-in fields, #94's source and
#      stream fields on GET /Items/{itemId}, #95's fields on the second stream of a transcode PlaybackInfo,
#      with the #47 field also listed in compat/differences.yaml as a gap (an entry excuses no violation);
#      and ProductName "jellyscale" (#44), which the @jellyfin/sdk server discovery refuses.
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
check "a passing run says how many JSON responses it checked, and on how many routes" 1 "$(grep -c '^differential: checked [1-9][0-9]* JSON responses on [1-9][0-9]* routes$' "$work/good.out")"
check "a passing run says how many client requests it sent" 1 "$(grep -c '^clients: sent [1-9][0-9]* requests$' "$work/good.out")"
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

# conform_mutate <name> <file> <sed script> <marker> <text the report must hold>... [-- <file to append to compat/differences.yaml>]
# The tier must fail and its report must hold each text (route, JSON path, field).
conform_mutate() {
  local name=$1 file=$2 script=$3 marker=$4; shift 4
  local mutant=$work/mutant-$name text allow=""
  rm -rf "$mutant" && mkdir -p "$mutant"
  tar -C "$repo" --exclude=./.git --exclude=./.results --exclude=./web-dist -cf - . | tar -C "$mutant" -xf - || exit 1
  [[ -e $repo/web-dist ]] && ln -s "$repo/web-dist" "$mutant/web-dist"
  sed -i "$script" "$mutant/$file"
  check "($name) the mutation is in place" 1 "$(grep -c -F -- "$marker" "$mutant/$file")"
  if [[ ${ALLOW_GAP:-} ]]; then printf '%s\n' "$ALLOW_GAP" >>"$mutant/compat/differences.yaml"; fi
  run_tier "$work/logs-$name" "$mutant/test/differential/run.sh" "$work/bad-$name" "$work/$name.out"
  check "($name) the tier fails" 1 "$([[ $status -ne 0 ]] && echo 1 || echo 0)"
  for text in "$@"; do
    check "($name) the report holds '$text'" 1 "$([[ $(grep -c -F -- "$text" "$work/$name.out") -gt 0 ]] && echo 1 || echo 0)"
  done
  check "($name) no container is left after a failing run" "" "$(leaked)"
  rm -rf "$mutant" "$work/bad-$name"
}
signin='POST /Users/AuthenticateByName'
conform_mutate lastcheckin internal/api/users.go 's/"LastPlaybackCheckIn": "0001/"LastPlaybackCheckInX": "0001/' '"LastPlaybackCheckInX"' "$signin" 'SessionInfo' 'LastPlaybackCheckIn'
conform_mutate playbackorder internal/api/users.go 's/"PlaybackOrder": "Default"},/"PlaybackOrderX": "Default"},/' '"PlaybackOrderX"' "$signin" 'SessionInfo.PlayState' 'PlaybackOrder'
conform_mutate authprovider internal/api/users.go 's/"AuthenticationProviderId":/"AuthenticationProviderIdX":/' '"AuthenticationProviderIdX"' "$signin" 'User.Policy' 'AuthenticationProviderId'
conform_mutate resetprovider internal/api/users.go 's/"PasswordResetProviderId": /"PasswordResetProviderIdX": /' '"PasswordResetProviderIdX"' "$signin" 'User.Policy' 'PasswordResetProviderId'
# The scan writes SupportsDirectPlay itself; the mutant drops the eight source fields it does not write.
conform_mutate itemfields internal/items/sdk.go 's/^\tfill(source, sourceDefaults)$/\t_ = sourceDefaults \/\/ mutant/' '_ = sourceDefaults // mutant' 'GET /Items/{itemId}' 'MediaSources[0]' 'HasSegments'
# #95: the audio streams planPlayback adds from the full probe get their fields from the loop at the end of the
# PlaybackInfo handler (internal/api/stream.go); only a film with a second audio track shows its loss. The index of
# the stream depends on the film, so the report is asked for 'MediaStreams[' and the field.
conform_mutate secondstream internal/api/stream.go 's/^\t\titems.WithSDKFields(source)$/\t\t_ = source \/\/ mutant/' '_ = source // mutant' 'POST /Items/{itemId}/PlaybackInfo' 'MediaStreams[' 'IsInterlaced'
# #91: a route a client calls on its home path and jellyscale does not serve. The sweep of compat/clients.yaml
# against the running jellyscale names the client, the path and the route.
conform_mutate suggestions internal/api/routes.go 's|^.*Path: "/Items/Suggestions".*$|\t\t// mutant: no suggestions route|' '// mutant: no suggestions route' 'Findroid' 'home' '/Items/Suggestions'
# An entry of compat/differences.yaml for the same field excuses nothing.
ALLOW_GAP='
- requests:
    - "the signed-in user"
  kind: gap
  task: JS-033
  reason: "mutant: tries to excuse a schema violation"
  paths:
    - "SessionInfo.LastPlaybackCheckIn"' conform_mutate excused internal/api/users.go 's/"LastPlaybackCheckIn": "0001/"LastPlaybackCheckInX": "0001/' '"LastPlaybackCheckInX"' "$signin" 'LastPlaybackCheckIn'
# #44: a wrong value no schema catches; @jellyfin/sdk's discovery returns no server.
conform_mutate productname internal/api/system.go 's/"ProductName": *"Jellyfin Server"/"ProductName": "jellyscale"/' '"ProductName": "jellyscale"' '@jellyfin/sdk' 'ProductName'
exit $((fails > 0))
