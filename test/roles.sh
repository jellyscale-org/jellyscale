#!/usr/bin/env bash
# Starts each role as its own process, at the same moment, and checks they
# cooperate through Postgres alone. Usage: roles.sh <workdir>
# It makes its own films and config in <workdir> (test/fixtures/posix.sh), so
# it needs ffmpeg, Docker, Go, curl and python3, and nothing from test/e2e/run.sh.
# Logs go to $JELLYSCALE_LOG_DIR, default <workdir>/logs: roles.log with every
# role's output, whose errors carry the end of ffmpeg's stderr, and ffmpeg.log
# with each command line a worker ran.
# In CI ($GITHUB_ACTIONS set) failed checks go to the job summary and become
# annotations (test/report.sh); locally the output stays as it is.
# Temp files go to a short dir under /tmp/jellyscale-test (TMPDIR) and are removed at exit (test/lib.sh), and a run
# that passes records its result under .results.
set -uo pipefail
source "$(dirname "$0")/lib.sh"
for tool in docker ffmpeg go curl python3; do
  command -v "$tool" >/dev/null || { echo "test/roles.sh needs $tool on PATH" >&2; exit 1; }
done
make_work roles "$1" || exit 1
logs=${JELLYSCALE_LOG_DIR:-$work/logs}
mkdir -p "$logs"
logs=$(cd "$logs" && pwd)
pg=jellyscale-roles-pg-$$
pids=()
cleanup() {
  local status=$?
  [[ ${#pids[@]} -gt 0 ]] && kill "${pids[@]}" 2>/dev/null
  docker rm -fv "$pg" >/dev/null 2>&1
  result_finish $status
  rm -rf "$tmp_base" "$work/t"
}
trap cleanup EXIT
result_start
result_name=roles
result_log=$logs
bash "$repo/test/fixtures/posix.sh" "$work" || exit 1
docker run -d --rm --name "$pg" -e POSTGRES_PASSWORD=r -p 127.0.0.1::5432 postgres:18-alpine >/dev/null || exit 1
port=$(docker port "$pg" 5432/tcp | head -1 | sed 's/.*://')
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale) || exit 1
# api and stream run with no ffmpeg or ffprobe on their PATH (an empty
# directory), because only workers run media tools. Anything they still ran
# would fail with "executable file not found".
mkdir -p "$work/nobin"

export JELLYSCALE_DATABASE_URL="postgres://postgres:r@127.0.0.1:$port/postgres" JELLYSCALE_ADMIN_PASSWORD=r-pass
free() { python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
api1=$(free) api2=$(free) stream=$(free)
# CONFIG picks another config file, as the processes with a metrics port have.
start() { "$work/jellyscale" "$@" --config "${CONFIG:-$work/config.json}" 2>>"$logs/roles.log" & pids+=($!); }
# Three of the processes serve /metrics, each at an address of its own.
worker_metrics=$(free) worker2_metrics=$(free) stream_metrics=$(free)
with_metrics() { python3 -I -c 'import json,sys;c=json.load(open(sys.argv[1]));c["metricsListen"]="127.0.0.1:"+sys.argv[2];json.dump(c,open(sys.argv[3],"w"))' "$work/config.json" "$1" "$work/config-metrics-$1.json"; }
with_metrics "$worker_metrics"; with_metrics "$worker2_metrics"; with_metrics "$stream_metrics"
: >"$logs/roles.log"
# The workers' ffmpeg reads at twice the film's speed, so it is still running
# when the checks below look for it. It also counts each subtitle extraction (the
# run that writes track files) and each copy of a PGS track out of a film, and waits while $work/bin/hold exists, noting
# in $work/bin/held that it has begun.
mkdir -p "$work/bin"
: >"$work/bin/runs"
: >"$work/bin/sups"
rm -f "$work/bin/hold" "$work/bin/held"
cat >"$work/bin/ffmpeg" <<E2
#!/bin/sh
echo "\$(date +%T) \$*" >>$logs/ffmpeg.log
case "\$*" in
*"-f hls"*) exec $(command -v ffmpeg) -readrate 2 "\$@";;
*"-f sup"*) echo run >>"$work/bin/sups";;
*track.vtt*)
  echo run >>"$work/bin/runs"
  if [ -e "$work/bin/hold" ]; then touch "$work/bin/held"; while [ -e "$work/bin/hold" ]; do sleep 0.1 >/dev/null 2>&1; done; fi;;
