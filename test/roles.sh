#!/usr/bin/env bash
# Starts each role as its own process, at the same moment, and checks they
# cooperate through Postgres alone. Usage: roles.sh <workdir_with_e2e_config>
set -uo pipefail
work=$1
repo=$(cd "$(dirname "$0")/.." && pwd)
pg=jellyscale-roles-pg
pids=()
cleanup() { [[ ${#pids[@]} -gt 0 ]] && kill "${pids[@]}" 2>/dev/null; docker rm -f "$pg" >/dev/null 2>&1; }
trap cleanup EXIT
docker rm -f "$pg" >/dev/null 2>&1
docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=r -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale) || exit 1

export JELLYSCALE_DATABASE_URL="postgres://postgres:r@127.0.0.1:$port/postgres" JELLYSCALE_ADMIN_PASSWORD=r-pass
free() { python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
api1=$(free) api2=$(free) stream=$(free)
start() { "$work/jellyscale" "$@" --config "$work/config.json" 2>>"$work/roles.log" & pids+=($!); }
: >"$work/roles.log"
start --roles=worker
start --roles=worker
start --roles=api --listen "127.0.0.1:$api1"
start --roles=api --listen "127.0.0.1:$api2"
start --roles=stream --listen "127.0.0.1:$stream"

for _ in $(seq 1 60); do
  curl -fs -o /dev/null "127.0.0.1:$api1/healthz" && curl -fs -o /dev/null "127.0.0.1:$api2/healthz" &&
    curl -fs -o /dev/null "127.0.0.1:$stream/healthz" && [[ $(grep -c 'scan finished' "$work/roles.log") -ge 2 ]] && break
  sleep 0.5
done

failed=0
check() { # name want got
  if [[ $2 == "$3" ]]; then echo "ok    $1"; else echo "FAIL  $1: want $2, got $3"; failed=1; fi
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

for pid in "${pids[@]}"; do kill -0 "$pid" 2>/dev/null || { echo "FAIL  a process exited at startup"; failed=1; }; done
token=$(curl -s -X POST "127.0.0.1:$api1/Users/AuthenticateByName" -H 'Content-Type: application/json' \
  -H 'Authorization: MediaBrowser Client="c", Device="d", DeviceId="i", Version="1"' -d '{"Username":"admin","Pw":"r-pass"}' |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["AccessToken"])')
header="X-Emby-Token: $token"
check "token issued by one replica works on another" 200 "$(code -H "$header" "127.0.0.1:$api2/Users/Me")"
id=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&Limit=1" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
total=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["TotalRecordCount"])')
check "two workers scanned without duplicates" 3 "$total"
check "stream role serves images" 200 "$(code "127.0.0.1:$stream/Items/$id/Images/Primary")"
check "api role doesn't serve images" 404 "$(code "127.0.0.1:$api1/Items/$id/Images/Primary")"
check "stream role doesn't serve the API" 404 "$(code -H "$header" "127.0.0.1:$stream/Items")"
check "stream role direct-plays with a token in the query" 206 "$(code -r 0-99 "127.0.0.1:$stream/Videos/$id/stream?ApiKey=$token&Static=true")"
check "stream role refuses media without a token" 401 "$(code "127.0.0.1:$stream/Videos/$id/stream")"
check "api role doesn't serve media" 404 "$(code "127.0.0.1:$api1/Videos/$id/stream?ApiKey=$token")"
check "wrong password is refused" 401 "$(code -X POST "127.0.0.1:$api1/Users/AuthenticateByName" -H 'Content-Type: application/json' -d '{"Username":"admin","Pw":"no"}')"
check "invalid token is refused" 401 "$(code -H 'X-Emby-Token: nope' "127.0.0.1:$api1/Users/Me")"
check "audio routes aren't served" 404 "$(code -H "$header" "127.0.0.1:$api1/Audio/$id/stream")"
check "music lists are empty" '{"Items":[],"TotalRecordCount":0,"StartIndex":0}' "$(curl -s -H "$header" "127.0.0.1:$api1/Items?Recursive=true&IncludeItemTypes=Audio")"
exit $failed
