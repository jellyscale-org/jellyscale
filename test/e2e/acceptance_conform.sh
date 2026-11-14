#!/usr/bin/env bash
# Shows the e2e gates hold every JSON response of jellyscale to the schema (JS-070):
#   1. a run of all four gates on this tree passes, each gate prints "e2e <gate>: checked N JSON
#      responses on M routes" with N > 0, and leaves <logs>/<gate>/responses.jsonl;
#   (a line of responses.jsonl is {request, method, path, status, contentType, body}; path is the URL path with its ids)
#   2. on a copy of the tree with a #47 sign-in field put back missing (all four gates sign in), a #94
#      item field, and a #95 stream field of a transcode PlaybackInfo, every gate whose responses.jsonl
#      from run 1 holds that route fails, naming the route, the JSON path and the field; and a gate that
#      did not receive the route still passes.
# Usage: acceptance_conform.sh <workdir> <playwright_dir>. Needs Docker and web-dist; a missing one fails.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${1:?usage: acceptance_conform.sh <workdir> <playwright_dir>}
playwright=${2:?usage: acceptance_conform.sh <workdir> <playwright_dir>}
mkdir -p "$work" && work=$(cd "$work" && pwd)
gates="browse play subtitles failover transcode"
fails=0
check() { if [[ $2 == "$3" ]]; then echo "ok    $1"; else echo "FAIL  $1: want '$2', got '$3'"; fails=$((fails + 1)); fi; }

JELLYSCALE_LOG_DIR=$work/logs-good bash "$repo/test/e2e/run.sh" "$work/good" "$playwright" >"$work/good.out" 2>&1
check "the gates pass on the tree" 0 $?
for gate in $gates; do
  check "$gate says how many responses it checked" 1 "$(grep -c "^e2e $gate: checked [1-9][0-9]* JSON responses on [1-9][0-9]* routes$" "$work/good.out")"
  check "$gate leaves its responses" 1 "$([[ -s $work/logs-good/$gate/responses.jsonl ]] && echo 1 || echo 0)"
done

# mutate <name> <file or directory/> <sed script> <marker> <route> <json path> <field> [<regex a baseline response of the route must also match>]
# A gate "receives" a response when its baseline responses.jsonl holds a line for the route that
# matches the regex too (the #95 mutant only bites a reply that lists a second audio stream, which
# only a transcode offer of a film with two audio tracks does).
mutate() {
  local name=$1 file=$2 script=$3 marker=$4 route=$5 jpath=$6 field=$7 also=${8:-.} gate want got matches
  local mutant=$work/mutant-$name # After the local above: its words expand before name is set.
  rm -rf "$mutant" && mkdir -p "$mutant"
  tar -C "$repo" --exclude=./.git --exclude=./.results --exclude=./web-dist -cf - . | tar -C "$mutant" -xf - || exit 1
  ln -s "$repo/web-dist" "$mutant/web-dist"
  # A file, or a directory (a name ending in /) whose non-test Go files are all the typed model's.
  targets=("$mutant/$file")
  [[ $file == */ ]] && targets=($(ls "$mutant/$file"*.go | grep -v '_test\.go$'))
  sed -i "$script" "${targets[@]}"
  check "($name) the mutation is in place" 1 "$([[ $(cat "${targets[@]}" | grep -c -F -- "$marker") -gt 0 ]] && echo 1 || echo 0)"
  want="" got=""
  rx=$(sed -E 's/\{[^}]*\}/[0-9a-fA-F-]{32,36}/g; s/^[A-Z]+ //' <<<"$route")
  for gate in $gates; do
    # The baseline decides which gates receive the route; the mutant must fail those, and only those.
    # Case-blind (jellyfin-web sends /Users/authenticatebyname), the path ended by its query or its
    # quote (so /Items/{id}/Similar is not /Items/{id}), and read whole before the second grep: under
    # pipefail, grep -q closing the pipe early made a match read as none (status 141).
    matches=$(grep -i -E "\"method\":\"${route%% *}\",\"path\":\"$rx(\\?|\")" "$work/logs-good/$gate/responses.jsonl" || true)
    grep -q -E -- "$also" <<<"$matches" && want+="$gate "
    E2E_GATES=$gate JELLYSCALE_LOG_DIR=$work/logs-$name bash "$mutant/test/e2e/run.sh" "$work/bad-$name" "$playwright" >"$work/$name-$gate.out" 2>&1 \
      || { got+="$gate "; check "($name) $gate names '$route', '$jpath' and '$field'" 1 \
        "$(grep -F -- "$route" "$work/$name-$gate.out" | grep -F -- "$jpath" | grep -c -F -- "$field" | sed 's/^[1-9].*/1/')"; }
  done
  # A mutant may also break a route a gate receives for another reason, so a gate outside $want may
  # fail; every gate in $want must.
  check "($name) every gate that receives the response fails" "$want" "$(for gate in $want; do [[ " $got " == *" $gate "* ]] && printf '%s ' "$gate"; done)"
  check "($name) at least one gate receives the response" 1 "$([[ -n $want ]] && echo 1 || echo 0)"
  rm -rf "$mutant" "$work/bad-$name"
}
mutate lastcheckin internal/api/users.go 's/"LastPlaybackCheckIn": "0001/"LastPlaybackCheckInX": "0001/' '"LastPlaybackCheckInX"' 'POST /Users/AuthenticateByName' '$.SessionInfo' LastPlaybackCheckIn
# #94: a required field of the typed MediaSource (internal/media, JS-086) that the item's details lose: the
# model keeps each under a tag of its own and the mutant renames one.
mutate itemfields internal/media/ 's/json:"HasSegments"/json:"HasSegmentsX"/' 'json:"HasSegmentsX"' 'GET /Items/{itemId}' 'MediaSources[0]' HasSegments
# #95: a required field of the typed MediaStream, lost on the audio streams a transcode offer adds from the full
# probe and on every other stream. Which index they have depends on the film, so the report is asked for
# 'MediaStreams[' and the field only; only a reply that lists a second audio stream receives the route.
mutate secondstream internal/media/ 's/json:"IsInterlaced"/json:"IsInterlacedX"/' 'json:"IsInterlacedX"' 'POST /Items/{itemId}/PlaybackInfo' 'MediaStreams[' IsInterlaced 'Type[^A-Za-z]+Audio.*Type[^A-Za-z]+Audio'
exit $((fails > 0))
