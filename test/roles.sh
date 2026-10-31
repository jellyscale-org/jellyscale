#!/usr/bin/env bash
# Starts each role as its own process, at the same moment, and checks they
# cooperate through Postgres alone. Usage: roles.sh <workdir_with_e2e_config>
set -uo pipefail
work=$(cd "$1" && pwd)
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
# The workers' ffmpeg reads at twice the film's speed, so it is still running
# when the checks below look for it. run.sh may have made the shim already.
mkdir -p "$work/bin"
printf '#!/bin/sh\nexec %s -readrate 2 "$@"\n' "$(command -v ffmpeg)" >"$work/bin/ffmpeg"
chmod +x "$work/bin/ffmpeg"
PATH="$work/bin:$PATH" start --roles=worker
PATH="$work/bin:$PATH" start --roles=worker
start --roles=api --listen "127.0.0.1:$api1"
start --roles=api --listen "127.0.0.1:$api2"
start --roles=stream --listen "127.0.0.1:$stream"

for _ in $(seq 1 60); do
  curl -fs -o /dev/null "127.0.0.1:$api1/healthz" && curl -fs -o /dev/null "127.0.0.1:$api2/healthz" &&
    curl -fs -o /dev/null "127.0.0.1:$stream/healthz" && [[ $(grep -c 'scan finished' "$work/roles.log") -ge 6 ]] && break
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
check "two workers scanned without duplicates" 13 "$total"
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

# Transcoding across processes: the stream role serves HLS, a worker process
# makes the segments, and no ffmpeg outlives the DELETE that ends the session.
profile=$(python3 -I -c 'import json,sys;print(json.dumps(json.load(open(sys.argv[1]))["deviceProfile"]))' "$repo/compat/captures/transcode-12.4.json")
zeta=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=Zeta" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
info=$(curl -s -X POST -H "$header" -H 'Content-Type: application/json' "127.0.0.1:$api1/Items/$zeta/PlaybackInfo" \
  -d "{\"DeviceProfile\":$profile,\"MaxStreamingBitrate\":2147483647,\"AudioStreamIndex\":\"1\"}")
url=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["MediaSources"][0]["TranscodingUrl"])')
session=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["PlaySessionId"])')
check "AC3 audio gets a TranscodingUrl" "AudioCodecNotSupported" "$(echo "$url" | sed -n 's/.*TranscodeReasons=\([A-Za-z,]*\).*/\1/p')"
check "api role doesn't serve the playlist" 404 "$(code "127.0.0.1:$api1$url")"
check "stream role serves the master playlist" 200 "$(code "127.0.0.1:$stream$url")"
main=$(curl -s "127.0.0.1:$stream$url" | sed -n 's/^\(main.m3u8.*\)$/\1/p')
base=${url%%/master.m3u8*}
playlist=$(curl -s "127.0.0.1:$stream$base/$main")
check "main playlist is complete" 45 "$(echo "$playlist" | grep -c '^#EXTINF')"
check "main playlist has no discontinuity" 0 "$(echo "$playlist" | grep -c DISCONTINUITY)"
segment=$(echo "$playlist" | grep -m1 '^hls1/main/0.mp4')
init=$(echo "$playlist" | sed -n 's/^#EXT-X-MAP:URI="\(.*\)"$/\1/p')
check "init segment comes from a worker's output" "ftyp" "$(curl -s "127.0.0.1:$stream$base/$init" | head -c 8 | tail -c 4)"
check "first segment comes from a worker's output" "styp" "$(curl -s "127.0.0.1:$stream$base/$segment" | head -c 8 | tail -c 4)"
workers=$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)
check "a worker is running ffmpeg for it" 1 "$workers"
check "DELETE ActiveEncodings answers 204" 204 "$(code -X DELETE -H "$header" "127.0.0.1:$stream/Videos/ActiveEncodings?deviceId=i&PlaySessionId=$session")"
for _ in $(seq 1 40); do [[ $(pgrep -f "ffmpeg.*$work/scratch" | wc -l) -eq 0 ]] && break; sleep 0.5; done
check "no ffmpeg outlives the session" 0 "$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)"
check "a stopped session is refused" 410 "$(code "127.0.0.1:$stream$base/$segment")"

# A worker killed outright takes its ffmpeg with it.
info=$(curl -s -X POST -H "$header" -H 'Content-Type: application/json' "127.0.0.1:$api1/Items/$zeta/PlaybackInfo" \
  -d "{\"DeviceProfile\":$profile,\"MaxStreamingBitrate\":2147483647}")
url=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["MediaSources"][0]["TranscodingUrl"])')
base=${url%%/master.m3u8*}
main=$(curl -s "127.0.0.1:$stream$url" | sed -n 's/^\(main.m3u8.*\)$/\1/p')
segment=$(curl -s "127.0.0.1:$stream$base/$main" | grep -m1 '^hls1/main/30.mp4') # Not made by the first session.
curl -s -o /dev/null "127.0.0.1:$stream$base/$segment"
check "ffmpeg runs for the second session" 1 "$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)"
kill -9 "${pids[0]}" "${pids[1]}"
for _ in $(seq 1 20); do [[ $(pgrep -f "ffmpeg.*$work/scratch" | wc -l) -eq 0 ]] && break; sleep 0.25; done
check "ffmpeg dies with a worker that was killed" 0 "$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)"

# Rolling restart: a worker stops gracefully and its replacement arrives a few
# seconds later. The request that comes in between waits for it, where it used
# to be answered 503 at once.
PATH="$work/bin:$PATH" start --roles=worker
old=${pids[-1]}
info=$(curl -s -X POST -H "$header" -H 'Content-Type: application/json' "127.0.0.1:$api1/Items/$zeta/PlaybackInfo" \
  -d "{\"DeviceProfile\":$profile,\"MaxStreamingBitrate\":1500000}")
url=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["MediaSources"][0]["TranscodingUrl"])')
base=${url%%/master.m3u8*}
main=$(curl -s "127.0.0.1:$stream$url" | sed -n 's/^\(main.m3u8.*\)$/\1/p')
playlist=$(curl -s "127.0.0.1:$stream$base/$main")
check "the old worker makes a segment" 200 "$(code "127.0.0.1:$stream$base/$(echo "$playlist" | grep -m1 '^hls1/main/0.mp4')")"
kill -TERM "$old"
for _ in $(seq 1 40); do kill -0 "$old" 2>/dev/null || break; sleep 0.25; done
(curl -s -o /dev/null -w '%{http_code}' "127.0.0.1:$stream$base/$(echo "$playlist" | grep -m1 '^hls1/main/6.mp4')" >"$work/replaced.code") &
requester=$!
sleep 3
PATH="$work/bin:$PATH" start --roles=worker
wait "$requester"
check "a request made while no worker runs waits for the replacement" 200 "$(cat "$work/replaced.code")"
exit $failed