esac
exec $(command -v ffmpeg) "\$@"
E2
chmod +x "$work/bin/ffmpeg"
CONFIG="$work/config-metrics-$worker_metrics.json" PATH="$work/bin:$PATH" start --roles=worker
CONFIG="$work/config-metrics-$worker2_metrics.json" PATH="$work/bin:$PATH" start --roles=worker
PATH="$work/nobin" start --roles=api --listen "127.0.0.1:$api1"
PATH="$work/nobin" start --roles=api --listen "127.0.0.1:$api2"
CONFIG="$work/config-metrics-$stream_metrics.json" PATH="$work/nobin" start --roles=stream --listen "127.0.0.1:$stream"

for _ in $(seq 1 60); do
  curl -fs -o /dev/null "127.0.0.1:$api1/healthz" && curl -fs -o /dev/null "127.0.0.1:$api2/healthz" &&
    curl -fs -o /dev/null "127.0.0.1:$stream/healthz" && [[ $(grep -c 'scan finished' "$logs/roles.log") -ge 6 ]] && break
  sleep 0.5
done

failed=0
passed=0 failures=0
check() { # name want got
  if [[ $2 == "$3" ]]; then
    echo "ok    $1"; passed=$((passed + 1))
  else
    echo "FAIL  $1: want $2, got $3"; failed=1; failures=$((failures + 1))
    bash "$repo/test/report.sh" fail test/roles.sh "${BASH_LINENO[0]}" "roles" "$1: want $2, got $3"
  fi
}
# The scans listed the subtitle files and fonts beside films, and converted and
# extracted none of them: the runs counted are those of extractions, which write
# track files, and the fonts' are the only ones that dump attachments.
check "the scans ran no subtitle extraction or conversion" 0 "$(wc -l <"$work/bin/runs")"
check "the scans took no font out of a film" 0 "$(grep -c -e '-dump_attachment' "$logs/ffmpeg.log")"
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

for pid in "${pids[@]}"; do kill -0 "$pid" 2>/dev/null || check "a process exited at startup" running exited; done
token=$(curl -s -X POST "127.0.0.1:$api1/Users/AuthenticateByName" -H 'Content-Type: application/json' \
  -H 'Authorization: MediaBrowser Client="c", Device="d", DeviceId="i", Version="1"' -d '{"Username":"admin","Pw":"r-pass"}' |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["AccessToken"])')
