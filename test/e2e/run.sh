#!/usr/bin/env bash
# Runs the browse, play, subtitles, failover and transcode gates against disposable
# infrastructure, with each role as its own process behind a router, as in
# production: Postgres, S3 (SeaweedFS), two api replicas, a stream replica and a worker.
# Usage: run.sh <workdir> <dir_with_playwright>
# E2E_GATES picks which gates run, default "browse play subtitles failover transcode".
# The failover gate (JS-031) kills and rolls api and stream replicas it starts itself, behind the router.
# E2E_SHARD=i/n runs every n-th scenario of the subtitles and transcode gates,
# starting at the i-th (test/e2e/shard.mjs): CI runs the shards side by side.
# The scenarios that stop the worker run last, on the last shard. browse and play
# are one sequence of steps and always run whole, so give them to one job (E2E_GATES).
# Every gate ends with "shard i/n: ran k of T"; a gate without that line, or with
# k = 0, fails the run. <logs>/shard-<gate>-<i>of<n>.txt holds "gate i n k T" for
# test/check-shards.sh, which checks that the shards added up to every scenario.
# E2E_ONLY="regex" runs the scenarios whose name matches (subtitles, transcode).
# E2E_REPEAT=n runs the gates n times on one stack, to hunt a flake, and reports
# "passed p of n" for each scenario. Between iterations it clears the segments
# directory, the scratch directory and the S3 segments, resets the transcode
# jobs and restarts the worker, and keeps the fixtures. Nothing else is reset:
# the scan, the subtitles and fonts already made, and users stay, so these are
# warm reruns. A scenario that needs a first play of a film's subtitles or fonts
# can fail from the second iteration on.
# E2E_FIXTURE_CACHE is where the films are cached between runs (test/fixtures/cached.sh).
# E2E_HEVC=1 says the browser plays HEVC (a machine with a hardware decoder); the
# transcode gate then expects HEVC to direct play and copy. CI leaves it unset.
# Expects jellyfin-web in ./web-dist: docker cp <jellyfin>:/jellyfin/jellyfin-web web-dist
# Writes <workdir>/config.json (POSIX libraries only, from test/fixtures/posix.sh)
# and <workdir>/config-s3.json (adds a library on S3 for media and assets).
# Temp files go to a short dir under /tmp/jellyscale-test (TMPDIR) and are removed at exit (test/lib.sh). A gate run
# whole (no E2E_ONLY, no E2E_REPEAT) records its result under .results.
# Logs (server.log with every role's output, whose errors carry the end of
# ffmpeg's stderr, and ffmpeg.log with each command line the worker ran) go to
# $JELLYSCALE_LOG_DIR, default <workdir>/logs, whole and, for what each gate
# wrote, in <gate>/server.log and <gate>/ffmpeg.log.
# JS-070: each gate records the JSON responses its browser gets in <logs>/<gate>/responses.jsonl (test/e2e/conform.mjs),
# and when the gate ends compat/conform/cmd/conform holds them to compat/schema-12.4.json and prints
# "e2e <gate>: checked N JSON responses on M routes"; a violation, or no response, fails the gate.
# In CI ($GITHUB_ACTIONS set) each gate's failures go to the job summary and
# become annotations (test/report.sh); locally the output stays as it is.
set -uo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
# First, before any trap, fixture or container: without it every scenario fails as a login timeout.
if [[ ! -f $repo/web-dist/index.html ]]; then
  cat >&2 <<E1
test/e2e/run.sh needs jellyfin-web in $repo/web-dist (a git worktree has none of its own). Copy it from the baseline image:
  docker create --name jf jellyfin/jellyfin:12.4
  docker cp jf:/jellyfin/jellyfin-web $repo/web-dist && docker rm -v jf
E1
  exit 1
