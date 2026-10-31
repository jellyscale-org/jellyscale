#!/usr/bin/env bash
# Runs the browse and play gates against disposable infrastructure, with each
# role as its own process behind a router, as in production: Postgres, S3
# (SeaweedFS), two api replicas, a stream replica and a worker.
# Usage: run.sh <workdir> <dir_with_playwright>
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

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=e2e -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
start_seaweed "$sw" "$work" e2e || exit 1
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2

s3="endpoint=http://127.0.0.1:$seaweed_port&region=us-east-1"
cat >"$work/config.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"}
]}
E2
cat >"$work/config-s3.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"},
  {"name": "Cloud", "kind": "movies", "media": "s3://e2e/media?$s3", "assets": "s3://e2e/assets?$s3"}
]}
E2

export JELLYSCALE_S3_ACCESS_KEY=$seaweed_key JELLYSCALE_S3_SECRET_KEY=$seaweed_secret
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale && go build -o "$work/putfile" ./test/e2e/putfile) || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Epsilon Film (2022)/Epsilon Film (2022).mkv" "$epsilon" || exit 1

free() { python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
export JELLYSCALE_DATABASE_URL="postgres://postgres:e2e@127.0.0.1:$port/postgres" JELLYSCALE_ADMIN_PASSWORD=e2e-pass
api1=$(free) api2=$(free) stream=$(free) router=$(free)
: >"$work/server.log"
start() { "$work/jellyscale" "$@" --config "$work/config-s3.json" >>"$work/server.log" 2>&1 & pids+=($!); }
start --roles=worker
start --roles=api --listen "127.0.0.1:$api1"
start --roles=api --listen "127.0.0.1:$api2"
start --roles=stream --listen "127.0.0.1:$stream"
node "$repo/test/e2e/router.mjs" "$router" "$stream" "$api1,$api2" >>"$work/server.log" 2>&1 &
pids+=($!)
for p in $api1 $api2 $stream; do
  for _ in $(seq 1 60); do curl -fs -o /dev/null "http://127.0.0.1:$p/healthz" && break; sleep 0.25; done
done
for _ in $(seq 1 120); do [[ $(grep -c 'scan finished' "$work/server.log") -ge 3 ]] && break; sleep 0.5; done

base="http://127.0.0.1:$router"
cp "$repo/test/e2e/browse.mjs" "$repo/test/e2e/play.mjs" "$playwright/"
result=0
for gate in browse play; do
  echo "=== $gate"
  (cd "$playwright" && timeout 240 node $gate.mjs "$base" admin e2e-pass) || result=1
done
echo "--- server warnings"
grep -E 'level=(WARN|ERROR)' "$work/server.log" | sed -E 's/^time=[^ ]+ //' | sort | uniq -c | sort -rn | head -20
if grep -q "$seaweed_secret" "$work/server.log"; then echo "FAIL  the S3 secret appears in the log"; result=1; fi
exit $result