header="X-Emby-Token: $token"
check "token issued by one replica works on another" 200 "$(code -H "$header" "127.0.0.1:$api2/Users/Me")"
id=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&Limit=1" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
total=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["TotalRecordCount"])')
movies=$(find "$work/media/movies" "$work/media/transcode" -type f \( -name '*.mkv' -o -name '*.mp4' \) | wc -l)
check "two workers scanned without duplicates" "$movies" "$total"
check "stream role serves images" 200 "$(code "127.0.0.1:$stream/Items/$id/Images/Primary")"
check "api role doesn't serve images" 404 "$(code "127.0.0.1:$api1/Items/$id/Images/Primary")"
check "stream role doesn't serve the API" 404 "$(code -H "$header" "127.0.0.1:$stream/Items")"
check "stream role direct-plays with a token in the query" 206 "$(code -r 0-99 "127.0.0.1:$stream/Videos/$id/stream?ApiKey=$token&Static=true")"
check "stream role refuses media without a token" 401 "$(code "127.0.0.1:$stream/Videos/$id/stream")"
check "api role doesn't serve media" 404 "$(code "127.0.0.1:$api1/Videos/$id/stream?ApiKey=$token")"
# Download: the bytes saved are the file's own, whichever process serves them.
name=$(curl -s -D - -o "$work/download.bin" "127.0.0.1:$stream/Items/$id/Download?ApiKey=$token" | tr -d '\r' | sed -n 's/^[Cc]ontent-[Dd]isposition: attachment; filename="\?\([^";]*\)"\?.*/\1/p')
original=$(find "$work/media" -type f -name "$name" | head -1)
check "a download is the file's own bytes" "same" "$([[ -n $original ]] && cmp -s "$original" "$work/download.bin" && echo same || echo "differs ($name)")"
rm -f "$work/download.bin"
check "stream role refuses a download without a token" 401 "$(code "127.0.0.1:$stream/Items/$id/Download")"
check "api role doesn't serve downloads" 404 "$(code -H "Authorization: MediaBrowser Token=\"$token\"" "127.0.0.1:$api1/Items/$id/Download")"
# The same download link through the e2e router, which carries the compose Caddyfile's rule.
router=$(free)
node "$repo/test/e2e/router.mjs" "$router" "$stream" "$api1,$api2" >>"$logs/roles.log" 2>&1 & pids+=($!)
# Node can take seconds to start on a busy runner; 5 s was once too few in CI.
for _ in $(seq 1 300); do [[ $(code "127.0.0.1:$router/healthz") != 000 ]] && break; sleep 0.1; done # A hang bound of 30 s.
check "the router is up" yes "$([[ $(code "127.0.0.1:$router/healthz") != 000 ]] && echo yes || echo no)"
check "the ingress sends a download to the stream role" "200 stream" "$(curl -s -o /dev/null -D - "127.0.0.1:$router/Items/$id/Download?ApiKey=$token" | tr -d '\r' | awk 'BEGIN{c=""} /^HTTP/{c=$2} tolower($1)=="x-routed-to:"{r=$2} END{print c, r}')"
# UserData: written through one api replica, read through the other.
curl -s -o /dev/null -X POST -H "Authorization: MediaBrowser Token=\"$token\"" -H 'Content-Type: application/json' -d '{"PlaybackPositionTicks":123000000,"IsFavorite":true}' "127.0.0.1:$api1/UserItems/$id/UserData"
check "UserData written on one replica is read on another" "123000000 True" "$(curl -s -H "$header" "127.0.0.1:$api2/Items/$id" |
  python3 -I -c 'import json,sys;d=json.load(sys.stdin)["UserData"];print(d["PlaybackPositionTicks"],d["IsFavorite"])')"
check "wrong password is refused" 401 "$(code -X POST "127.0.0.1:$api1/Users/AuthenticateByName" -H 'Content-Type: application/json' -d '{"Username":"admin","Pw":"no"}')"
check "invalid token is refused" 401 "$(code -H 'X-Emby-Token: nope' "127.0.0.1:$api1/Users/Me")"
check "audio routes aren't served" 404 "$(code -H "$header" "127.0.0.1:$api1/Audio/$id/stream")"
check "music lists are empty" '{"Items":[],"TotalRecordCount":0,"StartIndex":0}' "$(curl -s -H "$header" "127.0.0.1:$api1/Items?Recursive=true&IncludeItemTypes=Audio")"

# Subtitles: two stream replicas with no ffmpeg are asked for one track at the
# same moment. A worker reads it out of the file once, whichever of the two
# takes the job, and a later request reads the cache.
sigma=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=Sigma" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
counted1=$(free) counted2=$(free)
PATH="$work/nobin" start --roles=stream --listen "127.0.0.1:$counted1"
PATH="$work/nobin" start --roles=stream --listen "127.0.0.1:$counted2"
for p in $counted1 $counted2; do for _ in $(seq 1 60); do curl -fs -o /dev/null "127.0.0.1:$p/healthz" && break; sleep 0.25; done; done
track="/Videos/$sigma/$sigma/Subtitles/3/0/Stream.ass?ApiKey=$token"
rm -f "$work/subs".*
requests=()
for i in $(seq 1 12); do
  p=$counted1; [[ $((i % 2)) -eq 0 ]] && p=$counted2
  curl -s -o "$work/subs.$i" -w '%{http_code} %{content_type}\n' "127.0.0.1:$p$track" >>"$work/subs.codes" &
  requests+=($!)
