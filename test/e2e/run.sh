#!/usr/bin/env bash
# Runs the browse and play gates against disposable infrastructure, with each
# role as its own process behind a router, as in production: Postgres, S3
# (SeaweedFS), two api replicas, a stream replica and a worker.
# Usage: run.sh <workdir> <dir_with_playwright>
# E2E_GATES picks which gates run, default "browse play subtitles transcode".
# Expects jellyfin-web in ./web-dist: docker cp <jellyfin>:/jellyfin/jellyfin-web web-dist
# Writes <workdir>/config.json (POSIX libraries only, for test/roles.sh) and
# <workdir>/config-s3.json (adds a library on S3 for media and assets).
set -uo pipefail
work=$1
playwright=$2
repo=$(cd "$(dirname "$0")/../.." && pwd)
pg=jellyscale-e2e-pg-$$ sw=jellyscale-e2e-sw-$$
pids=()
mkdir -p "$work"
work=$(cd "$work" && pwd)

cleanup() {
  [[ ${#pids[@]} -gt 0 ]] && kill "${pids[@]}" 2>/dev/null
  docker rm -f "$pg" "$sw" >/dev/null 2>&1
}
trap cleanup EXIT
cleanup
source "$repo/test/s3-lib.sh"

bash "$repo/test/fixtures/make.sh" "$work/media"
rm -rf "$work/assets"
epsilon="$work/Epsilon Film (2022).mkv"
[[ -f $epsilon ]] || ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=24000/1001:duration=30" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=30" \
  -c:v libx264 -preset ultrafast -c:a aac -metadata:s:a:0 language=eng "$epsilon" || exit 1

# Films the captured Chrome profile can't direct-play: AC3 audio forces a
# transcode, and a second track gives the audio menu something to switch to.
# Each scenario gets its own item, with its own segments, and a hard link is
# enough because no scenario rewrites these files, which keeps the media small.
zeta="$work/media/transcode/Zeta Film (2023)/Zeta Film (2023).mkv"
if [[ ! -f $zeta ]]; then
  mkdir -p "$(dirname "$zeta")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=180" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=180" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=180" \
    -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -crf 26 -g 48 -c:a:0 ac3 -c:a:1 aac \
    -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$zeta" || exit 1
fi
for copy in "Theta Film (2025)" "Iota Film (2026)" "Kappa Film (2027)" "Lambda Film (2028)" "Mu Film (2029)" "Omicron Film (2032)" "Pi Film (2033)"; do
  [[ -f "$work/media/transcode/$copy/$copy.mkv" ]] || { mkdir -p "$work/media/transcode/$copy" && ln "$zeta" "$work/media/transcode/$copy/$copy.mkv"; }
done
# The failure scenario cuts this copy short, so every run makes it again.
mkdir -p "$work/media/transcode/Xi Film (2031)" && cp "$zeta" "$work/media/transcode/Xi Film (2031)/Xi Film (2031).mkv"

# Plays as it is, with a second audio track: switching to it asks for a transcode.
nu="$work/media/transcode/Nu Film (2030)/Nu Film (2030).mkv"
if [[ ! -f $nu ]]; then
  mkdir -p "$(dirname "$nu")"
  ffmpeg -nostdin -v error -y \
    -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=60" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=60" \
    -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=60" \
    -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -crf 26 -g 48 -c:a aac \
    -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$nu" || exit 1
fi

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=e2e -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
start_seaweed "$sw" "$work" e2e || exit 1
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2

s3="endpoint=http://127.0.0.1:$seaweed_port&region=us-east-1"
rm -rf "$work/scratch" "$work/segments"
# Segments go to a directory in config.json and to S3 in config-s3.json, so the
# transcode store is exercised on both. Idle is short so closing a tab shows.
cat >"$work/config.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h",
 "transcodeStore": "file://$work/segments", "transcodeScratch": "$work/scratch", "transcodeIdle": "10s", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"},
  {"name": "Transcode", "kind": "movies", "media": "file://$work/media/transcode", "assets": "file://$work/assets/transcode"}
]}
E2
cat >"$work/config-s3.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h",
 "transcodeStore": "s3://e2e/segments?$s3", "transcodeScratch": "$work/scratch", "transcodeIdle": "10s", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"},
  {"name": "Transcode", "kind": "movies", "media": "file://$work/media/transcode", "assets": "file://$work/assets/transcode"},
  {"name": "Cloud", "kind": "movies", "media": "s3://e2e/media?$s3", "assets": "s3://e2e/assets?$s3"}
]}
E2

