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
# JS-070: it also holds every JSON answer of both servers to compat/schema-12.4.json (and fails when that
# file is not what the running Jellyfin's OpenAPI document gives), sends every request of compat/clients.yaml
# to jellyscale, decodes answers with jellyfin-sdk-kotlin (test/sdkjvm, needs java) and asks @jellyfin/sdk's
# server discovery (test/sdknode, needs node and npm) which server it recommends.
# The output ends with "shard i/n: ran k of T", and <logs>/shard-differential-0of1.txt
# holds "differential 0 1 k T" for test/check-shards.sh, as for an e2e gate.
# DIFFERENTIAL_HOLD=600 keeps the servers up for ten minutes after the run, to poke at them.
# Logs (jellyscale.log, jellyfin.log) go to $JELLYSCALE_LOG_DIR, default <workdir>/logs.
set -uo pipefail
source "$(dirname "$0")/../lib.sh"
for tool in docker ffmpeg go curl python3 node npm java; do
  command -v "$tool" >/dev/null || { echo "test/differential/run.sh needs $tool on PATH" >&2; exit 1; }
done
[[ $# -eq 1 ]] || { echo "usage: test/differential/run.sh <workdir>" >&2; exit 2; }
make_work differential "$1" || exit 1
logs=${JELLYSCALE_LOG_DIR:-$work/logs}
mkdir -p "$logs"
logs=$(cd "$logs" && pwd)
image=jellyfin/jellyfin:12.4
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
# Three libraries: the films and shows of a small server, and the HDR films (Dolby Vision, HDR10+).
media=$work/diffmedia
rm -rf "$media" && mkdir -p "$media" || exit 1
cp -al "$work/media/movies" "$work/media/shows" "$work/media/hdr" "$media/" || exit 1
# Zeta Film: two audio tracks, for the check (JS-070) that the second stream of a PlaybackInfo has the fields
# a strict SDK needs. Its file is older than the others, so Latest lists it last on both servers.
zeta="$media/movies/Zeta Film (2023)/Zeta Film (2023).mkv"
mkdir -p "$(dirname "$zeta")" || exit 1
ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=20" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=20" \
  -f lavfi -i "sine=frequency=880:sample_rate=48000:duration=20" \
  -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -crf 30 -g 48 -c:a:0 ac3 -c:a:1 aac \
  -metadata:s:a:0 language=eng -metadata:s:a:1 language=spa "$zeta" || exit 1
touch -d 2000-01-01 "$zeta"
# Sub Font Cut: a text subtitle (stream 2) and an attached font (stream 3), which no other film of the libraries has,
# for the requests that ask with no token (JS-126, test/differential/run/clients.go names it as subtitleFilmKey). It has a
# library of its own, so that no listing of the Movies library or search for "film" holds a film whose HasSubtitles
# jellyscale does not send; it is old too, so Latest lists it after the others.
subfont="$media/subtitled/Sub Font Cut (1999)/Sub Font Cut (1999).mkv"
mkdir -p "$(dirname "$subfont")" || exit 1
printf '1\n00:00:01,000 --> 00:00:03,000\nEnglish line\n\n' >"$work/lines.srt"
ffmpeg -nostdin -v error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=24:duration=12" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=12" -i "$work/lines.srt" \
  -map 0:v -map 1:a -map 2 -c:v libx264 -preset ultrafast -g 24 -c:a aac -c:s srt \
  -metadata:s:a:0 language=eng -metadata:s:s:0 language=eng \
  -attach "$(fc-match -f '%{file}' serif)" -metadata:s:t:0 mimetype=font/ttf "$subfont" || exit 1
touch -d 1999-12-31 "$subfont"
# The language film of test/fixtures/babel.sh (JS-119): audio and subtitle tracks in several languages and two subtitle
# files beside it, in a library of its own, for the requests of an opted-in viewer. Its one recipe is babel.sh, so this
# script names the film only by the directory babel.sh makes. It is renamed so that the requests that search for "film"
# and list every film do not meet a film with subtitles, which jellyscale does not mark HasSubtitles (JS-068).
bash "$repo/test/fixtures/babel.sh" "$media" || exit 1
made=$(basename "$(find "$media/languages" -mindepth 1 -maxdepth 1 -type d | head -1)")
[ -n "$made" ] || { echo "FAIL  babel.sh made no film under languages" >&2; exit 1; }
mv "$media/languages/$made" "$media/languages/Polyglot (2030)" || exit 1
for file in "$media/languages/Polyglot (2030)/$made"*; do
  mv "$file" "${file/$made/Polyglot (2030)}" || exit 1
done
# @jellyfin/sdk, which asks the server discovery (test/sdknode/discover.mjs).
(cd "$repo/test/sdknode" && npm ci --ignore-scripts --no-audit --no-fund >/dev/null) || { echo "FAIL  npm ci in test/sdknode failed" >&2; exit 1; }

docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=d -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
started=1
docker run -d --rm --name "$jf" -v "$media:/media:ro" -p 127.0.0.1::8096 "$image" >/dev/null || exit 1
jf_port=$(docker port "$jf" 8096/tcp | head -1 | sed 's/.*://')
pg_port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2

cat >"$work/config.json" <<E2
{"name": "differential", "adminUser": "admin", "scanInterval": "1h",
 "transcodeStore": "file://$work/segments", "transcodeScratch": "$work/scratch", "transcodeCores": 4, "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$media/shows", "assets": "file://$work/assets/shows"},
  {"name": "HDR", "kind": "movies", "media": "file://$media/hdr", "assets": "file://$work/assets/hdr"},
  {"name": "Subtitled", "kind": "movies", "media": "file://$media/subtitled", "assets": "file://$work/assets/subtitled"},
  {"name": "Languages", "kind": "movies", "media": "file://$media/languages", "assets": "file://$work/assets/languages"}
]}
E2
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale && go build -o "$work/differential" ./test/differential/run) || exit 1
js_port=$(python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
JELLYSCALE_DATABASE_URL="postgres://postgres:d@127.0.0.1:$pg_port/postgres" JELLYSCALE_ADMIN_PASSWORD=js-pass \
  "$work/jellyscale" --roles=all --listen "127.0.0.1:$js_port" --config "$work/config.json" >"$logs/jellyscale.log" 2>&1 &
pids+=($!)

out=$work/out.txt
(cd "$repo" && "$work/differential" -jellyfin "http://127.0.0.1:$jf_port" -jellyscale "http://127.0.0.1:$js_port" \
  -jellyfin-password jf-pass -jellyscale-password js-pass -image "$image") 2>&1 | tee "$out"
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