done
wait "${requests[@]}" # Not a bare wait, which would wait for the servers too.
check "twelve requests across two replicas all get the track" "12" "$(grep -c '^200 text/x-ssa' "$work/subs.codes")"
check "they all get the same text" 1 "$(md5sum "$work"/subs.[0-9]* | cut -d' ' -f1 | sort -u | wc -l)"
check "the text keeps its style" 1 "$(grep -c 'Style: Loud,Arial,64,&H0000FF00' "$work/subs.1")"
check "the file was read once, by one worker's ffmpeg" 1 "$(wc -l <"$work/bin/runs")"
check "another format and replica reads the cache" 200 "$(code "127.0.0.1:$counted2/Videos/$sigma/$sigma/Subtitles/3/0/Stream.vtt?ApiKey=$token")"
check "a start position shifts the cues" 1 "$(curl -s "127.0.0.1:$counted1/Videos/$sigma/$sigma/Subtitles/2/50000000/Stream.srt?ApiKey=$token" | grep -c '^00:00:00,000 --> 00:00:01,0[0-9][0-9]$')"
check "that was the other track, read by the same pass (JS-028), with no second extraction" 1 "$(wc -l <"$work/bin/runs")"
check "no extension is not found" 404 "$(code "127.0.0.1:$counted1/Videos/$sigma/$sigma/Subtitles/3/0/Stream?ApiKey=$token")"
check "subtitles need a token" 401 "$(code "127.0.0.1:$counted1/Videos/$sigma/$sigma/Subtitles/3/0/Stream.vtt")"
check "the api role doesn't serve subtitles" 404 "$(code "127.0.0.1:$api1/Videos/$sigma/$sigma/Subtitles/3/0/Stream.vtt?ApiKey=$token")"

