#!/usr/bin/env bash
# The differential tier: asks a real Jellyfin 12.4 and jellyscale the same
# questions over the same synthetic films and lists where the answers differ.
# Usage: test/differential/run.sh <workdir>
# Starts jellyfin/jellyfin:12.4 and Postgres in Docker on Docker-assigned ports,
# and jellyscale --roles=all as a process, over hard links to the e2e films
# (test/fixtures, cached as test/e2e/run.sh has them). Never a real library.
# test/differential/run/main.go sets Jellyfin up, sends each request of
# test/differential/requests.yaml to both and compares them; a difference that
# compat/differences.yaml does not list fails the run. See docs/testing.md.
# The output ends with "shard i/n: ran k of T", and <logs>/shard-differential-0of1.txt
# holds "differential 0 1 k T" for test/check-shards.sh, as for an e2e gate.
# DIFFERENTIAL_HOLD=600 keeps the servers up for ten minutes after the run, to poke at them.
# Logs (jellyscale.log, jellyfin.log) go to $JELLYSCALE_LOG_DIR, default <workdir>/logs.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
for tool in docker ffmpeg go curl python3; do
  command -v "$tool" >/dev/null || { echo "test/differential/run.sh needs $tool on PATH" >&2; exit 1; }
done
[[ $# -eq 1 ]] || { echo "usage: test/differential/run.sh <workdir>" >&2; exit 2; }
make_work differential "$1" || exit 1
logs=${JELLYSCALE_LOG_DIR:-$work/logs}
mkdir -p "$logs"
logs=$(cd "$logs" && pwd)
pg=jellyscale-differential-pg-$$ jf=jellyscale-differential-jf-$$
pids=()
cleanup() {
  local status=$?
  [[ ${#pids[@]} -gt 0 ]] && kill "${pids[@]}" 2>/dev/null
  [[ -n ${started:-} ]] && docker logs "$jf" >"$logs/jellyfin.log" 2>&1
  docker rm -fv "$pg" "$jf" >/dev/null 2>&1
  result_finish $status
  rm -rf "$tmp_base" "$work/t"
}
trap cleanup EXIT
result_start
result_name=differential
result_log=$logs

bash "$repo/test/fixtures/cached.sh" "$work" || exit 1
# Two libraries, as on a small server.
media=$work/diffmedia
rm -rf "$media" && mkdir -p "$media" || exit 1
cp -al "$work/media/movies" "$work/media/shows" "$media/" || exit 1

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=d -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
started=1
docker run -d --rm --name "$jf" -v "$media:/media:ro" -p 127.0.0.1::8096 jellyfin/jellyfin:12.4 >/dev/null || exit 1
jf_port=$(docker port "$jf" 8096/tcp | head -1 | sed 's/.*://')
pg_port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2

cat >"$work/config.json" <<E2
{"name": "differential", "adminUser": "admin", "scanInterval": "1h",
 "transcodeStore": "file://$work/segments", "transcodeScratch": "$work/scratch", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$media/shows", "assets": "file://$work/assets/shows"}
]}
E2
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale && go build -o "$work/differential" ./test/differential/run) || exit 1
js_port=$(python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
JELLYSCALE_DATABASE_URL="postgres://postgres:d@127.0.0.1:$pg_port/postgres" JELLYSCALE_ADMIN_PASSWORD=js-pass \
  "$work/jellyscale" --roles=all --listen "127.0.0.1:$js_port" --config "$work/config.json" >"$logs/jellyscale.log" 2>&1 &
pids+=($!)

out=$work/out.txt
(cd "$repo" && "$work/differential" -jellyfin "http://127.0.0.1:$jf_port" -jellyscale "http://127.0.0.1:$js_port" \
  -jellyfin-password jf-pass -jellyscale-password js-pass) 2>&1 | tee "$out"
status=${PIPESTATUS[0]}
# For poking at the two servers by hand: DIFFERENTIAL_HOLD=600 keeps them up for ten minutes.
[[ -n ${DIFFERENTIAL_HOLD:-} ]] && { echo "holding jellyfin on $jf_port, jellyscale on $js_port"; sleep "$DIFFERENTIAL_HOLD"; }
if record=$(bash "$repo/test/check-shards.sh" line "$out"); then
  read -r i n k total <<<"$record"
  echo "differential $i $n $k $total" >"$logs/shard-differential-${i}of$n.txt"
  result_summary="$k of $total requests compared"
else
  status=1
fi
exit $status