fi
repeat=${E2E_REPEAT:-1}
[[ $repeat =~ ^[1-9][0-9]*$ ]] || { echo "E2E_REPEAT must be a whole number of runs, 1 or more; it is '$repeat'" >&2; exit 2; }
source "$repo/test/lib.sh"
[[ $# -eq 2 ]] || { echo "usage: test/e2e/run.sh <workdir> <dir_with_playwright>" >&2; exit 2; }
playwright=$2
pg=jellyscale-e2e-pg-$$ sw=jellyscale-e2e-sw-$$
pids=()
make_work e2e "$1" || exit 1
logs=${JELLYSCALE_LOG_DIR:-$work/logs}
mkdir -p "$logs"
logs=$(cd "$logs" && pwd)

gate_results=() # name|exit status|summary|log of each gate that ran whole
leaked=0
cleanup() {
  [[ ${#pids[@]} -gt 0 ]] && kill "${pids[@]}" 2>/dev/null
  # Replicas a failover scenario started and a gate that was killed left behind: they belong to this workdir.
  pkill -f "^$work/jellyscale --roles=(api|stream) --listen" 2>/dev/null
  docker rm -fv "$pg" "$sw" >/dev/null 2>&1
  local entry name gate_status summary log
  for entry in "${gate_results[@]}"; do
    IFS='|' read -r name gate_status summary log <<<"$entry"
    result_write "$name" $((gate_status || leaked)) "$summary" "$log"
  done
  # A failing scenario's screenshot is the one thing in the temp dir worth keeping.
  mv "$TMPDIR"/jellyscale-e2e*.png "$logs/" 2>/dev/null
  rm -rf "$tmp_base" "$work/t"
}
trap cleanup EXIT
result_start
source "$repo/test/s3-lib.sh"

# The films come from the cache (or are made into it) under one key, so a run
# makes no fixture of its own that an earlier run made: test/fixtures/cached.sh.
bash "$repo/test/fixtures/cached.sh" "$work" || exit 1
bash "$repo/test/fixtures/posix.sh" "$work" || exit 1
epsilon="$work/media/sources/Epsilon Film (2022).mkv"
zeta="$work/media/transcode/Zeta Film (2023)/Zeta Film (2023).mkv"
rho="$work/media/transcode/Rho Film (2041)/Rho Film (2041).mkv"
qoppa="$work/media/transcode/Qoppa Film (2042)/Qoppa Film (2042).mp4"
big="$work/media/transcode/Stigma Film (2047)/Stigma Film (2047).mkv"

# What a run does with the cached films, none of it cached: hard links and copies.
# link <from> <to>: a hard link to a film under media/, which is enough where no
# scenario rewrites the file, and keeps the media small.
link() {
  mkdir -p "$(dirname "$work/media/$2")" && ln "$work/media/$1" "$work/media/$2" || exit 1
}
t=transcode
# Films the captured Chrome profile can't direct-play (Zeta, from posix.sh, has
# AC3 audio). Each scenario gets its own item, with its own segments.
for copy in "Theta Film (2025)" "Iota Film (2026)" "Kappa Film (2027)" "Lambda Film (2028)" "Mu Film (2029)" "Omicron Film (2032)" "Pi Film (2033)"; do
  link "$t/Zeta Film (2023)/Zeta Film (2023).mkv" "$t/$copy/$copy.mkv"
done
# The VobSub film again, for the scenario that burns it in for a viewer who draws PGS:
# the segments of a spec are kept, so it needs a film of its own.
link "$t/Vobsub Film (2071)/Vobsub Film (2071).mkv" "$t/Vobsub Render Film (2076)/Vobsub Render Film (2076).mkv"
# And the PGS film for the scenario that shares its burn-in between workers.
link "$t/Pgs Film (2070)/Pgs Film (2070).mkv" "$t/Pgs Parallel Film (2128)/Pgs Parallel Film (2128).mkv"
# And the VobSub pair beside a film, the same.
link "$t/Idx Beside Film (2122)/Idx Beside Film (2122).mkv" "$t/Idx Beside Render Film (2124)/Idx Beside Render Film (2124).mkv"
for ext in en.sub en.idx; do
  link "$t/Idx Beside Film (2122)/Idx Beside Film (2122).$ext" "$t/Idx Beside Render Film (2124)/Idx Beside Render Film (2124).$ext"
done
# The failure scenario cuts this copy short, so it is a copy and not a link.
mkdir -p "$work/media/$t/Xi Film (2031)" && cp "$zeta" "$work/media/$t/Xi Film (2031)/Xi Film (2031).mkv" || exit 1
# Two more of Zeta and of its interlaced twin, for the scenario that checks how far a
# job runs ahead of the viewer: it needs segments nobody has made yet.
# The same twice more for the scenarios that kill the worker playing a film.
for pair in "Lead Copy Film (2094):Zeta Film (2023)" "Lead Encode Film (2095):Zeta Interlaced (2050)" "Failover Copy Film (2110):Zeta Film (2023)" "Failover Encode Film (2111):Zeta Interlaced (2050)" "Parallel Encode Film (2112):Zeta Interlaced (2050)" "Outage Encode Film (2114):Zeta Interlaced (2050)"; do
  to=${pair%%:*} from=${pair#*:}
  link "$t/$from/$from.mkv" "$t/$to/$to.mkv"
done
# The films the failover gate (JS-031) kills and rolls replicas under: one direct play (the 60 MB
# film of the outage scenario, which the router slows to a download of tens of seconds) and one
# re-encode each for an api kill, a stream kill and a stream roll, and the three films that play on
# through a Postgres outage from a stream replica's snapshot of active sessions (JS-085).
for pair in "Api Kill Film (2140):Outage Direct Film (2113)" "Stream Kill Direct Film (2141):Outage Direct Film (2113)" "Stream Roll Direct Film (2142):Outage Direct Film (2113)" "Stream Kill Encode Film (2143):Zeta Interlaced (2050)" "Stream Roll Encode Film (2144):Zeta Interlaced (2050)" "Away Direct Film (2145):Outage Direct Film (2113)" "Away Late Film (2146):Outage Direct Film (2113)" "Away Encode Film (2147):Zeta Interlaced (2050)" "Return Direct Film (2148):Return Source Film (2154)" "Return Encode Film (2149):Zeta Interlaced (2050)" "Just Started Film (2152):Outage Direct Film (2113)" "Just Started Other Film (2153):Outage Direct Film (2113)" "Outage Pick Film (2155):Sigma Film (2034)" "Outage Font Film (2156):Font Film (2063)"; do
  to=${pair%%:*} from=${pair#*:}
  link "$t/$from/$from.mkv" "$t/$to/$to.mkv"
done
# The same four chapter films again in a library with chapter pictures on, where
# the details page lists the chapters. The other library has them off, as Jellyfin does by default.
link "$t/Chapters Direct (2095)/Chapters Direct (2095).mkv" "scenes/Scenes Direct (2105)/Scenes Direct (2105).mkv"
link "$t/Chapters Mp4 (2096)/Chapters Mp4 (2096).mp4" "scenes/Scenes Mp4 (2106)/Scenes Mp4 (2106).mp4"
link "$t/Chapters Copy (2097)/Chapters Copy (2097).mkv" "scenes/Scenes Copy (2107)/Scenes Copy (2107).mkv"
link "$t/Chapters Encode (2098)/Chapters Encode (2098).mkv" "scenes/Scenes Encode (2108)/Scenes Encode (2108).mkv"
# The 1080p film twice more, so resuming it finds nothing made: each item has its own segments.
link "$t/Stigma Film (2047)/Stigma Film (2047).mkv" "$t/Sho Film (2048)/Sho Film (2048).mkv"
# Sigma's SubRip and ASS tracks again, for the scenario that picks one and then the
# other: each item has its own subtitle objects, so its first pick reads the film.
link "$t/Sigma Film (2034)/Sigma Film (2034).mkv" "$t/Pick Film (2130)/Pick Film (2130).mkv"


# A scenario stops Postgres and starts it again (JS-029), so the container is not
# removed when it stops (no --rm; cleanup removes it) and its host port is
# chosen here, because a port Docker assigns can change when a container starts again.
port=$(python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')
docker run -d --name "$pg" -e POSTGRES_PASSWORD=e2e -p "127.0.0.1:$port:5432" postgres:18-alpine >/dev/null || exit 1
start_seaweed "$sw" "$work" e2e || exit 1
for _ in $(seq 1 60); do docker exec "$pg" psql -U postgres -Atc 'select 1' >/dev/null 2>&1 && break; sleep 0.5; done
sleep 2

s3="endpoint=http://127.0.0.1:$seaweed_port&region=us-east-1"
# config.json, which posix.sh wrote, keeps segments in a directory; this one puts
# them on S3, so the transcode store is exercised on both.
cat >"$work/config-s3.json" <<E2
{"name": "e2e", "adminUser": "admin", "webDir": "$repo/web-dist", "scanInterval": "1h",
 "transcodeStore": "s3://e2e/segments?$s3", "transcodeScratch": "$work/scratch", "transcodeIdle": "10s", "transcodeLead": "20s", "libraries": [
  {"name": "Movies", "kind": "movies", "media": "file://$work/media/movies", "assets": "file://$work/assets/movies"},
  {"name": "Shows", "kind": "tvshows", "media": "file://$work/media/shows", "assets": "file://$work/assets/shows"},
  {"name": "Transcode", "kind": "movies", "media": "file://$work/media/transcode", "assets": "file://$work/assets/transcode"},
  {"name": "HDR", "kind": "movies", "media": "file://$work/media/hdr", "assets": "file://$work/assets/hdr"},
  {"name": "Scenes", "kind": "movies", "media": "file://$work/media/scenes", "assets": "file://$work/assets/scenes", "chapterImages": true},
  {"name": "Cloud", "kind": "movies", "media": "s3://e2e/media?$s3", "assets": "s3://e2e/assets?$s3"}
]}
E2

export JELLYSCALE_S3_ACCESS_KEY=$seaweed_key JELLYSCALE_S3_SECRET_KEY=$seaweed_secret
(cd "$repo" && go build -o "$work/jellyscale" ./cmd/jellyscale && go build -o "$work/putfile" ./test/e2e/putfile && go build -o "$work/conform" ./compat/conform/cmd/conform) || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Epsilon Film (2022)/Epsilon Film (2022).mkv" "$epsilon" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Eta Film (2024)/Eta Film (2024).mkv" "$zeta" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Heta Film (2043)/Heta Film (2043).mkv" "$rho" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Koppa Film (2044)/Koppa Film (2044).mp4" "$qoppa" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Heh Film (2049)/Heh Film (2049).mkv" "$big" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Mem Film (2085)/Mem Film (2085).mkv" "$work/media/transcode/Sampi Film (2080)/Sampi Film (2080).mkv" || exit 1
# The subtitle films again, on S3: one that direct plays, one that transcodes,
# and the black one that burn-in shows against.
sub="$work/media/transcode"
"$work/putfile" "s3://e2e/media?$s3" "Chi Film (2038)/Chi Film (2038).mkv" "$sub/Sigma Film (2034)/Sigma Film (2034).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Psi Film (2039)/Psi Film (2039).mkv" "$sub/Tau Film (2035)/Tau Film (2035).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Omega Film (2040)/Omega Film (2040).mkv" "$sub/Phi Film (2037)/Phi Film (2037).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Pick Cloud Film (2131)/Pick Cloud Film (2131).mkv" "$sub/Sigma Film (2034)/Sigma Film (2034).mkv" || exit 1
# And the films with their subtitles in files beside them: the files go to S3
# too, where the scan finds them by listing.
for pair in "Direct Sub Film (2060)|Beside Direct Film (2065)" "Transcode Sub Film (2061)|Beside Transcode Film (2066)" "Burn Sub Film (2062)|Beside Burn Film (2067)"; do
  from=${pair%%|*} to=${pair##*|}
  for ext in mkv en.srt ja.ass; do
    "$work/putfile" "s3://e2e/media?$s3" "$to/$to.$ext" "$sub/$from/$from.$ext" || exit 1
  done
done

# And two with image tracks, a PGS one and a VobSub one.
"$work/putfile" "s3://e2e/media?$s3" "Pgs Cloud Film (2074)/Pgs Cloud Film (2074).mkv" "$sub/Pgs Film (2070)/Pgs Film (2070).mkv" || exit 1
"$work/putfile" "s3://e2e/media?$s3" "Vobsub Cloud Film (2075)/Vobsub Cloud Film (2075).mkv" "$sub/Vobsub Film (2071)/Vobsub Film (2071).mkv" || exit 1
# And the same two with their image subtitles in files beside them.
for ext in mkv en.sup; do
  "$work/putfile" "s3://e2e/media?$s3" "Sup Beside Cloud Film (2125)/Sup Beside Cloud Film (2125).$ext" "$sub/Sup Beside Film (2120)/Sup Beside Film (2120).$ext" || exit 1
done
for ext in mkv en.sub en.idx; do
  "$work/putfile" "s3://e2e/media?$s3" "Idx Beside Cloud Film (2126)/Idx Beside Cloud Film (2126).$ext" "$sub/Idx Beside Film (2122)/Idx Beside Film (2122).$ext" || exit 1
done

free() { python3 -I -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
export JELLYSCALE_DATABASE_URL="postgres://postgres:e2e@127.0.0.1:$port/postgres" JELLYSCALE_ADMIN_PASSWORD=e2e-pass
api1=$(free) api2=$(free) stream=$(free) router=$(free)
: >"$logs/server.log"
: >"$logs/ffmpeg.log"
# api and stream run with no ffmpeg on their PATH (an empty directory): only
# workers run media tools, and anything else that tried would fail.
mkdir -p "$work/nobin"
start() { PATH="$work/nobin" "$work/jellyscale" "$@" --config "$work/config-s3.json" >>"$logs/server.log" 2>&1 & pids+=($!); }
# The worker's ffmpeg reads its input at twice the film's speed, so a transcode
# takes a while on any machine and the stop tests find it still running.
# E2E_NO_SHIM=1 leaves ffmpeg at full speed, to measure latency.
mkdir -p "$work/bin"
speed="-readrate 2"
[[ -n ${E2E_NO_SHIM:-} ]] && speed="-hide_banner"
# A scenario that needs a slow transcode (under 1.2x, which is when helpers start)
# writes the rate it wants to $work/bin/rate, and removes the file again.
# Only the transcode (the one that writes HLS) is slowed; extracting a subtitle
# track reads the whole file and would take as long as the film. Taking the
# fonts out of "Cracked Fonts Film" fails as a film ffmpeg rejects does.
# A run that draws an image track reads the track from the film's first input,
# and ffmpeg's -readrate waits on a stream with few packets, so the video there
# would be held back for the length of a gap between cues (ffmpeg reads with no
# limit in production, where the same film's segments come as fast as it is
# read). Its throttle goes on the second input, the audio, which paces the run
# as well.
cat >"$work/bin/ffmpeg" <<E2
#!/bin/sh
echo "\$(date +%T) \$*" >>"$logs/ffmpeg.log"
case "\$*" in *-dump_attachment*"Cracked Fonts"*) echo "Invalid data found when processing input" >&2; exit 1;; esac
case "\$*" in
*overlay=eof_action*)
  inputs=0
  # A scenario that writes a rate slows this run too, by the same input.
  pace="$speed"; [ -s "$work/bin/rate" ] && pace="-readrate \$(cat "$work/bin/rate")"
  for arg; do
    shift
    if [ "\$arg" = -i ]; then inputs=\$((inputs + 1)); [ "\$inputs" = 2 ] && set -- "\$@" \$pace; fi
    set -- "\$@" "\$arg"
  done
  exec $(command -v ffmpeg) "\$@";;
*"-f hls"*) if [ -s "$work/bin/rate" ]; then exec $(command -v ffmpeg) -readrate "\$(cat "$work/bin/rate")" "\$@"; fi; exec $(command -v ffmpeg) $speed "\$@";;
esac
exec $(command -v ffmpeg) "\$@"
E2
chmod +x "$work/bin/ffmpeg"
start_worker() {
  PATH="$work/bin:$PATH" "$work/jellyscale" --roles=worker --config "$work/config-s3.json" >>"$logs/server.log" 2>&1 &
  pids+=($!)
  export E2E_WORKER_PID=$!
}
start_worker
start --roles=api --listen "127.0.0.1:$api1"
start --roles=api --listen "127.0.0.1:$api2"
start --roles=stream --listen "127.0.0.1:$stream"
node "$repo/test/e2e/router.mjs" "$router" "$stream" "$api1,$api2" >>"$logs/server.log" 2>&1 &
pids+=($!)
# A second stream replica set up wrongly, with no transcodeStore, behind its own
# router. The scenario that needs it starts it with E2E_NOSTORE and stops it
# again, because every role is compared and the others would be refused meanwhile.
sed 's/"transcodeStore": "[^"]*", //' "$work/config-s3.json" >"$work/config-nostore.json"
nostream=$(free) norouter=$(free)
printf '#!/bin/sh\nPATH="%s" exec "%s" --roles=stream --listen "127.0.0.1:%s" --config "%s" >>"%s" 2>&1\n' \
  "$work/nobin" "$work/jellyscale" "$nostream" "$work/config-nostore.json" "$logs/server.log" >"$work/bin/nostore"
chmod +x "$work/bin/nostore"
export E2E_NOSTORE="$work/bin/nostore"
node "$repo/test/e2e/router.mjs" "$norouter" "$nostream" "$api1,$api2" >>"$logs/server.log" 2>&1 &
pids+=($!)
export E2E_BASE_NOSTORE="http://127.0.0.1:$norouter"
# More workers for the scenarios that kill one with another to take over. A
# scenario starts them and stops the main worker meanwhile, so they get the jobs.
printf '#!/bin/sh\nPATH="%s:%s" exec "%s" --roles=worker --config "%s" >>"%s" 2>&1\n' \
  "$work/bin" "$PATH" "$work/jellyscale" "$work/config-s3.json" "$logs/server.log" >"$work/bin/worker"
chmod +x "$work/bin/worker"
export E2E_WORKER="$work/bin/worker"
# More api and stream replicas, for the failover gate: each scenario starts its own on ports it
# chooses (the launcher takes the port) and tells the router, then kills or rolls them.
# Like the others they have no ffmpeg on their PATH.
for role in api stream; do
  printf '#!/bin/sh\nPATH="%s" exec "%s" --roles=%s --listen "127.0.0.1:$1" --config "%s" >>"%s" 2>&1\n' \
    "$work/nobin" "$work/jellyscale" "$role" "$work/config-s3.json" "$logs/server.log" >"$work/bin/$role-replica"
  chmod +x "$work/bin/$role-replica"
done
export E2E_API_REPLICA="$work/bin/api-replica" E2E_STREAM_REPLICA="$work/bin/stream-replica"
for p in $api1 $api2 $stream; do
  for _ in $(seq 1 60); do curl -fs -o /dev/null "http://127.0.0.1:$p/healthz" && break; sleep 0.25; done
done
for _ in $(seq 1 120); do [[ $(grep -c 'scan finished' "$logs/server.log") -ge 5 ]] && break; sleep 0.5; done

base="http://127.0.0.1:$router"
cp "$repo/test/e2e/conform.mjs" "$repo/test/e2e/shard.mjs" "$repo/test/e2e/player.mjs" "$repo/test/e2e/browse.mjs" "$repo/test/e2e/play.mjs" "$repo/test/e2e/transcode.mjs" "$repo/test/e2e/subtitles.mjs" "$repo/test/e2e/failover.mjs" "$repo/test/e2e/away.mjs" "$playwright/"
result=0
export E2E_SCRATCH="$work/scratch" E2E_MEDIA="$work/media/transcode" E2E_ASSETS="$work/assets/transcode" E2E_PG="$pg" E2E_LOG="$logs/server.log"
# Only a whole, ordinary run says what the tree does: a selection, a repeat, a
# browser that plays HEVC or a worker at full speed is none of that.
whole=1
[[ -n ${E2E_ONLY:-} || -n ${E2E_REPEAT:-} || -n ${E2E_HEVC:-} || -n ${E2E_NO_SHIM:-} || -n ${E2E_FLAKY_DEMO:-} ]] && whole=0

# Runs one gate once, leaving its output in $out, and checks its last line.
# Sets gate_status, and gate_summary for the record.
run_gate() { # gate out_dir
  local gate=$1 dir=$2 out=$2/out.txt status record
  mkdir -p "$dir"
  export E2E_RESPONSES=$dir/responses.jsonl
  : >"$E2E_RESPONSES"
  # One stack serves every gate, so what a gate wrote is the log from here on.
  local server_from ffmpeg_from
  server_from=$(stat -c %s "$logs/server.log") ffmpeg_from=$(stat -c %s "$logs/ffmpeg.log")
  # The transcode gate ends by killing the worker, so subtitles run before it.
  # Each gate has 1500 s; the transcode gate, whole, took 818 s on main before the
  # read-ahead scenarios and over 900 s after. A shard has a part of that.
  if [[ -n ${GITHUB_ACTIONS:-} ]]; then
    (cd "$playwright" && timeout 1500 node $gate.mjs "$base" admin e2e-pass) 2>&1 | tee "$out" | bash "$repo/test/report.sh" lines "test/e2e/$gate.mjs" "e2e $gate"
    status=${PIPESTATUS[0]}
    if [[ $status -ne 0 ]]; then
      bash "$repo/test/report.sh" fail "test/e2e/$gate.mjs" 1 "e2e $gate" "the gate exited $status (124 is its 1500 s limit)"
    fi
  else
    (cd "$playwright" && timeout 1500 node $gate.mjs "$base" admin e2e-pass) 2>&1 | tee "$out"
    status=${PIPESTATUS[0]}
  fi
  # The JSON responses the browser got, held to the schema (test/e2e/conform.mjs recorded them).
  if ! (cd "$repo" && "$work/conform" check -schema compat/schema-12.4.json -rules compat/conform.yaml -label "e2e $gate" <"$E2E_RESPONSES") 2>&1 | tee "$dir/conform.txt"; then
    status=1
    if [[ -n ${GITHUB_ACTIONS:-} ]]; then
      bash "$repo/test/report.sh" fail "test/e2e/$gate.mjs" 1 "e2e $gate" "its responses break compat/schema-12.4.json (see the FAIL lines above)"
    fi
  fi
  tail -c +$((server_from + 1)) "$logs/server.log" >"$dir/server.log"
  tail -c +$((ffmpeg_from + 1)) "$logs/ffmpeg.log" >"$dir/ffmpeg.log"
  # The last line says how many scenarios this shard ran of how many there are.
  gate_summary="$gate: $(grep -c '^ok  ' "$out") ok, $(grep -c '^FAIL  ' "$out") failed"
  gate_name=e2e-$gate
  if record=$(bash "$repo/test/check-shards.sh" line "$out"); then
    local i n k total
    read -r i n k total <<<"$record"
    gate_summary+=", shard $i/$n: ran $k of $total"
    echo "$gate $i $n $k $total" >"$logs/shard-$gate-${i}of$n.txt"
    (( n > 1 )) && gate_name+=-${i}of$n
  else
    status=1 gate_name= # which shard it was is unknown, so nothing is recorded
    bash "$repo/test/report.sh" fail "test/e2e/$gate.mjs" 1 "e2e $gate" "the gate stopped partway or ran no scenario (see the line above)"
  fi
  gate_status=$status
}

# Between E2E_REPEAT iterations: what the last one left in the worker's way goes,
# the fixtures and the scan stay (a warm rerun).
reset_state() {
  echo "=== reset: worker, segments, scratch, jobs"
  kill "$E2E_WORKER_PID" 2>/dev/null
  # Its ffmpeg belongs to this workdir alone.
  pkill -f "ffmpeg.*$work/scratch" 2>/dev/null
  for _ in $(seq 1 40); do pgrep -f "ffmpeg.*$work/scratch" >/dev/null || break; sleep 0.25; done
  rm -rf "$work/segments" "$work/scratch"
  "$work/putfile" -delete "s3://e2e/segments?$s3" || result=1
  docker exec "$pg" psql -U postgres -Atc 'TRUNCATE transcode_viewers, transcode_jobs' >/dev/null || result=1
  # A film the last iteration left half watched would resume there, not at the start
  # the scenarios that play it from the top expect.
  docker exec "$pg" psql -U postgres -Atc 'UPDATE user_item_data SET position_ticks = 0, played = false' >/dev/null || result=1
  start_worker
  # A worker counts once its lock is held; the killed one's row has no lock.
  for _ in $(seq 1 60); do
    [[ $(docker exec "$pg" psql -U postgres -Atc "SELECT count(*) FROM transcode_nodes n WHERE locked AND EXISTS (SELECT 1 FROM pg_locks l
      WHERE l.locktype = 'advisory' AND l.classid = 27219 AND l.objsubid = 2 AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295)" 2>/dev/null) == 1 ]] && break
    sleep 0.5
  done
}

repeat_outs=()
for ((iteration = 1; iteration <= repeat; iteration++)); do
  export E2E_ITERATION=$iteration
  [[ $iteration -gt 1 ]] && reset_state
  for gate in ${E2E_GATES:-browse play subtitles failover transcode}; do
    echo "=== $gate${E2E_REPEAT:+ (iteration $iteration of $repeat)}"
    dir=$logs/$gate; [[ $repeat -gt 1 ]] && dir=$logs/$gate-$iteration
    run_gate "$gate" "$dir"
    repeat_outs+=("$dir/out.txt")
    [[ $gate_status -ne 0 ]] && result=1
    [[ $whole -eq 1 && -n $gate_name ]] && gate_results+=("$gate_name|$gate_status|$gate_summary|$dir")
  done
done

# What each scenario did in each iteration. A name is what follows "ok  " or
# "FAIL  ", up to the reason: the name of a passed scenario is known, and a failure
# is matched to it; one that never passed is listed as it was printed.
if [[ $repeat -gt 1 ]]; then
  echo "--- passed p of $repeat, per scenario"
  cat "${repeat_outs[@]}" | python3 -I -c '
import re, sys
repeat = int(sys.argv[1])
passed, failed, known = {}, [], set()
for line in sys.stdin:
    line = line.rstrip("\n")
    if line.startswith("run   "):
        known.add(line[6:].strip())
    elif line.startswith("ok  "):
        name = re.sub(r" \(\d+\.\ds\)$", "", line[4:].strip())  # The duration is not part of the name.
        passed[name] = passed.get(name, 0) + 1
    elif line.startswith("FAIL  "):
        failed.append(line[6:])
# A failure belongs to the scenario name it starts with, and the longest one wins.
counts = dict(passed)
for text in failed:
    owners = [name for name in known | set(passed) if text == name or text.startswith(name + ": ")]
    if owners:
        counts.setdefault(max(owners, key=len), 0)
    else:
        # Never passed: the name is what comes before the reason.
        counts.setdefault(text.split(": ", 1)[0], 0)
bad = 0
for name, p in counts.items():
    print(f"passed {p} of {repeat}  {name}")
    bad += p < repeat
sys.exit(1 if bad else 0)
' "$repeat" || result=1
fi
# For poking at the stack by hand: E2E_HOLD=600 keeps it up for ten minutes.
[[ -n ${E2E_HOLD:-} ]] && { echo "holding $base"; sleep "$E2E_HOLD"; }
echo "--- server warnings"
grep -E 'level=(WARN|ERROR)' "$logs/server.log" | sed -E 's/^time=[^ ]+ //' | sort | uniq -c | sort -rn | head -20
if grep -q "$seaweed_secret" "$logs/server.log"; then
  echo "FAIL  the S3 secret appears in the log"; result=1; leaked=1
  bash "$repo/test/report.sh" fail test/e2e/run.sh "$LINENO" "e2e" "the S3 secret appears in the log"
fi
exit $result