# Subtitle files beside a film are converted, and the fonts attached to one taken
# out, by a worker's job when somebody asks, once each. A stream replica with no
# ffmpeg serves the results from the asset store.
sql() { docker exec "$pg" psql -U postgres -Atc "$1"; }
find_film() { curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=$1" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])'; }
beside=$(find_film "Direct%20Sub")
fontfilm=$(find_film "Font%20Film")
jobs_before=$(sql "SELECT count(*) FROM subtitle_jobs")
runs_before_beside=$(wc -l <"$work/bin/runs")
check "a subtitle file beside the film is served by a stream replica with no ffmpeg" "200 text/vtt 1" "$(curl -s -o "$work/beside.vtt" -w '%{http_code} %{content_type} ' "127.0.0.1:$counted1/Videos/$beside/$beside/Subtitles/2/0/Stream.vtt?ApiKey=$token"; grep -c '^English line 1$' "$work/beside.vtt")"
check "its ASS file keeps its style" 1 "$(curl -s "127.0.0.1:$counted2/Videos/$beside/$beside/Subtitles/3/0/Stream.ass?ApiKey=$token" | grep -c 'Style: Loud,Arial,64,&H0000FF00')"
check "each file was converted by one job" "$((jobs_before + 2))" "$(sql "SELECT count(*) FROM subtitle_jobs")"
check "each was converted by one worker ffmpeg run" "$((runs_before_beside + 2))" "$(wc -l <"$work/bin/runs")"
check "asked again, the file is served from the asset store" "200 $((jobs_before + 2)) $((runs_before_beside + 2))" "$(code "127.0.0.1:$counted2/Videos/$beside/$beside/Subtitles/2/0/Stream.vtt?ApiKey=$token") $(sql "SELECT count(*) FROM subtitle_jobs") $(wc -l <"$work/bin/runs")"
listed() { curl -s -H "$header" "127.0.0.1:$api2/Items/$fontfilm" | python3 -I -c 'import json,sys;print(",".join(str(a["Index"]) for a in json.load(sys.stdin)["MediaSources"][0]["MediaAttachments"]))'; }
check "a font is not listed before a worker has taken it out" "" "$(listed)"
curl -s -o /dev/null -X POST -H "$header" -H 'Content-Type: application/json' -d '{}' "127.0.0.1:$api2/Items/$fontfilm/PlaybackInfo"
for _ in $(seq 60); do attachment=$(listed); [[ -n $attachment ]] && break; sleep 0.5; done
check "PlaybackInfo has a worker take the font out, and it is listed then" "yes" "$([[ -n $attachment ]] && echo yes)"
check "the font needs a token" 401 "$(code "127.0.0.1:$counted1/Videos/$fontfilm/$fontfilm/Attachments/$attachment")"
check "the font is served by a stream replica with no ffmpeg" "200 font/ttf same" "$(curl -s -o "$work/font.bin" -w '%{http_code} %{content_type} ' "127.0.0.1:$counted1/Videos/$fontfilm/$fontfilm/Attachments/$attachment?ApiKey=$token"; cmp -s "$work/font.bin" "$(fc-match -f '%{file}' serif)" && echo same || echo differs)"
check "a worker took the fonts out of the film once" "1 1" "$(sql "SELECT count(*) FROM subtitle_jobs WHERE kind = 'fonts'") $(grep -c -e '-dump_attachment' "$logs/ffmpeg.log")"
check "a stream that is no attachment is not found" 404 "$(code "127.0.0.1:$counted1/Videos/$fontfilm/$fontfilm/Attachments/0?ApiKey=$token")"
check "the api role doesn't serve fonts" 404 "$(code "127.0.0.1:$api1/Videos/$fontfilm/$fontfilm/Attachments/$attachment?ApiKey=$token")"
check "the api role serves the encoding configuration" 200 "$(code -H "$header" "127.0.0.1:$api1/System/Configuration/encoding")"

profile=$(python3 -I -c 'import json,sys;print(json.dumps(json.load(open(sys.argv[1]))["deviceProfile"]))' "$repo/compat/captures/transcode-12.4.json")
# Image subtitles. A PGS track is copied out of the film by one worker job,
# however many ask, and a stream replica with no ffmpeg sends the file. VobSub is
# drawn into a transcode by the worker's own ffmpeg run, with no subtitle job,
# and the api and stream roles that serve it have no ffmpeg either.
pgs=$(find_film "Pgs%20Film")
vobsub=$(find_film "Vobsub%20Film")
check "the PGS track is listed as Jellyfin lists it" "PGSSUB 1920x1080 True" "$(curl -s -H "$header" "127.0.0.1:$api2/Items/$pgs" |
  python3 -I -c 'import json,sys;s=[s for s in json.load(sys.stdin)["MediaStreams"] if s["Type"] == "Subtitle"][0];print(s["Codec"], "%dx%d" % (s["Width"], s["Height"]), s["SupportsExternalStream"])')"
check "so are the VobSub track and its codec's name" "DVDSUB False" "$(curl -s -H "$header" "127.0.0.1:$api2/Items/$vobsub" |
  python3 -I -c 'import json,sys;s=[s for s in json.load(sys.stdin)["MediaStreams"] if s["Type"] == "Subtitle"][0];print(s["Codec"], s["SupportsExternalStream"])')"
sups_before=$(wc -l <"$work/bin/sups")
supjobs_before=$(sql "SELECT count(*) FROM subtitle_jobs WHERE kind = 'sup'")
rm -f "$work/pgs".* "$work/pgs.codes"
requests=()
for i in $(seq 1 12); do
  p=$counted1; [[ $((i % 2)) -eq 0 ]] && p=$counted2
  curl -s -o "$work/pgs.$i" -w '%{http_code} %{content_type}\n' "127.0.0.1:$p/Videos/$pgs/$pgs/Subtitles/2/0/Stream.pgssub?ApiKey=$token" >>"$work/pgs.codes" &
  requests+=($!)