export JELLYSCALE_S3_ACCESS_KEY=$seaweed_key JELLYSCALE_S3_SECRET_KEY=$seaweed_secret
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale && go build -o "$work/putfile" ./test/e2e/putfile) || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Epsilon Film (2022)/Epsilon Film (2022).mkv" "$epsilon" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Eta Film (2024)/Eta Film (2024).mkv" "$zeta" || exit 1
# The subtitle films again, on S3: one that direct plays, one that transcodes,
# and the black one that burn-in shows against.
sub="$work/media/transcode"
"$work/putfile" "s3://e2e/media?$s3" "Chi Film (2038)/Chi Film (2038).mkv" "$sub/Sigma Film (2034)/Sigma Film (2034).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Psi Film (2039)/Psi Film (2039).mkv" "$sub/Tau Film (2035)/Tau Film (2035).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Omega Film (2040)/Omega Film (2040).mkv" "$sub/Phi Film (2037)/Phi Film (2037).mkv" || exit 1

free() { python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
export JELLYSCALE_DATABASE_URL="postgres://postgres:e2e@127.0.0.1:$port/postgres" JELLYSCALE_ADMIN_PASSWORD=e2e-pass
api1=$(free) api2=$(free) stream=$(free) router=$(free)
: >"$work/server.log"
start() { "$work/jellyscale" "$@" --config "$work/config-s3.json" >>"$work/server.log" 2>&1 & pids+=($!); }
# The worker's ffmpeg reads its input at twice the film's speed, so a transcode
# takes a while on any machine and the stop tests find it still running.
# E2E_NO_SHIM=1 leaves ffmpeg at full speed, to measure latency.
mkdir -p "$work/bin"
speed="-readrate 2"
[[ -n ${E2E_NO_SHIM:-} ]] && speed="-hide_banner"
# Only the transcode (the one that writes HLS) is slowed; extracting a subtitle
# track reads the whole file and would take as long as the film.
printf '#!/bin/sh\ncase "$*" in *"-f hls"*) exec %s %s "$@";; esac\nexec %s "$@"\n' "$(command -v ffmpeg)" "$speed" "$(command -v ffmpeg)" >"$work/bin/ffmpeg"
chmod +x "$work/bin/ffmpeg"
PATH="$work/bin:$PATH" "$work/jellyscale" --roles=worker --config "$work/config-s3.json" >>"$work/server.log" 2>&1 &
pids+=($!)
export E2E_WORKER_PID=$!
start --roles=api --listen "127.0.0.1:$api1"
start --roles=api --listen "127.0.0.1:$api2"
start --roles=stream --listen "127.0.0.1:$stream"
node "$repo/test/e2e/router.mjs" "$router" "$stream" "$api1,$api2" >>"$work/server.log" 2>&1 &
pids+=($!)
# A second stream replica set up wrongly, with no transcodeStore, behind its own
# router. The scenario that needs it starts it with E2E_NOSTORE and stops it
# again, because every role is compared and the others would be refused meanwhile.
sed 's/"transcodeStore": "[^"]*", //' "$work/config-s3.json" >"$work/config-nostore.json"
nostream=$(free) norouter=$(free)
printf '#!/bin/sh\nexec "%s" --roles=stream --listen "127.0.0.1:%s" --config "%s" >>"%s" 2>&1\n' \
  "$work/jellyscale" "$nostream" "$work/config-nostore.json" "$work/server.log" >"$work/bin/nostore"
chmod +x "$work/bin/nostore"
export E2E_NOSTORE="$work/bin/nostore"
node "$repo/test/e2e/router.mjs" "$norouter" "$nostream" "$api1,$api2" >>"$work/server.log" 2>&1 &
pids+=($!)
export E2E_BASE_NOSTORE="http://127.0.0.1:$norouter"
for p in $api1 $api2 $stream; do
  for _ in $(seq 1 60); do curl -fs -o /dev/null "http://127.0.0.1:$p/healthz" && break; sleep 0.25; done
done
for _ in $(seq 1 120); do [[ $(grep -c 'scan finished' "$work/server.log") -ge 4 ]] && break; sleep 0.5; done

base="http://127.0.0.1:$router"
cp "$repo/test/e2e/browse.mjs" "$repo/test/e2e/play.mjs" "$repo/test/e2e/transcode.mjs" "$repo/test/e2e/subtitles.mjs" "$playwright/"
result=0
export E2E_SCRATCH="$work/scratch" E2E_MEDIA="$work/media/transcode" E2E_PG="$pg" E2E_LOG="$work/server.log"
# The transcode gate ends by killing the worker, so subtitles run before it.
for gate in ${E2E_GATES:-browse play subtitles transcode}; do
  echo "=== $gate"
  (cd "$playwright" && timeout 900 node $gate.mjs "$base" admin e2e-pass) || result=1
done
# For poking at the stack by hand: E2E_HOLD=600 keeps it up for ten minutes.
[[ -n ${E2E_HOLD:-} ]] && { echo "holding $base"; sleep "$E2E_HOLD"; }
echo "--- server warnings"
grep -E 'level=(WARN|ERROR)' "$work/server.log" | sed -E 's/^time=[^ ]+ //' | sort | uniq -c | sort -rn | head -20
if grep -q "$seaweed_secret" "$work/server.log"; then echo "FAIL  the S3 secret appears in the log"; result=1; fi
exit $result