done
wait "${requests[@]}"
check "twelve requests across two replicas with no ffmpeg all get the PGS file" "12" "$(grep -c '^200 application/octet-stream' "$work/pgs.codes")"
check "they all get the same bytes, which are a PGS stream" "1 PG" "$(md5sum "$work"/pgs.[0-9]* | cut -d' ' -f1 | sort -u | wc -l) $(head -c 2 "$work/pgs.1")"
check "one worker job copied it, with one ffmpeg run" "1 1" "$(($(sql "SELECT count(*) FROM subtitle_jobs WHERE kind = 'sup'") - supjobs_before)) $(($(wc -l <"$work/bin/sups") - sups_before))"
check "the file comes in ranges, as jellyfin-web asks for it" "206 bytes 0-0/$(wc -c <"$work/pgs.1") P" "$(curl -s -D "$work/pgs.head" -o "$work/pgs.range" -r 0-0 -w '%{http_code}' "127.0.0.1:$counted1/Videos/$pgs/$pgs/Subtitles/2/0/Stream.pgssub?ApiKey=$token") $(tr -d '\r' <"$work/pgs.head" | sed -n 's/^[Cc]ontent-[Rr]ange: //p') $(cat "$work/pgs.range")"
check "PGS asked for as WebVTT is a bad request" 400 "$(code "127.0.0.1:$counted1/Videos/$pgs/$pgs/Subtitles/2/0/Stream.vtt?ApiKey=$token")"
check "VobSub has no file to ask for" 400 "$(code "127.0.0.1:$counted1/Videos/$vobsub/$vobsub/Subtitles/2/0/Stream.sup?ApiKey=$token")"
subtitle_jobs_before=$(sql "SELECT count(*) FROM subtitle_jobs")
overlays_before=$(grep -c 'overlay=eof_action' "$logs/ffmpeg.log")
info=$(curl -s -X POST -H "$header" -H 'Content-Type: application/json' "127.0.0.1:$api1/Items/$vobsub/PlaybackInfo" \
  -d "{\"DeviceProfile\":$profile,\"MaxStreamingBitrate\":2147483647,\"AudioStreamIndex\":\"1\",\"SubtitleStreamIndex\":\"2\",\"MediaSourceId\":\"$vobsub\"}")
url=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["MediaSources"][0]["TranscodingUrl"])')
check "a chosen VobSub track is a transcode that draws it" "SubtitleCodecNotSupported 2" "$(echo "$url" | sed -n 's/.*TranscodeReasons=\([A-Za-z,]*\).*/\1/p') $(echo "$url" | sed -n 's/.*SubtitleStreamIndex=\([0-9]*\).*/\1/p')"
base=${url%%/master.m3u8*}
main=$(curl -s "127.0.0.1:$stream$url" | sed -n 's/^\(main.m3u8.*\)$/\1/p')
playlist=$(curl -s "127.0.0.1:$stream$base/$main")
check "the stream role with no ffmpeg serves a segment with the track drawn in" "styp" "$(curl -s "127.0.0.1:$stream$base/$(echo "$playlist" | grep -m1 '^hls1/main/5.mp4')" | head -c 8 | tail -c 4)"
check "a worker's ffmpeg overlaid it, on its own" "1 $subtitle_jobs_before" "$(($(grep -c 'overlay=eof_action' "$logs/ffmpeg.log") - overlays_before)) $(sql "SELECT count(*) FROM subtitle_jobs")"
curl -s -o /dev/null -X DELETE -H "$header" "127.0.0.1:$stream/Videos/ActiveEncodings?deviceId=i&PlaySessionId=$(echo "$info" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["PlaySessionId"])')"
for _ in $(seq 1 40); do [[ $(pgrep -f "ffmpeg.*$work/scratch" | wc -l) -eq 0 ]] && break; sleep 0.5; done

# A worker killed outright in the middle of an extraction loses nothing: its
# ffmpeg dies with it, the lease runs out, and the other worker finishes the
# track for the request that is still waiting.
tau=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=Tau" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
runs_before=$(wc -l <"$work/bin/runs")
touch "$work/bin/hold"
(curl -s -o "$work/tau.vtt" -w '%{http_code}' "127.0.0.1:$counted1/Videos/$tau/$tau/Subtitles/2/0/Stream.vtt?ApiKey=$token" >"$work/tau.code") &
requester=$!
for _ in $(seq 1 40); do [[ -e "$work/bin/held" ]] && break; sleep 0.25; done
check "a worker has begun the extraction" yes "$([[ -e "$work/bin/held" ]] && echo yes)"
owner=$(sql "SELECT worker FROM subtitle_jobs WHERE state = 'running'")
kill -9 "${owner%%-*}"
rm -f "$work/bin/hold"
wait "$requester"
check "the request waiting on the killed worker gets the track" 200 "$(cat "$work/tau.code")"
check "the track has its cues" 1 "$(grep -c 'English line 0' "$work/tau.vtt")"
check "the job was claimed twice" 2 "$(sql "SELECT max(attempts) FROM subtitle_jobs")"
check "the killed worker's ffmpeg started and the survivor's finished" 2 "$(($(wc -l <"$work/bin/runs") - runs_before))"

# Transcoding across processes: the stream role serves HLS, a worker process
# makes the segments, and no ffmpeg outlives the DELETE that ends the session.
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
# Both workers are scraped while the job runs: the one with it shows its speed
# and a busy slot, and the stream role shows the wait for the segments.
scrape() { curl -s "127.0.0.1:$1/metrics"; }
speed=""
for _ in $(seq 1 40); do
  speed=$(scrape "$worker_metrics"; scrape "$worker2_metrics")
  echo "$speed" | grep -q '^jellyscale_transcode_job_speed_ratio{' && break
  sleep 0.5
done
check "a running job's speed is on a worker" 1 "$(echo "$speed" | grep -c '^jellyscale_transcode_job_speed_ratio{')"
check "a worker has a busy slot of its 2" "1 2" "$(echo "$speed" | awk '/^jellyscale_transcode_slots_busy / {b+=$2} /^jellyscale_transcode_slots / {t=$2} END {print b, t}')"
check "the job is counted by kind (the AC3 film is a copy)" "1 0" "$(echo "$speed" | awk '/^jellyscale_transcode_jobs_running\{kind="copy"\}/ {c+=$2} /^jellyscale_transcode_jobs_running\{kind="reencode"\}/ {r+=$2} END {print c, r}')"
check "the stream role counts the waits for segments" "yes" "$(scrape "$stream_metrics" | grep -q '^jellyscale_segment_wait_seconds_count{kind="reencode",outcome="ok",source="started"} [1-9]' && echo yes)"
check "the public port has no /metrics" 404 "$(code "127.0.0.1:$stream/metrics")"
echo "--- /metrics of the worker running the job, trimmed:" >&2
echo "$speed" | grep -E '^jellyscale_transcode_(job_speed_ratio|slots|slots_busy|jobs_running|queue_depth)' >&2
scrape "$stream_metrics" | grep -E '^jellyscale_segment_wait_seconds_(count|sum)' >&2
check "DELETE ActiveEncodings answers 204" 204 "$(code -X DELETE -H "$header" "127.0.0.1:$stream/Videos/ActiveEncodings?deviceId=i&PlaySessionId=$session")"
for _ in $(seq 1 40); do [[ $(pgrep -f "ffmpeg.*$work/scratch" | wc -l) -eq 0 ]] && break; sleep 0.5; done
check "no ffmpeg outlives the session" 0 "$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)"
check "the speed series ends with the job" 0 "$( (scrape "$worker_metrics"; scrape "$worker2_metrics") | grep -c '^jellyscale_transcode_job_speed_ratio{')"
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
killed=$(date +%s%N)
kill -9 "${pids[0]}" "${pids[1]}" 2>/dev/null
for _ in $(seq 1 20); do [[ $(pgrep -f "ffmpeg.*$work/scratch" | wc -l) -eq 0 ]] && break; sleep 0.25; done
check "ffmpeg dies with a worker that was killed" 0 "$(pgrep -f "ffmpeg.*$work/scratch" | wc -l)"
# Their locks went with them, so a replica no longer counts them, and PlaybackInfo
# refuses within about a second and a half of the kill. The replica deletes no
# row: only a worker forgets a dead worker (JS-029: a replica can miss an outage
# and would forget live ones).
nodes() { docker exec "$pg" psql -U postgres -Atc "select count(*) from transcode_nodes where slots > 0"; }
refused() {
  curl -s -X POST -H "$header" -H 'Content-Type: application/json' "127.0.0.1:$api1/Items/$zeta/PlaybackInfo" \
    -d "{\"DeviceProfile\":$profile,\"MaxStreamingBitrate\":1200000}" | python3 -I -c 'import json,sys;print(json.load(sys.stdin).get("ErrorCode",""))'
}
killed_rows=$(nodes) # One or both: the second may have forgotten the first before it died.
until [[ $(refused) == RateLimitExceeded ]] || (( $(date +%s%N) - killed > 5000000000 )); do sleep 0.1; done # A hang bound.
took=$(( ($(date +%s%N) - killed) / 1000000 ))
echo "the kill to PlaybackInfo's refusal: $took ms"
check "with every worker killed, PlaybackInfo refuses within about 2 s of the kill" RateLimitExceeded "$(refused)"
check "the refusal came within 2 s" 1 "$(( took <= 2000 ))"
check "a replica forgets no killed worker" "$killed_rows" "$(nodes)"

# Rolling restart: a worker stops gracefully and its replacement arrives a few
# seconds later. The request that comes in between waits for it, where it used
# to be answered 503 at once.
PATH="$work/bin:$PATH" start --roles=worker
old=${pids[-1]}
# Until the new worker reports in; its first look forgets the killed ones.
live() { docker exec "$pg" psql -U postgres -Atc "select count(*) from transcode_nodes where slots > 0 and seen_at > now() - interval '2 seconds'"; }
for _ in $(seq 1 60); do [[ $(live) -ge 1 ]] && break; sleep 0.5; done
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

# With every worker gone, a track that was extracted before still plays and one
# that wasn't is neither listed nor left hanging.
pkill -9 -f "$work/jellyscale --roles=worker"
sleep 11 # The subtitle queue counts a worker as running for ten seconds after it last reported.
phi=$(curl -s -H "$header" "127.0.0.1:$api2/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=Phi" |
  python3 -I -c 'import json,sys;print(json.load(sys.stdin)["Items"][0]["Id"])')
check "a cached track plays with no worker" 200 "$(code "127.0.0.1:$stream/Videos/$sigma/$sigma/Subtitles/3/0/Stream.vtt?ApiKey=$token")"
began=$SECONDS
# A refusal, not a hang: curl gives up after two minutes (000), which is not a 503. How long it
# took is logged and not judged, as a slower runner takes longer.
check "an uncached track is refused" 503 "$(code -m 120 "127.0.0.1:$stream/Videos/$phi/$phi/Subtitles/2/0/Stream.vtt?ApiKey=$token")"
echo "the refusal came after $(( SECONDS - began )) s"
subtitles() { curl -s -H "$header" "127.0.0.1:$api2/Items/$1" | python3 -I -c 'import json,sys;print(sum(s["Type"] == "Subtitle" for s in json.load(sys.stdin)["MediaStreams"]))'; }
check "the uncached film lists no subtitle track" 0 "$(subtitles "$phi")"
check "the cached film lists both of its tracks" 2 "$(subtitles "$sigma")"
result_summary="roles: $passed passed, $failures failed"
bash "$repo/test/report.sh" totals roles "$passed" "$failures"
exit $failed
